package instance

import (
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file carries the resource support for internal nested-component
// composition (see instantiateNestedInstances in graph.go). A resource DEFINED
// in one nested component and IMPORTED + used by a sibling must have one
// identity across both: the same handle table, the same handle values, and --
// when the importer drops an own<R> it received -- the DEFINER's destructor.
//
// The pieces, all gated behind the composition path so a flat single-component
// instance (and every WASI guest) behaves exactly as before:
//
//   - A single handle table is SHARED by all composed sub-instances, so a
//     handle minted by the definer is directly valid in the importer.
//   - Each sub-instance carries a per-component canonicalizer (resCanon) that
//     maps its LOCAL resource type indices to a GLOBAL id, so the definer's and
//     importer's differing indices for one resource agree on a table tag.
//   - A cross-component func's delegating import presents resources to the
//     importer's host wrapper as opaque u32 (resourcesToU32FD), so the handle
//     passes straight through without being re-minted in a foreign index space.
//   - The definer's destructor is registered on the shared table by global id
//     (handleTable.dtors), and canon resource.drop runs it.

// resolveDefinedResourceDtors resolves the guest destructor for every resource
// this component DEFINES (a type-section ResourceDesc that declares a dtor func
// index), keyed by that resource's TypeSpace definition index. Best-effort: a
// dtor whose core func can't be resolved is skipped rather than failing
// instantiation (it simply won't run on drop). Returns nil when the component
// defines no destructor-bearing resource.
func resolveDefinedResourceDtors(comp *binary.Component, coreFuncTarget func(int) (api.Module, string, error)) map[uint32]func() api.Function {
	var out map[uint32]func() api.Function
	for idx, entry := range comp.TypeSpace {
		if entry.Kind != binary.TypeSpaceDef || int(entry.Def) >= len(comp.Types) {
			continue
		}
		rd, ok := comp.Types[entry.Def].Descriptor.(binary.ResourceDesc)
		if !ok || rd.Dtor == nil {
			continue
		}
		dtorIdx := int(*rd.Dtor)
		if out == nil {
			out = make(map[uint32]func() api.Function)
		}
		// Lazy: the dtor's core module may not be instantiated at registration
		// (it is by drop time -- see handleTable.dtors).
		out[uint32(idx)] = func() api.Function {
			mod, name, err := coreFuncTarget(dtorIdx)
			if err != nil {
				return nil
			}
			return mod.ExportedFunction(name)
		}
	}
	return out
}

// exportedResourceDefs returns, for a component, each resource it EXPORTS by
// name mapped to its canonical definition index (ResourceDefIndex of the type
// the export names). Used to line a provider's exported resources up with an
// importer's imports of the same name.
func exportedResourceDefs(comp *binary.Component) map[string]uint32 {
	var out map[string]uint32
	for _, exp := range comp.Exports {
		if exp.ExternType != 0x03 { // type export
			continue
		}
		if td, err := comp.ResolveType(exp.ExternIndex); err != nil {
			continue
		} else if _, ok := td.(binary.ResourceDesc); !ok {
			continue
		}
		if out == nil {
			out = make(map[string]uint32)
		}
		out[exp.Name] = comp.ResourceDefIndex(exp.ExternIndex)
	}
	return out
}

// importedResourceIndices returns, for a component, each resource it IMPORTS
// through the instance named importName mapped to the local TypeSpace index that
// names it (a type-sort export alias into that imported instance). That index is
// the one the importer's own resource.drop / own<R> / borrow<R> reference.
func importedResourceIndices(comp *binary.Component, importName string) map[string]uint32 {
	var out map[string]uint32
	for idx := range comp.TypeSpace {
		iface, name, ok := resolveImportedResourceName(comp, uint32(idx))
		if !ok || iface != importName {
			continue
		}
		if out == nil {
			out = make(map[string]uint32)
		}
		if _, seen := out[name]; !seen {
			out[name] = uint32(idx)
		}
	}
	return out
}

// translateResourceIdxBase is where synthetic type indices for translated
// own/borrow types start -- well above any real component type index, so the
// custom resolver can tell a synthesized entry from a passed-through provider
// index.
const translateResourceIdxBase = 1 << 24

// translateResourceFD returns a copy of fd with every top-level own<R>/borrow<R>
// param and result re-pointed from the PROVIDER's resource type index to the
// IMPORTER's (via translate, matched by name), plus a resolver that resolves the
// rewritten types. Each translated handle becomes a fresh synthetic type index
// backed by an own<importerIdx>/borrow<importerIdx> the returned resolver
// serves; anything else keeps its provider ref and resolves through
// providerResolve. The importer's host wrapper then mints/looks up the crossing
// handle in ITS OWN table under ITS OWN resource index -- consistent with the
// importer's resource.drop and with per-instance handle numbering.
func translateResourceFD(fd binary.FuncDesc, providerResolve abi.Resolver, translate func(uint32) (uint32, bool)) (binary.FuncDesc, abi.Resolver) {
	synth := map[uint32]binary.TypeDesc{}
	next := uint32(translateResourceIdxBase)
	tr := func(ref binary.TypeRef) binary.TypeRef {
		td, err := resolveTypeRef(&ref, providerResolve)
		if err != nil {
			return ref
		}
		var desc binary.TypeDesc
		switch d := td.(type) {
		case binary.OwnDesc:
			if dIdx, ok := translate(d.ResourceType); ok {
				desc = binary.OwnDesc{ResourceType: dIdx}
			}
		case binary.BorrowDesc:
			if dIdx, ok := translate(d.ResourceType); ok {
				desc = binary.BorrowDesc{ResourceType: dIdx}
			}
		}
		if desc == nil {
			return ref
		}
		idx := next
		next++
		synth[idx] = desc
		return binary.TypeRef{TypeIndex: &idx}
	}
	out := binary.FuncDesc{Params: make([]binary.FuncParam, len(fd.Params))}
	for i, p := range fd.Params {
		out.Params[i] = binary.FuncParam{Name: p.Name, Type: tr(p.Type)}
	}
	if fd.Results.Unnamed != nil {
		r := tr(*fd.Results.Unnamed)
		out.Results.Unnamed = &r
	} else if len(fd.Results.Named) > 0 {
		out.Results.Named = make([]binary.FuncResult, len(fd.Results.Named))
		for i, r := range fd.Results.Named {
			out.Results.Named[i] = binary.FuncResult{Name: r.Name, Type: tr(r.Type)}
		}
	}
	resolve := func(idx uint32) binary.TypeDesc {
		if d, ok := synth[idx]; ok {
			return d
		}
		return providerResolve(idx)
	}
	return out, resolve
}

// outerFuncArgHostImport resolves a `(with "x" (func N))` component instantiate-
// arg to the host import it names: N is a func alias into an imported instance
// (e.g. the host's [constructor]resource1), whose Go implementation the caller
// registered via WithImport. Used to pass a host func into a nested component
// that imports it.
func outerFuncArgHostImport(comp *binary.Component, cfg *config, funcIdx uint32) (*hostImport, error) {
	if int(funcIdx) >= len(comp.ComponentFuncSpace) {
		return nil, fmt.Errorf("func arg index %d out of range of the component func index space", funcIdx)
	}
	e := comp.ComponentFuncSpace[funcIdx]
	if e.Kind != binary.ComponentFuncFromAlias || int(e.Alias) >= len(comp.Aliases) {
		return nil, fmt.Errorf("func arg index %d is not an instance-export alias", funcIdx)
	}
	al := comp.Aliases[e.Alias]
	if al.Sort != 0x01 || al.TargetKind != 0x00 {
		return nil, fmt.Errorf("func arg index %d is a %#x/%#x alias, not a func export alias", funcIdx, al.Sort, al.TargetKind)
	}
	iface, err := importInterfaceName(comp, al.InstanceIdx)
	if err != nil {
		return nil, err
	}
	hi, ok := cfg.imports[mkImportKey(iface, al.Name)]
	if !ok {
		return nil, fmt.Errorf("no host import registered for %q %q", iface, al.Name)
	}
	return hi, nil
}

// importedTypeIndex returns the TypeSpace index of a component's top-level type
// import by name (a `(import "r" (type (sub resource)))`), for wiring a
// `(with "r" (type ...))` instantiate-arg to the nested component's use of it.
func importedTypeIndex(comp *binary.Component, importName string) (uint32, bool) {
	for idx := range comp.TypeSpace {
		e := comp.TypeSpace[idx]
		if e.Kind != binary.TypeSpaceImport || int(e.Import) >= len(comp.Imports) {
			continue
		}
		if im := comp.Imports[e.Import]; im.Name == importName && im.ExternType == 0x03 {
			return uint32(idx), true
		}
	}
	return 0, false
}

// canonTag applies a component's resource-type canonicalizer (identity if nil).
func (in *Instance) canonTag(idx uint32) uint32 {
	if in.resCanon != nil {
		return in.resCanon(idx)
	}
	return idx
}

// repToProviderHandle mints, in the provider's own table, the handle its
// exported func expects for an own<R>/borrow<R> param -- from the rep the
// importer's host wrapper handed across the boundary (it had already reduced its
// own handle to the rep via lift_own/lift_borrow). Non-handle params pass
// through. Mirrors lower_own/lower_borrow into the provider's table.
//
// scope is the reference lower_borrow's minting arm (~1809-1815) call scope
// (Phase 3, docs/component-model-async-phase3-design.md §4.1): the
// composition delegate's borrow handle is minted call-scoped
// (handleTable.NewBorrowScoped) so the callee can drop it and its exit is
// trapped if it doesn't -- but ONLY when the PROVIDER is not itself the
// resource's owning instance. When the provider DOES own the resource
// (sub.isGuestResource(tag) true), this is exactly the reference's
// same-instance exemption (lower_borrow ~1811: `if cx.inst is t.rt.impl:
// return rep` -- cx.inst here IS sub, the callee instance being lowered
// into): the provider's own sync invoke()/resolveArgHandles path
// immediately reduces this same minted handle back to a rep via a
// read-only Rep() lookup (never a drop) for its own guest-owned-resource
// core call, so the callee's core code never even observes a handle to
// drop -- scoping it would trap on EVERY such call. §4.2's #1 hazard is
// this exact case, just reached from the opposite direction (a provider
// FORWARDING a resource it does not itself own, a deeper re-export chain,
// is the only shape that genuinely needs the call-scoped mint below).
func repToProviderHandle(sub *Instance, desc binary.TypeDesc, v abi.Value, scope *task) (abi.Value, error) {
	switch d := desc.(type) {
	case binary.OwnDesc:
		rep, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("delegated own<%d> arg: expected a uint32 rep, got %T", d.ResourceType, v)
		}
		return sub.resources.NewOwn(sub.canonTag(d.ResourceType), rep), nil
	case binary.BorrowDesc:
		rep, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("delegated borrow<%d> arg: expected a uint32 rep, got %T", d.ResourceType, v)
		}
		tag := sub.canonTag(d.ResourceType)
		providerOwnsIt := sub.isGuestResource != nil && sub.isGuestResource(tag)
		if scope != nil && !providerOwnsIt {
			return sub.resources.NewBorrowScoped(tag, rep, scope), nil
		}
		return sub.resources.NewBorrow(tag, rep), nil

	case binary.StreamDesc:
		// The mirror of the TakeOwn/NewOwn lines above: the importer's own
		// host wrapper already reduced its own handle to the *sharedStream
		// identity (resolveHandleArg's StreamDesc case); mint the provider's
		// own readable end for it.
		shared, ok := v.(*sharedStream)
		if !ok {
			return nil, fmt.Errorf("delegated stream arg: expected a *sharedStream, got %T", v)
		}
		return sub.resources.addEntry(&streamEnd{side: sideReadable, state: copyIdle, shared: shared}), nil

	case binary.FutureDesc:
		shared, ok := v.(*sharedFuture)
		if !ok {
			return nil, fmt.Errorf("delegated future arg: expected a *sharedFuture, got %T", v)
		}
		return sub.resources.addEntry(&futureEnd{side: sideReadable, state: copyIdle, shared: shared}), nil

	default:
		return v, nil
	}
}

// providerHandleToRep reduces an own<R>/borrow<R> result the provider returned
// (a handle in ITS table) back to the rep, for the importer's host wrapper to
// lower into the importer's own table (lower_own/lower_borrow). own consumes the
// provider handle (lift_own); borrow only reads it. Non-handle results pass
// through.
func providerHandleToRep(sub *Instance, desc binary.TypeDesc, v abi.Value) (abi.Value, error) {
	switch d := desc.(type) {
	case binary.OwnDesc:
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("delegated own<%d> result: expected a uint32 handle, got %T", d.ResourceType, v)
		}
		return sub.resources.TakeOwn(sub.canonTag(d.ResourceType), h)
	case binary.BorrowDesc:
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("delegated borrow<%d> result: expected a uint32 handle, got %T", d.ResourceType, v)
		}
		return sub.resources.Rep(sub.canonTag(d.ResourceType), h)

	case binary.StreamDesc:
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("delegated stream result: expected a uint32 handle, got %T", v)
		}
		var elemDesc binary.TypeDesc
		if d.Element != nil {
			var eerr error
			if elemDesc, eerr = resolveTypeRef(d.Element, sub.resolve); eerr != nil {
				return nil, fmt.Errorf("delegated stream result: element type: %w", eerr)
			}
		}
		return takeReadableStreamEnd(sub.resources, elemDesc, h)

	case binary.FutureDesc:
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("delegated future result: expected a uint32 handle, got %T", v)
		}
		var elemDesc binary.TypeDesc
		if d.Element != nil {
			var eerr error
			if elemDesc, eerr = resolveTypeRef(d.Element, sub.resolve); eerr != nil {
				return nil, fmt.Errorf("delegated future result: element type: %w", eerr)
			}
		}
		return takeReadableFutureEnd(sub.resources, elemDesc, h)

	default:
		return v, nil
	}
}

// resultDescs returns a boundExport's result TypeDescs (one for the single
// unnamed/first result form these compositions use).
func resultDescs(be *boundExport) []binary.TypeDesc {
	if be.hasResult {
		return []binary.TypeDesc{be.resultType}
	}
	return nil
}

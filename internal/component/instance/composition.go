package instance

import (
	"fmt"
	"reflect"

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

// outerFuncArgImport resolves a `(with "x" (func N))` component instantiate-
// arg to whatever N's alias names: N is a func alias (Sort 0x01, TargetKind
// 0x00 "export") into a component instance, and the component-instance index
// space it's aliased against is combined -- top-level imported instances
// first, then comp.Instances (the same space numImported/byIdx use; see
// instantiateNestedInstances' doc). Two cases fall out of al.InstanceIdx
// against numImported:
//
//   - < numImported: a genuine host import (e.g. the host's
//     [constructor]resource1), whose Go implementation the caller registered
//     via WithImport -- resolved via importInterfaceName exactly as before.
//   - >= numImported: a single named export of an earlier sibling nested
//     component instance (byIdx[InstanceIdx]) -- the async .wast suites'
//     multi-nested-component composition shape uses this to feed one
//     sibling's async-lifted export into a later sibling's plain func
//     import (e.g. async-calls-sync.wast's `(alias export $async_inner1
//     "blocking-call" (func ...))` then `(with "blocking-call" (func N))`).
//     Wired identically to the whole-instance (arg.Sort == 0x05) case one
//     export at a time: a delegatingHostImport plus its guestAsyncTarget.
func outerFuncArgImport(comp *binary.Component, cfg *config, byIdx map[int]*Instance, numImported int, funcIdx uint32) (*hostImport, error) {
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
	if int(al.InstanceIdx) >= numImported {
		sib, ok := byIdx[int(al.InstanceIdx)]
		if !ok {
			return nil, fmt.Errorf("func arg index %d aliases component instance %d, which is not a prior nested instantiation", funcIdx, al.InstanceIdx)
		}
		be, ok := sib.exports[al.Name]
		if !ok {
			return nil, fmt.Errorf("func arg index %d: sibling component instance %d has no export %q", funcIdx, al.InstanceIdx, al.Name)
		}
		// No resource-type wiring for a bare (non-instance) func arg -- a
		// resource crossing this boundary would need its own `(with "r"
		// (type ...))` arg, handled by the arg.Sort == 0x03 case alongside
		// this one; provToImp is only consulted when the func signature
		// itself has an own<R>/borrow<R> param or result.
		provToImp := func(uint32) (uint32, bool) { return 0, false }
		hi := delegatingHostImport(sib, al.Name, be, provToImp)
		hi.asyncTarget = &guestAsyncTarget{sub: sib, be: be, exportName: al.Name, provToImp: provToImp}
		return hi, nil
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

// resourceIdentity is a composition-wide-unique key for one resource: the
// sub-Instance that (as far as this composition's wiring can trace) DEFINES
// it, plus that definer's own canonicalized local tag. Two local resource
// tags -- possibly on two different sub-Instances, possibly reached through
// different alias/import paths -- name the same underlying resource exactly
// when their resourceIdentity values are equal (directly comparable via ==,
// since def is a stable *Instance pointer once instantiated).
type resourceIdentity struct {
	def *Instance
	tag uint32
}

// originOf resolves tag (already canonicalized via in.canonTag) to its
// resourceIdentity: the entry in in.resourceOrigin if this instance received
// the resource through a composition arg, else tag is self-defining (in
// itself is the definer -- covers both a component's own locally-declared
// resource and any tag this mechanism doesn't have further origin info for,
// which is the correct stable identity for either case).
func (in *Instance) originOf(tag uint32) resourceIdentity {
	if o, ok := in.resourceOrigin[tag]; ok {
		return o
	}
	return resourceIdentity{def: in, tag: tag}
}

// elemTypesCompatible compares a stream/future element type carried by two
// (possibly different) instances -- Feature 3, docs/component-model-async-
// final3-fable.md §3.3. aIn/bIn are the instances whose resolver/TypeSpace a/b's
// type indices are relative to (nil for a host-created stream/future).
//
// A plain reflect.DeepEqual on the two binary.TypeDesc values is WRONG for an
// own<R> element the moment R crosses an instance boundary: composition
// (aliasing, re-exporting) gives the SAME underlying resource a different
// LOCAL TypeSpace index on every instance that names it (a producer's own
// $R vs. a consumer's imported alias of it -- see passing-resources.wast),
// so aIn's OwnDesc{ResourceType: 3} and bIn's OwnDesc{ResourceType: 7} can
// name the identical resource despite unequal raw indices.
//
// Only own<R> needs this: stream_builtins.go's resolveStreamOrFutureElem
// bind-time ceiling already guarantees every OTHER element shape reaching
// this compare is entirely handle-free (borrow is rejected anywhere in the
// element; own is rejected below the top level), so DeepEqual is always
// correct for them regardless of which instance defined them -- a
// non-resource type's shape carries no instance-local index that
// composition can rename.
func elemTypesCompatible(aIn *Instance, a binary.TypeDesc, bIn *Instance, b binary.TypeDesc) bool {
	ao, aOk := a.(binary.OwnDesc)
	bo, bOk := b.(binary.OwnDesc)
	if aOk && bOk && aIn != nil && bIn != nil {
		return aIn.originOf(aIn.canonTag(ao.ResourceType)) == bIn.originOf(bIn.canonTag(bo.ResourceType))
	}
	return reflect.DeepEqual(a, b)
}

// repToProviderHandle mints, in the provider's own table, the handle its
// exported func expects for an own<R>/borrow<R> param -- from the rep the
// importer's host wrapper handed across the boundary (it had already reduced its
// own handle to the rep via lift_own/lift_borrow). Non-handle params pass
// through. Mirrors lower_own/lower_borrow into the provider's table.
//
// alreadyProviderRep marks a value repToProviderHandle already reduced to a
// bare host rep because the PROVIDER instance itself owns the resource --
// the reference's lower_borrow same-instance exemption (definitions.py
// ~1811: `if cx.inst is t.rt.impl: return rep`), which mints NO handle at
// all. resolveArgHandlesDepth's BorrowDesc arm (instance.go) checks for this
// wrapper before doing its own handle->rep table lookup, so the provider's
// normal call pipeline doesn't try to treat an already-a-rep value as a
// handle index, AND -- the point that actually matters observably -- so no
// synthetic, permanently-undroppable handle is left sitting in the
// provider's own table (a real, spec-observable difference: passing-
// resources.wast's fail-accessing-res1 probes the provider's table state
// after a same-instance-borrow call exactly like this one, and a leaked
// handle can silently occupy a LATER-freed index, e.g. one Feature 3's own
// per-element stream transfer just freed via TakeOwn, making a supposedly
// removed handle appear to resolve again).
type alreadyProviderRep uint32

// scope is the reference lower_borrow's minting arm (~1809-1815) call scope
// (Phase 3, docs/component-model-async-phase3-design.md §4.1): the
// composition delegate's borrow handle is minted call-scoped
// (handleTable.NewBorrowScoped) so the callee can drop it and its exit is
// trapped if it doesn't -- but ONLY when the PROVIDER is not itself the
// resource's owning instance. When the provider DOES own the resource
// (sub.isGuestResource(tag) true), this is exactly the reference's
// same-instance exemption described above: NO handle is minted at all, the
// rep passes straight through as alreadyProviderRep.
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
		if providerOwnsIt {
			return alreadyProviderRep(rep), nil
		}
		if scope != nil {
			return sub.resources.NewBorrowScoped(tag, rep, scope), nil
		}
		return sub.resources.NewBorrow(tag, rep), nil

	case binary.StreamDesc:
		// Unlike own/borrow above, a stream<T> arg is NOT minted into the
		// provider's table here: the provider's own resolveArgHandles
		// (instance.go), reached moments later via sub.invoke's lowerParams
		// (sync arm) or the async callee's onStart->lowerParams (async arm,
		// startAsyncExportTask), does that same minting itself from the raw
		// *sharedStream identity -- that's the ONE place a readable end is
		// created in the callee's table (mirrors lower_stream). Minting here
		// too would hand that second resolveArgHandles pass an already-a-
		// handle uint32 where it expects the *sharedStream, which is exactly
		// what the "expected a *sharedStream, got uint32" trap catches.
		// So this case only needs to unwrap the *StreamReader the importer's
		// own resolveHandleArg (host_import.go) wrapped the identity in (the
		// shape a real Go AsyncHostFunc consumes, not what this guest<->guest
		// composition path needs) and hand the bare *sharedStream onward.
		switch sv := v.(type) {
		case *sharedStream:
			return sv, nil
		case *StreamReader:
			return sv.shared, nil
		default:
			return nil, fmt.Errorf("delegated stream arg: expected a *sharedStream, got %T", v)
		}

	case binary.FutureDesc:
		// Mirrors the StreamDesc case immediately above -- see its comment.
		switch sv := v.(type) {
		case *sharedFuture:
			return sv, nil
		case *FutureReader:
			return sv.shared, nil
		default:
			return nil, fmt.Errorf("delegated future arg: expected a *sharedFuture, got %T", v)
		}

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
		return takeReadableStreamEnd(sub, sub.resources, elemDesc, h)

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
		return takeReadableFutureEnd(sub, sub.resources, elemDesc, h)

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

package instance

import (
	"fmt"
	"reflect"
	"strings"

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

// checkResourceDtors checks that every destructor comp's resource
// definitions name is a core function of type [rep] -> [], as the component
// model's validator does when the resource is defined; dtorFunc resolves one
// by its core func index once the core instances exporting it exist. wazy
// calls a destructor only when a handle is dropped, so a wrong one would
// otherwise be refused only then, or never. A destructor that does not
// resolve is skipped, as resolveDefinedResourceDtors skips it.
func checkResourceDtors(comp *binary.Component, dtorFunc func(idx uint32) api.Function) error {
	for _, t := range comp.Types {
		rd, ok := t.Descriptor.(binary.ResourceDesc)
		if !ok || rd.Dtor == nil {
			continue
		}
		fn := dtorFunc(*rd.Dtor)
		if fn == nil {
			continue
		}
		rep := api.ValueTypeI32
		if rd.Rep.Primitive == "i64" {
			rep = api.ValueTypeI64
		}
		def := fn.Definition()
		if p := def.ParamTypes(); len(p) != 1 || p[0] != rep || len(def.ResultTypes()) != 0 {
			return fmt.Errorf("component/instance: core function %d has wrong signature for a destructor", *rd.Dtor)
		}
	}
	return nil
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

// exportedInstanceResourceDefs is exportedResourceDefs one level in: it returns
// the resources a component exports as MEMBERS of the instance it exports under
// instanceExportName, each mapped to its canonical definition index. It is what
// an instance-alias instantiate-arg needs, because that arg names an interface
// INSIDE the provider, not the provider's top level.
//
// The distinction is not academic. A wit-component-produced provider has no
// top-level type export at all -- its resource is a top-level type DEFINITION
// re-exported as a member of the exported instance -- so exportedResourceDefs
// returns nil for it, which would silently leave every own<R> crossing the
// composition boundary with no destructor and no resourceOrigin, i.e. tagged
// inconsistently with the provider's own table.
//
// Both instance-definition shapes are read:
//
//   - Kind 0x01, the inline-export form, states the member's type index
//     directly.
//   - Kind 0x00, the instantiate-a-shim form every real wit-component provider
//     emits, states it only indirectly: the shim RE-EXPORTS a type it itself
//     IMPORTS, and the outer component supplies the real definition as a
//     `(with "<the shim's import name>" (type N))` instantiate-arg. So the walk
//     is shim export -> shim TypeSpace entry -> shim import name -> the outer's
//     arg of that name -> the outer's own type index.
//
// A non-resource member (a plain `record`, say) fails the ResourceDesc guard
// and is correctly ignored, as is anything whose type will not resolve.
// Best-effort throughout, exactly like exportedResourceDefs: an unreadable
// member contributes nothing rather than failing instantiation.
//
// One shape it does not read, by construction: a component that does not DEFINE
// the interface at all and only re-exposes a nested sub-instance's --
// `(alias export $p "X" (instance $g))` + `(export "X" (instance $g))`, the
// shape `wac compose` and `wasm-tools compose` emit for a composed component.
// componentInstanceDef returns ok == false for an alias-produced index-space
// slot, so this returns nil, which is the right answer: such a component has no
// instance DEFINITION of its own to read, and its TypeSpace does not contain
// the resource at all.
//
// It is therefore the CALLER's job to resolve the true owner first
// (Instance.reexposedInstances, recorded by bindInstanceExportGraph's
// projection arm) and to call this against the OWNER's comp under the OWNER's
// own export name for the interface -- the two names need not agree, since a
// composer may rename across levels. instantiateNestedInstances' arg.Sort ==
// 0x05 arm does exactly that.
//
// Calling it against the re-exposing component instead is not a
// resource-specific gap, and it does not trap on the first handle to cross:
// the same mistake also hands the re-exposed exports' whole signatures to the
// wrapper's resolver, and instantiation then fails before any guest code runs
// with "type index N not found" -- a message naming neither resources nor
// composition. See boundExport.home and Instance.reexposedInstances for the
// two records that make the owner reachable.
func exportedInstanceResourceDefs(comp *binary.Component, instanceExportName string) map[string]uint32 {
	var localIdx int
	found := false
	for _, exp := range comp.Exports {
		if exp.ExternType != 0x05 || exp.Name != instanceExportName { // instance export
			continue
		}
		if localIdx, found = componentInstanceDef(comp, exp.ExternIndex); found {
			break
		}
	}
	if !found || localIdx >= len(comp.Instances) {
		return nil
	}
	var out map[string]uint32
	add := func(name string, typeIdx uint32) {
		td, err := comp.ResolveType(typeIdx)
		if err != nil {
			return
		}
		if _, isRes := td.(binary.ResourceDesc); !isRes {
			return
		}
		if out == nil {
			out = make(map[string]uint32)
		}
		out[name] = comp.ResourceDefIndex(typeIdx)
	}
	inst := comp.Instances[localIdx]
	switch inst.Kind {
	case 0x01:
		for _, member := range inst.Exports {
			if member.Sort == 0x03 { // type
				add(member.Name, member.SortIdx)
			}
		}
	case 0x00:
		// cfg nil: this is a static shape question, and a shim reached
		// through a component-sort IMPORT is not one this can answer (nor one
		// wit-component emits) -- resolveComponentDef's own documented
		// bind-time behavior.
		shim, err := resolveComponentDef(comp, nil, inst.ComponentIdx)
		if err != nil {
			return nil
		}
		argByName := make(map[string]binary.InstantiateArg, len(inst.Args))
		for _, arg := range inst.Args {
			argByName[arg.Name] = arg
		}
		for _, member := range shim.Exports {
			if member.ExternType != 0x03 || int(member.ExternIndex) >= len(shim.TypeSpace) { // type
				continue
			}
			e := shim.TypeSpace[member.ExternIndex]
			if e.Kind != binary.TypeSpaceImport || int(e.Import) >= len(shim.Imports) {
				continue
			}
			arg, ok := argByName[shim.Imports[e.Import].Name]
			if !ok || arg.Sort != 0x03 { // type
				continue
			}
			add(member.Name, arg.SortIdx)
		}
	}
	return out
}

// importedResourceFirstAlias maps every TypeSpace index of comp that aliases a
// resource out of an imported instance to the FIRST index aliasing the same
// one -- the index importedResourceIndices reports, and so the one the
// composition's tags are keyed by. A component may alias one resource many
// times (wit-component re-aliases wasi:io/streams' output-stream once per
// interface that uses it, and again where its resource.drop is built), and
// every alias must tag the handle table alike, or a handle minted under one
// is refused -- or worse, taken for another resource -- under the next.
func importedResourceFirstAlias(comp *binary.Component) map[uint32]uint32 {
	type key struct{ iface, name string }
	var first map[key]uint32
	var out map[uint32]uint32
	for idx := range comp.TypeSpace {
		iface, name, ok := resolveImportedResourceName(comp, uint32(idx))
		if !ok {
			continue
		}
		if first == nil {
			first, out = map[key]uint32{}, map[uint32]uint32{}
		}
		k := key{iface, name}
		f, seen := first[k]
		if !seen {
			first[k] = uint32(idx)
			continue
		}
		out[uint32(idx)] = f
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

// uninstantiatedTypeExport returns the type of comp that the type export name
// of inst denotes, for an instance that instantiates nothing: an inline
// bundle's member, or what was passed for the type import a pure re-export
// shim's export re-exports.
func uninstantiatedTypeExport(comp *binary.Component, cfg *config, inst *binary.Instance, name string) (uint32, bool) {
	if inst.Kind == 0x01 {
		for _, x := range inst.Exports {
			if x.Name == name && x.Sort == ctSortType {
				return x.SortIdx, true
			}
		}
		return 0, false
	}
	shim, err := resolveComponentDef(comp, cfg, inst.ComponentIdx)
	if err != nil || validateShimComponent(shim) != nil {
		return 0, false
	}
	for _, ex := range shim.Exports {
		if ex.Name != name || ex.ExternType != ctSortType || int(ex.ExternIndex) >= len(shim.TypeSpace) {
			continue
		}
		e := shim.TypeSpace[ex.ExternIndex]
		if e.Kind != binary.TypeSpaceImport || int(e.Import) >= len(shim.Imports) {
			return 0, false
		}
		for _, a := range inst.Args {
			if a.Name == shim.Imports[e.Import].Name && a.Sort == ctSortType {
				return a.SortIdx, true
			}
		}
		return 0, false
	}
	return 0, false
}

// traceResource follows a resource type of comp through the inline bundles
// and pure re-export shims it may have been passed along in, to where it comes
// from. A nested instance defining it is prov, with its canonical index for it
// in idx; an instance comp imports leaves prov nil, with idx comp's alias of
// it. ok is false for a resource comp defines itself.
func traceResource(comp *binary.Component, cfg *config, byIdx map[int]*Instance, typeIdx uint32) (prov *Instance, idx uint32, ok bool) {
	for range instanceArgMaxDepth {
		if int(typeIdx) >= len(comp.TypeSpace) {
			break
		}
		e := comp.TypeSpace[typeIdx]
		if e.Kind != binary.TypeSpaceAlias || int(e.Alias) >= len(comp.Aliases) {
			break
		}
		al := comp.Aliases[e.Alias]
		if al.Sort != ctSortType || al.TargetKind != 0x00 {
			break
		}
		tgt, err := resolveInstanceArgTarget(comp, al.InstanceIdx)
		if err != nil {
			break
		}
		if tgt.imported {
			return nil, typeIdx, true
		}
		if sib, ok := byIdx[int(tgt.spaceIdx)]; ok {
			if len(tgt.projection) == 0 {
				def, ok := exportedResourceDefs(sib.comp)[al.Name]
				return sib, def, ok
			}
			iface := strings.Join(tgt.projection, "#")
			if r, ok := sib.reexposedInstances[iface]; ok {
				sib, iface = r.owner, r.name
			}
			def, ok := exportedInstanceResourceDefs(sib.comp, iface)[al.Name]
			return sib, def, ok
		}
		d, ok := componentInstanceDef(comp, tgt.spaceIdx)
		if !ok || len(tgt.projection) > 0 {
			break
		}
		if typeIdx, ok = uninstantiatedTypeExport(comp, cfg, &comp.Instances[d], al.Name); !ok {
			break
		}
	}
	return nil, 0, false
}

// lineUp records that this component's resource tag names prov's resource
// def: the resource it traces back to, and the destructor its drop runs.
func (n *nesting) lineUp(tag uint32, prov *Instance, def uint32) {
	o := prov.originOf(def)
	if dtor := o.def.resourceDtors[o.tag]; dtor != nil {
		n.importedResDtors[tag] = dtor
	}
	n.resourceOrigin[tag] = o
}

// importerTag maps a resource type of prov's, as prov's func types name it, to
// this component's tag for the same resource.
func (n *nesting) importerTag(prov *Instance) func(uint32) (uint32, bool) {
	return func(idx uint32) (uint32, bool) {
		o := prov.originOf(prov.canonTag(idx))
		for tag, x := range n.resourceOrigin {
			if x == o {
				return tag, true
			}
		}
		return 0, false
	}
}

// instanceArgMaxDepth bounds resolveInstanceArgTarget's export/alias chain
// walk, exactly as resolveInstanceMaxDepth does in the binary package. A
// well-formed binary can only chain forward-declared definitions, so this is a
// defensive guard against a hand-built or corrupt structure, not a real limit.
const instanceArgMaxDepth = 64

// instanceArgTarget is what an instance-sort instantiate-arg index ultimately
// denotes, as resolved by resolveInstanceArgTarget.
type instanceArgTarget struct {
	// imported picks which of the two producers backs this arg, because they
	// are satisfied in completely different ways. An instance the component
	// IMPORTS (imported, importName set) has no runtime object at all -- only
	// the flat name-keyed entries the embedder registered on *config, which
	// forwardImportedInstance re-keys. One the component INSTANTIATES itself
	// (!imported, spaceIdx set) is a sibling *Instance whose exports are
	// wired in as delegating host imports.
	imported   bool
	spaceIdx   uint32 // component-instance index space slot of the DEFINITION (!imported)
	importName string // the outer component's own import name (imported)

	// projection is the chain of instance-typed EXPORT names to project
	// through to reach the denoted instance, base-first: empty for the base
	// instance itself, ["X"] for an `(alias export $base "X" (instance))`,
	// ["X", "Y"] for an alias of that alias.
	//
	// An instance alias INSTANTIATES NOTHING -- per Explainer.md it only
	// introduces an index denoting an instance-typed export of an instance
	// that already exists. So spaceIdx/importName always name the base that
	// does get instantiated, and projection is what has to be looked up
	// INSIDE it. That split is what keeps the two independent names of a
	// composition straight: `(alias export $p "X" (instance $g))` passed as
	// `(with "Y" (instance $g))` means "satisfy the importee's import Y with
	// the provider's export X", and the binary format nowhere requires
	// X == Y (subtyping is checked on the instance TYPE, never on the export
	// name) even though every composer in practice makes them equal.
	projection []string
}

// resolveInstanceArgTarget resolves an index in comp's component-instance index
// space to the instance it denotes, following every producer that space has
// (see componentinstancespace.go):
//
//   - an instance import   -- the answer: {imported, importName}.
//   - an instance definition -- the answer: {!imported, spaceIdx}, spaceIdx
//     being this slot, which is the key componentInstanceSpaceIndices assigns
//     the definition and therefore the key instantiateNestedInstances files
//     the resulting sub-Instance under.
//   - an instance export   -- `(export "E" (instance N))` re-denotes N under a
//     new index without projecting anything, so this just continues at N.
//   - an instance alias    -- `(alias export $p "X" (instance))` denotes $p's
//     instance-typed export "X": record "X" and continue at $p.
//
// Alias names accumulate innermost-first as the walk moves outward toward the
// base, so they are reversed once at the end to yield the base-first order
// projection documents.
//
// It fails loud rather than returning a bogus target: an out-of-range index, a
// malformed entry, an `alias outer` (which names an instance in an ENCLOSING
// component's index space, something this engine does not model), or a chain
// that exceeds instanceArgMaxDepth all return an error naming what was found.
func resolveInstanceArgTarget(comp *binary.Component, idx uint32) (instanceArgTarget, error) {
	if len(comp.ComponentInstanceSpace) == 0 {
		// Hand-built (non-decoded) Component: its instance index space is the
		// flat [imports] ++ [definitions] arithmetic componentInstanceDef
		// falls back to, so an index below the import count is an import and
		// everything else is a local definition at that same slot. No alias
		// is representable in that model, so no projection can arise.
		if int(idx) < numImportedInstances(comp) {
			name, err := importInterfaceName(comp, idx)
			if err != nil {
				return instanceArgTarget{}, err
			}
			return instanceArgTarget{imported: true, importName: name}, nil
		}
		return instanceArgTarget{spaceIdx: idx}, nil
	}
	var projection []string
	reversed := func() []string {
		for i, j := 0, len(projection)-1; i < j; i, j = i+1, j-1 {
			projection[i], projection[j] = projection[j], projection[i]
		}
		return projection
	}
	cur := idx
	for depth := 0; depth < instanceArgMaxDepth; depth++ {
		if int(cur) >= len(comp.ComponentInstanceSpace) {
			return instanceArgTarget{}, fmt.Errorf("instance %d is past the end of the %d-entry component instance index space", cur, len(comp.ComponentInstanceSpace))
		}
		switch e := comp.ComponentInstanceSpace[cur]; e.Kind {
		case binary.ComponentInstanceFromDefinition:
			return instanceArgTarget{spaceIdx: cur, projection: reversed()}, nil
		case binary.ComponentInstanceFromImport:
			if int(e.Import) >= len(comp.Imports) {
				return instanceArgTarget{}, fmt.Errorf("instance %d names import %d, past the end of the %d import(s)", cur, e.Import, len(comp.Imports))
			}
			return instanceArgTarget{imported: true, importName: comp.Imports[e.Import].Name, projection: reversed()}, nil
		case binary.ComponentInstanceFromExport:
			if int(e.Export) >= len(comp.Exports) {
				return instanceArgTarget{}, fmt.Errorf("instance %d names export %d, past the end of the %d export(s)", cur, e.Export, len(comp.Exports))
			}
			cur = comp.Exports[e.Export].ExternIndex
		case binary.ComponentInstanceFromAlias:
			if int(e.Alias) >= len(comp.Aliases) {
				return instanceArgTarget{}, fmt.Errorf("instance %d names alias %d, past the end of the %d alias(es)", cur, e.Alias, len(comp.Aliases))
			}
			al := comp.Aliases[e.Alias]
			if al.Sort != 0x05 { // can't happen from decodeAliasSection, which files the space entry by sort
				return instanceArgTarget{}, fmt.Errorf("instance %d names alias %d, whose sort is %#x rather than instance (0x05)", cur, e.Alias, al.Sort)
			}
			if al.TargetKind != 0x00 {
				return instanceArgTarget{}, fmt.Errorf("instance %d is an alias with target kind %#x; only an instance-export alias (0x00) names an instance this component can resolve", cur, al.TargetKind)
			}
			projection = append(projection, al.Name)
			cur = al.InstanceIdx
		default:
			return instanceArgTarget{}, fmt.Errorf("instance %d has unrecognized index-space kind %d", cur, e.Kind)
		}
	}
	return instanceArgTarget{}, fmt.Errorf("instance %d: the export/alias chain exceeds %d links (cycle?)", idx, instanceArgMaxDepth)
}

// forwardImportedInstance satisfies a nested component's instance import named
// argName with the instance THIS component imports under outerName -- the
// pass-through an `(instantiate $inner (with "host" (instance $host)))` asks
// for, where $host is the outer component's own instance import.
//
// An imported instance is not an object in this runtime: it exists only as the
// flat, name-keyed entries the embedder registered on *config -- host funcs in
// cfg.imports, resource tags in cfg.resourceTags, and the destructors those
// tags key in cfg.hostResDtors. Forwarding it is therefore a re-keying: every
// entry filed under the OUTER component's import name is re-filed under the
// arg name, which by definition is the NESTED component's own import name and
// so is exactly what its lowered aliases will look up (graph.go's canon lower
// resolves through importInterfaceName on the nested component).
//
// Both resource lines are load-bearing. typeArgTags is what makes the nested
// component's own<r>/borrow<r>/resource.drop of the forwarded resource tag the
// shared handle table the way the host does -- it must go through typeArgTags
// rather than subCfg.resourceTags because subCfg.resCanon (always set for a
// nested instantiation) overrides effectiveResourceTypeIdx. subCfg.resourceTags
// is what a THIRD level needs: a grandchild receiving the resource as a
// `(with "r" (type ...))` arg resolves it through effectiveResourceTypeIdx,
// which reads resourceTags.
func forwardImportedInstance(cfg, subCfg *config, nested *binary.Component, outerName, argName string, typeArgTags map[uint32]uint32) {
	// Match on the version-suffix-stripped iface both sides, since that is the
	// normalization every importKey construction goes through (mkImportKey).
	outerIface := mkImportKey(outerName, "").iface
	for k, hi := range cfg.imports {
		if k.iface != outerIface {
			continue
		}
		subCfg.imports[mkImportKey(argName, k.name)] = hi
	}
	importerResIdx := importedResourceIndices(nested, argName)
	for k, tag := range cfg.resourceTags {
		if k.iface != outerIface {
			continue
		}
		if subCfg.resourceTags == nil {
			subCfg.resourceTags = map[importKey]uint32{}
		}
		subCfg.resourceTags[mkImportKey(argName, k.name)] = tag
		if idx, ok := importerResIdx[k.name]; ok {
			typeArgTags[idx] = tag
		}
		if dtor := cfg.hostResDtors[tag]; dtor != nil {
			subCfg.hostResDtors[tag] = dtor
		}
	}
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
// arg to whatever N names: N is USUALLY a func alias (Sort 0x01, TargetKind
// 0x00 "export") into a component instance, and the component-instance index
// space it's aliased against is combined -- top-level imported instances
// first, then comp.Instances (the same space numImported/byIdx use; see
// instantiateNestedInstances' doc). Three cases fall out of what
// al.InstanceIdx names:
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
//   - an inline bundle or a pure re-export shim, never instantiated: N is
//     resolved again as the func the bundle holds or the shim was passed
//     (funcAliasSource).
//
// N can ALSO be the outer component's own directly-declared local func -- a
// plain `canon lift`, never exported, never an alias at all
// (trap-on-reenter.wast's `(func $a async (canon lift ...))` used bare as a
// `(with "a" (func $a))` arg is exactly this shape). componentFunc/
// coreFuncTarget/resolve/abiCache/in (the outer Instance) let that case be
// built exactly like a real export would be (bindFuncExportGraph doesn't
// care whether funcIdx is reached via comp.Exports or an instantiate-arg),
// then delegated to precisely like a sibling's export -- the outer's own
// core func is always ready here (instantiateNestedInstances, this func's
// only caller, runs after the core-instance loop that builds it).
func outerFuncArgImport(comp *binary.Component, cfg *config, in *Instance, nest *nesting, byIdx map[int]*Instance, funcIdx uint32, componentFunc func(uint32) (bool, int, aliasTarget, error), coreFuncTarget func(int) (api.Module, string, error), resolve abi.Resolver) (*hostImport, error) {
	if int(funcIdx) >= len(comp.ComponentFuncSpace) {
		return nil, fmt.Errorf("func arg index %d out of range of the component func index space", funcIdx)
	}
	e := comp.ComponentFuncSpace[funcIdx]
	if e.Kind != binary.ComponentFuncFromAlias {
		isLift, _, _, ferr := componentFunc(funcIdx)
		if ferr != nil {
			return nil, fmt.Errorf("func arg index %d: %w", funcIdx, ferr)
		}
		if !isLift {
			return nil, fmt.Errorf("func arg index %d is not an instance-export alias", funcIdx)
		}
		diagName := fmt.Sprintf("$%d", funcIdx)
		be, berr := bindFuncExportGraph(comp, funcIdx, componentFunc, coreFuncTarget, resolve, diagName, cfg, nil)
		if berr != nil {
			return nil, fmt.Errorf("func arg index %d: %w", funcIdx, berr)
		}
		// This func arg names the outer component's OWN local func, handed
		// down to a directly-nested child as a `(with "x" (func N))`
		// instantiate-arg -- a direct static parent<->child lineage delegate
		// (see hostImport.lineage's doc). Refused at call time, so no handle
		// ever crosses it and its resources need no line-up.
		provToImp := func(uint32) (uint32, bool) { return 0, false }
		hi := delegatingHostImport(newGuestAsyncTarget(in, be, diagName, provToImp))
		hi.lineage = true
		return hi, nil
	}
	if int(e.Alias) >= len(comp.Aliases) {
		return nil, fmt.Errorf("func arg index %d is not an instance-export alias", funcIdx)
	}
	al := comp.Aliases[e.Alias]
	if al.Sort != 0x01 || al.TargetKind != 0x00 {
		return nil, fmt.Errorf("func arg index %d is a %#x/%#x alias, not a func export alias", funcIdx, al.Sort, al.TargetKind)
	}
	// A local instance DEFINITION not instantiated is an inline bundle or a
	// pure re-export shim, which instantiate nothing: the func is what the
	// bundle holds or the shim was passed, an earlier index of this
	// component's. Anything else is a forward reference. Say exactly that,
	// rather than falling through to the imported-instance path below, which
	// would blame the import list for a problem in the instantiation order.
	if _, isDef := componentInstanceDef(comp, al.InstanceIdx); isDef {
		if _, instantiated := byIdx[int(al.InstanceIdx)]; !instantiated {
			src, ok, err := funcAliasSource(comp, cfg, aliasTarget{instIdx: al.InstanceIdx, name: al.Name})
			switch {
			case err != nil:
				return nil, fmt.Errorf("func arg index %d: %w", funcIdx, err)
			case !ok:
				return nil, fmt.Errorf("func arg index %d aliases component instance %d, which is not a prior nested instantiation", funcIdx, al.InstanceIdx)
			case src >= funcIdx:
				return nil, fmt.Errorf("func arg index %d aliases %q of component instance %d, which names func %d, not an earlier one", funcIdx, al.Name, al.InstanceIdx, src)
			}
			return outerFuncArgImport(comp, cfg, in, nest, byIdx, src, componentFunc, coreFuncTarget, resolve)
		}
	}
	// A prior nested instantiation names a sibling; anything else is an
	// imported instance, resolved through importInterfaceName below.
	// resolveFuncAliasInstance is the resolution the func-EXPORT side uses
	// (bindFuncExportGraph), shared because the operand is the same thing on
	// both sides: a raw component-instance index-space slot, which besides a
	// definition can be an alias of its own -- `(alias export $p "iface"
	// (instance $g))` then `(alias export $g "f" (func))` -- or an
	// export-produced slot, neither of which byIdx (keyed by definition slots)
	// has an entry for. It also resolves that space through
	// componentInstanceDef rather than numImported arithmetic, since an
	// interleaved instance export shifts every later definition.
	if sib, key := resolveFuncAliasInstance(comp, aliasTarget{instIdx: al.InstanceIdx, name: al.Name}, byIdx); sib != nil {
		be, ok := sib.exports[key]
		if !ok {
			return nil, fmt.Errorf("func arg index %d: sibling component instance %d has no export %q", funcIdx, al.InstanceIdx, key)
		}
		// The sibling may itself only RE-EXPOSE the aliased export, in which
		// case the call belongs to whatever instance defines it -- see
		// boundExport.home and ownerOf. A handle crossing it is retagged to
		// the nested component's resource lined up with the owner's.
		owner, ownerKey := ownerOf(be, sib, key)
		hi := delegatingHostImport(newGuestAsyncTarget(owner, be, ownerKey, nest.importerTag(owner)))
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

// toProvider maps an importer-side arg list, whose handles the importer's
// wrapper already reduced to reps, to the provider's: each own and borrow, at
// any depth, is minted in the provider's table (repToProviderHandle), a borrow
// scoped to scope.
func (tgt *guestAsyncTarget) toProvider(args []abi.Value, scope *task) ([]abi.Value, error) {
	paramDescs := tgt.be.paramTypes
	out := make([]abi.Value, len(args))
	for i, a := range args {
		if i < len(paramDescs) {
			var err error
			if a, err = repToProviderHandle(tgt.sub, paramDescs[i], a, scope); err != nil {
				return nil, err
			}
			if n := tgt.nested; n != nil && n.params[i] != nil {
				a, err = walkHandles(a, n.params[i], n.resolve, 0, func(t binary.TypeDesc, v abi.Value) (abi.Value, error) {
					return repToProviderHandle(tgt.sub, t, v, scope)
				})
				if err != nil {
					return nil, err
				}
			}
		}
		out[i] = a
	}
	return out, nil
}

// fromProvider reduces the handles in the provider's results, at any depth,
// back to reps (providerHandleToRep), in place, for the importer's wrapper to
// mint in its own table.
func (tgt *guestAsyncTarget) fromProvider(vals []abi.Value) error {
	if !tgt.be.hasResult || len(vals) == 0 {
		return nil
	}
	v, err := providerHandleToRep(tgt.sub, tgt.be.resultType, vals[0])
	if err != nil {
		return err
	}
	if n := tgt.nested; n != nil && n.results[0] != nil {
		v, err = walkHandles(v, n.results[0], n.resolve, 0, func(t binary.TypeDesc, v abi.Value) (abi.Value, error) {
			return providerHandleToRep(tgt.sub, t, v)
		})
		if err != nil {
			return err
		}
	}
	vals[0] = v
	return nil
}

// resultDescs returns a boundExport's result TypeDescs (one for the single
// unnamed/first result form these compositions use).
func resultDescs(be *boundExport) []binary.TypeDesc {
	if be.hasResult {
		return []binary.TypeDesc{be.resultType}
	}
	return nil
}

// componentExportsInstance reports whether comp declares an instance-typed
// export named name.
//
// It exists to tell two cases apart at a projection site: a provider that has
// no such export at all (a real error) and one whose exported interface simply
// carries no functions -- an `interface` declaring only a record or a resource,
// which is ordinary WIT. Only func exports become "iface#member" keys in
// buildInstanceExportIndex, so a types-only interface is absent from
// instanceExports even though the component genuinely exports it, and treating
// that absence as "no such export" made every composition over such an
// interface fail to instantiate.
func componentExportsInstance(comp *binary.Component, name string) bool {
	if comp == nil {
		return false
	}
	for i := range comp.Exports {
		if comp.Exports[i].Name == name && comp.Exports[i].ExternType == 0x05 {
			return true
		}
	}
	return false
}

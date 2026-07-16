package instance

import (
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
func resolveDefinedResourceDtors(comp *binary.Component, coreFuncTarget func(int) (api.Module, string, error)) map[uint32]api.Function {
	var out map[uint32]api.Function
	for idx, entry := range comp.TypeSpace {
		if entry.Kind != binary.TypeSpaceDef || int(entry.Def) >= len(comp.Types) {
			continue
		}
		rd, ok := comp.Types[entry.Def].Descriptor.(binary.ResourceDesc)
		if !ok || rd.Dtor == nil {
			continue
		}
		mod, name, err := coreFuncTarget(int(*rd.Dtor))
		if err != nil {
			continue
		}
		fn := mod.ExportedFunction(name)
		if fn == nil {
			continue
		}
		if out == nil {
			out = make(map[uint32]api.Function)
		}
		out[uint32(idx)] = fn
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

// resourcesToU32FD returns a copy of fd with every top-level own<R>/borrow<R>
// param and result replaced by a plain u32, so a delegating import's host
// wrapper treats a resource handle as an opaque integer and passes it through
// (the shared handle table makes the provider's handle directly valid in the
// importer) instead of re-minting it under a foreign resource index. Non-
// resource types are left untouched. resolve identifies which refs are handles.
func resourcesToU32FD(fd binary.FuncDesc, resolve abi.Resolver) binary.FuncDesc {
	u32 := func(ref binary.TypeRef) binary.TypeRef {
		if isHandleRef(ref, resolve) {
			return binary.TypeRef{Primitive: "u32"}
		}
		return ref
	}
	out := binary.FuncDesc{Params: make([]binary.FuncParam, len(fd.Params))}
	for i, p := range fd.Params {
		out.Params[i] = binary.FuncParam{Name: p.Name, Type: u32(p.Type)}
	}
	if fd.Results.Unnamed != nil {
		r := u32(*fd.Results.Unnamed)
		out.Results.Unnamed = &r
	} else if len(fd.Results.Named) > 0 {
		out.Results.Named = make([]binary.FuncResult, len(fd.Results.Named))
		for i, r := range fd.Results.Named {
			out.Results.Named[i] = binary.FuncResult{Name: r.Name, Type: u32(r.Type)}
		}
	}
	return out
}

// isHandleRef reports whether a TypeRef resolves to an own<R>/borrow<R> handle.
func isHandleRef(ref binary.TypeRef, resolve abi.Resolver) bool {
	td, err := resolveTypeRef(&ref, resolve)
	if err != nil {
		return false
	}
	switch td.(type) {
	case binary.OwnDesc, binary.BorrowDesc:
		return true
	default:
		return false
	}
}

// resBaseStride spaces each sub-instance's global resource ids apart so a
// component's local resource indices (small) never collide with another's after
// the base offset is applied. Well above any real per-component type-index
// count and below the synthetic WASI resource constants.
const resBaseStride = 0x1_0000

// makeResCanon builds a component's local-resource-index -> global-id
// canonicalizer for a composition. base offsets this component's own defined
// resources; importedGlobalID maps a resource this component imports (by the
// interface+name it imports it under) to the global id the DEFINING sibling
// already uses. comp is needed to tell an import from a local definition and to
// reduce aliases to a definition index.
func makeResCanon(comp *binary.Component, base uint32, importedGlobalID map[importKey]uint32) func(uint32) uint32 {
	return func(localIdx uint32) uint32 {
		if iface, name, ok := resolveImportedResourceName(comp, localIdx); ok {
			if gid, ok := importedGlobalID[mkImportKey(iface, name)]; ok {
				return gid
			}
		}
		return base + comp.ResourceDefIndex(localIdx)
	}
}

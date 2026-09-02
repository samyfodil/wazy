package binary

import "fmt"

// This file implements the CORE MODULE index space, the core-module-sort
// counterpart of typespace.go's type index space.
//
// Per the component-model binary format, a core:instance's `instantiate
// <core:moduleidx>` names an index in the component's core module index
// space, which is populated -- in declaration order across sections -- by:
//   - an embedded core module (section 1)
//   - a core-module ALIAS (section 6, sort core 0x00 / core:sort module 0x11)
//
// Only the `outer` form of that alias exists in practice: Explainer.md's
// outeraliassort grammar lists `core module` among the four sorts an
// `alias outer <count> <idx>` may name, and there is no other way to obtain a
// core module index (a core module is not an instance export, and there is no
// core-module import in the component model -- a component imports a
// *component* or an *instance*, never a bare core module).
//
// Equating the space with CoreModules (section 1) alone is what made
// fused.23.wast fail to instantiate: its two nested components each reach the
// PARENT's core module by `(alias outer 1 $m (core module))`, and one of them
// (`$c`) declares that alias AFTER its own embedded `$shim2`, so the aliased
// module is index 1 while `$shim2` is index 0. There is no "inheritance" of
// the parent's space to model here -- the child names the parent's definition
// explicitly, and the alias occupies a slot in the CHILD's own space.
//
// Like TypeSpace / CoreFuncSpace / ComponentFuncSpace / ComponentInstanceSpace,
// this is built incrementally by decodeComponent in file order, so it stays
// correct when sections interleave or repeat.

// CoreModuleSpaceEntryKind distinguishes what produced a core module index
// space entry.
type CoreModuleSpaceEntryKind byte

const (
	// CoreModuleFromDefinition: an embedded core module (section 1).
	CoreModuleFromDefinition CoreModuleSpaceEntryKind = iota
	// CoreModuleFromAlias: a core-module alias (section 6, Sort == 0x00 core,
	// CoreSort == 0x11 module).
	CoreModuleFromAlias
)

// CoreModuleSpaceEntry is one entry in the component's core module index
// space. Exactly one of Module/Alias is meaningful, selected by Kind.
type CoreModuleSpaceEntry struct {
	Kind   CoreModuleSpaceEntryKind
	Module uint32 // index into Component.CoreModules (Kind == CoreModuleFromDefinition)
	Alias  uint32 // index into Component.Aliases     (Kind == CoreModuleFromAlias)
}

// resolveCoreModuleMaxDepth bounds ResolveCoreModule's alias-chain walk. A
// chain is only as long as the nesting depth (an outer alias of an outer
// alias), so this is a defensive guard against a corrupt structure, not a
// real limit.
const resolveCoreModuleMaxDepth = 64

// ResolveCoreModule resolves an index in this component's core module index
// space to the component that actually EMBEDS the module and that component's
// index into its own CoreModules. The owner is not always c: an `alias outer`
// names a module embedded by an enclosing component, whose bytes live in the
// enclosing component's own Bytes (CoreModule.Offset/Size index into it), so
// callers need the owner, not just an index.
//
// For a Component that was not produced by Decode (CoreModuleSpace left empty
// -- the common shape for hand-built binary.Component values in tests), idx is
// resolved directly against CoreModules with c as the owner, matching the
// behavior this package had before the space existed. Same convention as
// TypeSpace / CoreFuncSpace.
func (c *Component) ResolveCoreModule(idx uint32) (owner *Component, moduleIdx uint32, err error) {
	if len(c.CoreModuleSpace) == 0 {
		if int(idx) >= len(c.CoreModules) {
			return nil, 0, fmt.Errorf("core module %d out of range of %d modules", idx, len(c.CoreModules))
		}
		return c, idx, nil
	}
	cur := c
	for depth := 0; depth < resolveCoreModuleMaxDepth; depth++ {
		if int(idx) >= len(cur.CoreModuleSpace) {
			return nil, 0, fmt.Errorf("core module %d out of range of the %d-entry core module index space", idx, len(cur.CoreModuleSpace))
		}
		e := cur.CoreModuleSpace[idx]
		switch e.Kind {
		case CoreModuleFromDefinition:
			if int(e.Module) >= len(cur.CoreModules) {
				return nil, 0, fmt.Errorf("core module %d: internal error: module index %d out of range of %d embedded modules", idx, e.Module, len(cur.CoreModules))
			}
			return cur, e.Module, nil
		case CoreModuleFromAlias:
			if int(e.Alias) >= len(cur.Aliases) {
				return nil, 0, fmt.Errorf("core module %d: internal error: alias index %d out of range of %d aliases", idx, e.Alias, len(cur.Aliases))
			}
			al := cur.Aliases[e.Alias]
			if al.TargetKind != 0x02 { // outer
				// A core module can only be obtained by `alias outer`; an
				// export alias (0x00/0x01) of a core module is not a thing
				// the grammar admits. decodeAliasSection does not itself
				// reject the combination, so fail loud rather than mis-index.
				return nil, 0, fmt.Errorf("core module %d: alias target kind %#x cannot name a core module (only `alias outer` can)", idx, al.TargetKind)
			}
			// The de Bruijn count is the number of enclosing components to
			// skip; 0 means this same component (Explainer.md permits it),
			// which is just another index in cur's own space.
			for i := uint32(0); i < al.OuterCount; i++ {
				if cur.Outer == nil {
					return nil, 0, fmt.Errorf("core module %d: outer alias (count=%d) targets %d enclosing component(s), but only %d are known (this component was decoded standalone, or is hand-built)", idx, al.OuterCount, al.OuterCount, i)
				}
				cur = cur.Outer
			}
			idx = al.OuterIndex
		default:
			return nil, 0, fmt.Errorf("core module %d: unknown core-module-space entry kind %d", idx, e.Kind)
		}
	}
	return nil, 0, fmt.Errorf("core module %d: alias chain exceeds depth %d (cycle?)", idx, resolveCoreModuleMaxDepth)
}

// CoreModuleSpaceLen is the number of indices in the core module index space,
// applying ResolveCoreModule's hand-built fallback (a Component that never
// went through Decode has no space, and its CoreModules IS its space).
func (c *Component) CoreModuleSpaceLen() int {
	if len(c.CoreModuleSpace) == 0 {
		return len(c.CoreModules)
	}
	return len(c.CoreModuleSpace)
}

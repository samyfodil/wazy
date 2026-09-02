package binary

import "fmt"

// This file implements the COMPONENT index space -- the space an instance
// definition's `(instantiate <componentidx> ...)` names, and the last of the
// component-level index spaces this package models (see typespace.go,
// corefuncspace.go, componentfuncspace.go, componentinstancespace.go,
// coremodulespace.go).
//
// Per the component-model binary format it is populated, in declaration order
// across sections, by every definition that yields a component:
//   - a component import      (section 10, externdesc sort component 0x04)
//   - a nested component      (section 4)
//   - a component alias       (section 6, Sort == 0x04) -- `alias outer`
//   - a component export      (section 11, externdesc sort component 0x04),
//     since "all exports (of all sorts) introduce a new index that aliases the
//     exported definition"
//
// Equating the space with NestedComponents (section 4) alone is what made
// types.11.wast fail to instantiate: its nested `$c` defines NO nested
// component of its own, imports one (`(import "x" (component ...))`) and then
// does `(instantiate 0)` -- index 0 being that import. A component-sort import
// is a component DEFINITION (code) the embedder supplies, not an instance: the
// importing component instantiates it itself, with its own args, at its own
// instance section. See internal/component/instance's config.componentArgs.
type ComponentSpaceEntryKind byte

const (
	// ComponentFromImport: a component import (section 10, sort component 0x04).
	ComponentFromImport ComponentSpaceEntryKind = iota
	// ComponentFromDefinition: a nested component (section 4).
	ComponentFromDefinition
	// ComponentFromAlias: a component alias (section 6, Sort == 0x04).
	ComponentFromAlias
	// ComponentFromExport: a component export (section 11, sort component
	// 0x04) -- an alias of the exported component into the next component index.
	ComponentFromExport
)

// ComponentSpaceEntry is one entry in the component index space. Exactly one
// of Import/Component/Alias/Export is meaningful, selected by Kind.
type ComponentSpaceEntry struct {
	Kind      ComponentSpaceEntryKind
	Import    uint32 // index into Component.Imports          (ComponentFromImport)
	Component uint32 // index into Component.NestedComponents (ComponentFromDefinition)
	Alias     uint32 // index into Component.Aliases          (ComponentFromAlias)
	Export    uint32 // index into Component.Exports          (ComponentFromExport)
}

// resolveComponentMaxDepth bounds ResolveComponent's alias/export chain walk,
// defensively, the same way resolveInstanceMaxDepth does for instances.
const resolveComponentMaxDepth = 64

// ResolveComponent resolves an index in this component's component index space
// to the component DEFINITION it names.
//
// Exactly one of def/importName is set on success. def is the decoded nested
// component (possibly one an ENCLOSING component defines, when the index is
// reached through an `alias outer`). importName is the name of a component
// IMPORT: the definition is not in this binary at all, and the caller must
// look up what the embedder supplied for that name -- see
// internal/component/instance's config.componentArgs.
//
// For a Component that was not produced by Decode (ComponentSpace left empty
// -- the hand-built shape in tests), idx is resolved directly against
// NestedComponents, matching this package's behavior before the space existed.
func (c *Component) ResolveComponent(idx uint32) (def *Component, importName string, err error) {
	if len(c.ComponentSpace) == 0 {
		if int(idx) >= len(c.NestedComponents) {
			return nil, "", fmt.Errorf("component %d out of range of %d nested components", idx, len(c.NestedComponents))
		}
		return c.NestedComponents[idx], "", nil
	}
	cur := c
	for depth := 0; depth < resolveComponentMaxDepth; depth++ {
		if int(idx) >= len(cur.ComponentSpace) {
			return nil, "", fmt.Errorf("component %d out of range of the %d-entry component index space", idx, len(cur.ComponentSpace))
		}
		e := cur.ComponentSpace[idx]
		switch e.Kind {
		case ComponentFromDefinition:
			if int(e.Component) >= len(cur.NestedComponents) {
				return nil, "", fmt.Errorf("component %d: internal error: nested component index %d out of range of %d", idx, e.Component, len(cur.NestedComponents))
			}
			return cur.NestedComponents[e.Component], "", nil
		case ComponentFromImport:
			if int(e.Import) >= len(cur.Imports) {
				return nil, "", fmt.Errorf("component %d: internal error: import index %d out of range of %d imports", idx, e.Import, len(cur.Imports))
			}
			if cur != c {
				// An enclosing component's own component IMPORT, reached
				// through an outer alias. Satisfying it would need that
				// component's instantiate-args, not this one's, and no
				// producer emits this shape; fail loud rather than silently
				// resolving against the wrong embedder.
				return nil, "", fmt.Errorf("component %d: outer alias names an enclosing component's component import %q, which is not supported", idx, cur.Imports[e.Import].Name)
			}
			return nil, cur.Imports[e.Import].Name, nil
		case ComponentFromExport:
			if int(e.Export) >= len(cur.Exports) {
				return nil, "", fmt.Errorf("component %d: internal error: export index %d out of range of %d exports", idx, e.Export, len(cur.Exports))
			}
			idx = cur.Exports[e.Export].ExternIndex
		case ComponentFromAlias:
			if int(e.Alias) >= len(cur.Aliases) {
				return nil, "", fmt.Errorf("component %d: internal error: alias index %d out of range of %d aliases", idx, e.Alias, len(cur.Aliases))
			}
			al := cur.Aliases[e.Alias]
			if al.TargetKind != 0x02 { // outer
				// `component` is one of the four outeraliassort sorts; an
				// export alias naming a component is not in the grammar.
				return nil, "", fmt.Errorf("component %d: alias target kind %#x cannot name a component (only `alias outer` can)", idx, al.TargetKind)
			}
			for i := uint32(0); i < al.OuterCount; i++ {
				if cur.Outer == nil {
					return nil, "", fmt.Errorf("component %d: outer alias (count=%d) targets %d enclosing component(s), but only %d are known (this component was decoded standalone, or is hand-built)", idx, al.OuterCount, al.OuterCount, i)
				}
				cur = cur.Outer
			}
			idx = al.OuterIndex
		default:
			return nil, "", fmt.Errorf("component %d: unknown component-space entry kind %d", idx, e.Kind)
		}
	}
	return nil, "", fmt.Errorf("component %d: alias chain exceeds depth %d (cycle?)", idx, resolveComponentMaxDepth)
}

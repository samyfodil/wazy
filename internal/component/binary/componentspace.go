package binary

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
// instance section. internal/component/instance resolves an index against what
// the instantiation was given (its scope).
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

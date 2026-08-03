package binary

// This file implements the COMPONENT-level instance index space, the
// instance-sort counterpart of componentfuncspace.go's func index space.
//
// Per the component-model binary format, the component instance index space is
// populated, in declaration order across sections, by every definition that
// yields a component instance:
//   - an instance import   (section 10, externdesc sort instance 0x05)
//   - an instance definition (section 5) -- either `(instantiate <componentidx>
//     <instantiatearg>*)` or the inline-export form
//   - an instance alias    (section 6, Sort == 0x05)
//   - an instance export   (section 11, externdesc sort instance 0x05) --
//     Binary.md is explicit that "all exports (of all sorts) introduce a new
//     index that aliases the exported definition and can be used by all
//     subsequent definitions just like an alias", so exporting an instance
//     shifts every LATER instance definition's index.
//
// That last producer is why the space has to exist. wit-component emits a
// component's exported interfaces as an interleaved
// `(instance ...) (export ...) (instance ...) (export ...)` run -- a real
// componentize-py CPython component does exactly this -- so the flat
// `exp.ExternIndex - numImportedInstances` arithmetic instance.go used lands
// one slot short for every export after the first, either binding the wrong
// nested instance or failing outright with an out-of-range error.
//
// Like TypeSpace / CoreFuncSpace / ComponentFuncSpace, this is built
// incrementally by decodeComponent in file order, so it stays correct even
// when sections interleave or repeat.

// ComponentInstanceSpaceEntryKind distinguishes what produced a component
// instance index-space entry.
type ComponentInstanceSpaceEntryKind byte

const (
	// ComponentInstanceFromImport: an instance import (section 10, sort instance).
	ComponentInstanceFromImport ComponentInstanceSpaceEntryKind = iota
	// ComponentInstanceFromDefinition: an instance definition (section 5).
	ComponentInstanceFromDefinition
	// ComponentInstanceFromAlias: an instance alias (section 6, Sort == 0x05).
	ComponentInstanceFromAlias
	// ComponentInstanceFromExport: an instance export (section 11, sort
	// instance) -- an alias of the exported instance into the next instance
	// index.
	ComponentInstanceFromExport
)

// ComponentInstanceSpaceEntry is one entry in the component's instance index
// space. Exactly one of Import/Instance/Alias/Export is meaningful, selected by
// Kind.
type ComponentInstanceSpaceEntry struct {
	Kind     ComponentInstanceSpaceEntryKind
	Import   uint32 // index into Component.Imports   (Kind == ComponentInstanceFromImport)
	Instance uint32 // index into Component.Instances (Kind == ComponentInstanceFromDefinition)
	Alias    uint32 // index into Component.Aliases   (Kind == ComponentInstanceFromAlias)
	Export   uint32 // index into Component.Exports   (Kind == ComponentInstanceFromExport)
}

// resolveInstanceMaxDepth bounds ResolveComponentInstance's export-alias chain
// walk. A well-formed binary can only ever chain forward-declared exports, so
// this is a defensive guard against a hand-built or corrupt structure, not a
// real limit.
const resolveInstanceMaxDepth = 64

// ResolveComponentInstance resolves a component instance index to the index in
// Component.Instances of the instance DEFINITION it ultimately names, following
// export aliases (an `(export "x" (instance N))` both re-exports instance N and
// introduces a new instance index aliasing it).
//
// ok is false when the index is out of range, names an imported instance, or
// names an alias this decoder does not resolve structurally (an outer alias, or
// an alias of another instance's instance-typed export). Callers that need to
// distinguish those cases should inspect ComponentInstanceSpace directly.
//
// It returns (0, false) for a component that was not produced by Decode: a
// hand-built Component has no instance index space at all, and the caller is
// expected to fall back to its own arithmetic (see instance.go's
// componentInstanceDef).
func (c *Component) ResolveComponentInstance(idx uint32) (int, bool) {
	for depth := 0; depth < resolveInstanceMaxDepth; depth++ {
		if int(idx) >= len(c.ComponentInstanceSpace) {
			return 0, false
		}
		e := c.ComponentInstanceSpace[idx]
		switch e.Kind {
		case ComponentInstanceFromDefinition:
			if int(e.Instance) >= len(c.Instances) {
				return 0, false
			}
			return int(e.Instance), true
		case ComponentInstanceFromExport:
			if int(e.Export) >= len(c.Exports) {
				return 0, false
			}
			idx = c.Exports[e.Export].ExternIndex
		default: // import or alias: not an instance definition of this component
			return 0, false
		}
	}
	return 0, false
}

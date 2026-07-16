package binary

// This file implements the COMPONENT-level func index space, distinct from the
// core func index space (corefuncspace.go) and from any core module's own
// function space.
//
// Per the component-model binary format, the component func index space is
// populated, in declaration order across sections, by every definition that
// yields a component func:
//   - a func import        (section 10, externdesc sort func 0x01)
//   - a func alias         (section 6, Sort == 0x01)
//   - a canon lift         (section 8, Kind == 0x00)
//   - a func export        (section 11, externdesc sort func 0x01) -- exporting
//     a func creates a NEW func-index entry aliasing the exported func, so
//     exports interleaved between lifts (exactly what wit-bindgen/cargo-component
//     emit: `(func (;15;) (canon lift ...))` immediately followed by
//     `(export (;16;) "name" (func 15))`) shift every subsequent lift's index.
//
// These producers interleave arbitrarily, which the decoder's flat
// Imports/Aliases/Canons/Exports slices cannot reconstruct on their own (order
// is preserved within each slice, not across them). ComponentFuncSpace is built
// incrementally by decodeComponent in file order (mirroring CoreFuncSpace /
// TypeSpace), so it stays correct even when sections interleave or repeat.
//
// Before this existed, instance.go reconstructed the space ad-hoc as
// [func aliases] ++ [canon lifts], ignoring func imports and -- critically --
// the export-created entries, so a component whose exports interleave with its
// lifts (standard cargo-component output) had every export bound to the WRONG
// lift.

// ComponentFuncSpaceEntryKind distinguishes what produced a component func
// index-space entry.
type ComponentFuncSpaceEntryKind byte

const (
	// ComponentFuncFromImport: a func import (section 10, sort func).
	ComponentFuncFromImport ComponentFuncSpaceEntryKind = iota
	// ComponentFuncFromAlias: a func alias (section 6, Sort == 0x01).
	ComponentFuncFromAlias
	// ComponentFuncFromCanonLift: a canon lift (section 8, Kind == 0x00).
	ComponentFuncFromCanonLift
	// ComponentFuncFromExport: a func export (section 11, sort func) -- an
	// alias of the exported func into the next func index.
	ComponentFuncFromExport
)

// ComponentFuncSpaceEntry is one entry in the component's func index space.
// Exactly one of Import/Alias/Canon/Export is meaningful, selected by Kind.
type ComponentFuncSpaceEntry struct {
	Kind   ComponentFuncSpaceEntryKind
	Import uint32 // index into Component.Imports (Kind == ComponentFuncFromImport)
	Alias  uint32 // index into Component.Aliases  (Kind == ComponentFuncFromAlias)
	Canon  uint32 // index into Component.Canons   (Kind == ComponentFuncFromCanonLift)
	Export uint32 // index into Component.Exports  (Kind == ComponentFuncFromExport)
}

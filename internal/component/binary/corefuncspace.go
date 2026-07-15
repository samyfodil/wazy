package binary

// This file implements the component's core FUNC index space, as distinct
// from any one core module's own (per-module) function index space and from
// the component-level func index space (which TypeSpace-style aliasing does
// not cover -- see instance.go's compFuncAliases/liftCanonIdxs).
//
// Per the component-model binary format, the core func index space is
// populated, in declaration order, by every core-level func alias (section
// 6, Sort == 0x00 core, CoreSort == 0x00 func) AND every canon that produces
// a brand new core func: lower (0x01), resource.new (0x02), resource.drop
// (0x03), resource.rep (0x04) (section 8). These two producers can interleave
// arbitrarily -- e.g. a canon resource.drop can sit between two core-func
// aliases that were declared before and after it -- which the decoder's flat
// Aliases and Canons slices, each populated independently by their own
// section case in decodeComponent, cannot reconstruct on their own (they
// preserve order *within* each slice, but not the cross-slice interleaving).
// CoreFuncSpace is built incrementally by decodeComponent as it walks
// sections in file order (mirroring TypeSpace's approach in typespace.go),
// so it stays correct even when alias and canon sections interleave or
// repeat.
//
// Core memory/table/global index spaces do not need an analogous structure:
// no canon ever produces a memory, table, or global, so filtering Aliases by
// CoreSort in the slice's own (already section-order-preserving) order is
// sufficient for those three sorts.

// CoreFuncSpaceEntryKind distinguishes what produced a core func index-space
// entry.
type CoreFuncSpaceEntryKind byte

const (
	// CoreFuncFromAlias marks an entry produced by a core-level func alias
	// (section 6, Sort == 0x00, CoreSort == 0x00).
	CoreFuncFromAlias CoreFuncSpaceEntryKind = iota
	// CoreFuncFromCanon marks an entry produced by a canon that yields a new
	// core func: lower (0x01), resource.new (0x02), resource.drop (0x03), or
	// resource.rep (0x04).
	CoreFuncFromCanon
)

// CoreFuncSpaceEntry is one entry in the component's core func index space.
// Exactly one of Alias, Canon is meaningful, selected by Kind.
type CoreFuncSpaceEntry struct {
	Kind CoreFuncSpaceEntryKind

	// Alias is the index into Component.Aliases, valid when
	// Kind == CoreFuncFromAlias.
	Alias uint32

	// Canon is the index into Component.Canons, valid when
	// Kind == CoreFuncFromCanon.
	Canon uint32
}

package binary

import (
	"reflect"
	"testing"
)

// ResolveType hoists the common case -- a frozen component resolving a plain
// type-section deftype -- out of resolveTypeDepth so it makes no call. Two
// implementations of one lookup is exactly how they drift, so pin them
// together: across every index shape, the fast path must agree with the walk
// it shortcuts, value and error alike.
func TestResolveTypeFastPathMatchesWalk(t *testing.T) {
	build := func() *Component {
		return &Component{
			typesFrozen: true,
			Types: []Type{
				{Index: 0, Kind: "u32", Descriptor: PrimitiveDesc{Prim: "u32"}},
				{Index: 1, Kind: "string", Descriptor: PrimitiveDesc{Prim: "string"}},
			},
			TypeSpace: []TypeSpaceEntry{
				{Kind: TypeSpaceDef, Def: 0},
				{Kind: TypeSpaceDef, Def: 1},
				{Kind: TypeSpaceDef, Def: 99},      // deftype index past Types
				{Kind: TypeSpaceExport, Export: 0}, // not a deftype: must take the walk
				{Kind: TypeSpaceEntryKind(200)},    // unknown kind
			},
			Exports: []Export{{Name: "t", ExternType: 0x03, ExternIndex: 1}},
		}
	}

	for _, idx := range []uint32{
		0, 1, // plain deftypes -- the fast path
		2,     // deftype index out of range of Types
		3,     // an export alias: resolves, but only via the walk
		4,     // unknown entry kind
		5, 99, // past the end of TypeSpace
		extraTypeBase, // the escape range, with an empty table
		1 << 31,       // negative as a signed int on a 32-bit build
		^uint32(0),    // and the very top
	} {
		fast, fastErr := build().ResolveType(idx)

		// The same component with the fast path disabled: resolveTypeSlow on
		// a frozen component is resolveTypeDepth, the walk being shortcut.
		slow, slowErr := build().resolveTypeSlow(idx)

		if !reflect.DeepEqual(fast, slow) {
			t.Errorf("idx %d: fast path gave %#v, the walk gives %#v", idx, fast, slow)
		}
		switch {
		case fastErr == nil && slowErr != nil:
			t.Errorf("idx %d: fast path succeeded, the walk errors: %v", idx, slowErr)
		case fastErr != nil && slowErr == nil:
			t.Errorf("idx %d: fast path errors (%v), the walk succeeds", idx, fastErr)
		case fastErr != nil && fastErr.Error() != slowErr.Error():
			t.Errorf("idx %d: errors differ:\n fast=%v\n slow=%v", idx, fastErr, slowErr)
		}
	}
}

// An unfrozen component must not take the fast path at all -- that is the
// whole point of the flag, since resolution can still intern there.
func TestResolveTypeFastPathSkippedWhenUnfrozen(t *testing.T) {
	c := &Component{
		Types:     []Type{{Index: 0, Kind: "u32", Descriptor: PrimitiveDesc{Prim: "u32"}}},
		TypeSpace: []TypeSpaceEntry{{Kind: TypeSpaceDef, Def: 0}},
	}
	if c.typesFrozen {
		t.Fatal("a hand-built Component must not start frozen")
	}
	td, err := c.ResolveType(0)
	if err != nil {
		t.Fatalf("ResolveType: %v", err)
	}
	if td != (PrimitiveDesc{Prim: "u32"}) {
		t.Fatalf("got %#v, want u32", td)
	}
}

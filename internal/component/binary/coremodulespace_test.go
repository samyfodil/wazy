package binary

import (
	"bytes"
	"strings"
	"testing"
)

// synthCoreModuleSection encodes section 1 carrying a minimal (preamble-only)
// core wasm module. Its bytes are never decoded by this package -- only its
// [Offset, Offset+Size) range is recorded -- so a bare preamble is enough.
func synthCoreModuleSection() []byte {
	return synthSection(1, []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00})
}

// synthOuterCoreModuleAlias encodes section 6 carrying one
// `(alias outer <count> <idx> (core module))`.
func synthOuterCoreModuleAlias(count, idx byte) []byte {
	// sort core (0x00), core:sort module (0x11), target outer (0x02).
	return synthSection(6, []byte{0x01, 0x00, 0x11, 0x02, count, idx})
}

// synthNestedComponentSection wraps an already-assembled component binary as
// section 4 of an enclosing component.
func synthNestedComponentSection(nested []byte) []byte {
	return synthSection(4, nested)
}

// The whole point of CoreModuleSpace: a core-module outer alias occupies an
// index of its own, interleaved with section-1 embedded modules in declaration
// order. This is fused.23.wast's `$c` exactly -- its own `$shim2` at index 0,
// the parent's module aliased in at index 1 -- which the pre-existing
// "ModuleIdx indexes CoreModules" model got wrong by a full slot.
func TestCoreModuleSpace_AliasInterleavesWithEmbeddedModules(t *testing.T) {
	nested := synthComponent(
		synthCoreModuleSection(),        // child's own module -> space index 0
		synthOuterCoreModuleAlias(1, 0), // parent's module    -> space index 1
	)
	raw := synthComponent(
		synthCoreModuleSection(), // parent's module, its space index 0
		synthNestedComponentSection(nested),
	)
	parent, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(parent.NestedComponents) != 1 {
		t.Fatalf("nested components = %d, want 1", len(parent.NestedComponents))
	}
	child := parent.NestedComponents[0]
	if child.Outer != parent {
		t.Fatalf("child.Outer = %p, want the parent %p", child.Outer, parent)
	}
	if got, want := len(child.CoreModuleSpace), 2; got != want {
		t.Fatalf("child core module space = %d entries, want %d: %+v", got, want, child.CoreModuleSpace)
	}
	if k := child.CoreModuleSpace[0].Kind; k != CoreModuleFromDefinition {
		t.Errorf("space[0].Kind = %v, want CoreModuleFromDefinition", k)
	}
	if k := child.CoreModuleSpace[1].Kind; k != CoreModuleFromAlias {
		t.Errorf("space[1].Kind = %v, want CoreModuleFromAlias", k)
	}
	// The space length, not len(CoreModules) -- which is 1 here, one short.
	if got, want := child.CoreModuleSpaceLen(), 2; got != want {
		t.Errorf("CoreModuleSpaceLen() = %d, want %d (len(CoreModules) is %d)", got, want, len(child.CoreModules))
	}

	// Index 0 is the child's OWN module: owner is the child.
	owner, modIdx, err := child.ResolveCoreModule(0)
	if err != nil {
		t.Fatalf("ResolveCoreModule(0): %v", err)
	}
	if owner != child || modIdx != 0 {
		t.Errorf("ResolveCoreModule(0) = (%p, %d), want (child %p, 0)", owner, modIdx, child)
	}
	// Index 1 is the PARENT's module -- and the owner matters, because the
	// module's Offset/Size index into the parent's Bytes, not the child's.
	owner, modIdx, err = child.ResolveCoreModule(1)
	if err != nil {
		t.Fatalf("ResolveCoreModule(1): %v", err)
	}
	if owner != parent || modIdx != 0 {
		t.Errorf("ResolveCoreModule(1) = (%p, %d), want (parent %p, 0)", owner, modIdx, parent)
	}
	cm := owner.CoreModules[modIdx]
	if cm.Offset+cm.Size > len(owner.Bytes) {
		t.Fatalf("aliased module range [%d:%d) does not fit the owner's %d bytes", cm.Offset, cm.Offset+cm.Size, len(owner.Bytes))
	}
}

// A de Bruijn count of 2 skips two enclosing components.
func TestCoreModuleSpace_OuterCountTwo(t *testing.T) {
	grandchild := synthComponent(synthOuterCoreModuleAlias(2, 0))
	child := synthComponent(synthNestedComponentSection(grandchild))
	raw := synthComponent(
		synthCoreModuleSection(),
		synthNestedComponentSection(child),
	)
	root, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	gc := root.NestedComponents[0].NestedComponents[0]
	owner, modIdx, err := gc.ResolveCoreModule(0)
	if err != nil {
		t.Fatalf("ResolveCoreModule(0): %v", err)
	}
	if owner != root || modIdx != 0 {
		t.Errorf("ResolveCoreModule(0) = (%p, %d), want (root %p, 0)", owner, modIdx, root)
	}
}

// An OuterCount of 0 is legal ("the first idx may be 0 = the current
// component") and is just another index in this same space.
func TestCoreModuleSpace_OuterCountZeroIsSelf(t *testing.T) {
	raw := synthComponent(synthCoreModuleSection(), synthOuterCoreModuleAlias(0, 0))
	c, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	owner, modIdx, err := c.ResolveCoreModule(1)
	if err != nil {
		t.Fatalf("ResolveCoreModule(1): %v", err)
	}
	if owner != c || modIdx != 0 {
		t.Errorf("ResolveCoreModule(1) = (%p, %d), want (self %p, 0)", owner, modIdx, c)
	}
}

// A hand-built Component (never decoded) has no space at all; CoreModules IS
// its space, exactly as before this file existed.
func TestCoreModuleSpace_HandBuiltFallback(t *testing.T) {
	c := &Component{CoreModules: []CoreModule{{Offset: 0, Size: 8}, {Offset: 8, Size: 8}}}
	if got := c.CoreModuleSpaceLen(); got != 2 {
		t.Errorf("CoreModuleSpaceLen() = %d, want 2", got)
	}
	owner, modIdx, err := c.ResolveCoreModule(1)
	if err != nil {
		t.Fatalf("ResolveCoreModule(1): %v", err)
	}
	if owner != c || modIdx != 1 {
		t.Errorf("ResolveCoreModule(1) = (%p, %d), want (self %p, 1)", owner, modIdx, c)
	}
	if _, _, err := c.ResolveCoreModule(2); err == nil || !strings.Contains(err.Error(), "out of range of 2 modules") {
		t.Errorf("ResolveCoreModule(2) error = %v, want an out-of-range error", err)
	}
}

func TestCoreModuleSpace_FailLoudBranches(t *testing.T) {
	// A cycle: a self (count 0) alias naming its own space slot.
	cyclic := &Component{
		CoreModuleSpace: []CoreModuleSpaceEntry{{Kind: CoreModuleFromAlias, Alias: 0}},
		Aliases:         []AliasDef{{Sort: 0x00, CoreSort: 0x11, TargetKind: 0x02, OuterCount: 0, OuterIndex: 0}},
	}
	tests := []struct {
		name string
		comp *Component
		idx  uint32
		want string
	}{
		{
			name: "index past the space",
			comp: &Component{CoreModuleSpace: []CoreModuleSpaceEntry{{Kind: CoreModuleFromDefinition}}, CoreModules: []CoreModule{{}}},
			idx:  1,
			want: "out of range of the 1-entry core module index space",
		},
		{
			name: "definition entry naming a missing module",
			comp: &Component{CoreModuleSpace: []CoreModuleSpaceEntry{{Kind: CoreModuleFromDefinition, Module: 3}}},
			want: "module index 3 out of range of 0 embedded modules",
		},
		{
			name: "alias entry naming a missing alias",
			comp: &Component{CoreModuleSpace: []CoreModuleSpaceEntry{{Kind: CoreModuleFromAlias, Alias: 7}}},
			want: "alias index 7 out of range of 0 aliases",
		},
		{
			name: "an export alias cannot name a core module",
			comp: &Component{
				CoreModuleSpace: []CoreModuleSpaceEntry{{Kind: CoreModuleFromAlias, Alias: 0}},
				Aliases:         []AliasDef{{Sort: 0x00, CoreSort: 0x11, TargetKind: 0x00}},
			},
			want: "alias target kind 0x0 cannot name a core module",
		},
		{
			name: "outer alias with no enclosing component",
			comp: &Component{
				CoreModuleSpace: []CoreModuleSpaceEntry{{Kind: CoreModuleFromAlias, Alias: 0}},
				Aliases:         []AliasDef{{Sort: 0x00, CoreSort: 0x11, TargetKind: 0x02, OuterCount: 1}},
			},
			want: "but only 0 are known",
		},
		{
			name: "unknown entry kind",
			comp: &Component{CoreModuleSpace: []CoreModuleSpaceEntry{{Kind: CoreModuleSpaceEntryKind(9)}}},
			want: "unknown core-module-space entry kind 9",
		},
		{
			name: "cyclic alias chain",
			comp: cyclic,
			want: "alias chain exceeds depth",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.comp.ResolveCoreModule(tt.idx)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ResolveCoreModule(%d) error = %v, want it to contain %q", tt.idx, err, tt.want)
			}
		})
	}
}

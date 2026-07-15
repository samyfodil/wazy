package binary

import (
	"bytes"
	"strings"
	"testing"
)

func wantErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

// ------- legacy fallback (no TypeSpace: a hand-built Component) -------

func TestResolveType_LegacyFallback_Success(t *testing.T) {
	c := &Component{Types: []Type{{Descriptor: PrimitiveDesc{Prim: "u32"}}}}
	td, err := c.ResolveType(0)
	if err != nil {
		t.Fatalf("ResolveType: %v", err)
	}
	if _, ok := td.(PrimitiveDesc); !ok {
		t.Fatalf("got %T, want PrimitiveDesc", td)
	}
}

func TestResolveType_LegacyFallback_OutOfRange(t *testing.T) {
	c := &Component{}
	_, err := c.ResolveType(0)
	wantErrContains(t, err, "out of range of 0 types")
}

// ------- TypeSpace-driven resolution (as built by Decode) -------

func TestResolveType_Def_Success(t *testing.T) {
	c := &Component{
		Types:     []Type{{Descriptor: PrimitiveDesc{Prim: "u32"}}},
		TypeSpace: []TypeSpaceEntry{{Kind: TypeSpaceDef, Def: 0}},
	}
	td, err := c.ResolveType(0)
	if err != nil {
		t.Fatalf("ResolveType: %v", err)
	}
	if _, ok := td.(PrimitiveDesc); !ok {
		t.Fatalf("got %T, want PrimitiveDesc", td)
	}
}

func TestResolveType_OutOfRangeTypeSpace(t *testing.T) {
	c := &Component{
		Types:     []Type{{Descriptor: PrimitiveDesc{Prim: "u32"}}},
		TypeSpace: []TypeSpaceEntry{{Kind: TypeSpaceDef, Def: 0}},
	}
	_, err := c.ResolveType(1)
	wantErrContains(t, err, "out of range of the 1-entry component type index space")
}

func TestResolveType_Def_InternalErrorOutOfRangeTypes(t *testing.T) {
	// A TypeSpaceDef entry whose Def points past Types cannot arise from
	// Decode (decodeComponent always appends the deftype right after
	// recording its TypeSpace entry), but ResolveType must still fail loud
	// rather than panic if it's ever hand-constructed this way.
	c := &Component{
		TypeSpace: []TypeSpaceEntry{{Kind: TypeSpaceDef, Def: 5}},
	}
	_, err := c.ResolveType(0)
	wantErrContains(t, err, "internal error")
}

func TestResolveType_Import_Unresolved(t *testing.T) {
	c := &Component{
		Imports:   []Import{{Name: "test:pkg/thing", ExternType: 0x03}},
		TypeSpace: []TypeSpaceEntry{{Kind: TypeSpaceImport, Import: 0}},
	}
	_, err := c.ResolveType(0)
	wantErrContains(t, err, `imported type (import "test:pkg/thing")`)
}

func TestResolveType_Import_UnresolvedUnknownName(t *testing.T) {
	// Import index out of range of Imports: the error still names the type
	// index instead of panicking on the Imports lookup.
	c := &Component{
		TypeSpace: []TypeSpaceEntry{{Kind: TypeSpaceImport, Import: 99}},
	}
	_, err := c.ResolveType(0)
	wantErrContains(t, err, `imported type (import "?")`)
}

func TestResolveType_UnknownEntryKind(t *testing.T) {
	c := &Component{
		TypeSpace: []TypeSpaceEntry{{Kind: TypeSpaceEntryKind(99)}},
	}
	_, err := c.ResolveType(0)
	wantErrContains(t, err, "unknown type-space entry kind")
}

// ------- alias resolution -------

func TestResolveType_Alias_Export_LocalInlineInstance_Success(t *testing.T) {
	// Component-local instance 0 (Kind 0x01, inline exports) re-exports type
	// index 0 under the name "t"; an alias exporting "t" from instance 0
	// should resolve, transitively, to that type index's descriptor.
	c := &Component{
		Types: []Type{{Descriptor: PrimitiveDesc{Prim: "u32"}}},
		Instances: []Instance{
			{Kind: 0x01, Exports: []InlineExport{{Name: "t", Sort: 0x03, SortIdx: 0}}},
		},
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x00, InstanceIdx: 0, Name: "t"},
		},
		TypeSpace: []TypeSpaceEntry{
			{Kind: TypeSpaceDef, Def: 0},
			{Kind: TypeSpaceAlias, Alias: 0},
		},
	}
	td, err := c.ResolveType(1)
	if err != nil {
		t.Fatalf("ResolveType: %v", err)
	}
	if _, ok := td.(PrimitiveDesc); !ok {
		t.Fatalf("got %T, want PrimitiveDesc", td)
	}
}

func TestResolveType_Alias_Export_FromImportedInstance_Unresolved(t *testing.T) {
	// The real-guest shape: aliasing a type export out of an IMPORTED
	// instance. This decoder does not decode nested type declarations
	// inside an imported instance type, so this must fail loud rather than
	// silently misresolve -- see stdout_write_alias.wat in the instance
	// package for the end-to-end proof that this is fine in practice (the
	// own/borrow ResourceType index is never dereferenced through a
	// resolver).
	c := &Component{
		Imports: []Import{{Name: "test:cli/streams", ExternType: 0x05}},
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x00, InstanceIdx: 0, Name: "output-stream"},
		},
		TypeSpace: []TypeSpaceEntry{
			{Kind: TypeSpaceAlias, Alias: 0},
		},
	}
	_, err := c.ResolveType(0)
	wantErrContains(t, err, "cannot resolve structurally")
}

func TestResolveType_Alias_Export_NameNotFound(t *testing.T) {
	// InstanceIdx names a local inline-export instance, but no export
	// matches the alias's name/sort -- still unresolved, not a panic.
	c := &Component{
		Instances: []Instance{
			{Kind: 0x01, Exports: []InlineExport{{Name: "other", Sort: 0x03, SortIdx: 0}}},
		},
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x00, InstanceIdx: 0, Name: "t"},
		},
		TypeSpace: []TypeSpaceEntry{
			{Kind: TypeSpaceAlias, Alias: 0},
		},
	}
	_, err := c.ResolveType(0)
	wantErrContains(t, err, "cannot resolve structurally")
}

func TestResolveType_Alias_Export_InstantiateKindInstance_Unresolved(t *testing.T) {
	// InstanceIdx names a local Instance, but it's a Kind 0x00 (instantiate
	// a nested component), whose exports this decoder cannot see either.
	c := &Component{
		Instances: []Instance{{Kind: 0x00, ComponentIdx: 0}},
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x00, InstanceIdx: 0, Name: "t"},
		},
		TypeSpace: []TypeSpaceEntry{
			{Kind: TypeSpaceAlias, Alias: 0},
		},
	}
	_, err := c.ResolveType(0)
	wantErrContains(t, err, "cannot resolve structurally")
}

func TestResolveType_Alias_Outer_SelfReferential_Success(t *testing.T) {
	c := &Component{
		Types: []Type{{Descriptor: PrimitiveDesc{Prim: "string"}}},
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x02, OuterCount: 0, OuterIndex: 0},
		},
		TypeSpace: []TypeSpaceEntry{
			{Kind: TypeSpaceDef, Def: 0},
			{Kind: TypeSpaceAlias, Alias: 0},
		},
	}
	td, err := c.ResolveType(1)
	if err != nil {
		t.Fatalf("ResolveType: %v", err)
	}
	if p, ok := td.(PrimitiveDesc); !ok || p.Prim != "string" {
		t.Fatalf("got %#v, want PrimitiveDesc{string}", td)
	}
}

func TestResolveType_Alias_Outer_Enclosing_Unresolved(t *testing.T) {
	c := &Component{
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x02, OuterCount: 1, OuterIndex: 0},
		},
		TypeSpace: []TypeSpaceEntry{
			{Kind: TypeSpaceAlias, Alias: 0},
		},
	}
	_, err := c.ResolveType(0)
	wantErrContains(t, err, "enclosing component")
}

func TestResolveType_Alias_InvalidTargetKind(t *testing.T) {
	// TargetKind 0x01 (core export) cannot legally carry a type-sort alias.
	c := &Component{
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x01, InstanceIdx: 0, Name: "t"},
		},
		TypeSpace: []TypeSpaceEntry{
			{Kind: TypeSpaceAlias, Alias: 0},
		},
	}
	_, err := c.ResolveType(0)
	wantErrContains(t, err, "cannot resolve to a type")
}

func TestResolveType_Alias_InternalErrorOutOfRangeAliases(t *testing.T) {
	c := &Component{
		TypeSpace: []TypeSpaceEntry{{Kind: TypeSpaceAlias, Alias: 5}},
	}
	_, err := c.ResolveType(0)
	wantErrContains(t, err, "internal error")
}

func TestResolveType_AliasChain_CycleGuard(t *testing.T) {
	// A self-referential outer alias chain that never bottoms out (alias 0
	// targets alias 0's own outer index) must fail loud on depth rather
	// than looping forever.
	c := &Component{
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x02, OuterCount: 0, OuterIndex: 0},
		},
		TypeSpace: []TypeSpaceEntry{
			{Kind: TypeSpaceAlias, Alias: 0},
		},
	}
	_, err := c.ResolveType(0)
	wantErrContains(t, err, "alias chain exceeds depth")
}

func TestResolveType_AliasChain_MultiHop_Success(t *testing.T) {
	// Two outer aliases chained together, both self-referential, still
	// bottom out at the real deftype.
	c := &Component{
		Types: []Type{{Descriptor: PrimitiveDesc{Prim: "bool"}}},
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x02, OuterCount: 0, OuterIndex: 0}, // -> index 0 (the deftype)
			{Sort: 0x03, TargetKind: 0x02, OuterCount: 0, OuterIndex: 1}, // -> index 1 (the first alias)
		},
		TypeSpace: []TypeSpaceEntry{
			{Kind: TypeSpaceDef, Def: 0},
			{Kind: TypeSpaceAlias, Alias: 0},
			{Kind: TypeSpaceAlias, Alias: 1},
		},
	}
	td, err := c.ResolveType(2)
	if err != nil {
		t.Fatalf("ResolveType: %v", err)
	}
	if p, ok := td.(PrimitiveDesc); !ok || p.Prim != "bool" {
		t.Fatalf("got %#v, want PrimitiveDesc{bool}", td)
	}
}

// ------- decoder-level TypeSpace construction (via Decode) -------

// TestDecode_TypeSpace_ImportedType proves a top-level type import (a rare
// shape -- most real components import instances, not bare types, but the
// grammar allows it) occupies an index in TypeSpace, shifting a later
// type-section deftype's true index exactly like a type-sort alias does.
func TestDecode_TypeSpace_ImportedType(t *testing.T) {
	buf := preamble()

	// Import section: one import "t", externdesc = type bound sub (0x03 0x01).
	importBody := []byte{
		0x01,            // count = 1
		0x00, 0x01, 't', // name: kind=0x00, len=1, "t"
		0x03, 0x01, // externdesc: sort=type(0x03), bound=sub(0x01)
	}
	buf = append(buf, 10, byte(len(importBody)))
	buf = append(buf, importBody...)

	// Type section: one deftype, u32 (0x79).
	typeBody := []byte{0x01, 0x79}
	buf = append(buf, 7, byte(len(typeBody)))
	buf = append(buf, typeBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.TypeSpace) != 2 {
		t.Fatalf("TypeSpace has %d entries, want 2 (1 import + 1 deftype)", len(c.TypeSpace))
	}
	if c.TypeSpace[0].Kind != TypeSpaceImport {
		t.Fatalf("TypeSpace[0].Kind = %v, want TypeSpaceImport", c.TypeSpace[0].Kind)
	}
	if c.TypeSpace[1].Kind != TypeSpaceDef {
		t.Fatalf("TypeSpace[1].Kind = %v, want TypeSpaceDef", c.TypeSpace[1].Kind)
	}

	// Type index 0 (the import) is unresolved; type index 1 (the deftype,
	// shifted past the import) resolves to the u32 primitive.
	if _, err := c.ResolveType(0); err == nil {
		t.Fatal("expected ResolveType(0) (the imported type) to fail loud")
	}
	td, err := c.ResolveType(1)
	if err != nil {
		t.Fatalf("ResolveType(1): %v", err)
	}
	if p, ok := td.(PrimitiveDesc); !ok || p.Prim != "u32" {
		t.Fatalf("got %#v, want PrimitiveDesc{u32}", td)
	}
}

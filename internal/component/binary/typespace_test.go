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
	// instance. resolveAlias CAN follow this structurally when the import's
	// own declared instancetype is known (see the
	// FromImportedInstance_Resolved tests below) -- but here the import has
	// no ComponentInstanceSpace entry at all (a hand-built Component that
	// never went through Decode, or one instantiated standalone without the
	// index space this resolution needs), so it must still fail loud rather
	// than silently misresolve. See stdout_write_alias.wat in the instance
	// package for the end-to-end proof that even a genuinely unresolvable
	// case (an own/borrow ResourceType index) is fine in practice: that
	// index is never dereferenced through a resolver.
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

// ------- alias export from an IMPORTED instance, structurally resolved -------
//
// `use iface.{T}` compiles to exactly this shape: a type-sort alias whose
// TargetKind is export (0x00) and whose InstanceIdx names an IMPORTED
// instance (via ComponentInstanceSpace), not a locally inline-exported one.
// These hand-build that shape directly against Component/InstanceDesc rather
// than going through Decode -- TestDecode_ImportedInstanceType_EndToEnd below
// covers the real byte-level grammar these fields are built from.

// TestResolveType_Alias_Export_FromImportedInstance_Resolved_Primitive covers
// the simplest shape: an export whose local type has no further nested type
// index at all (e.g. `type component = list<u8>`, componentized:component's
// own `types` interface).
func TestResolveType_Alias_Export_FromImportedInstance_Resolved_Primitive(t *testing.T) {
	localIdx := uint32(0)
	c := &Component{
		Imports: []Import{{Name: "test:pkg/types", ExternType: 0x05, ExternIndex: 0}},
		Types: []Type{{Descriptor: InstanceDesc{
			Types:   []TypeDesc{ListDesc{Element: TypeRef{Primitive: "u8"}}},
			Exports: map[string]TypeRef{"component": {TypeIndex: &localIdx}},
		}}},
		ComponentInstanceSpace: []ComponentInstanceSpaceEntry{
			{Kind: ComponentInstanceFromImport, Import: 0},
		},
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x00, InstanceIdx: 0, Name: "component"},
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
	list, ok := td.(ListDesc)
	if !ok {
		t.Fatalf("got %T, want ListDesc", td)
	}
	if list.Element.Primitive != "u8" {
		t.Errorf("Element = %#v, want primitive u8", list.Element)
	}
}

// TestResolveType_Alias_Export_FromImportedInstance_Resolved_NestedLocalRef
// covers the shape that actually broke real components: an exported type
// whose own definition references ANOTHER type declared in the same
// instancetype body by a LOCAL index (componentized:component's `types`
// interface: `variant error { other(option<string>) }`, where `error` and
// `option<string>` are two separate local deftypes). The nested local index
// must come back globalized -- resolvable through c's ordinary Resolver, not
// dangling as a meaningless index into this Component's own TypeSpace (which
// is exactly what produced the reported "unknown type descriptor: <nil>").
func TestResolveType_Alias_Export_FromImportedInstance_Resolved_NestedLocalRef(t *testing.T) {
	optionLocalIdx := uint32(0)  // local type 0: option<string>
	variantLocalIdx := uint32(1) // local type 1: variant error { other(<local 0>) }
	errorCaseType := TypeRef{TypeIndex: &optionLocalIdx}
	c := &Component{
		Imports: []Import{{Name: "test:pkg/types", ExternType: 0x05, ExternIndex: 0}},
		Types: []Type{{Descriptor: InstanceDesc{
			Types: []TypeDesc{
				OptionDesc{Element: TypeRef{Primitive: "string"}},
				VariantDesc{Cases: []VariantCase{{Name: "other", Type: &errorCaseType}}},
			},
			Exports: map[string]TypeRef{"error": {TypeIndex: &variantLocalIdx}},
		}}},
		ComponentInstanceSpace: []ComponentInstanceSpaceEntry{
			{Kind: ComponentInstanceFromImport, Import: 0},
		},
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x00, InstanceIdx: 0, Name: "error"},
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
	variant, ok := td.(VariantDesc)
	if !ok {
		t.Fatalf("got %T, want VariantDesc", td)
	}
	if len(variant.Cases) != 1 || variant.Cases[0].Name != "other" || variant.Cases[0].Type == nil {
		t.Fatalf("Cases = %#v, want one case %q with a payload", variant.Cases, "other")
	}
	// The case's own type must be resolvable through c -- NOT the original
	// dangling local index 0, which (against this Component's own TypeSpace)
	// would misresolve to whatever real type happens to occupy index 0, or
	// simply fail out of range.
	caseIdx := variant.Cases[0].Type.TypeIndex
	if caseIdx == nil {
		t.Fatalf("case type has no TypeIndex: %#v", variant.Cases[0].Type)
	}
	if *caseIdx < extraTypeBase {
		t.Errorf("case type index %d was not globalized into the escape range (>= %d)", *caseIdx, extraTypeBase)
	}
	elemTD, err := c.ResolveType(*caseIdx)
	if err != nil {
		t.Fatalf("ResolveType(case type): %v", err)
	}
	opt, ok := elemTD.(OptionDesc)
	if !ok {
		t.Fatalf("case type = %T, want OptionDesc", elemTD)
	}
	if opt.Element.Primitive != "string" {
		t.Errorf("option element = %#v, want primitive string", opt.Element)
	}
}

// TestResolveType_Alias_Export_FromImportedInstance_AbstractResourceUnresolved
// covers a `sub`-bound (abstract resource) export: this decoder has no
// structural definition for it at all, so it must stay unresolved rather
// than fabricate one.
func TestResolveType_Alias_Export_FromImportedInstance_AbstractResourceUnresolved(t *testing.T) {
	c := &Component{
		Imports: []Import{{Name: "test:pkg/types", ExternType: 0x05, ExternIndex: 0}},
		Types: []Type{{Descriptor: InstanceDesc{
			Types:   []TypeDesc{nil}, // the resource export's own reserved (unresolvable) slot
			Exports: map[string]TypeRef{},
		}}},
		ComponentInstanceSpace: []ComponentInstanceSpaceEntry{
			{Kind: ComponentInstanceFromImport, Import: 0},
		},
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x00, InstanceIdx: 0, Name: "my-resource"},
		},
		TypeSpace: []TypeSpaceEntry{
			{Kind: TypeSpaceDef, Def: 0},
			{Kind: TypeSpaceAlias, Alias: 0},
		},
	}
	_, err := c.ResolveType(1)
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

// synthInstanceDeclType encodes one instancedecl of kind "type" (tag 0x01)
// wrapping an already-encoded deftype body.
func synthInstanceDeclType(deftypeBody []byte) []byte {
	return append([]byte{0x01}, deftypeBody...)
}

// synthInstanceDeclExportEq encodes one instancedecl of kind "export" (tag
// 0x04) naming a type-sort export bound `eq localIdx` -- localIdx indexes
// the enclosing instancetype's OWN nested type-sort space (built by earlier
// "type"/"export" instancedecls in the same body), never the enclosing
// component's.
func synthInstanceDeclExportEq(name string, localIdx byte) []byte {
	out := []byte{0x04, 0x00} // tag=export, externname kind=0x00
	out = append(out, synthLabel(name)...)
	out = append(out, 0x03, 0x00, localIdx) // externdesc: type-sort, eq bound, localIdx
	return out
}

// TestDecode_ImportedInstanceType_EndToEnd decodes a real component binary
// carrying the exact shape that broke componentized:component/wit-tools: an
// imported interface (`types`) declaring `variant error { other(option<string>) }`,
// and a type-sort alias elsewhere (`use types.{error}`) naming it by export.
// Before this fix, resolveAlias could not follow an alias into an IMPORTED
// instance's own type declarations at all; this proves the whole path --
// decodeImportSection capturing the instance import's declared type,
// readInstancetypeDesc building its nested Types/Exports, and resolveAlias
// globalizing the exported type's own local cross-reference -- works
// end-to-end through the real binary decoder, not just against hand-built
// Component/InstanceDesc values (see the FromImportedInstance_Resolved_*
// tests above, which isolate resolveAlias/globalizeLocalRef on their own).
func TestDecode_ImportedInstanceType_EndToEnd(t *testing.T) {
	// The "types" interface's instancetype body:
	//   local type 0: option<string>                  (0x6b, string=0x73)
	//   local type 1: variant error { other(<local 0>) } (0x71, 1 case)
	//   export "error" -> eq local type 1
	optionBody := []byte{0x6b, 0x73} // option<string>
	caseOther := append(synthLabel("other"), 0x01, 0x00, 0x00)
	// case: label "other", opt(valtype)=some(typeidx 0), opt(refines)=none
	variantBody := append([]byte{0x71, 0x01}, caseOther...) // variant, 1 case

	instancetypeBody := []byte{0x03} // 3 instancedecls
	instancetypeBody = append(instancetypeBody, synthInstanceDeclType(optionBody)...)
	instancetypeBody = append(instancetypeBody, synthInstanceDeclType(variantBody)...)
	instancetypeBody = append(instancetypeBody, synthInstanceDeclExportEq("error", 1)...)

	instancetypeDeftype := append([]byte{0x42}, instancetypeBody...) // 0x42 = instancetype

	typeSec := synthSection(7, append([]byte{0x01}, instancetypeDeftype...)) // 1 type-section entry
	importSec := synthSection(10, append([]byte{0x01}, synthInstanceImport("test:pkg/types")...))

	// Alias section: one type-sort export alias naming "error" out of
	// ComponentInstanceSpace index 0 (our import) -- the `use types.{error}`
	// shape.
	aliasBody := []byte{0x01, 0x03, 0x00, 0x00} // count=1, sort=type, targetKind=export, instanceIdx=0
	aliasBody = append(aliasBody, synthLabel("error")...)
	aliasSec := synthSection(6, aliasBody)

	raw := synthComponent(typeSec, importSec, aliasSec)

	c, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	// Type index 0: the instancetype deftype itself (TypeSpaceDef).
	// Type index 1: the alias (TypeSpaceAlias) -- what we actually want.
	if len(c.TypeSpace) != 2 {
		t.Fatalf("TypeSpace has %d entries, want 2 (1 deftype + 1 alias)", len(c.TypeSpace))
	}

	td, err := c.ResolveType(1)
	if err != nil {
		t.Fatalf("ResolveType(1) (the `use types.{error}` alias): %v", err)
	}
	variant, ok := td.(VariantDesc)
	if !ok {
		t.Fatalf("got %T, want VariantDesc", td)
	}
	if len(variant.Cases) != 1 || variant.Cases[0].Name != "other" {
		t.Fatalf("Cases = %#v, want one case %q", variant.Cases, "other")
	}
	caseType := variant.Cases[0].Type
	if caseType == nil || caseType.TypeIndex == nil {
		t.Fatalf("case %q has no resolvable type: %#v", "other", caseType)
	}
	optTD, err := c.ResolveType(*caseType.TypeIndex)
	if err != nil {
		t.Fatalf("ResolveType(case %q's type): %v", "other", err)
	}
	opt, ok := optTD.(OptionDesc)
	if !ok || opt.Element.Primitive != "string" {
		t.Fatalf("case %q's type = %#v, want OptionDesc{Element: string}", "other", optTD)
	}
}

// synthOuterTypeAlias encodes section 6 carrying one
// `(alias outer <count> <idx> (type))`.
func synthOuterTypeAlias(count, idx byte) []byte {
	return synthSection(6, []byte{0x01, 0x03, 0x02, count, idx})
}

// A nested component re-exporting a type its PARENT defines -- fused.22.wast's
// `$c1` does this for all six of the root's flags types. Before Component.Outer
// existed, resolveAlias bailed on any OuterCount > 0, so the lifted func
// signature that named the aliased type could not be flattened at all.
func TestResolveType_OuterAliasIntoEnclosingComponent(t *testing.T) {
	// Root defines one flags type; the child aliases it and re-exports it.
	rootTypes := synthSection(7, []byte{0x01, 0x6e, 0x01, 0x02, 'f', '1'}) // one flags with one label "f1"
	child := synthComponent(synthOuterTypeAlias(1, 0))
	raw := synthComponent(rootTypes, synthNestedComponentSection(child))

	root, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	c := root.NestedComponents[0]
	td, err := c.ResolveType(0)
	if err != nil {
		t.Fatalf("child.ResolveType(0): %v", err)
	}
	fd, ok := td.(FlagsDesc)
	if !ok {
		t.Fatalf("child.ResolveType(0) = %T, want FlagsDesc", td)
	}
	if len(fd.Names) != 1 || fd.Names[0] != "f1" {
		t.Errorf("flags labels = %v, want [f1]", fd.Names)
	}
}

// A count of 2 skips two enclosing components -- the shape an `alias outer 2`
// inside a nested component's own nested component needs.
func TestResolveType_OuterAliasCountTwo(t *testing.T) {
	rootTypes := synthSection(7, []byte{0x01, 0x6e, 0x01, 0x02, 'f', '1'})
	grandchild := synthComponent(synthOuterTypeAlias(2, 0))
	child := synthComponent(synthNestedComponentSection(grandchild))
	raw := synthComponent(rootTypes, synthNestedComponentSection(child))

	root, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	gc := root.NestedComponents[0].NestedComponents[0]
	if _, err := gc.ResolveType(0); err != nil {
		t.Fatalf("grandchild.ResolveType(0): %v", err)
	}
}

// The fail-loud branch: an outer alias whose de Bruijn count runs past the
// known enclosing chain (a nested component binary decoded standalone, or a
// hand-built Component).
func TestResolveType_OuterAliasBeyondKnownEnclosingComponents(t *testing.T) {
	standalone, err := Decode(bytes.NewReader(synthComponent(synthOuterTypeAlias(1, 0))))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, err = standalone.ResolveType(0)
	if err == nil || !strings.Contains(err.Error(), "but only 0 are known") {
		t.Fatalf("ResolveType(0) error = %v, want it to report the missing enclosing component", err)
	}
}

// ------- globalizeLocalRef -------

// TestGlobalizeLocalRef_CycleGuard proves globalizeLocalRef terminates on a
// genuine cycle between two locally-declared types (unconstructible from
// real WIT, which always indirects through list/option/etc. -- but a
// resource declared and referenced within the SAME imported instancetype
// could plausibly nest this way) instead of recursing forever, the same way
// resolveTypeDepth's own alias-chain depth guard protects a self-referential
// alias (TestResolveType_AliasChain_CycleGuard above).
func TestGlobalizeLocalRef_CycleGuard(t *testing.T) {
	toOne := uint32(1)
	toZero := uint32(0)
	localTypes := []TypeDesc{
		TupleDesc{Elements: []TypeRef{{TypeIndex: &toOne}}},  // local 0: tuple<local 1>
		TupleDesc{Elements: []TypeRef{{TypeIndex: &toZero}}}, // local 1: tuple<local 0>
	}
	var c Component
	start := uint32(0)
	global, err := c.globalizeLocalRef(localTypes, TypeRef{TypeIndex: &start}, map[uint32]uint32{})
	if err != nil {
		t.Fatalf("globalizeLocalRef: %v", err)
	}
	if global.TypeIndex == nil {
		t.Fatal("globalized ref has no TypeIndex")
	}
	td, err := c.ResolveType(*global.TypeIndex)
	if err != nil {
		t.Fatalf("ResolveType(globalized local 0): %v", err)
	}
	tup, ok := td.(TupleDesc)
	if !ok || len(tup.Elements) != 1 || tup.Elements[0].TypeIndex == nil {
		t.Fatalf("got %#v, want a 1-element TupleDesc with a resolvable element", td)
	}
	// The cycle closes back on the escape index reserved for local 0 itself
	// -- resolving the nested element must not loop either.
	if _, err := c.ResolveType(*tup.Elements[0].TypeIndex); err != nil {
		t.Fatalf("ResolveType(nested, closing the cycle): %v", err)
	}
}

// A handle's ResourceType is an index into the *Component's* TypeSpace --
// instance/composition.go's canonTag, resourceOrigin and importedTypeIndex all
// read it that way -- so a handle carrying an imported instance's local index
// cannot be globalized, only refused. Returning the descriptor unchanged would
// canonicalize the wrong resource, silently.
func TestGlobalizeLocalRef_HandleIsRefusedNotCarriedOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		desc TypeDesc
	}{
		{"own", OwnDesc{ResourceType: 7}},
		{"borrow", BorrowDesc{ResourceType: 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A record whose field is the handle: the record globalizes, and
			// the handle inside it is what must not slip through.
			handle := uint32(1)
			localTypes := []TypeDesc{
				RecordDesc{Fields: []RecordField{{Name: "h", Type: TypeRef{TypeIndex: &handle}}}},
				tc.desc,
			}
			var c Component
			start := uint32(0)
			_, err := c.globalizeLocalRef(localTypes, TypeRef{TypeIndex: &start}, map[uint32]uint32{})
			wantErrContains(t, err, "imported instance's type declarations")
			wantErrContains(t, err, "local type space")
		})
	}
}

// globalizeLocalRef reserves an escape slot before recursing, so a failed
// recursion leaves a nil behind. Nothing hands that index out today, but
// resolving one must not report success with a nil TypeDesc -- that is exactly
// the "unknown type descriptor: <nil>" shape this decoder is meant to stop.
func TestResolveType_ReservedButUnfilledExtraType(t *testing.T) {
	var c Component
	idx := c.internExtraType(nil)
	td, err := c.ResolveType(idx)
	wantErrContains(t, err, "reserved but never filled")
	if td != nil {
		t.Fatalf("got %#v, want a nil TypeDesc alongside the error", td)
	}
}

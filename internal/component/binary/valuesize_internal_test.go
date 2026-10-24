package binary

import (
	"strings"
	"testing"
)

// The layout rules the vendored suites do not reach: flags and variants big
// enough for a wider discriminant, a size too large to multiply, and what
// is not a value type or does not resolve.
func TestLayout64(t *testing.T) {
	idx := func(i uint32) TypeRef { return TypeRef{TypeIndex: &i} }
	types := []TypeDesc{
		FixedListDesc{Element: TypeRef{Primitive: "u64"}, Length: 1 << 31}, // 0: 2^34 bytes
		FuncDesc{}, // 1: not a value type
		FixedListDesc{Element: TypeRef{TypeIndex: new(uint32)}, Length: 1 << 31}, // 2: 2^65 bytes
	}
	s := &sizeCheck{}
	sp := s.push(sizeSpace{local: true, types: types, up: -1})
	labels := func(n int) []string { return make([]string, n) }
	cases := func(n int) []VariantCase { return make([]VariantCase, n) }
	for _, tc := range []struct {
		name string
		d    TypeDesc
		want layout64
	}{
		{"flags 9", FlagsDesc{Names: labels(9)}, layout64{2, 2, true}},
		{"flags 40", FlagsDesc{Names: labels(40)}, layout64{8, 4, true}},
		{"variant 300", VariantDesc{Cases: cases(300)}, layout64{2, 2, true}},
		{"enum 70000", EnumDesc{Cases: labels(70000)}, layout64{4, 4, true}},
		{"string", PrimitiveDesc{Prim: "string"}, layout64{16, 8, true}},
		{"result u8 string", ResultDesc{Ok: &TypeRef{Primitive: "u8"}, Err: &TypeRef{Primitive: "string"}}, layout64{24, 8, true}},
		{"overflowing product", FixedListDesc{Element: idx(0), Length: 1 << 31}, layout64{maxLayoutSize, 8, true}},
		{"record of an overflow", RecordDesc{Fields: []RecordField{{Type: idx(2)}, {Type: idx(2)}}}, layout64{maxLayoutSize, 8, true}},
		{"not a value type", FuncDesc{}, layout64{}},
		{"element not a value type", FixedListDesc{Element: idx(1), Length: 2}, layout64{0, 0, false}},
		{"unknown primitive", PrimitiveDesc{Prim: "u128"}, layout64{}},
		{"no index", ListDesc{}, layout64{16, 8, true}},
		{"field without index", TupleDesc{Elements: []TypeRef{{}}}, layout64{}},
		{"case without index", VariantDesc{Cases: []VariantCase{{Type: &TypeRef{}}}}, layout64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.layoutOf(sp, tc.d, 0); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := s.layoutRef(sp, &TypeRef{TypeIndex: new(uint32)}, maxLayoutDepth+1); got.known {
		t.Errorf("past the depth limit: %+v", got)
	}
}

// The declarations the vendored suites reach only one shape of: types
// nested two deep, aliases of an enclosing type at every distance, and
// aliases that lead nowhere this decoder can size, which are not checked.
func TestValueSizes_Declarations(t *testing.T) {
	ref := func(i uint32) TypeRef { return TypeRef{TypeIndex: &i} }
	big := FixedListDesc{Element: TypeRef{Primitive: "u64"}, Length: 1 << 25}     // 2^28 bytes
	almost := FixedListDesc{Element: TypeRef{Primitive: "u8"}, Length: 1<<28 - 1} // one short
	twice := FixedListDesc{Element: ref(0), Length: 2}                            // of local type 0
	for _, tc := range []struct {
		name  string
		types []TypeDesc
		want  string
	}{
		{"instance type in a component type", []TypeDesc{ComponentDesc{Types: []TypeDesc{InstanceDesc{Types: []TypeDesc{big}}}}}, "type[0]: component type[0]: instance type[0]"},
		{"outer alias two out", []TypeDesc{almost, ComponentDesc{Types: []TypeDesc{InstanceDesc{
			Types: []TypeDesc{nil, FixedListDesc{Element: ref(0), Length: 2}},
			Outer: map[uint32]OuterAlias{0: {Count: 2, Index: 0}},
		}}}}, "type[1]: component type[0]: instance type[1]"},
		{"outer alias of its own", []TypeDesc{InstanceDesc{
			Types: []TypeDesc{almost, nil, FixedListDesc{Element: ref(1), Length: 2}},
			Outer: map[uint32]OuterAlias{1: {Count: 0, Index: 0}},
		}}, "type[0]: instance type[2]"},
		{"unaliased slot", []TypeDesc{InstanceDesc{Types: []TypeDesc{nil, twice}}}, ""},
		{"outer alias too far out", []TypeDesc{InstanceDesc{Types: []TypeDesc{nil, twice}, Outer: map[uint32]OuterAlias{0: {Count: 5}}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Component{}
			for _, d := range tc.types {
				c.Types = append(c.Types, Type{Descriptor: d})
			}
			err := c.validateValueSizes()
			switch {
			case tc.want == "" && err != nil:
				t.Fatal(err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want+": ")):
				t.Fatalf("%v, want it at %s", err, tc.want)
			}
		})
	}

	// Indices ResolveType does not resolve, and that are no imported
	// instance's export either, are not sized.
	inst := uint32(0)
	c := &Component{
		Types: []Type{{Descriptor: InstanceDesc{Exports: map[string]TypeRef{"t": ref(0)}, Types: []TypeDesc{nil}}}},
		TypeSpace: []TypeSpaceEntry{
			{Kind: TypeSpaceDef, Def: 0},
			{Kind: TypeSpaceImport},
			{Kind: TypeSpaceAlias, Alias: 0},
			{Kind: TypeSpaceAlias, Alias: 1},
			{Kind: TypeSpaceAlias, Alias: 2},
			{Kind: TypeSpaceAlias, Alias: 3},
		},
		Aliases: []AliasDef{
			{Sort: 0x03, TargetKind: 0x02, OuterCount: 1},                // no enclosing component
			{Sort: 0x03, TargetKind: 0x00, InstanceIdx: 9, Name: "t"},    // no such instance
			{Sort: 0x03, TargetKind: 0x00, InstanceIdx: inst, Name: "u"}, // no such export
			{Sort: 0x03, TargetKind: 0x00, InstanceIdx: inst, Name: "t"}, // an unresolved one
		},
		Imports:                []Import{{Name: "i", ExternType: 0x05, ExternIndex: 0}},
		ComponentInstanceSpace: []ComponentInstanceSpaceEntry{{Kind: ComponentInstanceFromImport, Import: 0}},
	}
	s := &sizeCheck{comp: c}
	top := s.push(sizeSpace{up: -1})
	for _, idx := range []uint32{1, 2, 3, 4, 5, 99} {
		if l := s.importedExportLayout(top, idx, 0); l.known {
			t.Errorf("type %d: %+v", idx, l)
		}
	}
}

func TestPeekOuterAlias(t *testing.T) {
	for _, tc := range []struct {
		buf  []byte
		want OuterAlias
		ok   bool
	}{
		{[]byte{0x02, 0x01, 0x07}, OuterAlias{1, 7}, true},
		{[]byte{0x00, 0x01, 0x07}, OuterAlias{}, false}, // an export alias
		{nil, OuterAlias{}, false},
		{[]byte{0x02, 0x80}, OuterAlias{}, false},       // a bad count
		{[]byte{0x02, 0x01, 0x80}, OuterAlias{}, false}, // a bad index
	} {
		if got, ok := peekOuterAlias(tc.buf, 0); got != tc.want || ok != tc.ok {
			t.Errorf("%x: %+v %v", tc.buf, got, ok)
		}
	}
}

// A component type's type import adds a slot to its type index space, as an
// instance type's type export does.
func TestReadComponenttypeDesc_TypeImport(t *testing.T) {
	buf := []byte{
		0x02,       // two decls
		0x01, 0x7d, // type u8
		0x03, 0x00, 0x01, 'x', 0x03, 0x00, 0x00, // import "x" (type (eq 0))
	}
	d, off, err := readComponenttypeDesc(buf, 0)
	if err != nil || off != len(buf) || len(d.Types) != 2 || d.Types[1] != (PrimitiveDesc{Prim: "u8"}) {
		t.Fatalf("%+v, %d, %v", d, off, err)
	}
}

package binary

import "testing"

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
	s := sizer{resolve: func(i uint32) (TypeDesc, error) { return types[i], nil }, memo: map[uint32]layout64{}}
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
			if got := s.layoutOf(tc.d, 0); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := s.layoutRef(&TypeRef{TypeIndex: new(uint32)}, maxLayoutDepth+1); got.known {
		t.Errorf("past the depth limit: %+v", got)
	}
}

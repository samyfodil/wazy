package abi

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// The reference implementation checks fixed-length lists' layout, flattening
// and values through the oracle batteries (testdata/oracle_*.json, "len").
// Those hand every primitive list over as a []Value or a typed slice, one of
// the two; these tests hold each typed slice against the []Value it stands
// for, through every path, and reach the errors no battery entry can.

func fixed(prim string, n uint32) binary.FixedListDesc {
	return binary.FixedListDesc{Element: binary.TypeRef{Primitive: prim}, Length: n}
}

// typedAndBoxed is, per fixed-width primitive, a typed slice and the []Value
// of the same elements as liftScalarPrimitive boxes them.
var typedAndBoxed = []struct {
	prim  string
	typed Value
	boxed []Value
}{
	{"u8", []byte{0, 128, 255}, []Value{uint32(0), uint32(128), uint32(255)}},
	{"s8", []int8{-128, 0, 127}, []Value{int32(-128), int32(0), int32(127)}},
	{"bool", []bool{true, false, true}, []Value{true, false, true}},
	{"u16", []uint16{1, 0x8000, 0xffff}, []Value{uint32(1), uint32(0x8000), uint32(0xffff)}},
	{"s16", []int16{-32768, -1, 32767}, []Value{int32(-32768), int32(-1), int32(32767)}},
	{"u32", []uint32{1, 0x80000000, math.MaxUint32}, []Value{uint32(1), uint32(0x80000000), uint32(math.MaxUint32)}},
	{"s32", []int32{math.MinInt32, -1, math.MaxInt32}, []Value{int32(math.MinInt32), int32(-1), int32(math.MaxInt32)}},
	{"u64", []uint64{1, 1 << 63, math.MaxUint64}, []Value{uint64(1), uint64(1 << 63), uint64(math.MaxUint64)}},
	{"s64", []int64{math.MinInt64, -1, math.MaxInt64}, []Value{int64(math.MinInt64), int64(-1), int64(math.MaxInt64)}},
	{"f32", []float32{1.5, float32(math.Inf(-1)), float32(math.Copysign(0, -1))}, []Value{float32(1.5), float32(math.Inf(-1)), float32(math.Copysign(0, -1))}},
	{"f64", []float64{0.5, math.MaxFloat64, -2}, []Value{0.5, math.MaxFloat64, -2.0}},
	{"char", []rune{'a', 'é', 0x1F600}, []Value{'a', 'é', rune(0x1F600)}},
}

func TestFixedList_TypedMatchesBoxed(t *testing.T) {
	for _, tc := range typedAndBoxed {
		t.Run(tc.prim, func(t *testing.T) {
			d := fixed(tc.prim, 3)
			size, err := Size(d, nil)
			if err != nil {
				t.Fatal(err)
			}

			// In memory: the same bytes from either shape, read back typed.
			memT, memB := make([]byte, 64), make([]byte, 64)
			if err := Store(memT, 8, d, tc.typed, nil, Realloc{}); err != nil {
				t.Fatalf("store typed: %v", err)
			}
			if err := Store(memB, 8, d, tc.boxed, nil, Realloc{}); err != nil {
				t.Fatalf("store boxed: %v", err)
			}
			if string(memT[8:8+size]) != string(memB[8:8+size]) || string(memT) != string(memB) {
				t.Fatalf("stored bytes differ:\n typed %x\n boxed %x", memT, memB)
			}
			got, err := Load(memT, 8, d, nil)
			if err != nil || !reflect.DeepEqual(got, tc.typed) {
				t.Fatalf("load: %#v, %v", got, err)
			}

			// Flat: the same core values from either shape, lifted back typed.
			flatT, err := LowerFlat(tc.typed, d, nil, Realloc{}, nil)
			if err != nil {
				t.Fatalf("lower typed: %v", err)
			}
			flatB, err := LowerFlat(tc.boxed, d, nil, Realloc{}, nil)
			if err != nil {
				t.Fatalf("lower boxed: %v", err)
			}
			if !reflect.DeepEqual(flatT, flatB) || len(flatT) != 3 {
				t.Fatalf("flat differ: %v vs %v", flatT, flatB)
			}
			lifted, err := LiftFlat(flatT, d, nil, nil)
			if err != nil || !reflect.DeepEqual(lifted, tc.typed) {
				t.Fatalf("lift: %#v, %v", lifted, err)
			}
		})
	}
}

// A string, a record or a nested list has no typed slice: it is a []Value
// either way, and a string's bytes are allocated as for any string.
func TestFixedList_IndirectElements(t *testing.T) {
	strs := fixed("string", 2)
	mem := make([]byte, 256)
	alloc := NewBumpAllocator(128)
	realloc := ReallocFunc(alloc.Alloc)
	v := []Value{"ab", "héllo"}
	if err := Store(mem, 8, strs, v, nil, realloc); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(mem, 8, strs, nil); err != nil || !reflect.DeepEqual(got, v) {
		t.Fatalf("load %#v, %v", got, err)
	}
	flat, err := LowerFlat(v, strs, nil, realloc, mem)
	if err != nil || len(flat) != 4 {
		t.Fatalf("lower %v, %v", flat, err)
	}
	if got, err := LiftFlat(flat, strs, nil, mem); err != nil || !reflect.DeepEqual(got, v) {
		t.Fatalf("lift %#v, %v", got, err)
	}

	// list<list<u16, 2>, 2>: the inner lists are typed, the outer is not.
	inner := uint32(0)
	types := map[uint32]binary.TypeDesc{inner: fixed("u16", 2)}
	resolve := func(i uint32) binary.TypeDesc { return types[i] }
	nested := binary.FixedListDesc{Element: binary.TypeRef{TypeIndex: &inner}, Length: 2}
	nv := []Value{[]uint16{1, 2}, []uint16{3, 4}}
	if err := Store(mem, 8, nested, nv, resolve, Realloc{}); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(mem, 8, nested, resolve); err != nil || !reflect.DeepEqual(got, nv) {
		t.Fatalf("nested load %#v, %v", got, err)
	}
	if got, err := LiftFlat([]CoreValue{NewCoreValueI32(1), NewCoreValueI32(2), NewCoreValueI32(3), NewCoreValueI32(4)}, nested, resolve, nil); err != nil || !reflect.DeepEqual(got, nv) {
		t.Fatalf("nested lift %#v, %v", got, err)
	}
}

// Wider than MaxFlatParams, a fixed-length list spills: lowered through
// memory and lifted from it, while Flatten and FlatWidth stop counting once
// it is too wide to flatten.
func TestFixedList_Wide(t *testing.T) {
	d := fixed("u32", 100)
	flat, err := Flatten(d, nil)
	if err != nil || len(flat) != MaxFlatParams+1 {
		t.Fatalf("Flatten: %d entries, %v", len(flat), err)
	}
	if w, err := FlatWidth(d, nil); err != nil || w != len(flat) {
		t.Fatalf("FlatWidth %d, %v", w, err)
	}
	if flat, _ := Flatten(fixed("u64", 2), nil); !reflect.DeepEqual(flat, []string{"i64", "i64"}) {
		t.Errorf("narrow Flatten %v", flat)
	}
	// An element of no width at all spells out nothing.
	if flat, err := Flatten(binary.FixedListDesc{Element: refTo(binary.RecordDesc{}), Length: 9}, oneType(binary.RecordDesc{})); err != nil || len(flat) != 0 {
		t.Errorf("zero-width Flatten %v, %v", flat, err)
	}

	v := make([]uint32, 100)
	for i := range v {
		v[i] = uint32(i * 3)
	}
	mem := make([]byte, 1024)
	alloc := NewBumpAllocator(16)
	core, err := LowerFlat(v, d, nil, ReallocFunc(alloc.Alloc), mem)
	if err != nil || len(core) != 1 {
		t.Fatalf("lower: %v, %v", core, err)
	}
	got, err := Load(mem, core[0].AsI32(), d, nil)
	if err != nil || !reflect.DeepEqual(got, v) {
		t.Fatalf("spilled value %v, %v", got, err)
	}

	// Binary.md bounds every value's size below 2^28 bytes.
	if _, err := Size(fixed("u64", 1<<25), nil); err == nil || !strings.Contains(err.Error(), "exceeds maximum byte size") {
		t.Errorf("Size: %v", err)
	}
	if s, err := Size(fixed("u8", maxValueSize), nil); err != nil || s != maxValueSize {
		t.Errorf("largest Size %d, %v", s, err)
	}
}

func refTo(binary.TypeDesc) binary.TypeRef {
	i := uint32(0)
	return binary.TypeRef{TypeIndex: &i}
}

func oneType(d binary.TypeDesc) Resolver { return func(uint32) binary.TypeDesc { return d } }

func TestFixedList_LengthAndShapeErrors(t *testing.T) {
	u32x4 := fixed("u32", 4)
	strs := fixed("string", 2)
	mem := make([]byte, 64)
	for _, tc := range []struct {
		name string
		d    binary.TypeDesc
		v    Value
		want string
	}{
		{"typed too short", u32x4, []uint32{1, 2, 3}, "expected 4 elements, got 3"},
		{"bytes too long", fixed("u8", 2), []byte{1, 2, 3}, "expected 2 elements, got 3"},
		{"boxed too long", u32x4, []Value{uint32(1), uint32(2), uint32(3), uint32(4), uint32(5)}, "expected 4 elements, got 5"},
		{"strings too short", strs, []Value{"a"}, "expected 2 elements, got 1"},
		{"not a list", u32x4, "nope", "expected []Value (or the typed slice for a primitive element), got string"},
		{"wrong typed slice", u32x4, []int32{1, 2, 3, 4}, "got []int32"},
		{"no typed slice for strings", strs, []byte{1, 2}, "got []uint8"},
		{"bad element", fixed("char", 2), []Value{'a', "x"}, "[1]"},
		{"bad char", fixed("char", 1), []rune{0xD800}, "surrogate half"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Store(mem, 0, tc.d, tc.v, nil, Realloc{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Store: %v, want %q", err, tc.want)
			}
			if _, err := LowerFlat(tc.v, tc.d, nil, Realloc{}, mem); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("LowerFlat: %v, want %q", err, tc.want)
			}
		})
	}

	// A typed store past the end of memory is refused, not a panic.
	if _, err := storeFixedList(make([]byte, 8), 4, []uint32{1, 2, 3, 4}, 4, binary.PrimitiveDesc{Prim: "u32"}, nil, Realloc{}); err == nil || !strings.Contains(err.Error(), "overflow memory") {
		t.Errorf("overflow: %v", err)
	}
}

func TestFixedList_LiftErrors(t *testing.T) {
	two := []CoreValue{NewCoreValueI32(1), NewCoreValueI32(2)}
	for _, tc := range []struct {
		name string
		d    binary.TypeDesc
		vals []CoreValue
		want string
	}{
		{"typed runs out", fixed("u32", 3), two, ""},
		{"boxed runs out", fixed("error-context", 3), two, ""},
		{"bad char", fixed("char", 2), []CoreValue{NewCoreValueI32('a'), NewCoreValueI32(0x110000)}, "[1]: liftFlat char: value 1114112 out of range"},
		{"bad element", binary.FixedListDesc{Element: refTo(nil), Length: 1}, two[:1], "unsupported type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := liftFlatImpl(NewCoreValueIter(tc.vals), tc.d, nil, func(uint32) binary.TypeDesc { return binary.FuncDesc{} }, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want %q", err, tc.want)
			}
		})
	}
	// A primitive with no typed slice lifts boxed.
	got, err := LiftFlat(two, fixed("error-context", 2), nil, nil)
	if err != nil || !reflect.DeepEqual(got, []Value{uint32(1), uint32(2)}) {
		t.Errorf("error-context: %#v, %v", got, err)
	}
}

// An element that does not resolve, or has no layout, fails every path.
func TestFixedList_BadElement(t *testing.T) {
	unresolved := binary.FixedListDesc{Element: binary.TypeRef{}, Length: 2}
	noLayout := binary.FixedListDesc{Element: refTo(nil), Length: 2}
	funcType := oneType(binary.FuncDesc{})
	tooBig := binary.FixedListDesc{Element: refTo(nil), Length: 2}
	hugeElem := oneType(fixed("u64", 1<<25))
	mem := make([]byte, 64)
	for _, tc := range []struct {
		name    string
		d       binary.FixedListDesc
		resolve Resolver
	}{
		{"unresolved", unresolved, nil},
		{"no layout", noLayout, funcType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Alignment(tc.d, tc.resolve); err == nil {
				t.Error("Alignment")
			}
			if _, err := Size(tc.d, tc.resolve); err == nil {
				t.Error("Size")
			}
			if _, err := Flatten(tc.d, tc.resolve); err == nil {
				t.Error("Flatten")
			}
			if _, err := FlatWidth(tc.d, tc.resolve); err == nil {
				t.Error("FlatWidth")
			}
			if _, err := loadValue(mem, 0, tc.d, tc.resolve); err == nil {
				t.Error("load")
			}
			if _, err := storeValue(mem, 0, tc.d, []Value{uint32(1), uint32(2)}, 4, tc.resolve, Realloc{}); err == nil {
				t.Error("store")
			}
			if _, err := lowerFlatImpl([]Value{uint32(1), uint32(2)}, tc.d, nil, tc.resolve, Realloc{}, mem); err == nil {
				t.Error("lower")
			}
			if _, err := liftFlatImpl(NewCoreValueIter([]CoreValue{NewCoreValueI32(1), NewCoreValueI32(2)}), tc.d, nil, tc.resolve, mem); err == nil {
				t.Error("lift")
			}
		})
	}
	// An element whose alignment is fine but whose size is not.
	if _, err := storeFixedList(mem, 0, []Value{}, 2, hugeElem(0), hugeElem, Realloc{}); err == nil {
		t.Error("store of an oversized element")
	}
	if _, err := Size(tooBig, hugeElem); err == nil {
		t.Error("Size of an oversized element")
	}
}

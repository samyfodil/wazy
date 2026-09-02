package abi

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// A list of a fixed-width primitive lifts to the typed slice for that
// primitive. The typed path decodes bytes itself rather than going through
// loadPrimitive per element, so it could drift from the general path in a way
// nothing would report: the guest would simply receive different numbers.
//
// Every test here therefore compares the two paths against each other rather
// than against hand-written expectations, over values chosen to catch the ways
// a decode goes wrong -- sign extension, endianness, float bit patterns, and
// the extremes of each width.

// scalarListCases is one entry per fixed-width primitive: the WIT name, the
// element size, and values spanning that type's range.
var scalarListCases = []struct {
	prim  string
	size  uint32
	elems []Value // in the element Value shape loadPrimitive produces
}{
	{"bool", 1, []Value{true, false, true}},
	{"u8", 1, []Value{uint32(0), uint32(1), uint32(127), uint32(255)}},
	{"s8", 1, []Value{int32(0), int32(1), int32(-1), int32(127), int32(-128)}},
	{"u16", 2, []Value{uint32(0), uint32(1), uint32(0x00FF), uint32(0xFFFF)}},
	{"s16", 2, []Value{int32(0), int32(-1), int32(32767), int32(-32768)}},
	{"u32", 4, []Value{uint32(0), uint32(1), uint32(0xDEADBEEF), uint32(math.MaxUint32)}},
	{"s32", 4, []Value{int32(0), int32(-1), int32(math.MaxInt32), int32(math.MinInt32)}},
	{"u64", 8, []Value{uint64(0), uint64(1), uint64(math.MaxUint64)}},
	{"s64", 8, []Value{int64(0), int64(-1), int64(math.MaxInt64), int64(math.MinInt64)}},
	// negative zero is a distinct bit pattern from zero, and float32(-0) is
	// not it -- Go folds that to positive zero, so it has to be built.
	{"f32", 4, []Value{float32(0), float32(math.Copysign(0, -1)), float32(1.5), float32(math.Inf(-1))}},
	{"f64", 8, []Value{float64(0), math.Copysign(0, -1), float64(1.5), math.Inf(1), math.MaxFloat64}},
	{"char", 4, []Value{rune(0), rune('a'), rune(0x10FFFF), rune(0xD7FF)}},
}

// storeElems writes elems into mem at ptr using storePrimitive -- the general
// path -- so the typed decode is read against bytes it did not write.
func storeElems(t *testing.T, mem []byte, ptr uint32, prim string, size uint32, elems []Value) {
	t.Helper()
	for i, e := range elems {
		if _, err := storePrimitive(mem, ptr+uint32(i)*size, prim, e, Realloc{}); err != nil {
			t.Fatalf("storePrimitive %s[%d]: %v", prim, i, err)
		}
	}
}

// TestScalarListLiftMatchesGeneralPath is the contract: the typed slice holds
// exactly the values the []Value path would have held, element for element.
func TestScalarListLiftMatchesGeneralPath(t *testing.T) {
	for _, tc := range scalarListCases {
		t.Run(tc.prim, func(t *testing.T) {
			mem := make([]byte, 256)
			const ptr = 16
			storeElems(t, mem, ptr, tc.prim, tc.size, tc.elems)

			elemType := binary.PrimitiveDesc{Prim: tc.prim}
			n := uint32(len(tc.elems))

			general, err := loadListFromRange(mem, ptr, n, elemType, nil)
			if err != nil {
				t.Fatalf("general path: %v", err)
			}
			typed, handled, err := liftScalarList(mem, ptr, n, tc.prim)
			if err != nil {
				t.Fatalf("typed path: %v", err)
			}
			if !handled {
				t.Fatalf("%s should have a typed slice", tc.prim)
			}

			rv := reflect.ValueOf(typed)
			if rv.Kind() != reflect.Slice {
				t.Fatalf("typed path returned %T, want a slice", typed)
			}
			if rv.Len() != len(general) {
				t.Fatalf("typed path has %d elements, general path %d", rv.Len(), len(general))
			}
			for i := range general {
				// Compare numerically: the typed element is the natural Go
				// type of the primitive's width, the general one is widened to
				// the element Value type, so identical values differ in Go type.
				if !sameNumber(t, rv.Index(i).Interface(), general[i]) {
					t.Errorf("element %d: typed %#v, general %#v", i, rv.Index(i).Interface(), general[i])
				}
			}
		})
	}
}

// TestScalarListRoundTrip stores a typed slice and lifts it back, which is the
// path a host func writing a numeric list actually takes.
func TestScalarListRoundTrip(t *testing.T) {
	for _, tc := range scalarListCases {
		t.Run(tc.prim, func(t *testing.T) {
			mem := make([]byte, 256)
			const ptr = 16
			storeElems(t, mem, ptr, tc.prim, tc.size, tc.elems)

			n := uint32(len(tc.elems))
			typed, _, err := liftScalarList(mem, ptr, n, tc.prim)
			if err != nil {
				t.Fatalf("lift: %v", err)
			}

			// Store it back into a fresh arena through the typed path.
			out := make([]byte, 256)
			gotPtr, gotN, _, handled, err := storeScalarList(out, typed, tc.prim, bumpRealloc(64))
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			if !handled {
				t.Fatalf("%s typed slice should have been stored by the typed path", tc.prim)
			}
			if gotN != n {
				t.Fatalf("stored %d elements, want %d", gotN, n)
			}

			again, _, err := liftScalarList(out, gotPtr, gotN, tc.prim)
			if err != nil {
				t.Fatalf("re-lift: %v", err)
			}
			if !reflect.DeepEqual(typed, again) {
				t.Errorf("round trip changed the list:\n got %#v\nwant %#v", again, typed)
			}
		})
	}
}

// The general []Value shape stays acceptable for storing, since an embedder
// written before the typed shape existed still hands one over.
func TestScalarListStoreAcceptsValueShape(t *testing.T) {
	for _, tc := range scalarListCases {
		t.Run(tc.prim, func(t *testing.T) {
			mem := make([]byte, 256)
			elemType := binary.PrimitiveDesc{Prim: tc.prim}

			ptr, n, _, err := allocStoreAnyList(mem, tc.elems, elemType, nil, bumpRealloc(64))
			if err != nil {
				t.Fatalf("store []Value: %v", err)
			}
			if n != uint32(len(tc.elems)) {
				t.Fatalf("stored %d elements, want %d", n, len(tc.elems))
			}

			back, err := loadListFromRange(mem, ptr, n, elemType, nil)
			if err != nil {
				t.Fatalf("load back: %v", err)
			}
			for i := range tc.elems {
				if !sameNumber(t, tc.elems[i], back[i]) {
					t.Errorf("element %d: stored %#v, read back %#v", i, tc.elems[i], back[i])
				}
			}
		})
	}
}

// An element type with no typed slice keeps the general shape.
func TestScalarListLeavesAggregatesAlone(t *testing.T) {
	for _, prim := range []string{"string", "error-context"} {
		t.Run(prim, func(t *testing.T) {
			if _, handled, _ := liftScalarList(nil, 0, 0, prim); handled {
				t.Errorf("%s should not have a typed slice", prim)
			}
			if _, _, _, handled, _ := storeScalarList(nil, []Value{}, prim, Realloc{}); handled {
				t.Errorf("%s should not be stored by the typed path", prim)
			}
		})
	}
}

// The typed path keeps the general path's bounds checks, including the
// wraparound a length near the address space produces.
func TestScalarListBounds(t *testing.T) {
	mem := make([]byte, 32)

	if _, _, err := liftScalarList(mem, 8, 100, "u32"); err == nil {
		t.Error("reading past the end of memory should fail")
	}
	// ptr + length*size wraps, so an unchecked bound would pass.
	if _, _, err := liftScalarList(mem, 8, 1<<30, "u32"); err == nil {
		t.Error("a length whose byte span overflows should fail")
	}
	if _, _, err := liftScalarList(mem, 0, 0, "u32"); err != nil {
		t.Errorf("an empty list should lift cleanly: %v", err)
	}
}

// A char list is validated per element, exactly as a lone char is: a list is
// not a way to smuggle in a surrogate half.
func TestScalarListCharValidation(t *testing.T) {
	mem := make([]byte, 32)
	// 0xD800 is the low end of the surrogate range.
	mem[0], mem[1], mem[2], mem[3] = 0x00, 0xD8, 0x00, 0x00
	if _, _, err := liftScalarList(mem, 0, 1, "char"); err == nil {
		t.Error("a surrogate half should not lift as a char")
	}

	mem[0], mem[1], mem[2], mem[3] = 0x00, 0x00, 0x11, 0x00 // 0x110000
	if _, _, err := liftScalarList(mem, 0, 1, "char"); err == nil {
		t.Error("a code point above the Unicode range should not lift")
	}

	if _, _, _, _, err := storeScalarList(make([]byte, 32), []rune{0xD800}, "char", bumpRealloc(0)); err == nil {
		t.Error("a surrogate half should not store as a char")
	}
}

// sameNumber compares two values that may be the same number in different Go
// types (uint16 from the typed path against uint32 from the general one).
func sameNumber(t *testing.T, a, b any) bool {
	t.Helper()
	norm := func(v any) any {
		switch n := v.(type) {
		case bool:
			return n
		case int8:
			return int64(n)
		case int16:
			return int64(n)
		case int32: // rune is int32, so this covers char too
			return int64(n)
		case int64:
			return n
		case byte:
			return uint64(n)
		case uint16:
			return uint64(n)
		case uint32:
			return uint64(n)
		case uint64:
			return n
		case float32:
			return float64(n)
		case float64:
			return n
		}
		return v
	}
	na, nb := norm(a), norm(b)
	if f, ok := na.(float64); ok {
		g, ok := nb.(float64)
		// NaN never equals itself; none of the cases use it, and a bit
		// comparison would be the way if they did.
		return ok && f == g
	}
	return na == nb
}

// bumpRealloc hands out memory from base upward, which is all these tests need
// of an allocator.
func bumpRealloc(base uint32) Realloc {
	next := base
	return Realloc{Call: func(_ context.Context, _, _, align, size uint32) (uint32, error) {
		if align > 1 {
			next = (next + align - 1) &^ (align - 1)
		}
		p := next
		next += size
		return p, nil
	}}
}

// A signed byte lifts as int32 whatever its sign, so what the host receives can
// be stored back.
//
// loadInt used to return early only for negatives, leaving a non-negative s8 to
// fall through as a uint32 -- which storePrimitive refused, so an s8 a host
// lifted could not be handed back unless it happened to be negative. The wider
// signed widths never had the split, which is why this only ever bit s8.
func TestSignedByteLiftsSigned(t *testing.T) {
	mem := make([]byte, 16)
	for _, v := range []int8{0, 1, 127, -1, -128} {
		mem[0] = byte(v)

		lifted, err := loadPrimitive(mem, 0, "s8")
		if err != nil {
			t.Fatalf("load s8 %d: %v", v, err)
		}
		got, ok := lifted.(int32)
		if !ok {
			t.Fatalf("s8 %d lifted as %T, want int32", v, lifted)
		}
		if got != int32(v) {
			t.Errorf("s8 %d lifted as %d", v, got)
		}
		if _, err := storePrimitive(mem, 8, "s8", lifted, Realloc{}); err != nil {
			t.Errorf("storing back the lifted s8 %d: %v", v, err)
		}
	}
}

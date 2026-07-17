package abi

import (
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file covers the error branches of the code that used to be stubbed
// (lowerFlatString/lowerFlatList/liftFlatString/liftFlatList), plus the
// option/result padding+coercion logic that had to be fixed alongside them
// (see flat.go's lowerFlatOption/lowerFlatResult/liftFlatOption/
// liftFlatResult and the coercingValueIter/deCoerceValue fix) once real
// oracle coverage exposed that they were never exercised.

// ---------- lowerFlatString error branches ----------

func TestLowerFlatStringReallocFails(t *testing.T) {
	mem := make([]byte, 100)
	_, err := lowerFlatString("hi", failRealloc, mem)
	if err == nil || !strings.Contains(err.Error(), "lowerFlatString") {
		t.Errorf("expected wrapped lowerFlatString error, got %v", err)
	}
}

func TestLowerFlatStringAllocOutOfBounds(t *testing.T) {
	mem := make([]byte, 4)
	badRealloc := ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 1000, nil })
	_, err := lowerFlatString("hello", badRealloc, mem)
	if err == nil {
		t.Error("expected error for lowerFlatString alloc out of bounds")
	}
}

// ---------- lowerFlatList error branches ----------

func TestLowerFlatListNotAList(t *testing.T) {
	mem := make([]byte, 100)
	_, err := lowerFlatList("not a list", binary.PrimitiveDesc{Prim: "u32"}, nil, okRealloc, mem)
	if err == nil || !strings.Contains(err.Error(), "expected []Value") {
		t.Errorf("expected type error, got %v", err)
	}
}

func TestLowerFlatListSizeError(t *testing.T) {
	mem := make([]byte, 100)
	_, err := lowerFlatList([]Value{uint32(1)}, binary.FuncDesc{}, nil, okRealloc, mem)
	if err == nil {
		t.Error("expected error for lowerFlatList element Size failure")
	}
}

func TestLowerFlatListReallocFails(t *testing.T) {
	mem := make([]byte, 100)
	_, err := lowerFlatList([]Value{uint32(1)}, binary.PrimitiveDesc{Prim: "u32"}, nil, failRealloc, mem)
	if err == nil || !strings.Contains(err.Error(), "lowerFlatList") {
		t.Errorf("expected wrapped lowerFlatList error, got %v", err)
	}
}

func TestLowerFlatListAllocOutOfBounds(t *testing.T) {
	mem := make([]byte, 4)
	badRealloc := ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 1000, nil })
	_, err := lowerFlatList([]Value{uint32(1)}, binary.PrimitiveDesc{Prim: "u32"}, nil, badRealloc, mem)
	if err == nil {
		t.Error("expected error for lowerFlatList alloc out of bounds")
	}
}

// TestLowerFlatListElementError proves element errors during lowering
// propagate out of lowerFlatList (not just realloc/bounds failures) --
// i.e. that allocStoreList's per-element storeValue errors are wrapped and
// returned, not swallowed.
func TestLowerFlatListElementError(t *testing.T) {
	mem := make([]byte, 100)
	_, err := lowerFlatList([]Value{"not-a-u32"}, binary.PrimitiveDesc{Prim: "u32"}, nil, okRealloc, mem)
	if err == nil || !strings.Contains(err.Error(), "lowerFlatList") {
		t.Errorf("expected wrapped lowerFlatList element error, got %v", err)
	}
}

// ---------- liftFlatString error branches ----------

func TestLiftFlatStringOutOfBounds(t *testing.T) {
	mem := make([]byte, 10)
	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(5), NewCoreValueI32(100)})
	_, err := liftFlatString(vi, mem)
	if err == nil || !strings.Contains(err.Error(), "liftFlatString") {
		t.Errorf("expected wrapped liftFlatString error, got %v", err)
	}
}

func TestLiftFlatStringIterExhausted(t *testing.T) {
	mem := make([]byte, 10)
	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(0)})
	if _, err := liftFlatString(vi, mem); err == nil {
		t.Error("expected error when iterator is exhausted before len")
	}
	vi2 := NewCoreValueIter(nil)
	if _, err := liftFlatString(vi2, mem); err == nil {
		t.Error("expected error when iterator is exhausted before ptr")
	}
}

// ---------- liftFlatList error branches ----------

func TestLiftFlatListOutOfBounds(t *testing.T) {
	mem := make([]byte, 10)
	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(5), NewCoreValueI32(100)})
	_, err := liftFlatList(vi, binary.PrimitiveDesc{Prim: "u32"}, nil, mem)
	if err == nil || !strings.Contains(err.Error(), "liftFlatList") {
		t.Errorf("expected wrapped liftFlatList error, got %v", err)
	}
}

// TestLiftFlatListElementError proves that an error loading a specific
// element (not just an out-of-bounds region) propagates out of
// liftFlatList, e.g. an invalid encoded char.
func TestLiftFlatListElementError(t *testing.T) {
	mem := make([]byte, 16)
	// Invalid char code point (>= 0x110000) at ptr=0.
	_ = storeInt(mem, 0, uint32(0x110000), 4)
	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(0), NewCoreValueI32(1)})
	_, err := liftFlatList(vi, binary.PrimitiveDesc{Prim: "char"}, nil, mem)
	if err == nil || !strings.Contains(err.Error(), "liftFlatList") {
		t.Errorf("expected wrapped liftFlatList element error, got %v", err)
	}
}

func TestLiftFlatListIterExhausted(t *testing.T) {
	mem := make([]byte, 10)
	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(0)})
	if _, err := liftFlatList(vi, binary.PrimitiveDesc{Prim: "u32"}, nil, mem); err == nil {
		t.Error("expected error when iterator is exhausted before len")
	}
}

// ---------- zeroCoreValueForKind ----------

func TestZeroCoreValueForKind(t *testing.T) {
	cases := []struct {
		kind string
		want CoreValue
	}{
		{"i32", NewCoreValueI32(0)},
		{"i64", NewCoreValueI64(0)},
		{"f32", NewCoreValueF32(0)},
		{"f64", NewCoreValueF64(0)},
	}
	for _, c := range cases {
		got, err := zeroCoreValueForKind(c.kind)
		if err != nil {
			t.Errorf("zeroCoreValueForKind(%s): unexpected error %v", c.kind, err)
		}
		if got != c.want {
			t.Errorf("zeroCoreValueForKind(%s) = %+v, want %+v", c.kind, got, c.want)
		}
	}

	if _, err := zeroCoreValueForKind("bogus"); err == nil {
		t.Error("expected error for unknown kind")
	}
}

// ---------- deCoerceValue ----------

func TestDeCoerceValue(t *testing.T) {
	i32 := NewCoreValueI32(5)
	if got := deCoerceValue(i32, "i32", "i32"); got != i32 {
		t.Errorf("identity: got %+v, want %+v", got, i32)
	}

	f32bits := NewCoreValueI32(0x3fc00000) // 1.5f bit pattern
	got := deCoerceValue(f32bits, "i32", "f32")
	if got.Kind != "f32" || got.AsF32() != 1.5 {
		t.Errorf("i32->f32: got %+v, want f32 1.5", got)
	}

	i64 := NewCoreValueI64(0x1_0000_002A) // low 32 bits = 42, with garbage above
	got = deCoerceValue(i64, "i64", "i32")
	if got.Kind != "i32" || got.AsI32() != 42 {
		t.Errorf("i64->i32: got %+v, want i32 42", got)
	}

	i64f32 := NewCoreValueI64(0x3fc00000)
	got = deCoerceValue(i64f32, "i64", "f32")
	if got.Kind != "f32" || got.AsF32() != 1.5 {
		t.Errorf("i64->f32: got %+v, want f32 1.5", got)
	}

	f64bits := NewCoreValueI64(0x3ff8000000000000) // 1.5 bit pattern
	got = deCoerceValue(f64bits, "i64", "f64")
	if got.Kind != "f64" || got.AsF64() != 1.5 {
		t.Errorf("i64->f64: got %+v, want f64 1.5", got)
	}

	// Unrecognized combination falls through to the identity default.
	weird := NewCoreValueF64(9.5)
	got = deCoerceValue(weird, "f64", "i32")
	if got != weird {
		t.Errorf("unknown combo: got %+v, want unchanged %+v", got, weird)
	}
}

// ---------- lowerFlatOption error branches ----------

func TestLowerFlatOptionFlattenError(t *testing.T) {
	mem := make([]byte, 100)
	_, err := lowerFlatOption(nil, binary.FuncDesc{}, nil, okRealloc, mem)
	if err == nil {
		t.Error("expected error when elemType cannot be flattened")
	}
}

func TestLowerFlatOptionSomePayloadError(t *testing.T) {
	mem := make([]byte, 100)
	_, err := lowerFlatOption("not-a-u32", binary.PrimitiveDesc{Prim: "u32"}, nil, okRealloc, mem)
	if err == nil {
		t.Error("expected error propagated from Some payload lowering")
	}
}

// TestLowerFlatOptionNonePaddingWidth proves the "none" case reserves the
// element's full flat width (not just the discriminant), for an element
// wider than 1 core value.
func TestLowerFlatOptionNonePaddingWidth(t *testing.T) {
	mem := make([]byte, 100)
	result, err := lowerFlatOption(nil, binary.PrimitiveDesc{Prim: "string"}, nil, okRealloc, mem)
	if err != nil {
		t.Fatalf("lowerFlatOption: %v", err)
	}
	if len(result) != 3 { // disc + 2-wide string padding
		t.Fatalf("expected 3 core values (disc + 2 padding), got %d: %+v", len(result), result)
	}
	if result[0].AsI32() != 0 || result[1].AsI32() != 0 || result[2].AsI32() != 0 {
		t.Errorf("expected all-zero none+padding, got %+v", result)
	}
}

// ---------- lowerFlatResult error branches ----------

func TestLowerFlatResultFlattenError(t *testing.T) {
	mem := make([]byte, 100)
	badRef := binary.TypeRef{}
	_, err := lowerFlatResult(ResultValue{IsErr: false, Payload: nil}, binary.ResultDesc{Ok: &badRef}, nil, okRealloc, mem)
	if err == nil {
		t.Error("expected error when result Ok type cannot be flattened/resolved")
	}
}

func TestLowerFlatResultNotAResultValue(t *testing.T) {
	mem := make([]byte, 100)
	_, err := lowerFlatResult("not a result", binary.ResultDesc{}, nil, okRealloc, mem)
	if err == nil || !strings.Contains(err.Error(), "expected ResultValue") {
		t.Errorf("expected type error, got %v", err)
	}
}

func TestLowerFlatResultPayloadError(t *testing.T) {
	mem := make([]byte, 100)
	okRef := binary.TypeRef{Primitive: "u32"}
	_, err := lowerFlatResult(ResultValue{IsErr: false, Payload: "bad"}, binary.ResultDesc{Ok: &okRef}, nil, okRealloc, mem)
	if err == nil {
		t.Error("expected error propagated from Ok payload lowering")
	}
}

// TestLowerFlatResultPadsNarrowerArm proves that when the two arms flatten
// to different widths, the inactive/narrower arm's leftover joined
// positions are zero-padded (and the active arm's values coerced), exactly
// like a variant -- exercising the coerceValue call and the padding loop
// inside lowerFlatResult.
func TestLowerFlatResultPadsNarrowerArm(t *testing.T) {
	mem := make([]byte, 100)
	okRef := binary.TypeRef{Primitive: "string"} // width 2: (ptr, len)
	errRef := binary.TypeRef{Primitive: "u32"}   // width 1
	desc := binary.ResultDesc{Ok: &okRef, Err: &errRef}

	result, err := lowerFlatResult(ResultValue{IsErr: true, Payload: uint32(42)}, desc, nil, okRealloc, mem)
	if err != nil {
		t.Fatalf("lowerFlatResult: %v", err)
	}
	// disc(1) + err payload(42) + zero padding for the position only the ok
	// arm's second string component fills.
	if len(result) != 3 {
		t.Fatalf("expected 3 core values, got %d: %+v", len(result), result)
	}
	if result[0].AsI32() != 1 {
		t.Errorf("disc: got %d, want 1", result[0].AsI32())
	}
	if result[1].AsI32() != 42 {
		t.Errorf("payload: got %d, want 42", result[1].AsI32())
	}
	if result[2].AsI32() != 0 {
		t.Errorf("padding: got %d, want 0", result[2].AsI32())
	}
}

// ---------- liftFlatOption error branches ----------

func TestLiftFlatOptionFlattenError(t *testing.T) {
	mem := make([]byte, 100)
	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(0)})
	_, err := liftFlatOption(vi, binary.FuncDesc{}, nil, mem)
	if err == nil {
		t.Error("expected error when elemType cannot be flattened")
	}
}

func TestLiftFlatOptionNonePaddingExhausted(t *testing.T) {
	mem := make([]byte, 100)
	// disc=0 (none) but no padding value follows, even though string needs 2.
	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(0)})
	_, err := liftFlatOption(vi, binary.PrimitiveDesc{Prim: "string"}, nil, mem)
	if err == nil {
		t.Error("expected error when none-padding is exhausted early")
	}
}

func TestLiftFlatOptionInvalidDiscriminant(t *testing.T) {
	mem := make([]byte, 100)
	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(7)})
	_, err := liftFlatOption(vi, binary.PrimitiveDesc{Prim: "u32"}, nil, mem)
	if err == nil {
		t.Error("expected error for invalid option discriminant")
	}
}

func TestLiftFlatOptionSomePayloadError(t *testing.T) {
	mem := make([]byte, 100)
	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(1), NewCoreValueI32(0x110000)})
	_, err := liftFlatOption(vi, binary.PrimitiveDesc{Prim: "char"}, nil, mem)
	if err == nil {
		t.Error("expected error propagated from Some payload lifting")
	}
}

// ---------- liftFlatResult error branches ----------

func TestLiftFlatResultFlattenError(t *testing.T) {
	mem := make([]byte, 100)
	badRef := binary.TypeRef{}
	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(0)})
	_, err := liftFlatResult(vi, binary.ResultDesc{Ok: &badRef}, nil, mem)
	if err == nil {
		t.Error("expected error when result Ok type cannot be flattened/resolved")
	}
}

func TestLiftFlatResultInvalidDiscriminant(t *testing.T) {
	mem := make([]byte, 100)
	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(9)})
	_, err := liftFlatResult(vi, binary.ResultDesc{}, nil, mem)
	if err == nil {
		t.Error("expected error for invalid result discriminant")
	}
}

func TestLiftFlatResultPayloadError(t *testing.T) {
	mem := make([]byte, 100)
	okRef := binary.TypeRef{Primitive: "char"}
	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(0), NewCoreValueI32(0x110000)})
	_, err := liftFlatResult(vi, binary.ResultDesc{Ok: &okRef}, nil, mem)
	if err == nil {
		t.Error("expected error propagated from Ok payload lifting")
	}
}

// TestLiftFlatResultUnexpectedIterType proves liftFlatResult (like
// liftFlatVariant) rejects a valueIter that is neither *CoreValueIter nor
// *coercingValueIter, rather than silently misbehaving.
type mockValueIter struct{}

func (mockValueIter) Next() (CoreValue, error) { return NewCoreValueI32(0), nil }
func (mockValueIter) Done() bool               { return true }

// TestLiftFlatResultComposesArbitraryIter verifies liftFlatResult accepts any
// valueIter (not just a *CoreValueIter), which is what lets nested
// variant/result payloads compose through an enclosing coercingValueIter -- the
// fix for the "CoreValueIter index out of range" bug on result<record,variant>.
// mockValueIter yields a 0 discriminant (Ok arm) then a 0 u32; lifting must
// succeed and read through the generic iterator without a type-assertion error.
func TestLiftFlatResultComposesArbitraryIter(t *testing.T) {
	mem := make([]byte, 100)
	okRef := binary.TypeRef{Primitive: "u32"}
	v, err := liftFlatResult(mockValueIter{}, binary.ResultDesc{Ok: &okRef}, nil, mem)
	if err != nil {
		t.Fatalf("liftFlatResult with a generic valueIter should succeed, got %v", err)
	}
	rv, ok := v.(ResultValue)
	if !ok || rv.IsErr || rv.Payload != uint32(0) {
		t.Fatalf("result = %#v, want Ok(u32 0)", v)
	}
}

// TestLiftFlatResultRoundTripsPaddedArm proves the mirror image of
// TestLowerFlatResultPadsNarrowerArm: lifting the exact core values that
// lowering produced for the narrower arm correctly discards the padding and
// reconstructs the original payload.
func TestLiftFlatResultRoundTripsPaddedArm(t *testing.T) {
	mem := make([]byte, 100)
	okRef := binary.TypeRef{Primitive: "string"}
	errRef := binary.TypeRef{Primitive: "u32"}
	desc := binary.ResultDesc{Ok: &okRef, Err: &errRef}

	vi := NewCoreValueIter([]CoreValue{NewCoreValueI32(1), NewCoreValueI32(42), NewCoreValueI32(0)})
	got, err := liftFlatResult(vi, desc, nil, mem)
	if err != nil {
		t.Fatalf("liftFlatResult: %v", err)
	}
	rv, ok := got.(ResultValue)
	if !ok || !rv.IsErr || rv.Payload != uint32(42) {
		t.Errorf("got %#v, want ResultValue{IsErr:true, Payload:uint32(42)}", got)
	}
}

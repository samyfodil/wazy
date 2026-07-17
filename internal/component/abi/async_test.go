package abi

import (
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// Phase 0 ABI coverage for the async value types: stream, future, and
// error-context. Per the implementation brief, all three are opaque i32
// handles throughout the ABI layer -- identical treatment to own/borrow
// (StreamDesc/FutureDesc are new descriptor kinds; error-context is a
// PrimitiveDesc{Prim:"error-context"}, since wasm-tools confirms tag 0x64 is
// a terminal primitive valtype, not a parameterized defined type -- see
// binary.isPrimValtype's doc comment). These tests mirror TestOwnSize/
// TestBorrowSize/TestFlattenHandles/TestStoreLoadHandleRoundTrip in
// coverage_test.go/layout_test.go/memory_errbranch_test.go.

// --- Size / Alignment ---

func TestStreamFutureErrorContextSize(t *testing.T) {
	for _, d := range []binary.TypeDesc{
		binary.StreamDesc{},
		binary.FutureDesc{},
		binary.PrimitiveDesc{Prim: "error-context"},
	} {
		size, err := Size(d, noResolver)
		if err != nil {
			t.Fatalf("Size(%T): %v", d, err)
		}
		if size != 4 {
			t.Errorf("Size(%T) = %d, want 4", d, size)
		}
		align, err := Alignment(d, noResolver)
		if err != nil {
			t.Fatalf("Alignment(%T): %v", d, err)
		}
		if align != 4 {
			t.Errorf("Alignment(%T) = %d, want 4", d, align)
		}
	}
}

// --- Flatten ---

func TestFlattenStreamFutureErrorContext(t *testing.T) {
	for _, d := range []binary.TypeDesc{
		binary.StreamDesc{},
		binary.FutureDesc{},
		binary.PrimitiveDesc{Prim: "error-context"},
	} {
		flat, err := Flatten(d, noResolver)
		if err != nil {
			t.Fatalf("Flatten(%T): %v", d, err)
		}
		if len(flat) != 1 || flat[0] != "i32" {
			t.Errorf("Flatten(%T) = %v, want [i32]", d, flat)
		}
	}
}

// --- Store/Load round trip (memory.go) ---

func TestStoreLoadStreamFutureErrorContextRoundTrip(t *testing.T) {
	for _, d := range []binary.TypeDesc{
		binary.StreamDesc{},
		binary.FutureDesc{},
		binary.PrimitiveDesc{Prim: "error-context"},
	} {
		mem := make([]byte, 8)
		if err := storeValue(mem, 0, d, uint32(42), nil, okRealloc); err != nil {
			t.Fatalf("storeValue(%T): %v", d, err)
		}
		v, err := loadValue(mem, 0, d, nil)
		if err != nil {
			t.Fatalf("loadValue(%T): %v", d, err)
		}
		if v.(uint32) != 42 {
			t.Errorf("round trip (%T) = %v, want 42", d, v)
		}
	}
}

func TestStoreStreamFutureErrorContextWrongType(t *testing.T) {
	for _, d := range []binary.TypeDesc{
		binary.StreamDesc{},
		binary.FutureDesc{},
		binary.PrimitiveDesc{Prim: "error-context"},
	} {
		mem := make([]byte, 8)
		if err := storeValue(mem, 0, d, "not-a-handle", nil, okRealloc); err == nil {
			t.Errorf("storeValue(%T): expected error for non-uint32 value", d)
		}
	}
}

// --- LowerFlat / LiftFlat round trip (flat.go) ---

func TestLowerLiftFlatStreamFutureErrorContext(t *testing.T) {
	mem := make([]byte, 8)
	for _, d := range []binary.TypeDesc{
		binary.StreamDesc{},
		binary.FutureDesc{},
		binary.PrimitiveDesc{Prim: "error-context"},
	} {
		cvs, err := LowerFlat(uint32(7), d, noResolver, okRealloc, mem)
		if err != nil {
			t.Fatalf("LowerFlat(%T): %v", d, err)
		}
		if len(cvs) != 1 || cvs[0].AsI32() != 7 {
			t.Fatalf("LowerFlat(%T) = %v, want one CoreValue(i32=7)", d, cvs)
		}
		v, err := LiftFlat(cvs, d, noResolver, mem)
		if err != nil {
			t.Fatalf("LiftFlat(%T): %v", d, err)
		}
		if v.(uint32) != 7 {
			t.Errorf("LiftFlat(%T) = %v, want 7", d, v)
		}
	}
}

// --- CompileLower / CompileLift plans (plan.go) ---

func TestCompileLowerLiftStreamFutureHandle(t *testing.T) {
	mem := make([]byte, 8)
	for _, d := range []binary.TypeDesc{
		binary.StreamDesc{},
		binary.FutureDesc{},
	} {
		lstep, err := CompileLower(d, noResolver)
		if err != nil {
			t.Fatalf("CompileLower(%T): %v", d, err)
		}
		if lstep.kind != lowerKindHandle {
			t.Fatalf("CompileLower(%T) kind = %d, want lowerKindHandle", d, lstep.kind)
		}
		cvs, err := lstep.Lower(nil, uint32(9), Realloc{}, mem)
		if err != nil {
			t.Fatalf("Lower(%T): %v", d, err)
		}
		if len(cvs) != 1 || cvs[0].AsI32() != 9 {
			t.Fatalf("Lower(%T) = %v, want one CoreValue(i32=9)", d, cvs)
		}

		fstep, err := CompileLift(d, noResolver)
		if err != nil {
			t.Fatalf("CompileLift(%T): %v", d, err)
		}
		if fstep.kind != liftHandle {
			t.Fatalf("CompileLift(%T) kind = %d, want liftHandle", d, fstep.kind)
		}
		v, err := fstep.Lift(cvs, mem)
		if err != nil {
			t.Fatalf("Lift(%T): %v", d, err)
		}
		if v.(uint32) != 9 {
			t.Errorf("Lift(%T) = %v, want 9", d, v)
		}
	}
}

// error-context flows through the PrimitiveDesc arm of CompileLower/
// CompileLift (kind lowerKindPrimitive/liftPrimitive with prim=
// "error-context"), not lowerKindHandle -- this is the one place its
// "primitive, not handle-descriptor" encoding is externally observable, so
// it gets its own test rather than sharing the loop above.
func TestCompileLowerLiftErrorContextPrimitive(t *testing.T) {
	d := binary.PrimitiveDesc{Prim: "error-context"}
	mem := make([]byte, 8)

	lstep, err := CompileLower(d, noResolver)
	if err != nil {
		t.Fatalf("CompileLower: %v", err)
	}
	if lstep.kind != lowerKindPrimitive || lstep.prim != "error-context" {
		t.Fatalf("CompileLower kind/prim = %d/%q, want lowerKindPrimitive/error-context", lstep.kind, lstep.prim)
	}
	cvs, err := lstep.Lower(nil, uint32(11), Realloc{}, mem)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	fstep, err := CompileLift(d, noResolver)
	if err != nil {
		t.Fatalf("CompileLift: %v", err)
	}
	if fstep.kind != liftPrimitive || fstep.prim != "error-context" {
		t.Fatalf("CompileLift kind/prim = %d/%q, want liftPrimitive/error-context", fstep.kind, fstep.prim)
	}
	v, err := fstep.Lift(cvs, mem)
	if err != nil {
		t.Fatalf("Lift: %v", err)
	}
	if v.(uint32) != 11 {
		t.Errorf("Lift = %v, want 11", v)
	}
}

// --- descriptor Kind()/round-trip sanity (binary package types used here) ---

func TestStreamFutureDescKind(t *testing.T) {
	if (binary.StreamDesc{}).Kind() != "stream" {
		t.Errorf("StreamDesc.Kind() = %q, want stream", (binary.StreamDesc{}).Kind())
	}
	if (binary.FutureDesc{}).Kind() != "future" {
		t.Errorf("FutureDesc.Kind() = %q, want future", (binary.FutureDesc{}).Kind())
	}
	u8 := binary.TypeRef{Primitive: "u8"}
	sd := binary.StreamDesc{Element: &u8}
	if sd.Element.Primitive != "u8" {
		t.Errorf("StreamDesc.Element = %+v, want u8", sd.Element)
	}
}

// TestFlattenUnknownPrimitiveStillErrors is a guard-rail: adding the
// "error-context" case to flattenPrimitive/lowerPrimitiveCore/
// liftScalarPrimitive must not accidentally turn the default branch into a
// no-op for genuinely unknown primitive names.
func TestFlattenUnknownPrimitiveStillErrors(t *testing.T) {
	_, err := Flatten(binary.PrimitiveDesc{Prim: "not-a-real-prim"}, noResolver)
	if err == nil || !strings.Contains(err.Error(), "unknown primitive") {
		t.Fatalf("expected unknown primitive error, got %v", err)
	}
}

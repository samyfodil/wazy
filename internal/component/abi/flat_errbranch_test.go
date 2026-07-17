package abi

import (
	"math"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// TestLowerFlatErrors tests error conditions in LowerFlat.
func TestLowerFlatErrors(t *testing.T) {
	tests := []struct {
		name        string
		value       Value
		typeDesc    binary.TypeDesc
		expectError bool
	}{
		// Type mismatches
		{
			name:        "bool wrong type",
			value:       42, // not a bool
			typeDesc:    binary.PrimitiveDesc{Prim: "bool"},
			expectError: true,
		},
		{
			name:        "u32 wrong type",
			value:       "not an int",
			typeDesc:    binary.PrimitiveDesc{Prim: "u32"},
			expectError: true,
		},
		{
			name:        "s32 wrong type",
			value:       float32(3.14),
			typeDesc:    binary.PrimitiveDesc{Prim: "s32"},
			expectError: true,
		},
		{
			name:        "u64 wrong type",
			value:       "string",
			typeDesc:    binary.PrimitiveDesc{Prim: "u64"},
			expectError: true,
		},
		{
			name:        "s64 wrong type",
			value:       123,
			typeDesc:    binary.PrimitiveDesc{Prim: "s64"},
			expectError: true,
		},
		{
			name:        "f32 wrong type",
			value:       "not a float",
			typeDesc:    binary.PrimitiveDesc{Prim: "f32"},
			expectError: true,
		},
		{
			name:        "f64 wrong type",
			value:       42,
			typeDesc:    binary.PrimitiveDesc{Prim: "f64"},
			expectError: true,
		},
		{
			name:        "char wrong type",
			value:       "string",
			typeDesc:    binary.PrimitiveDesc{Prim: "char"},
			expectError: true,
		},
		{
			name:        "char out of range",
			value:       rune(0x110000),
			typeDesc:    binary.PrimitiveDesc{Prim: "char"},
			expectError: true,
		},
		{
			name:        "char surrogate half",
			value:       rune(0xD800),
			typeDesc:    binary.PrimitiveDesc{Prim: "char"},
			expectError: true,
		},
		{
			name:        "string wrong type",
			value:       42,
			typeDesc:    binary.PrimitiveDesc{Prim: "string"},
			expectError: true,
		},
		{
			name:        "record wrong type",
			value:       "not a record",
			typeDesc:    binary.RecordDesc{Fields: []binary.RecordField{}},
			expectError: true,
		},
		{
			name:        "record field count mismatch",
			value:       []Value{1, 2, 3},
			typeDesc:    binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: binary.TypeRef{Primitive: "u32"}}}},
			expectError: true,
		},
		{
			name:        "variant wrong type",
			value:       "not a variant",
			typeDesc:    binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a"}}},
			expectError: true,
		},
		{
			name:        "variant case out of range",
			value:       VariantValue{Disc: 99, Payload: nil},
			typeDesc:    binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a"}}},
			expectError: true,
		},
		{
			name:        "flags wrong type",
			value:       "not a uint32",
			typeDesc:    binary.FlagsDesc{Names: []string{"a", "b"}},
			expectError: true,
		},
		{
			name:        "enum wrong type",
			value:       "not a uint32",
			typeDesc:    binary.EnumDesc{Cases: []string{"a", "b"}},
			expectError: true,
		},
		{
			name:        "enum case out of range",
			value:       uint32(99),
			typeDesc:    binary.EnumDesc{Cases: []string{"a", "b"}},
			expectError: true,
		},
		{
			name:        "handle wrong type",
			value:       "not a uint32",
			typeDesc:    binary.OwnDesc{ResourceType: 0},
			expectError: true,
		},
	}

	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := ReallocFunc(func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil })
	mem := make([]byte, 1024)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LowerFlat(tt.value, tt.typeDesc, resolve, realloc, mem)
			if (err != nil) != tt.expectError {
				t.Errorf("LowerFlat: expected error=%v, got error=%v (result=%v)", tt.expectError, err != nil, result)
			}
		})
	}
}

// TestLowerFlatSignedEdgeCases tests edge cases for signed integer conversion.
func TestLowerFlatSignedEdgeCases(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := ReallocFunc(func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil })
	mem := make([]byte, 1024)

	tests := []struct {
		name         string
		value        Value
		typeDesc     binary.TypeDesc
		expectedBits uint64
		expectedKind string
	}{
		{
			name:         "s32 negative minimum",
			value:        int32(-2147483648),
			typeDesc:     binary.PrimitiveDesc{Prim: "s32"},
			expectedBits: 0x80000000,
			expectedKind: "i32",
		},
		{
			name:         "s32 negative one",
			value:        int32(-1),
			typeDesc:     binary.PrimitiveDesc{Prim: "s32"},
			expectedBits: 0xFFFFFFFF,
			expectedKind: "i32",
		},
		{
			name:         "s64 negative minimum",
			value:        int64(-9223372036854775808),
			typeDesc:     binary.PrimitiveDesc{Prim: "s64"},
			expectedBits: 0x8000000000000000,
			expectedKind: "i64",
		},
		{
			name:         "s64 negative one",
			value:        int64(-1),
			typeDesc:     binary.PrimitiveDesc{Prim: "s64"},
			expectedBits: 0xFFFFFFFFFFFFFFFF,
			expectedKind: "i64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LowerFlat(tt.value, tt.typeDesc, resolve, realloc, mem)
			if err != nil {
				t.Fatalf("LowerFlat failed: %v", err)
			}
			if len(result) != 1 {
				t.Fatalf("expected 1 core value, got %d", len(result))
			}
			cv := result[0]
			if cv.Kind != tt.expectedKind {
				t.Errorf("expected kind %s, got %s", tt.expectedKind, cv.Kind)
			}
			if cv.Bits != tt.expectedBits {
				t.Errorf("expected bits 0x%x, got 0x%x", tt.expectedBits, cv.Bits)
			}
		})
	}
}

// TestLowerFlatFloatSpecialValues tests special float values (NaN, Inf).
func TestLowerFlatFloatSpecialValues(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := ReallocFunc(func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil })
	mem := make([]byte, 1024)

	tests := []struct {
		name         string
		value        Value
		typeDesc     binary.TypeDesc
		expectedKind string
	}{
		{
			name:         "f32 positive infinity",
			value:        float32(math.Inf(1)),
			typeDesc:     binary.PrimitiveDesc{Prim: "f32"},
			expectedKind: "f32",
		},
		{
			name:         "f32 negative infinity",
			value:        float32(math.Inf(-1)),
			typeDesc:     binary.PrimitiveDesc{Prim: "f32"},
			expectedKind: "f32",
		},
		{
			name:         "f64 positive infinity",
			value:        math.Inf(1),
			typeDesc:     binary.PrimitiveDesc{Prim: "f64"},
			expectedKind: "f64",
		},
		{
			name:         "f64 negative infinity",
			value:        math.Inf(-1),
			typeDesc:     binary.PrimitiveDesc{Prim: "f64"},
			expectedKind: "f64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LowerFlat(tt.value, tt.typeDesc, resolve, realloc, mem)
			if err != nil {
				t.Fatalf("LowerFlat failed: %v", err)
			}
			if len(result) != 1 {
				t.Fatalf("expected 1 core value, got %d", len(result))
			}
			cv := result[0]
			if cv.Kind != tt.expectedKind {
				t.Errorf("expected kind %s, got %s", tt.expectedKind, cv.Kind)
			}
		})
	}
}

// TestLiftFlatErrors tests error conditions in LiftFlat.
func TestLiftFlatErrors(t *testing.T) {
	mem := make([]byte, 1024)
	resolve := func(idx uint32) binary.TypeDesc { return nil }

	tests := []struct {
		name        string
		values      []CoreValue
		typeDesc    binary.TypeDesc
		expectError bool
	}{
		{
			name:        "empty values for non-empty type",
			values:      []CoreValue{},
			typeDesc:    binary.PrimitiveDesc{Prim: "u32"},
			expectError: true,
		},
		{
			name:        "enum case out of range",
			values:      []CoreValue{NewCoreValueI32(99)},
			typeDesc:    binary.EnumDesc{Cases: []string{"a", "b"}},
			expectError: true,
		},
		{
			name:        "option invalid discriminant",
			values:      []CoreValue{NewCoreValueI32(99)},
			typeDesc:    binary.OptionDesc{Element: binary.TypeRef{Primitive: "u32"}},
			expectError: true,
		},
		{
			name:        "result invalid discriminant",
			values:      []CoreValue{NewCoreValueI32(99)},
			typeDesc:    binary.ResultDesc{Ok: &binary.TypeRef{Primitive: "u32"}},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LiftFlat(tt.values, tt.typeDesc, resolve, mem)
			if (err != nil) != tt.expectError {
				t.Errorf("LiftFlat: expected error=%v, got error=%v (result=%v)", tt.expectError, err != nil, result)
			}
		})
	}
}

// TestLowerFlatLiftFlatRoundTrip tests round-trip conversion for various types.
func TestLowerFlatLiftFlatRoundTrip(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := ReallocFunc(func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil })
	mem := make([]byte, 65536)

	tests := []struct {
		name     string
		value    Value
		typeDesc binary.TypeDesc
	}{
		{
			name:     "u32",
			value:    uint32(12345),
			typeDesc: binary.PrimitiveDesc{Prim: "u32"},
		},
		{
			name:     "s32 positive",
			value:    int32(12345),
			typeDesc: binary.PrimitiveDesc{Prim: "s32"},
		},
		{
			name:     "s32 negative",
			value:    int32(-12345),
			typeDesc: binary.PrimitiveDesc{Prim: "s32"},
		},
		{
			name:     "u64",
			value:    uint64(1234567890123),
			typeDesc: binary.PrimitiveDesc{Prim: "u64"},
		},
		{
			name:     "s64 positive",
			value:    int64(1234567890123),
			typeDesc: binary.PrimitiveDesc{Prim: "s64"},
		},
		{
			name:     "s64 negative",
			value:    int64(-1234567890123),
			typeDesc: binary.PrimitiveDesc{Prim: "s64"},
		},
		{
			name:     "f32",
			value:    float32(3.14),
			typeDesc: binary.PrimitiveDesc{Prim: "f32"},
		},
		{
			name:     "f64",
			value:    float64(3.141592653589793),
			typeDesc: binary.PrimitiveDesc{Prim: "f64"},
		},
		{
			name:     "bool true",
			value:    true,
			typeDesc: binary.PrimitiveDesc{Prim: "bool"},
		},
		{
			name:     "bool false",
			value:    false,
			typeDesc: binary.PrimitiveDesc{Prim: "bool"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Lower the value
			flat, err := LowerFlat(tt.value, tt.typeDesc, resolve, realloc, mem)
			if err != nil {
				t.Fatalf("LowerFlat failed: %v", err)
			}

			// Lift it back
			lifted, err := LiftFlat(flat, tt.typeDesc, resolve, mem)
			if err != nil {
				t.Fatalf("LiftFlat failed: %v", err)
			}

			// Compare the values (with floating point tolerance)
			switch v := tt.value.(type) {
			case float32:
				lv, ok := lifted.(float32)
				if !ok {
					t.Errorf("expected float32, got %T", lifted)
				}
				if math.Abs(float64(v-lv)) > 1e-6 {
					t.Errorf("expected %v, got %v", v, lv)
				}
			case float64:
				lv, ok := lifted.(float64)
				if !ok {
					t.Errorf("expected float64, got %T", lifted)
				}
				if math.Abs(v-lv) > 1e-15 {
					t.Errorf("expected %v, got %v", v, lv)
				}
			default:
				if tt.value != lifted {
					t.Errorf("expected %v, got %v", tt.value, lifted)
				}
			}
		})
	}
}

// TestCoreValueIter tests the CoreValueIter interface.
func TestCoreValueIter(t *testing.T) {
	values := []CoreValue{
		NewCoreValueI32(42),
		NewCoreValueI64(1234567890),
		NewCoreValueF32(3.14),
		NewCoreValueF64(2.718),
	}

	vi := NewCoreValueIter(values)

	// Test Next
	cv, err := vi.Next()
	if err != nil {
		t.Fatalf("Next failed: %v", err)
	}
	if cv.Kind != "i32" || cv.AsI32() != 42 {
		t.Errorf("expected i32(42), got %s(%v)", cv.Kind, cv.Bits)
	}

	// Skip some values
	_, _ = vi.Next()
	_, _ = vi.Next()

	// Test Done
	if vi.Done() {
		t.Error("expected !Done(), got Done()")
	}

	_, _ = vi.Next()
	if !vi.Done() {
		t.Error("expected Done(), got !Done()")
	}

	// Test out of range
	_, err = vi.Next()
	if err == nil {
		t.Error("expected error for out of range, got nil")
	}
}

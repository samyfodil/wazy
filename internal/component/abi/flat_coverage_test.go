package abi

import (
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// TestLowerFlatVariants tests lowering of variant types.
func TestLowerFlatVariants(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := ReallocFunc(func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil })
	mem := make([]byte, 65536)

	tests := []struct {
		name         string
		value        Value
		typeDesc     binary.TypeDesc
		expectedLen  int
		expectedDisc uint32
	}{
		{
			name:  "variant none",
			value: VariantValue{Disc: 0, Payload: nil},
			typeDesc: binary.VariantDesc{
				Cases: []binary.VariantCase{
					{Name: "none"},
					{Name: "some", Type: &binary.TypeRef{Primitive: "u32"}},
				},
			},
			expectedLen:  2,
			expectedDisc: 0,
		},
		{
			name:  "variant some",
			value: VariantValue{Disc: 1, Payload: uint32(42)},
			typeDesc: binary.VariantDesc{
				Cases: []binary.VariantCase{
					{Name: "none"},
					{Name: "some", Type: &binary.TypeRef{Primitive: "u32"}},
				},
			},
			expectedLen:  2,
			expectedDisc: 1,
		},
		{
			name:  "variant with no payload",
			value: VariantValue{Disc: 0, Payload: nil},
			typeDesc: binary.VariantDesc{
				Cases: []binary.VariantCase{
					{Name: "a"},
					{Name: "b"},
					{Name: "c"},
				},
			},
			expectedLen:  1,
			expectedDisc: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LowerFlat(tt.value, tt.typeDesc, resolve, realloc, mem)
			if err != nil {
				t.Fatalf("LowerFlat failed: %v", err)
			}
			if len(result) != tt.expectedLen {
				t.Errorf("expected %d core values, got %d", tt.expectedLen, len(result))
			}
			if len(result) > 0 && result[0].AsI32() != tt.expectedDisc {
				t.Errorf("expected discriminant %d, got %d", tt.expectedDisc, result[0].AsI32())
			}
		})
	}
}

// TestLowerFlatRecords tests lowering of record types.
func TestLowerFlatRecords(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := ReallocFunc(func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil })
	mem := make([]byte, 65536)

	tests := []struct {
		name        string
		value       Value
		typeDesc    binary.TypeDesc
		expectedLen int
	}{
		{
			name:  "record single field",
			value: []Value{uint32(42)},
			typeDesc: binary.RecordDesc{
				Fields: []binary.RecordField{
					{Name: "a", Type: binary.TypeRef{Primitive: "u32"}},
				},
			},
			expectedLen: 1,
		},
		{
			name:  "record multiple fields",
			value: []Value{uint32(42), int32(-100)},
			typeDesc: binary.RecordDesc{
				Fields: []binary.RecordField{
					{Name: "a", Type: binary.TypeRef{Primitive: "u32"}},
					{Name: "b", Type: binary.TypeRef{Primitive: "s32"}},
				},
			},
			expectedLen: 2,
		},
		{
			name:  "record empty",
			value: []Value{},
			typeDesc: binary.RecordDesc{
				Fields: []binary.RecordField{},
			},
			expectedLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LowerFlat(tt.value, tt.typeDesc, resolve, realloc, mem)
			if err != nil {
				t.Fatalf("LowerFlat failed: %v", err)
			}
			if len(result) != tt.expectedLen {
				t.Errorf("expected %d core values, got %d", tt.expectedLen, len(result))
			}
		})
	}
}

// TestLowerFlatTuples tests lowering of tuple types.
func TestLowerFlatTuples(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := ReallocFunc(func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil })
	mem := make([]byte, 65536)

	tests := []struct {
		name        string
		value       Value
		typeDesc    binary.TypeDesc
		expectedLen int
	}{
		{
			name:  "tuple single element",
			value: []Value{uint32(42)},
			typeDesc: binary.TupleDesc{
				Elements: []binary.TypeRef{
					{Primitive: "u32"},
				},
			},
			expectedLen: 1,
		},
		{
			name:  "tuple multiple elements",
			value: []Value{uint32(42), int32(-100)},
			typeDesc: binary.TupleDesc{
				Elements: []binary.TypeRef{
					{Primitive: "u32"},
					{Primitive: "s32"},
				},
			},
			expectedLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LowerFlat(tt.value, tt.typeDesc, resolve, realloc, mem)
			if err != nil {
				t.Fatalf("LowerFlat failed: %v", err)
			}
			if len(result) != tt.expectedLen {
				t.Errorf("expected %d core values, got %d", tt.expectedLen, len(result))
			}
		})
	}
}

// TestLowerFlatOptions tests lowering of option types.
func TestLowerFlatOptions(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := ReallocFunc(func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil })
	mem := make([]byte, 65536)

	tests := []struct {
		name         string
		value        Value
		typeDesc     binary.TypeDesc
		expectedDisc uint32
		expectedLen  int
	}{
		{
			name:         "option none",
			value:        nil,
			typeDesc:     binary.OptionDesc{Element: binary.TypeRef{Primitive: "u32"}},
			expectedDisc: 0,
			// None is zero-padded out to the element's flat width (1 for
			// u32), so the total is disc(1) + padding(1) = 2. See
			// lowerFlatOption: the canonical ABI always reserves the
			// element's full joined width regardless of which case fired.
			expectedLen: 2,
		},
		{
			name:         "option some",
			value:        uint32(42),
			typeDesc:     binary.OptionDesc{Element: binary.TypeRef{Primitive: "u32"}},
			expectedDisc: 1,
			expectedLen:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LowerFlat(tt.value, tt.typeDesc, resolve, realloc, mem)
			if err != nil {
				t.Fatalf("LowerFlat failed: %v", err)
			}
			if len(result) != tt.expectedLen {
				t.Errorf("expected %d core values, got %d", tt.expectedLen, len(result))
			}
			if len(result) > 0 && result[0].AsI32() != tt.expectedDisc {
				t.Errorf("expected discriminant %d, got %d", tt.expectedDisc, result[0].AsI32())
			}
		})
	}
}

// TestLowerFlatResults tests lowering of result types.
func TestLowerFlatResults(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := ReallocFunc(func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil })
	mem := make([]byte, 65536)

	tests := []struct {
		name         string
		value        Value
		typeDesc     binary.TypeDesc
		expectedDisc uint32
		expectedLen  int
	}{
		{
			name:         "result ok",
			value:        ResultValue{IsErr: false, Payload: uint32(42)},
			typeDesc:     binary.ResultDesc{Ok: &binary.TypeRef{Primitive: "u32"}},
			expectedDisc: 0,
			expectedLen:  2,
		},
		{
			name:         "result err",
			value:        ResultValue{IsErr: true, Payload: int32(-42)},
			typeDesc:     binary.ResultDesc{Err: &binary.TypeRef{Primitive: "s32"}},
			expectedDisc: 1,
			expectedLen:  2,
		},
		{
			name:         "result ok empty",
			value:        ResultValue{IsErr: false, Payload: nil},
			typeDesc:     binary.ResultDesc{},
			expectedDisc: 0,
			expectedLen:  1,
		},
		{
			name:         "result err empty",
			value:        ResultValue{IsErr: true, Payload: nil},
			typeDesc:     binary.ResultDesc{},
			expectedDisc: 1,
			expectedLen:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LowerFlat(tt.value, tt.typeDesc, resolve, realloc, mem)
			if err != nil {
				t.Fatalf("LowerFlat failed: %v", err)
			}
			if len(result) != tt.expectedLen {
				t.Errorf("expected %d core values, got %d", tt.expectedLen, len(result))
			}
			if len(result) > 0 && result[0].AsI32() != tt.expectedDisc {
				t.Errorf("expected discriminant %d, got %d", tt.expectedDisc, result[0].AsI32())
			}
		})
	}
}

// TestLiftFlatVariants tests lifting of variant types.
func TestLiftFlatVariants(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	mem := make([]byte, 65536)

	tests := []struct {
		name     string
		values   []CoreValue
		typeDesc binary.TypeDesc
	}{
		{
			name:   "variant none",
			values: []CoreValue{NewCoreValueI32(0), NewCoreValueI32(0)},
			typeDesc: binary.VariantDesc{
				Cases: []binary.VariantCase{
					{Name: "none"},
					{Name: "some", Type: &binary.TypeRef{Primitive: "u32"}},
				},
			},
		},
		{
			name:   "variant some",
			values: []CoreValue{NewCoreValueI32(1), NewCoreValueI32(42)},
			typeDesc: binary.VariantDesc{
				Cases: []binary.VariantCase{
					{Name: "none"},
					{Name: "some", Type: &binary.TypeRef{Primitive: "u32"}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LiftFlat(tt.values, tt.typeDesc, resolve, mem)
			if err != nil {
				t.Fatalf("LiftFlat failed: %v", err)
			}
			if result == nil {
				t.Error("expected non-nil result")
			}
		})
	}
}

// TestLiftFlatRecords tests lifting of record types.
func TestLiftFlatRecords(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	mem := make([]byte, 65536)

	tests := []struct {
		name     string
		values   []CoreValue
		typeDesc binary.TypeDesc
	}{
		{
			name:   "record single field",
			values: []CoreValue{NewCoreValueI32(42)},
			typeDesc: binary.RecordDesc{
				Fields: []binary.RecordField{
					{Name: "a", Type: binary.TypeRef{Primitive: "u32"}},
				},
			},
		},
		{
			name:   "record multiple fields",
			values: []CoreValue{NewCoreValueI32(42), NewCoreValueI32(0xFFFFFF9C)},
			typeDesc: binary.RecordDesc{
				Fields: []binary.RecordField{
					{Name: "a", Type: binary.TypeRef{Primitive: "u32"}},
					{Name: "b", Type: binary.TypeRef{Primitive: "s32"}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LiftFlat(tt.values, tt.typeDesc, resolve, mem)
			if err != nil {
				t.Fatalf("LiftFlat failed: %v", err)
			}
			if result == nil {
				t.Error("expected non-nil result")
			}
		})
	}
}

// TestLiftFlatTuples tests lifting of tuple types.
func TestLiftFlatTuples(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	mem := make([]byte, 65536)

	tests := []struct {
		name     string
		values   []CoreValue
		typeDesc binary.TypeDesc
	}{
		{
			name:   "tuple single element",
			values: []CoreValue{NewCoreValueI32(42)},
			typeDesc: binary.TupleDesc{
				Elements: []binary.TypeRef{
					{Primitive: "u32"},
				},
			},
		},
		{
			name:   "tuple multiple elements",
			values: []CoreValue{NewCoreValueI32(42), NewCoreValueI32(0xFFFFFF9C)},
			typeDesc: binary.TupleDesc{
				Elements: []binary.TypeRef{
					{Primitive: "u32"},
					{Primitive: "s32"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LiftFlat(tt.values, tt.typeDesc, resolve, mem)
			if err != nil {
				t.Fatalf("LiftFlat failed: %v", err)
			}
			if result == nil {
				t.Error("expected non-nil result")
			}
		})
	}
}

// TestLiftFlatOptions tests lifting of option types.
func TestLiftFlatOptions(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	mem := make([]byte, 65536)

	tests := []struct {
		name     string
		values   []CoreValue
		typeDesc binary.TypeDesc
	}{
		{
			// The zero-padding for u32's 1-wide flat type must be present
			// (LiftFlat is handed exactly len(Flatten(t)) values by its
			// caller in the real ABI), matching lowerFlatOption's output.
			name:   "option none",
			values: []CoreValue{NewCoreValueI32(0), NewCoreValueI32(0)},
			typeDesc: binary.OptionDesc{
				Element: binary.TypeRef{Primitive: "u32"},
			},
		},
		{
			name:   "option some",
			values: []CoreValue{NewCoreValueI32(1), NewCoreValueI32(42)},
			typeDesc: binary.OptionDesc{
				Element: binary.TypeRef{Primitive: "u32"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LiftFlat(tt.values, tt.typeDesc, resolve, mem)
			if err != nil {
				t.Fatalf("LiftFlat failed: %v", err)
			}
		})
	}
}

// TestLiftFlatResults tests lifting of result types.
func TestLiftFlatResults(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	mem := make([]byte, 65536)

	tests := []struct {
		name     string
		values   []CoreValue
		typeDesc binary.TypeDesc
	}{
		{
			name:   "result ok",
			values: []CoreValue{NewCoreValueI32(0), NewCoreValueI32(42)},
			typeDesc: binary.ResultDesc{
				Ok: &binary.TypeRef{Primitive: "u32"},
			},
		},
		{
			name:   "result err",
			values: []CoreValue{NewCoreValueI32(1), NewCoreValueI32(0xFFFFFF9C)},
			typeDesc: binary.ResultDesc{
				Err: &binary.TypeRef{Primitive: "s32"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LiftFlat(tt.values, tt.typeDesc, resolve, mem)
			if err != nil {
				t.Fatalf("LiftFlat failed: %v", err)
			}
		})
	}
}

// TestLiftFlatFlags tests lifting of flags types.
func TestLiftFlatFlags(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	mem := make([]byte, 65536)

	tests := []struct {
		name     string
		values   []CoreValue
		typeDesc binary.TypeDesc
	}{
		{
			name:   "flags zero",
			values: []CoreValue{NewCoreValueI32(0)},
			typeDesc: binary.FlagsDesc{
				Names: []string{"a", "b", "c"},
			},
		},
		{
			name:   "flags all",
			values: []CoreValue{NewCoreValueI32(7)},
			typeDesc: binary.FlagsDesc{
				Names: []string{"a", "b", "c"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LiftFlat(tt.values, tt.typeDesc, resolve, mem)
			if err != nil {
				t.Fatalf("LiftFlat failed: %v", err)
			}
		})
	}
}

// TestLiftFlatEnums tests lifting of enum types.
func TestLiftFlatEnums(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	mem := make([]byte, 65536)

	tests := []struct {
		name     string
		values   []CoreValue
		typeDesc binary.TypeDesc
	}{
		{
			name:   "enum red",
			values: []CoreValue{NewCoreValueI32(0)},
			typeDesc: binary.EnumDesc{
				Cases: []string{"red", "green", "blue"},
			},
		},
		{
			name:   "enum green",
			values: []CoreValue{NewCoreValueI32(1)},
			typeDesc: binary.EnumDesc{
				Cases: []string{"red", "green", "blue"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LiftFlat(tt.values, tt.typeDesc, resolve, mem)
			if err != nil {
				t.Fatalf("LiftFlat failed: %v", err)
			}
		})
	}
}

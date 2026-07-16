package abi

import (
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// TestLiftFlatCharInvalid tests invalid char values.
func TestLiftFlatCharInvalid(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	mem := make([]byte, 65536)

	tests := []struct {
		name        string
		values      []CoreValue
		typeDesc    binary.TypeDesc
		expectError bool
	}{
		{
			name:        "char out of range",
			values:      []CoreValue{NewCoreValueI32(0x110000)},
			typeDesc:    binary.PrimitiveDesc{Prim: "char"},
			expectError: true,
		},
		{
			name:        "char surrogate",
			values:      []CoreValue{NewCoreValueI32(0xD800)},
			typeDesc:    binary.PrimitiveDesc{Prim: "char"},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LiftFlat(tt.values, tt.typeDesc, resolve, mem)
			if (err != nil) != tt.expectError {
				t.Errorf("expected error=%v, got error=%v", tt.expectError, err != nil)
			}
		})
	}
}

// TestLowerFlatUnsupportedType tests unsupported types.
func TestLowerFlatUnsupportedType(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil }
	mem := make([]byte, 1024)

	tests := []struct {
		name        string
		value       Value
		typeDesc    binary.TypeDesc
		expectError bool
	}{
		{
			name:        "func type",
			value:       nil,
			typeDesc:    binary.FuncDesc{},
			expectError: true,
		},
		{
			name:        "instance type",
			value:       nil,
			typeDesc:    binary.InstanceDesc{},
			expectError: true,
		},
		{
			name:        "component type",
			value:       nil,
			typeDesc:    binary.ComponentDesc{},
			expectError: true,
		},
		{
			name:        "resource type",
			value:       nil,
			typeDesc:    binary.ResourceDesc{},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LowerFlat(tt.value, tt.typeDesc, resolve, realloc, mem)
			if (err != nil) != tt.expectError {
				t.Errorf("expected error=%v, got error=%v", tt.expectError, err != nil)
			}
		})
	}
}

// TestLiftFlatUnsupportedType tests unsupported types.
func TestLiftFlatUnsupportedType(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	mem := make([]byte, 1024)

	tests := []struct {
		name        string
		values      []CoreValue
		typeDesc    binary.TypeDesc
		expectError bool
	}{
		{
			name:        "func type",
			values:      []CoreValue{},
			typeDesc:    binary.FuncDesc{},
			expectError: true,
		},
		{
			name:        "instance type",
			values:      []CoreValue{},
			typeDesc:    binary.InstanceDesc{},
			expectError: true,
		},
		{
			name:        "component type",
			values:      []CoreValue{},
			typeDesc:    binary.ComponentDesc{},
			expectError: true,
		},
		{
			name:        "resource type",
			values:      []CoreValue{},
			typeDesc:    binary.ResourceDesc{},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LiftFlat(tt.values, tt.typeDesc, resolve, mem)
			if (err != nil) != tt.expectError {
				t.Errorf("expected error=%v, got error=%v", tt.expectError, err != nil)
			}
		})
	}
}

// TestLowerFlatVariantWithPayload tests variant lowering with various payload types.
func TestLowerFlatVariantWithPayload(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil }
	mem := make([]byte, 65536)

	tests := []struct {
		name     string
		value    Value
		typeDesc binary.TypeDesc
	}{
		{
			name: "variant with u64 payload",
			value: VariantValue{Disc: 0, Payload: uint64(123456789)},
			typeDesc: binary.VariantDesc{
				Cases: []binary.VariantCase{
					{Name: "big", Type: &binary.TypeRef{Primitive: "u64"}},
					{Name: "small", Type: &binary.TypeRef{Primitive: "u32"}},
				},
			},
		},
		{
			name: "variant with f64 payload",
			value: VariantValue{Disc: 1, Payload: float64(3.14159)},
			typeDesc: binary.VariantDesc{
				Cases: []binary.VariantCase{
					{Name: "int", Type: &binary.TypeRef{Primitive: "s32"}},
					{Name: "float", Type: &binary.TypeRef{Primitive: "f64"}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LowerFlat(tt.value, tt.typeDesc, resolve, realloc, mem)
			if err != nil {
				t.Fatalf("LowerFlat failed: %v", err)
			}
		})
	}
}

// TestLiftFlatComplexTypes tests lifting of complex nested types.
func TestLiftFlatComplexTypes(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	mem := make([]byte, 65536)

	// Record containing various types
	recordType := binary.RecordDesc{
		Fields: []binary.RecordField{
			{Name: "a", Type: binary.TypeRef{Primitive: "u32"}},
			{Name: "b", Type: binary.TypeRef{Primitive: "f32"}},
			{Name: "c", Type: binary.TypeRef{Primitive: "s64"}},
		},
	}

	values := []CoreValue{
		NewCoreValueI32(42),
		NewCoreValueF32(3.14),
		NewCoreValueI64(18446744073709550616),
	}

	_, err := LiftFlat(values, recordType, resolve, mem)
	if err != nil {
		t.Fatalf("LiftFlat failed: %v", err)
	}
}

// TestCoercingValueIterConsumption tests that coercingValueIter correctly tracks consumption.
func TestCoercingValueIterConsumption(t *testing.T) {
	values := []CoreValue{
		NewCoreValueI32(1),
		NewCoreValueI32(2),
		NewCoreValueI32(3),
	}
	vi := NewCoreValueIter(values)

	// Create a coercing iterator that wraps the core values. joinedFlat and
	// caseFlat intentionally match here (i32/i32, i.e. no coercion) since
	// this test only exercises idx/Done() bookkeeping, not the coercion
	// arithmetic itself (see TestDeCoerceValue* for that); the underlying
	// values really are i32, so claiming a mismatched "i64" wire type here
	// would make deCoerceValue try to read them as i64 and panic.
	cvi := &coercingValueIter{
		underlying: vi,
		joinedFlat: []string{"i32", "i32", "i32"},
		caseFlat:   []string{"i32", "i32", "i32"},
		idx:        0,
	}

	// Consume all values
	for i := 0; i < 3; i++ {
		_, err := cvi.Next()
		if err != nil {
			t.Fatalf("Next() failed: %v", err)
		}
	}

	// Should be done
	if !cvi.Done() {
		t.Error("expected Done() after consuming all values")
	}

	// Further calls should error
	_, err := cvi.Next()
	if err == nil {
		t.Error("expected error after consuming all values")
	}
}

// TestLowerFlatVariantMultipleCases tests variant with multiple cases.
func TestLowerFlatVariantMultipleCases(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil }
	mem := make([]byte, 65536)

	variantType := binary.VariantDesc{
		Cases: []binary.VariantCase{
			{Name: "a", Type: &binary.TypeRef{Primitive: "u8"}},
			{Name: "b", Type: &binary.TypeRef{Primitive: "u16"}},
			{Name: "c", Type: &binary.TypeRef{Primitive: "u32"}},
			{Name: "d", Type: &binary.TypeRef{Primitive: "u64"}},
		},
	}

	tests := []struct {
		name string
		disc uint32
		val  Value
	}{
		{"case_a", 0, uint32(255)},
		{"case_b", 1, uint32(65535)},
		{"case_c", 2, uint32(0xFFFFFFFF)},
		{"case_d", 3, uint64(123456789)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LowerFlat(VariantValue{Disc: tt.disc, Payload: tt.val}, variantType, resolve, realloc, mem)
			if err != nil {
				t.Fatalf("LowerFlat failed: %v", err)
			}
		})
	}
}

// TestLiftFlatVariantMultipleCases tests lifting variant with multiple cases.
func TestLiftFlatVariantMultipleCases(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	mem := make([]byte, 65536)

	variantType := binary.VariantDesc{
		Cases: []binary.VariantCase{
			{Name: "a", Type: &binary.TypeRef{Primitive: "u32"}},
			{Name: "b", Type: &binary.TypeRef{Primitive: "u32"}},
			{Name: "c", Type: &binary.TypeRef{Primitive: "u32"}},
		},
	}

	tests := []struct {
		name  string
		disc  uint32
		value uint32
	}{
		{"case_0", 0, 100},
		{"case_1", 1, 200},
		{"case_2", 2, 300},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := []CoreValue{
				NewCoreValueI32(tt.disc),
				NewCoreValueI32(tt.value),
			}
			_, err := LiftFlat(values, variantType, resolve, mem)
			if err != nil {
				t.Fatalf("LiftFlat failed: %v", err)
			}
		})
	}
}

// TestLowerFlatTupleMultipleTypes tests tuple with multiple different types.
func TestLowerFlatTupleMultipleTypes(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	realloc := func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil }
	mem := make([]byte, 65536)

	tupleType := binary.TupleDesc{
		Elements: []binary.TypeRef{
			{Primitive: "u32"},
			{Primitive: "f32"},
			{Primitive: "u64"},
			{Primitive: "bool"},
		},
	}

	value := []Value{
		uint32(42),
		float32(3.14),
		uint64(123456789),
		true,
	}

	_, err := LowerFlat(value, tupleType, resolve, realloc, mem)
	if err != nil {
		t.Fatalf("LowerFlat failed: %v", err)
	}
}

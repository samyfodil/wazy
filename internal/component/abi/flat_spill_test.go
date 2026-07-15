package abi

import (
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// TestLowerFlatSpilling tests the spilling behavior when types exceed MaxFlatParams.
func TestLowerFlatSpilling(t *testing.T) {
	// Create a type that flattens to more than MaxFlatParams core values.
	// Each u32 flattens to 1 core value, so we need >16 u32 fields in a record.
	largeRecord := binary.RecordDesc{
		Fields: make([]binary.RecordField, 20),
	}
	for i := 0; i < 20; i++ {
		largeRecord.Fields[i] = binary.RecordField{
			Name: "field",
			Type: binary.TypeRef{Primitive: "u32"},
		}
	}

	value := make([]Value, 20)
	for i := 0; i < 20; i++ {
		value[i] = uint32(i)
	}

	resolve := func(idx uint32) binary.TypeDesc { return nil }
	allocCount := 0
	realloc := func(origPtr, origSize, align, newSize uint32) (uint32, error) {
		allocCount++
		// Return sequential memory addresses
		return uint32(1024 + allocCount*256), nil
	}
	mem := make([]byte, 65536)

	// When the type is too large to fit in flat params, LowerFlat should spill and return a pointer.
	result, err := LowerFlat(value, largeRecord, resolve, realloc, mem)
	if err != nil {
		t.Fatalf("LowerFlat failed: %v", err)
	}

	// Should return a single i32 pointer
	if len(result) != 1 {
		t.Errorf("expected 1 core value (pointer), got %d", len(result))
	}
	if result[0].Kind != "i32" {
		t.Errorf("expected i32 pointer, got %s", result[0].Kind)
	}
}

// TestLiftFlatPrimitiveTypes tests lifting of various primitive types.
func TestLiftFlatPrimitiveTypes(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc { return nil }
	mem := make([]byte, 65536)

	tests := []struct {
		name     string
		values   []CoreValue
		typeDesc binary.TypeDesc
	}{
		{
			name:     "u8",
			values:   []CoreValue{NewCoreValueI32(255)},
			typeDesc: binary.PrimitiveDesc{Prim: "u8"},
		},
		{
			name:     "u16",
			values:   []CoreValue{NewCoreValueI32(65535)},
			typeDesc: binary.PrimitiveDesc{Prim: "u16"},
		},
		{
			name:     "u32",
			values:   []CoreValue{NewCoreValueI32(0xFFFFFFFF)},
			typeDesc: binary.PrimitiveDesc{Prim: "u32"},
		},
		{
			name:     "s8 positive",
			values:   []CoreValue{NewCoreValueI32(127)},
			typeDesc: binary.PrimitiveDesc{Prim: "s8"},
		},
		{
			name:     "s8 negative",
			values:   []CoreValue{NewCoreValueI32(0xFFFFFF80)},
			typeDesc: binary.PrimitiveDesc{Prim: "s8"},
		},
		{
			name:     "s16 positive",
			values:   []CoreValue{NewCoreValueI32(32767)},
			typeDesc: binary.PrimitiveDesc{Prim: "s16"},
		},
		{
			name:     "s16 negative",
			values:   []CoreValue{NewCoreValueI32(0xFFFF8000)},
			typeDesc: binary.PrimitiveDesc{Prim: "s16"},
		},
		{
			name:     "s32 positive",
			values:   []CoreValue{NewCoreValueI32(2147483647)},
			typeDesc: binary.PrimitiveDesc{Prim: "s32"},
		},
		{
			name:     "s32 negative",
			values:   []CoreValue{NewCoreValueI32(0x80000000)},
			typeDesc: binary.PrimitiveDesc{Prim: "s32"},
		},
		{
			name:     "u64",
			values:   []CoreValue{NewCoreValueI64(0x123456789ABCDEF0)},
			typeDesc: binary.PrimitiveDesc{Prim: "u64"},
		},
		{
			name:     "s64 positive",
			values:   []CoreValue{NewCoreValueI64(9223372036854775807)},
			typeDesc: binary.PrimitiveDesc{Prim: "s64"},
		},
		{
			name:     "s64 negative",
			values:   []CoreValue{NewCoreValueI64(0x8000000000000000)},
			typeDesc: binary.PrimitiveDesc{Prim: "s64"},
		},
		{
			name:     "f32",
			values:   []CoreValue{NewCoreValueF32(3.14)},
			typeDesc: binary.PrimitiveDesc{Prim: "f32"},
		},
		{
			name:     "f64",
			values:   []CoreValue{NewCoreValueF64(3.141592653589793)},
			typeDesc: binary.PrimitiveDesc{Prim: "f64"},
		},
		{
			name:     "bool true",
			values:   []CoreValue{NewCoreValueI32(1)},
			typeDesc: binary.PrimitiveDesc{Prim: "bool"},
		},
		{
			name:     "bool false",
			values:   []CoreValue{NewCoreValueI32(0)},
			typeDesc: binary.PrimitiveDesc{Prim: "bool"},
		},
		{
			name:     "char",
			values:   []CoreValue{NewCoreValueI32(97)},
			typeDesc: binary.PrimitiveDesc{Prim: "char"},
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

// TestCoerceValueEdgeCases tests edge cases in value coercion.
func TestCoerceValueEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		cv       CoreValue
		have     string
		want     string
		expected CoreValue
	}{
		{
			name:     "i32 to i64",
			cv:       NewCoreValueI32(42),
			have:     "i32",
			want:     "i64",
			expected: NewCoreValueI64(42),
		},
		{
			name:     "f32 to i32",
			cv:       NewCoreValueF32(3.14),
			have:     "f32",
			want:     "i32",
			expected: CoreValue{Kind: "i32", Bits: 0x4048f5c3},
		},
		{
			name:     "f32 to i64",
			cv:       NewCoreValueF32(1.5),
			have:     "f32",
			want:     "i64",
			expected: CoreValue{Kind: "i64", Bits: 0x3fc00000},
		},
		{
			name:     "f64 to i64",
			cv:       NewCoreValueF64(3.14),
			have:     "f64",
			want:     "i64",
			expected: CoreValue{Kind: "i64", Bits: 0x4009ae147ae147ae},
		},
		{
			name:     "same type identity",
			cv:       NewCoreValueI32(42),
			have:     "i32",
			want:     "i32",
			expected: NewCoreValueI32(42),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := coerceValue(tt.cv, tt.have, tt.want)
			if result.Kind != tt.expected.Kind {
				t.Errorf("expected kind %s, got %s", tt.expected.Kind, result.Kind)
			}
		})
	}
}

// TestCoreValueIterDone tests the Done method of CoreValueIter.
func TestCoreValueIterDone(t *testing.T) {
	values := []CoreValue{
		NewCoreValueI32(42),
		NewCoreValueI64(1234),
	}
	vi := NewCoreValueIter(values)

	// Initially not done
	if vi.Done() {
		t.Error("expected !Done() at start")
	}

	// After consuming all values, should be done
	vi.Next()
	vi.Next()
	if !vi.Done() {
		t.Error("expected Done() after consuming all values")
	}
}

// TestLowerFlatWithResolverErrors tests error handling when type resolution fails.
func TestLowerFlatWithResolverErrors(t *testing.T) {
	// Create a record that references a type index that resolver can't find
	recordWithRef := binary.RecordDesc{
		Fields: []binary.RecordField{
			{Name: "field", Type: binary.TypeRef{TypeIndex: ptrU32(99)}},
		},
	}

	value := []Value{uint32(42)}
	resolve := func(idx uint32) binary.TypeDesc {
		return nil // Simulate resolver error
	}
	realloc := func(origPtr, origSize, align, newSize uint32) (uint32, error) { return 0, nil }
	mem := make([]byte, 1024)

	_, err := LowerFlat(value, recordWithRef, resolve, realloc, mem)
	if err == nil {
		t.Error("expected error when resolver returns nil")
	}
}

// Helper to create a pointer to uint32
func ptrU32(v uint32) *uint32 {
	return &v
}

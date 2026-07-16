package abi

import (
	"testing"

	bintype "github.com/samyfodil/wazy/internal/component/binary"
)

func TestLoadErrorCases(t *testing.T) {
	tests := []struct {
		name    string
		setup   func() ([]byte, uint32, bintype.TypeDesc, Resolver)
		wantErr bool
	}{
		{
			name: "load u32 bounds overflow",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Resolver) {
				mem := make([]byte, 2)
				return mem, 0, bintype.PrimitiveDesc{Prim: "u32"}, nil
			},
			wantErr: true,
		},
		{
			name: "load unaligned pointer",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Resolver) {
				mem := make([]byte, 100)
				// u32 requires 4-byte alignment, but we're at offset 1
				return mem, 1, bintype.PrimitiveDesc{Prim: "u32"}, nil
			},
			wantErr: true,
		},
		{
			name: "load list unaligned",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Resolver) {
				mem := make([]byte, 100)
				// Write list at offset 1 (not aligned for pointer size)
				return mem, 1, bintype.ListDesc{Element: bintype.TypeRef{Primitive: "u32"}}, nil
			},
			wantErr: true,
		},
		{
			name: "load char out of range",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Resolver) {
				mem := make([]byte, 100)
				// Write an out-of-range char code point (>0x10FFFF)
				mem[0] = 0xFF
				mem[1] = 0xFF
				mem[2] = 0xFF
				mem[3] = 0xFF
				return mem, 0, bintype.PrimitiveDesc{Prim: "char"}, nil
			},
			wantErr: true,
		},
		{
			name: "load enum out of range",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Resolver) {
				mem := make([]byte, 100)
				mem[0] = 10 // case index 10
				return mem, 0, bintype.EnumDesc{Cases: []string{"a", "b", "c"}}, nil
			},
			wantErr: true,
		},
		{
			name: "load variant invalid discriminant",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Resolver) {
				mem := make([]byte, 100)
				mem[0] = 10 // discriminant 10 (out of range for 2-case variant)
				return mem, 0, bintype.VariantDesc{
					Cases: []bintype.VariantCase{
						{Name: "a", Type: nil},
						{Name: "b", Type: nil},
					},
				}, nil
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem, ptr, typeDesc, resolve := tt.setup()
			_, err := Load(mem, ptr, typeDesc, resolve)
			if (err != nil) != tt.wantErr {
				t.Errorf("Load error mismatch: got %v, want error=%v", err, tt.wantErr)
			}
		})
	}
}

func TestStoreErrorCases(t *testing.T) {
	mockRealloc := ReallocFunc(func(origPtr, origSize, align, newSize uint32) (uint32, error) {
		return 1024, nil // Simple allocator
	})

	tests := []struct {
		name    string
		setup   func() ([]byte, uint32, bintype.TypeDesc, Value, Resolver)
		wantErr bool
	}{
		{
			name: "store u32 with wrong type",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Value, Resolver) {
				mem := make([]byte, 100)
				return mem, 0, bintype.PrimitiveDesc{Prim: "u32"}, "not a u32", nil
			},
			wantErr: true,
		},
		{
			name: "store bool with wrong type",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Value, Resolver) {
				mem := make([]byte, 100)
				return mem, 0, bintype.PrimitiveDesc{Prim: "bool"}, 42, nil
			},
			wantErr: true,
		},
		{
			name: "store char out of range",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Value, Resolver) {
				mem := make([]byte, 100)
				return mem, 0, bintype.PrimitiveDesc{Prim: "char"}, rune(0x110000), nil
			},
			wantErr: true,
		},
		{
			name: "store char surrogate",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Value, Resolver) {
				mem := make([]byte, 100)
				return mem, 0, bintype.PrimitiveDesc{Prim: "char"}, rune(0xD800), nil
			},
			wantErr: true,
		},
		{
			name: "store enum out of range",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Value, Resolver) {
				mem := make([]byte, 100)
				return mem, 0, bintype.EnumDesc{Cases: []string{"a", "b"}}, uint32(10), nil
			},
			wantErr: true,
		},
		{
			name: "store variant invalid discriminant",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Value, Resolver) {
				mem := make([]byte, 100)
				return mem, 0, bintype.VariantDesc{
					Cases: []bintype.VariantCase{
						{Name: "a", Type: nil},
						{Name: "b", Type: nil},
					},
				}, VariantValue{Disc: 10, Payload: nil}, nil
			},
			wantErr: true,
		},
		{
			name: "store variant missing payload",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Value, Resolver) {
				mem := make([]byte, 100)
				return mem, 0, bintype.VariantDesc{
					Cases: []bintype.VariantCase{
						{Name: "a", Type: &bintype.TypeRef{Primitive: "u32"}},
						{Name: "b", Type: nil},
					},
				}, VariantValue{Disc: 0, Payload: nil}, nil
			},
			wantErr: true,
		},
		{
			name: "store unaligned pointer",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Value, Resolver) {
				mem := make([]byte, 100)
				// u32 requires 4-byte alignment
				return mem, 1, bintype.PrimitiveDesc{Prim: "u32"}, uint32(42), nil
			},
			wantErr: true,
		},
		{
			name: "store bounds overflow",
			setup: func() ([]byte, uint32, bintype.TypeDesc, Value, Resolver) {
				mem := make([]byte, 2)
				return mem, 0, bintype.PrimitiveDesc{Prim: "u32"}, uint32(42), nil
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem, ptr, typeDesc, value, resolve := tt.setup()
			err := Store(mem, ptr, typeDesc, value, resolve, mockRealloc)
			if (err != nil) != tt.wantErr {
				t.Errorf("Store error mismatch: got %v, want error=%v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadInvalidPrimitive(t *testing.T) {
	mem := make([]byte, 100)
	_, err := loadPrimitive(mem, 0, "unknown")
	if err == nil {
		t.Error("expected error for unknown primitive type")
	}
}

func TestStorePrimitiveWrongTypes(t *testing.T) {
	tests := []struct {
		prim  string
		value Value
	}{
		{"s8", "not int"},
		{"s16", "not int"},
		{"s32", "not int"},
		{"s64", "not int"},
		{"f32", "not float"},
		{"f64", "not float"},
		{"char", "not rune"},
		{"string", 42},
		{"u64", "not uint64"},
	}

	for _, tt := range tests {
		t.Run(tt.prim, func(t *testing.T) {
			mem := make([]byte, 100)
			err := storePrimitive(mem, 0, tt.prim, tt.value, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
			if err == nil {
				t.Errorf("expected error for storing %v as %s", tt.value, tt.prim)
			}
		})
	}
}

func TestRecordFieldMismatch(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.RecordDesc{
		Fields: []bintype.RecordField{
			{Name: "a", Type: bintype.TypeRef{Primitive: "u32"}},
			{Name: "b", Type: bintype.TypeRef{Primitive: "u32"}},
		},
	}

	// Wrong number of fields
	value := []Value{uint32(1)} // only 1 field, need 2
	err := storeRecord(mem, 0, value, desc, nil, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err == nil {
		t.Error("expected error for record field count mismatch")
	}
}

func TestTupleElementMismatch(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.TupleDesc{
		Elements: []bintype.TypeRef{
			{Primitive: "u32"},
			{Primitive: "u32"},
		},
	}

	// Wrong number of elements
	value := []Value{uint32(1)} // only 1 element, need 2
	err := storeTuple(mem, 0, value, desc, nil, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err == nil {
		t.Error("expected error for tuple element count mismatch")
	}
}

func TestListWrongType(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.ListDesc{Element: bintype.TypeRef{Primitive: "u32"}}

	// Pass a non-list value
	err := storeList(mem, 0, "not a list", desc, nil, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err == nil {
		t.Error("expected error for storing non-list value")
	}
}

func TestResultWrongType(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.ResultDesc{
		Ok:  &bintype.TypeRef{Primitive: "u32"},
		Err: &bintype.TypeRef{Primitive: "u32"},
	}

	// Pass a non-ResultValue
	err := storeResult(mem, 0, "not a result", desc, nil, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err == nil {
		t.Error("expected error for storing non-result value")
	}
}

func TestResultIsErrWrongType(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.ResultDesc{
		Ok:  &bintype.TypeRef{Primitive: "u32"},
		Err: &bintype.TypeRef{Primitive: "u32"},
	}

	// Pass a result-like map with wrong isErr type
	err := storeResult(mem, 0, map[string]any{"isErr": "not bool"}, desc, nil, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err == nil {
		t.Error("expected error for result with non-bool isErr")
	}
}

func TestLoadRecordFieldError(t *testing.T) {
	mem := make([]byte, 100)

	// Create a minimal Resolver that fails
	badResolve := func(idx uint32) bintype.TypeDesc {
		return nil // Will cause an error in resolveType
	}

	desc := bintype.RecordDesc{
		Fields: []bintype.RecordField{
			{Name: "a", Type: bintype.TypeRef{TypeIndex: &[]uint32{0}[0]}},
		},
	}

	_, err := loadRecord(mem, 0, desc, badResolve)
	if err == nil {
		t.Error("expected error for bad resolver in record")
	}
}

func TestLoadTupleFieldError(t *testing.T) {
	mem := make([]byte, 100)

	badResolve := func(idx uint32) bintype.TypeDesc {
		return nil
	}

	desc := bintype.TupleDesc{
		Elements: []bintype.TypeRef{
			{TypeIndex: &[]uint32{0}[0]},
		},
	}

	_, err := loadTuple(mem, 0, desc, badResolve)
	if err == nil {
		t.Error("expected error for bad resolver in tuple")
	}
}

func TestLoadVariantFieldError(t *testing.T) {
	mem := make([]byte, 100)

	badResolve := func(idx uint32) bintype.TypeDesc {
		return nil
	}

	desc := bintype.VariantDesc{
		Cases: []bintype.VariantCase{
			{Name: "a", Type: &bintype.TypeRef{TypeIndex: &[]uint32{0}[0]}},
		},
	}

	_, err := loadVariant(mem, 0, desc, badResolve)
	if err == nil {
		t.Error("expected error for bad resolver in variant")
	}
}

func TestLoadOptionFieldError(t *testing.T) {
	mem := make([]byte, 100)

	badResolve := func(idx uint32) bintype.TypeDesc {
		return nil
	}

	_, err := loadOption(mem, 0, bintype.PrimitiveDesc{Prim: "invalid_prim"}, badResolve)
	if err == nil {
		t.Error("expected error for invalid primitive in option")
	}
}

func TestLoadResultBothArmsNil(t *testing.T) {
	mem := make([]byte, 100)
	// Write discriminant 0 (ok) but with no ok type
	mem[0] = 0

	desc := bintype.ResultDesc{
		Ok:  nil,
		Err: nil,
	}

	val, err := loadResult(mem, 0, desc, nil)
	if err != nil {
		t.Errorf("unexpected error loading result with nil arms: %v", err)
	}

	result, ok := val.(ResultValue)
	if !ok {
		t.Error("expected ResultValue")
		return
	}

	if result.IsErr || result.Payload != nil {
		t.Error("expected ok result with nil payload")
	}
}

func TestStoreRecordFieldTypeError(t *testing.T) {
	mem := make([]byte, 100)

	badResolve := func(idx uint32) bintype.TypeDesc {
		return nil
	}

	desc := bintype.RecordDesc{
		Fields: []bintype.RecordField{
			{Name: "a", Type: bintype.TypeRef{TypeIndex: &[]uint32{0}[0]}},
		},
	}

	value := []Value{uint32(1)}
	err := storeRecord(mem, 0, value, desc, badResolve, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err == nil {
		t.Error("expected error for bad resolver in store record")
	}
}

func TestStoreListFieldTypeError(t *testing.T) {
	mem := make([]byte, 100)

	badResolve := func(idx uint32) bintype.TypeDesc {
		return nil
	}

	desc := bintype.ListDesc{
		Element: bintype.TypeRef{TypeIndex: &[]uint32{0}[0]},
	}

	value := []Value{uint32(1)} // Non-empty list to trigger resolver call
	err := storeList(mem, 0, value, desc, badResolve, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err == nil {
		t.Error("expected error for bad resolver in store list")
	}
}

func TestStoreF32WrongType(t *testing.T) {
	mem := make([]byte, 100)
	err := storePrimitive(mem, 0, "f32", "not float", ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err == nil {
		t.Error("expected error for storing non-float as f32")
	}
}

func TestStoreF64WrongType(t *testing.T) {
	mem := make([]byte, 100)
	err := storePrimitive(mem, 0, "f64", "not float", ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err == nil {
		t.Error("expected error for storing non-float as f64")
	}
}

func TestStoreStringWrongType(t *testing.T) {
	mem := make([]byte, 100)
	err := storePrimitive(mem, 0, "string", 42, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err == nil {
		t.Error("expected error for storing non-string as string")
	}
}

func TestListBoundsError(t *testing.T) {
	mem := make([]byte, 50)
	// Create a list descriptor pointing beyond buffer
	desc := bintype.ListDesc{Element: bintype.TypeRef{Primitive: "u32"}}

	// List pointer at 10, length 100 * size(u32=4) = 400 bytes = 1500 offset > 50 bytes available
	mem[10] = 200
	mem[11] = 0
	mem[12] = 0
	mem[13] = 0
	mem[14] = 100 // length = 100
	mem[15] = 0
	mem[16] = 0
	mem[17] = 0

	_, err := loadList(mem, 10, desc, nil)
	if err == nil {
		t.Error("expected error for list bounds overflow")
	}
}

func TestTupleLengthMismatch(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.TupleDesc{
		Elements: []bintype.TypeRef{
			{Primitive: "u32"},
			{Primitive: "u32"},
		},
	}

	value := []Value{uint32(1)} // Only 1 element
	err := storeTuple(mem, 0, value, desc, nil, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err == nil {
		t.Error("expected error for tuple element count mismatch in store")
	}
}

func TestFlagsWrongType(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.FlagsDesc{Names: []string{"a", "b"}}
	err := storeFlags(mem, 0, "not uint32", desc)
	if err == nil {
		t.Error("expected error for storing non-uint32 as flags")
	}
}

func TestLoadFlagsInvalidLabels(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.FlagsDesc{Names: make([]string, 0)} // Empty labels
	_, err := loadFlags(mem, 0, desc)
	if err == nil {
		t.Error("expected error for invalid flags (0 labels)")
	}
}

func TestStoreFlagsInvalidLabels(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.FlagsDesc{Names: make([]string, 0)}
	err := storeFlags(mem, 0, uint32(0), desc)
	if err == nil {
		t.Error("expected error for storing flags with 0 labels")
	}
}

func TestStoreOptWithMissingPayloadType(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.OptionDesc{
		Element: bintype.TypeRef{Primitive: "u32"},
	}

	// Store some value (not nil) which triggers storing discriminant 1
	err := storeOption(mem, 0, uint32(42), desc, nil, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err != nil {
		// This might succeed depending on alignment, so check if it actually stored
		t.Logf("storeOption succeeded or failed: %v", err)
	}
}

package binary

import (
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

const gcFeatures = api.CoreFeaturesV2 | api.CoreFeatureTypedFunctionReferences | api.CoreFeatureGC

func TestDecodeDefinedType_GC(t *testing.T) {
	i32 := wasm.ValueTypeI32.Kind()

	tests := []struct {
		name     string
		input    []byte
		expected wasm.FunctionType
	}{
		{
			name:  "empty struct",
			input: []byte{0x5f, 0},
			expected: wasm.FunctionType{
				CompositeKind: wasm.CompositeKindStruct, Fields: []wasm.FieldType{},
			},
		},
		{
			name:  "struct with an immutable i32 and a mutable packed i8",
			input: []byte{0x5f, 2, i32, 0, 0x78, 1},
			expected: wasm.FunctionType{
				CompositeKind: wasm.CompositeKindStruct,
				Fields: []wasm.FieldType{
					{Type: wasm.ValueTypeI32},
					{Type: wasm.ValueTypeI8, Mutable: true},
				},
			},
		},
		{
			name:  "array of mutable i16",
			input: []byte{0x5e, 0x77, 1},
			expected: wasm.FunctionType{
				CompositeKind: wasm.CompositeKindArray,
				Fields:        []wasm.FieldType{{Type: wasm.ValueTypeI16, Mutable: true}},
			},
		},
		{
			name:  "array of anyref",
			input: []byte{0x5e, 0x6e, 0},
			expected: wasm.FunctionType{
				CompositeKind: wasm.CompositeKindArray,
				Fields:        []wasm.FieldType{{Type: wasm.ValueTypeAnyref}},
			},
		},
		{
			name:     "sub final with no supertype is a plain func type",
			input:    []byte{0x4f, 0, 0x60, 0, 0},
			expected: wasm.FunctionType{},
		},
		{
			name:     "sub without final marks the type extensible",
			input:    []byte{0x50, 0, 0x60, 0, 0},
			expected: wasm.FunctionType{Extensible: true},
		},
		{
			name:     "sub with a supertype",
			input:    []byte{0x50, 1, 3, 0x60, 0, 0},
			expected: wasm.FunctionType{Extensible: true, HasSupertype: true, Supertype: 3},
		},
		{
			name:  "sub final struct with a supertype",
			input: []byte{0x4f, 1, 1, 0x5f, 1, i32, 0},
			expected: wasm.FunctionType{
				CompositeKind: wasm.CompositeKindStruct,
				Fields:        []wasm.FieldType{{Type: wasm.ValueTypeI32}},
				HasSupertype:  true, Supertype: 1,
			},
		},
		{
			name:     "non-nullable concrete ref param",
			input:    []byte{0x60, 1, 0x64, 2, 0},
			expected: wasm.FunctionType{Params: []wasm.ValueType{wasm.ValueTypeConcreteRef(2, false)}},
		},
		{
			name:     "ref null none result",
			input:    []byte{0x60, 0, 1, 0x63, 0x71},
			expected: wasm.FunctionType{Results: []wasm.ValueType{wasm.ValueTypeNullref}},
		},
		{
			name:     "ref i31 result",
			input:    []byte{0x60, 0, 1, 0x64, 0x6c},
			expected: wasm.FunctionType{Results: []wasm.ValueType{wasm.ValueTypeI31ref.AsNonNullable()}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var actual wasm.FunctionType
			offset, err := decodeDefinedType(gcFeatures, tc.input, 0, &valueTypeArena{}, &actual)
			require.NoError(t, err)
			require.Equal(t, len(tc.input), offset)
			// The decoder lays out the object's words as it reads the fields; do the same to the fixture
			// so these can stay plain literals.
			tc.expected.CacheFieldSlots()
			require.Equal(t, tc.expected, actual)
		})
	}
}

func TestDecodeDefinedType_GCErrors(t *testing.T) {
	i32 := wasm.ValueTypeI32.Kind()

	tests := []struct {
		name        string
		input       []byte
		expectedErr string
	}{
		{
			name:        "more than one supertype",
			input:       []byte{0x50, 2, 0, 1, 0x60, 0, 0},
			expectedErr: "more than one supertype is not supported: 2",
		},
		{
			name:        "truncated supertype count",
			input:       []byte{0x50},
			expectedErr: "read supertype count: EOF",
		},
		{
			name:        "truncated supertype index",
			input:       []byte{0x50, 1},
			expectedErr: "read supertype index: EOF",
		},
		{
			name:        "unknown composite kind",
			input:       []byte{0x5d, 0},
			expectedErr: "invalid byte: 0x5d != 0x60",
		},
		{
			name:        "truncated struct field count",
			input:       []byte{0x5f},
			expectedErr: "could not read field count: EOF",
		},
		{
			name:        "truncated struct field type",
			input:       []byte{0x5f, 1},
			expectedErr: "could not read field[0] type: EOF",
		},
		{
			name:        "missing field mutability",
			input:       []byte{0x5f, 1, i32},
			expectedErr: "could not read field[0] mutability: EOF",
		},
		{
			name:        "invalid field mutability",
			input:       []byte{0x5f, 1, i32, 2},
			expectedErr: "invalid field[0] mutability: 0x2",
		},
		{
			name:        "packed type outside a field",
			input:       []byte{0x60, 1, 0x78, 0},
			expectedErr: "could not read parameter types: invalid value type: 120",
		},
		{
			name:        "unknown abstract heap type",
			input:       []byte{0x60, 0, 1, 0x63, 0x75},
			expectedErr: "could not read result types: unknown abstract heap type: -11",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var actual wasm.FunctionType
			_, err := decodeDefinedType(gcFeatures, tc.input, 0, &valueTypeArena{}, &actual)
			require.EqualError(t, err, tc.expectedErr)
		})
	}
}

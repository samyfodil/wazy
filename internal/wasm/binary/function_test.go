package binary

import (
	"fmt"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

func TestFunctionType(t *testing.T) {
	i32, i64, funcRef, externRef := wasm.ValueTypeI32, wasm.ValueTypeI64, wasm.ValueTypeFuncref, wasm.ValueTypeExternref
	tests := []struct {
		name     string
		input    wasm.FunctionType
		expected []byte
	}{
		{
			name:     "empty",
			input:    wasm.FunctionType{},
			expected: []byte{0x60, 0, 0},
		},
		{
			name:     "one param no result",
			input:    wasm.FunctionType{Params: []wasm.ValueType{i32}},
			expected: []byte{0x60, 1, i32.Kind(), 0},
		},
		{
			name:     "no param one result",
			input:    wasm.FunctionType{Results: []wasm.ValueType{i32}},
			expected: []byte{0x60, 0, 1, i32.Kind()},
		},
		{
			name:     "one param one result",
			input:    wasm.FunctionType{Params: []wasm.ValueType{i64}, Results: []wasm.ValueType{i32}},
			expected: []byte{0x60, 1, i64.Kind(), 1, i32.Kind()},
		},
		{
			name:     "two params no result",
			input:    wasm.FunctionType{Params: []wasm.ValueType{i32, i64}},
			expected: []byte{0x60, 2, i32.Kind(), i64.Kind(), 0},
		},
		{
			name:     "two param one result",
			input:    wasm.FunctionType{Params: []wasm.ValueType{i32, i64}, Results: []wasm.ValueType{i32}},
			expected: []byte{0x60, 2, i32.Kind(), i64.Kind(), 1, i32.Kind()},
		},
		{
			name:     "no param two results",
			input:    wasm.FunctionType{Results: []wasm.ValueType{i32, i64}},
			expected: []byte{0x60, 0, 2, i32.Kind(), i64.Kind()},
		},
		{
			name:     "one param two results",
			input:    wasm.FunctionType{Params: []wasm.ValueType{i64}, Results: []wasm.ValueType{i32, i64}},
			expected: []byte{0x60, 1, i64.Kind(), 2, i32.Kind(), i64.Kind()},
		},
		{
			name:     "two param two results",
			input:    wasm.FunctionType{Params: []wasm.ValueType{i32, i64}, Results: []wasm.ValueType{i32, i64}},
			expected: []byte{0x60, 2, i32.Kind(), i64.Kind(), 2, i32.Kind(), i64.Kind()},
		},
		{
			name:     "two param two results with funcrefs",
			input:    wasm.FunctionType{Params: []wasm.ValueType{i32, funcRef}, Results: []wasm.ValueType{funcRef, i64}},
			expected: []byte{0x60, 2, i32.Kind(), funcRef.Kind(), 2, funcRef.Kind(), i64.Kind()},
		},
		{
			name:     "two param two results with externrefs",
			input:    wasm.FunctionType{Params: []wasm.ValueType{i32, externRef}, Results: []wasm.ValueType{externRef, i64}},
			expected: []byte{0x60, 2, i32.Kind(), externRef.Kind(), 2, externRef.Kind(), i64.Kind()},
		},
	}

	for _, tt := range tests {
		tc := tt

		b := binaryencoding.EncodeFunctionType(&tc.input)
		t.Run(fmt.Sprintf("encode - %s", tc.name), func(t *testing.T) {
			require.Equal(t, tc.expected, b)
		})

		t.Run(fmt.Sprintf("decode - %s", tc.name), func(t *testing.T) {
			var actual wasm.FunctionType
			_, err := decodeDefinedType(api.CoreFeaturesV2, b, 0, &valueTypeArena{}, &actual)
			require.NoError(t, err)
			// decodeTypeSection caches the key once the rec group fields are set, which is after this call.
			actual.CacheKey()
			tc.input.CacheKey()
			require.Equal(t, actual, tc.input)
		})
	}
}

func TestDecodeFunctionType_Errors(t *testing.T) {
	i32, i64 := wasm.ValueTypeI32.Kind(), wasm.ValueTypeI64.Kind()
	tests := []struct {
		name            string
		input           []byte
		enabledFeatures api.CoreFeatures
		expectedErr     string
	}{
		{
			name:        "undefined param no result",
			input:       []byte{0x60, 1, 0x00, 0},
			expectedErr: "could not read parameter types: invalid value type: 0",
		},
		{
			name:        "no param undefined result",
			input:       []byte{0x60, 0, 1, 0x00},
			expectedErr: "could not read result types: invalid value type: 0",
		},
		{
			name:        "undefined param undefined result",
			input:       []byte{0x60, 1, 0x00, 1, 0x00},
			expectedErr: "could not read parameter types: invalid value type: 0",
		},
		{
			name:        "anyref param - gc not enabled",
			input:       []byte{0x60, 1, 0x6e, 0},
			expectedErr: "could not read parameter types: value type anyref is invalid as feature \"gc\" is disabled",
		},
		{
			name:        "struct type - gc not enabled",
			input:       []byte{0x5f, 1, i32, 0},
			expectedErr: "struct type is invalid as feature \"gc\" is disabled",
		},
		{
			name:        "sub final - gc not enabled",
			input:       []byte{0x4f, 0, 0x60, 0, 0},
			expectedErr: "subtype declaration is invalid as feature \"gc\" is disabled",
		},
		{
			name:        "no param two results - multi-value not enabled",
			input:       []byte{0x60, 0, 2, i32, i64},
			expectedErr: "multiple result types invalid as feature \"multi-value\" is disabled",
		},
		{
			name:        "one param two results - multi-value not enabled",
			input:       []byte{0x60, 1, i64, 2, i32, i64},
			expectedErr: "multiple result types invalid as feature \"multi-value\" is disabled",
		},
		{
			name:        "two param two results - multi-value not enabled",
			input:       []byte{0x60, 2, i32, i64, 2, i32, i64},
			expectedErr: "multiple result types invalid as feature \"multi-value\" is disabled",
		},
	}

	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			var actual wasm.FunctionType
			_, err := decodeDefinedType(api.CoreFeaturesV1, tc.input, 0, &valueTypeArena{}, &actual)
			require.EqualError(t, err, tc.expectedErr)
		})
	}
}

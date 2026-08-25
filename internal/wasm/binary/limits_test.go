package binary

import (
	"fmt"
	"math"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestLimitsType(t *testing.T) {
	zero := uint64(0)
	largest := uint64(math.MaxUint32)
	largest64 := uint64(math.MaxUint64)

	tests := []struct {
		name     string
		min      uint64
		max      *uint64
		shared   bool
		index64  bool
		expected []byte
	}{
		{
			name:     "min 0",
			expected: []byte{0x0, 0},
		},
		{
			name:     "min 0, max 0",
			max:      &zero,
			expected: []byte{0x1, 0, 0},
		},
		{
			name:     "min largest",
			min:      largest,
			expected: []byte{0x0, 0xff, 0xff, 0xff, 0xff, 0xf},
		},
		{
			name:     "min 0, max largest",
			max:      &largest,
			expected: []byte{0x1, 0, 0xff, 0xff, 0xff, 0xff, 0xf},
		},
		{
			name:     "min largest max largest",
			min:      largest,
			max:      &largest,
			expected: []byte{0x1, 0xff, 0xff, 0xff, 0xff, 0xf, 0xff, 0xff, 0xff, 0xff, 0xf},
		},
		{
			name:     "min 0, shared",
			shared:   true,
			expected: []byte{0x2, 0},
		},
		{
			name:     "min 0, max 0, shared",
			max:      &zero,
			shared:   true,
			expected: []byte{0x3, 0, 0},
		},
		{
			name:     "min largest, shared",
			min:      largest,
			shared:   true,
			expected: []byte{0x2, 0xff, 0xff, 0xff, 0xff, 0xf},
		},
		{
			name:     "min 0, max largest, shared",
			max:      &largest,
			shared:   true,
			expected: []byte{0x3, 0, 0xff, 0xff, 0xff, 0xff, 0xf},
		},
		{
			name:     "min largest max largest, shared",
			min:      largest,
			max:      &largest,
			shared:   true,
			expected: []byte{0x3, 0xff, 0xff, 0xff, 0xff, 0xf, 0xff, 0xff, 0xff, 0xff, 0xf},
		},
		{
			name:     "min 0, i64",
			index64:  true,
			expected: []byte{0x4, 0},
		},
		{
			name:     "min 0, max 0, i64",
			max:      &zero,
			index64:  true,
			expected: []byte{0x5, 0, 0},
		},
		{
			name:     "min 0, shared, i64",
			shared:   true,
			index64:  true,
			expected: []byte{0x6, 0},
		},
		{
			name:     "min 0, max 0, shared, i64",
			max:      &zero,
			shared:   true,
			index64:  true,
			expected: []byte{0x7, 0, 0},
		},
		{
			name:     "min largest64, i64",
			min:      largest64,
			index64:  true,
			expected: []byte{0x4, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x1},
		},
		{
			name:    "min largest64 max largest64, i64",
			min:     largest64,
			max:     &largest64,
			index64: true,
			expected: []byte{
				0x5,
				0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x1,
				0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x1,
			},
		},
	}

	for _, tt := range tests {
		tc := tt

		b := binaryencoding.EncodeLimitsType(tc.min, tc.max, tc.shared, tc.index64)
		t.Run(fmt.Sprintf("encode - %s", tc.name), func(t *testing.T) {
			require.Equal(t, tc.expected, b)
		})

		t.Run(fmt.Sprintf("decode - %s", tc.name), func(t *testing.T) {
			min, max, shared, index64, _, err := decodeLimitsType(b, 0, api.CoreFeaturesV2|api.CoreFeatureMemory64)
			require.NoError(t, err)
			require.Equal(t, min, tc.min)
			require.Equal(t, max, tc.max)
			require.Equal(t, shared, tc.shared)
			require.Equal(t, index64, tc.index64)
		})
	}
}

func TestLimitsType_Errors(t *testing.T) {
	tests := []struct {
		name        string
		input       []byte
		features    api.CoreFeatures
		expectedErr string
	}{
		{
			name:        "unknown flag bit",
			input:       []byte{0x08, 0x00},
			features:    api.CoreFeaturesV2 | api.CoreFeatureMemory64,
			expectedErr: "invalid byte for limits: 0x8 not in (0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07)",
		},
		{
			name:        "i64 index type without memory64",
			input:       []byte{0x04, 0x00},
			features:    api.CoreFeaturesV2,
			expectedErr: `i64 index type in limits: feature "memory64" is disabled`,
		},
		{
			name:        "missing flag byte",
			input:       []byte{},
			features:    api.CoreFeaturesV2,
			expectedErr: "read leading byte: EOF",
		},
		{
			name:        "truncated min",
			input:       []byte{0x00},
			features:    api.CoreFeaturesV2,
			expectedErr: "read min of limit",
		},
		{
			name:        "truncated max",
			input:       []byte{0x01, 0x01},
			features:    api.CoreFeaturesV2,
			expectedErr: "read max of limit",
		},
		{
			name:        "truncated i64 min",
			input:       []byte{0x04},
			features:    api.CoreFeaturesV2 | api.CoreFeatureMemory64,
			expectedErr: "read min of limit",
		},
		{
			name:        "truncated i64 max",
			input:       []byte{0x05, 0x01},
			features:    api.CoreFeaturesV2 | api.CoreFeatureMemory64,
			expectedErr: "read max of limit",
		},
		{
			// A 32-bit index type keeps its u32 bounds: a value that needs more
			// than five LEB128 bytes stays malformed rather than decoding as u64.
			name:        "i32 min overflows u32",
			input:       []byte{0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01},
			features:    api.CoreFeaturesV2 | api.CoreFeatureMemory64,
			expectedErr: "read min of limit",
		},
		{
			name:        "i64 min overflows u64",
			input:       []byte{0x04, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02},
			features:    api.CoreFeaturesV2 | api.CoreFeatureMemory64,
			expectedErr: "read min of limit",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, err := decodeLimitsType(tc.input, 0, tc.features)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedErr)
		})
	}
}

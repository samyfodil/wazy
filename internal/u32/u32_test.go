package u32

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestBytes(t *testing.T) {
	tests := []struct {
		name  string
		input uint32
	}{
		{
			name:  "zero",
			input: 0,
		},
		{
			name:  "half",
			input: math.MaxInt32,
		},
		{
			name:  "max",
			input: math.MaxUint32,
		},
	}

	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			expected := make([]byte, 4)
			binary.LittleEndian.PutUint32(expected, tc.input)
			require.Equal(t, expected, LeBytes(tc.input))
		})
	}
}

func TestOutOfRange(t *testing.T) {
	// The point of the helper is the third case: an index above math.MaxInt32.
	// "int(index) >= length" reads that as negative where an int is 32 bits
	// wide (GOARCH=386, arm, wasm) and lets it through, so the caller indexes
	// out of range and panics.
	require.False(t, OutOfRange(0, 1))
	require.False(t, OutOfRange(9, 10))
	require.True(t, OutOfRange(10, 10))
	require.True(t, OutOfRange(math.MaxUint32, 10))
	require.True(t, OutOfRange(math.MaxInt32, 10))
	require.True(t, OutOfRange(math.MaxInt32+1, 10))
	require.True(t, OutOfRange(0, 0))
}

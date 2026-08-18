package wasm

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestVecOpcodeCodec(t *testing.T) {
	for _, op := range []OpcodeVec{
		OpcodeVecV128Load,                      // one byte
		OpcodeVecF32x4DemoteF64x2Zero,          // one byte, near the boundary
		OpcodeVecI16x8Q15mulrSatS,              // two bytes, first above the boundary
		OpcodeVecF64x2ConvertLowI32x4U,         // two bytes, last of the SIMD range
		OpcodeVecI8x16RelaxedSwizzle,           // two bytes, first relaxed
		OpcodeVecI32x4RelaxedDotI8x16I7x16AddS, // two bytes, last relaxed
	} {
		t.Run(VectorInstructionName(op), func(t *testing.T) {
			body := AppendVecOpcode(nil, op)
			if op < 0x80 {
				require.Equal(t, 1, len(body))
			} else {
				require.Equal(t, 2, len(body))
			}
			got, size := ReadVecOpcode(body, 0)
			require.Equal(t, op, got)
			require.Equal(t, len(body), size)
		})
	}

	t.Run("truncated", func(t *testing.T) {
		_, size := ReadVecOpcode([]byte{0x80}, 0)
		require.Zero(t, size)
	})

	t.Run("too wide", func(t *testing.T) {
		// Three-byte LEB128 is wider than any vector opcode.
		_, size := ReadVecOpcode([]byte{0x80, 0x80, 0x01}, 0)
		require.Zero(t, size)
	})
}

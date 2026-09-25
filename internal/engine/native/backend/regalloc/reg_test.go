package regalloc

import (
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestRegTypeOf(t *testing.T) {
	// Scalars ignore the v128 class entirely.
	for _, v128 := range []RegType{RegTypeFloat, RegTypeVec} {
		require.Equal(t, RegTypeInt, RegTypeOf(ssa.TypeI32, v128))
		require.Equal(t, RegTypeInt, RegTypeOf(ssa.TypeI64, v128))
		require.Equal(t, RegTypeFloat, RegTypeOf(ssa.TypeF32, v128))
		require.Equal(t, RegTypeFloat, RegTypeOf(ssa.TypeF64, v128))
	}
	// A v128 follows whichever file the ISA keeps vectors in: the same
	// registers as floats on arm64/amd64, a separate file on riscv64.
	require.Equal(t, RegTypeFloat, RegTypeOf(ssa.TypeV128, RegTypeFloat))
	require.Equal(t, RegTypeVec, RegTypeOf(ssa.TypeV128, RegTypeVec))
}

func TestVReg_String(t *testing.T) {
	require.Equal(t, "v0?", VReg(0).String())
	require.Equal(t, "v100?", VReg(100).String())
	require.Equal(t, "r5", FromRealReg(5, RegTypeInt).String())
}

func Test_FromRealReg(t *testing.T) {
	r := FromRealReg(5, RegTypeInt)
	require.Equal(t, RealReg(5), r.RealReg())
	require.Equal(t, VRegID(5), r.ID())
}

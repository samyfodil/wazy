package platform

import (
	"runtime"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestRiscV64CompilerSupports_SIMDFallback pins the gate that decides whether a
// riscv64 without the vector extension hands SIMD modules to the interpreter.
//
// Written against whatever this host reports rather than by overriding
// CpuFeatures, which is a constant on the platforms with no compiler at all.
func TestRiscV64CompilerSupports_SIMDFallback(t *testing.T) {
	defer func(prev bool) { RiscV64NoSIMDFallback = prev }(RiscV64NoSIMDFallback)
	hasV := CpuFeatures.Has(CpuFeatureRiscv64V)

	// A module that does not enable SIMD never depended on the vector extension.
	require.True(t, riscv64CompilerSupports(api.CoreFeaturesV1))

	// One that does needs the extension, or it goes to the interpreter.
	RiscV64NoSIMDFallback = false
	require.Equal(t, hasV, riscv64CompilerSupports(api.CoreFeaturesV2))

	// Unless the fallback is switched off, which is how a test reaches the scalar
	// lowering on hardware that cannot execute vector instructions.
	RiscV64NoSIMDFallback = true
	require.True(t, riscv64CompilerSupports(api.CoreFeaturesV2))
	// Still nothing to do with the other features.
	require.True(t, riscv64CompilerSupports(api.CoreFeaturesV1))
}

// TestRiscV64EmulatesSIMD_OnlyRiscV64 keeps the scalar lowering off every other
// architecture: an amd64 without SSE4.1 has no compiler at all, so it must not be
// mistaken for a machine that wants v128 lowered to word pairs.
func TestRiscV64EmulatesSIMD_OnlyRiscV64(t *testing.T) {
	if runtime.GOARCH != "riscv64" {
		require.False(t, RiscV64EmulatesSIMD(), "GOARCH=%s must never emulate", runtime.GOARCH)
		return
	}
	require.Equal(t, !CpuFeatures.Has(CpuFeatureRiscv64V), RiscV64EmulatesSIMD())
}

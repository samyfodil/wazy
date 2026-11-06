package adhoc

import (
	"context"
	_ "embed"
	"math/rand"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/simd_const_shapes.wasm
var simdConstShapesWasm []byte

// TestSIMDConstShapes runs v128 shuffles and shifts by constants against the interpreter: every window
// a shuffle can slide over two vectors, shuffles from one vector and from both, and every kind of
// constant shift, by amounts at and beyond the lane width and negative.
func TestSIMDConstShapes(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	compiler := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer compiler.Close(ctx)
	interp := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
	defer interp.Close(ctx)
	mc, err := compiler.Instantiate(ctx, simdConstShapesWasm)
	require.NoError(t, err)
	mi, err := interp.Instantiate(ctx, simdConstShapesWasm)
	require.NoError(t, err)
	rng := rand.New(rand.NewSource(1))
	inputs := [][4]uint64{{0x0706050403020100, 0x0f0e0d0c0b0a0908, 0x1716151413121110, 0x1f1e1d1c1b1a1918}}
	for i := 0; i < 20; i++ {
		inputs = append(inputs, [4]uint64{rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64()})
	}
	for name := range mc.ExportedFunctionDefinitions() {
		for _, in := range inputs {
			rc, err := mc.ExportedFunction(name).Call(ctx, in[:]...)
			require.NoError(t, err)
			ri, err := mi.ExportedFunction(name).Call(ctx, in[:]...)
			require.NoError(t, err)
			require.Equal(t, ri, rc, "%s(%#x)", name, in)
		}
	}
}

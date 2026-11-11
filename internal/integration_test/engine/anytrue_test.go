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

//go:embed testdata/anytrue_and.wasm
var anyTrueAndWasm []byte

// TestAnyTrueOfAnd runs v128.any_true(v128.and(a, b)) against the interpreter: vectors that share no
// bit, share one in either half, and random ones.
func TestAnyTrueOfAnd(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	compiler := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer compiler.Close(ctx)
	interp := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
	defer interp.Close(ctx)
	mc, err := compiler.Instantiate(ctx, anyTrueAndWasm)
	require.NoError(t, err)
	mi, err := interp.Instantiate(ctx, anyTrueAndWasm)
	require.NoError(t, err)

	cases := [][4]uint64{
		{0, 0, 0, 0},
		{^uint64(0), ^uint64(0), 0, 0},
		{0xf0, 0, 0x0f, 0},
		{1, 0, 1, 0},
		{0, 1 << 63, 0, 1 << 63},
		{0, 1, 1, 0},
		{1 << 63, 0, 1 << 63, 0},
		{0xaaaa, 0x5555, 0x5555, 0xaaaa},
		{0, 0x100, ^uint64(0), 0x100},
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		c := [4]uint64{rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64()}
		if i%2 == 0 { // Sparse: often disjoint.
			for k := range c {
				c[k] &= rng.Uint64() & rng.Uint64() & rng.Uint64()
			}
		}
		cases = append(cases, c)
	}
	for _, fn := range []string{"value", "branch", "branch_not", "shared"} {
		for _, c := range cases {
			rc, err := mc.ExportedFunction(fn).Call(ctx, c[:]...)
			require.NoError(t, err)
			ri, err := mi.ExportedFunction(fn).Call(ctx, c[:]...)
			require.NoError(t, err)
			require.Equal(t, ri, rc, "%s(%#x)", fn, c)
		}
	}
}

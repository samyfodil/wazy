package adhoc

import (
	"context"
	_ "embed"
	"math"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/linear_form.wasm
var linearFormWasm []byte

// TestLinearForm runs sums of products of constants, which the compiler rewrites as one coefficient per
// value, against the interpreter.
func TestLinearForm(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	mods := map[bool]api.Module{}
	for _, interp := range []bool{false, true} {
		config := wazy.NewRuntimeConfigCompiler()
		if interp {
			config = wazy.NewRuntimeConfigInterpreter()
		}
		r := wazy.NewRuntimeWithConfig(ctx, config)
		defer r.Close(ctx)
		m, err := r.Instantiate(ctx, linearFormWasm)
		require.NoError(t, err)
		mods[interp] = m
	}
	vals := []uint64{0, 1, 2, 7, 0x7fffffff, 0x80000000, 0xffffffff, 0x12345678, math.MaxInt64, 1 << 63, math.MaxUint64, 0x0123456789abcdef}
	for _, fn := range []string{"mesh", "mesh64", "wide", "cancel", "constant", "negative", "negconst", "shared", "few", "varmul", "leftconst", "twice", "long", "nogain", "pow2", "minusone"} {
		for _, a := range vals {
			for _, b := range vals {
				for _, c := range vals[:6] {
					args := []uint64{a, b, c}
					if fn != "mesh64" {
						args = []uint64{uint64(uint32(a)), uint64(uint32(b)), uint64(uint32(c))}
					}
					want, err := mods[true].ExportedFunction(fn).Call(ctx, args...)
					require.NoError(t, err)
					got, err := mods[false].ExportedFunction(fn).Call(ctx, args...)
					require.NoError(t, err)
					require.Equal(t, want, got, "%s(%#x, %#x, %#x)", fn, args[0], args[1], args[2])
				}
			}
		}
	}
}

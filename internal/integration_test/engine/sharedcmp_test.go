package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/sharedcmp.wasm
var sharedCmpWasm []byte

// TestSharedCompare runs comparisons several selects and branches take, which each get a copy of their
// own, against the interpreter.
func TestSharedCompare(t *testing.T) {
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
		m, err := r.Instantiate(ctx, sharedCmpWasm)
		require.NoError(t, err)
		mods[interp] = m
	}
	vals := []uint64{0, 1, 2, 0x7fffffff, 0x80000000, 0xffffffff}
	for _, fn := range []string{"selects", "mixed", "wide"} {
		for _, a := range vals {
			for _, b := range vals {
				want, err := mods[true].ExportedFunction(fn).Call(ctx, a, b)
				require.NoError(t, err)
				got, err := mods[false].ExportedFunction(fn).Call(ctx, a, b)
				require.NoError(t, err)
				require.Equal(t, want, got, "%s(%#x, %#x)", fn, a, b)
			}
		}
	}
}

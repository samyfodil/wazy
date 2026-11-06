package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/frontend"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/popcnt_loop.wasm
var popcntLoopWasm []byte

// TestPopcountLoop runs loops that count bits by clearing the lowest one, which the compiler lowers as a
// population count, against the interpreter, x zero on entry and its top bit set included, and checks
// that only those are.
func TestPopcountLoop(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	loops := 0
	frontend.PopcountLoopHook = func() { loops++ }
	mods := map[bool]api.Module{}
	for _, interp := range []bool{false, true} {
		config := wazy.NewRuntimeConfigCompiler()
		if interp {
			config = wazy.NewRuntimeConfigInterpreter()
		}
		r := wazy.NewRuntimeWithConfig(ctx, config)
		defer r.Close(ctx)
		m, err := r.Instantiate(ctx, popcntLoopWasm)
		require.NoError(t, err)
		mods[interp] = m
	}
	frontend.PopcountLoopHook = nil
	require.Equal(t, 2, loops, "count and swapped, not twice or other")
	for _, fn := range []string{"count", "swapped", "twice", "other"} {
		for _, x := range []uint32{0, 1, 2, 3, 0x80000000, 0xffffffff, 0x12345678, 0x7fffffff} {
			for _, n := range []uint32{0, 5, 0xffffffff} {
				args := []uint64{uint64(x), uint64(n)}
				want, err := mods[true].ExportedFunction(fn).Call(ctx, args...)
				require.NoError(t, err)
				got, err := mods[false].ExportedFunction(fn).Call(ctx, args...)
				require.NoError(t, err)
				require.Equal(t, want, got, "%s(%#x, %d)", fn, x, n)
			}
		}
	}
}

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

//go:embed testdata/brifmerge.wasm
var brIfMergeWasm []byte

// TestBrIfMerge runs br_ifs to one label after one another, which the compiler makes one branch on
// either condition, and a br_if ending a block after one to it, which it makes one branch on not the first
// and the second, against the interpreter for values around every constant they compare with.
func TestBrIfMerge(t *testing.T) {
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
		m, err := r.Instantiate(ctx, brIfMergeWasm)
		require.NoError(t, err)
		mods[interp] = m
	}
	i32s := []int32{math.MinInt32, -6, -5, -4, -1, 0, 1, 2, 6, 7, 8, 30, 31, 32, 99, 100, 101, 300, math.MaxInt32}
	i64s := []int64{math.MinInt64, -6, -5, -4, 0, 5, math.MaxInt64}
	for _, fn := range []string{"range", "three", "mixed", "twolabels", "loop", "overand", "overloop", "arith", "overresult"} {
		for _, a := range i32s {
			for _, b := range i32s {
				for _, c := range i64s {
					args := []uint64{api.EncodeI32(a), api.EncodeI32(b), api.EncodeI64(c)}
					want, err := mods[true].ExportedFunction(fn).Call(ctx, args...)
					require.NoError(t, err)
					got, err := mods[false].ExportedFunction(fn).Call(ctx, args...)
					require.NoError(t, err)
					require.Equal(t, want, got, "%s(%d, %d, %d)", fn, a, b, c)
				}
			}
		}
	}
}

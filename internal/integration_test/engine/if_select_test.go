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

//go:embed testdata/if_select.wasm
var ifSelectWasm []byte

// TestIfSelect runs ifs with short pure arms, which the compiler lowers as selects, and
// multiplications by powers of two, which it lowers as shifts, against the interpreter.
func TestIfSelect(t *testing.T) {
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
		m, err := r.Instantiate(ctx, ifSelectWasm)
		require.NoError(t, err)
		mods[interp] = m
	}
	i32s := []int32{math.MinInt32, -6, -5, -4, -1, 0, 1, 2, 6, 7, 8, 30, 31, 32, 99, 100, 101, 300, math.MaxInt32}
	i64s := []int64{math.MinInt64, -6, -5, -4, 0, 5, math.MaxInt64}
	for _, fn := range []string{"zero", "cmp", "wide", "long", "traps", "sets", "pow2"} {
		for _, a := range i32s {
			for _, b := range i32s {
				for _, c := range i64s {
					args := []uint64{api.EncodeI32(a), api.EncodeI32(b), api.EncodeI64(c)}
					want, wantErr := mods[true].ExportedFunction(fn).Call(ctx, args...)
					got, err := mods[false].ExportedFunction(fn).Call(ctx, args...)
					require.Equal(t, wantErr == nil, err == nil, "%s(%d, %d, %d): %v", fn, a, b, c, err)
					if wantErr != nil {
						require.Equal(t, wantErr.Error(), err.Error(), "%s(%d, %d, %d)", fn, a, b, c)
					}
					require.Equal(t, want, got, "%s(%d, %d, %d)", fn, a, b, c)
				}
			}
		}
	}
}

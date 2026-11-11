package adhoc

import (
	"context"
	_ "embed"
	"math"
	"runtime"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/frontend"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/clampdiv.wasm
var clampDivWasm []byte

// TestClampedDiv runs divisions whose quotient is clamped against the interpreter: the compiler skips
// the division where a positive divisor puts the quotient outside the clamp, and must give the same
// results, and the same traps, for every dividend and divisor, at the clamp's edges above all.
func TestClampedDiv(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	var clamps [][2]int64
	frontend.ClampedDivHook = func(lo, hi int64) { clamps = append(clamps, [2]int64{lo, hi}) }
	compiler := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer compiler.Close(ctx)
	cm, err := compiler.Instantiate(ctx, clampDivWasm)
	frontend.ClampedDivHook = nil
	require.NoError(t, err)
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		require.Equal(t, [][2]int64{{-8, 7}, {-3, 100}, {-8, 7}}, clamps, "clang, minfirst and adpcm, and not inter, raw or byte")
	}
	interp := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
	defer interp.Close(ctx)
	im, err := interp.Instantiate(ctx, clampDivWasm)
	require.NoError(t, err)

	call := func(m api.Module, fn string, args ...uint64) (uint64, string) {
		r, err := m.ExportedFunction(fn).Call(ctx, args...)
		if err != nil {
			return 0, err.Error()
		}
		return r[0], ""
	}
	var xs, ds []int32
	for _, d := range []int32{1, 2, 3, 7, 16, 1000, 65535, math.MaxInt32, 0, -1, -2, -7, math.MinInt32} {
		ds = append(ds, d)
		for _, q := range []int32{-10, -9, -8, -7, -4, -3, -2, 0, 6, 7, 8, 99, 100, 101} {
			for _, e := range []int32{-1, 0, 1} {
				xs = append(xs, q*d+e)
			}
		}
	}
	xs = append(xs, 0, 1, -1, math.MinInt32, math.MinInt32+1, math.MaxInt32, math.MaxInt32-1)
	for _, fn := range []string{"clang", "minfirst", "inter", "raw", "byte"} {
		for _, d := range ds {
			for _, x := range xs {
				args := []uint64{api.EncodeI32(x), api.EncodeI32(d)}
				want, wantErr := call(im, fn, args...)
				got, gotErr := call(cm, fn, args...)
				require.Equal(t, wantErr, gotErr, "%s(%d, %d)", fn, x, d)
				require.Equal(t, want, got, "%s(%d, %d)", fn, x, d)
			}
		}
	}
	for _, seed := range []uint64{0, 1, 0x9e3779b9} {
		want, _ := call(im, "adpcm", 20000, seed)
		got, _ := call(cm, "adpcm", 20000, seed)
		require.Equal(t, want, got)
	}
}

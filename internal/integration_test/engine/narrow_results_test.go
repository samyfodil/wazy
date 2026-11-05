package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/narrow_results.wasm
var narrowResultsWasm []byte

// TestNarrowResults checks that i32 and f32 results come back zero-extended, as api.EncodeI32 and
// api.EncodeF32 encode them, even in a slot that held a parameter with its upper half set.
func TestNarrowResults(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []wazy.RuntimeConfig{wazy.NewRuntimeConfigCompiler(), wazy.NewRuntimeConfigInterpreter()} {
		r := wazy.NewRuntimeWithConfig(ctx, cfg)
		m, err := r.Instantiate(ctx, narrowResultsWasm)
		require.NoError(t, err)
		res, err := m.ExportedFunction("i32").Call(ctx, 0xffffffff_00000005)
		require.NoError(t, err)
		require.Equal(t, []uint64{5}, res)
		res, err = m.ExportedFunction("f32").Call(ctx, 0xffffffff_3f800000)
		require.NoError(t, err)
		require.Equal(t, []uint64{api.EncodeF32(1)}, res)
		params := make([]uint64, 20)
		for i := range params {
			params[i] = ^uint64(0)
		}
		res, err = m.ExportedFunction("many").Call(ctx, params...)
		require.NoError(t, err)
		for i, v := range res {
			want := api.EncodeI32(int32(i + 1))
			if i%2 == 1 {
				want = api.EncodeF32(float32(i + 1))
			}
			require.Equal(t, want, v, "result %d", i)
		}
		require.NoError(t, r.Close(ctx))
	}
}

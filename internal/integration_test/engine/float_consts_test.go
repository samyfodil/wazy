package adhoc

import (
	"context"
	_ "embed"
	"math"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/float_consts_pressure.wasm
var floatConstsPressureWasm []byte

// TestFloatConstsUnderPressure runs a loop with more float constants than vector registers against the
// interpreter: the compiler hoists them out of the loop and, short of registers, rematerializes them.
func TestFloatConstsUnderPressure(t *testing.T) {
	ctx := context.Background()
	var got [2][]uint64
	for i, cfg := range []wazy.RuntimeConfig{wazy.NewRuntimeConfigCompiler(), wazy.NewRuntimeConfigInterpreter()} {
		r := wazy.NewRuntimeWithConfig(ctx, cfg)
		m, err := r.Instantiate(ctx, floatConstsPressureWasm)
		require.NoError(t, err)
		for _, n := range []uint64{1, 2, 7, 100} {
			res, err := m.ExportedFunction("run").Call(ctx, n, math.Float64bits(0.3))
			require.NoError(t, err)
			got[i] = append(got[i], res...)
		}
		require.NoError(t, r.Close(ctx))
	}
	require.Equal(t, got[1], got[0])
}

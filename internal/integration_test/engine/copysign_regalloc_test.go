package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/copysign_regalloc.wasm
var copysignRegallocWasm []byte

// TestCopysignRegalloc is a regression test for an amd64 compiler bug where
// f32.copysign, with its sign taken from a float select and its magnitude
// from a call result, could have a sign-mask temporary allocated to xmm0
// while xmm0 still held the call result, flipping the result's sign.
// See https://github.com/wazero/wazero/issues/2555.
func TestCopysignRegalloc(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		cfg  wazy.RuntimeConfig
	}{
		{"default", wazy.NewRuntimeConfig()},
		{"interpreter", wazy.NewRuntimeConfigInterpreter()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := wazy.NewRuntimeWithConfig(ctx, tc.cfg)
			defer r.Close(ctx)

			mod, err := r.Instantiate(ctx, copysignRegallocWasm)
			require.NoError(t, err)
			defer mod.Close(ctx)

			result, err := mod.ExportedFunction("f").Call(ctx, api.EncodeF32(1.5))
			require.NoError(t, err)
			require.Equal(t, float32(1.5), api.DecodeF32(result[0]))
		})
	}
}

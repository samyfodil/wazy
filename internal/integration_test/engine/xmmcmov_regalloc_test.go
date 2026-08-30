package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/xmmcmov_regalloc.wasm
var xmmCmovRegallocWasm []byte

// TestXmmCmovRegalloc is a regression test for an amd64 compiler bug where
// xmmCMov, which implements a float select, declared its destination as a def
// only and not as a use. Nothing then prevented the register allocator from
// evicting it, returning garbage.
// See https://github.com/wazero/wazero/issues/2534.
func TestXmmCmovRegalloc(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfig())
	defer r.Close(ctx)

	mod, err := r.Instantiate(ctx, xmmCmovRegallocWasm)
	require.NoError(t, err)
	defer mod.Close(ctx)

	result, err := mod.ExportedFunction("f").Call(ctx, api.EncodeI32(0))
	require.NoError(t, err)
	require.Equal(t, 2.5, api.DecodeF64(result[0]))
}

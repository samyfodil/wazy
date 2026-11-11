package interpreter_test

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/interpreter"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/call_buffers.wasm
var callBuffersWasm []byte

// TestCallBuffers checks that a handle gives its pooled operand stack and frames back after each of its early
// calls and keeps them after that, and that neither a host function re-entering the handle nor a trap breaks it.
func TestCallBuffers(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
	defer r.Close(ctx)

	var f api.Function
	_, err := wazy.HostFunc1(r.NewHostModuleBuilder("env").NewFunctionBuilder(),
		func(ctx context.Context, _ api.Module, n uint32) uint32 {
			res, err := f.Call(ctx, uint64(n))
			if err != nil {
				panic(err)
			}
			return uint32(res[0])
		}).Export("reenter").Instantiate(ctx)
	require.NoError(t, err)
	mod, err := r.Instantiate(ctx, callBuffersWasm)
	require.NoError(t, err)
	f = mod.ExportedFunction("f")

	for i := 0; i < interpreter.BuffersReleasesBeforeKeepForTest; i++ {
		// Each re-entry runs on the outer call's slices, which must still be there when it returns.
		res, err := f.Call(ctx, 5)
		require.NoError(t, err)
		require.Equal(t, uint64(105), res[0])
		require.False(t, interpreter.HoldsBuffersForTest(f), "call %d kept buffers it should have released", i)
	}
	for i := 0; i < 3; i++ {
		res, err := f.Call(ctx, 5)
		require.NoError(t, err)
		require.Equal(t, uint64(105), res[0])
		require.True(t, interpreter.HoldsBuffersForTest(f), "call %d released buffers it should have kept", i)
	}

	trap := mod.ExportedFunction("trap")
	_, err = trap.Call(ctx)
	require.Error(t, err)
	require.False(t, interpreter.HoldsBuffersForTest(trap))
	_, err = trap.Call(ctx)
	require.Error(t, err)
}

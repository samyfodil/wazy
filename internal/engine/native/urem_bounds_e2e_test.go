package native_test

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/urem_bounds.wasm
var uremBoundsWasm []byte

func TestURemBoundsCheckElisionE2E(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)

	compiled, err := r.CompileModule(ctx, uremBoundsWasm)
	require.NoError(t, err)
	mod, err := r.InstantiateModule(ctx, compiled, wazy.NewModuleConfig().WithName("m"))
	require.NoError(t, err)

	require.True(t, mod.Memory().WriteUint32Le(1048576+0x40, 0xcafef00d))

	// 65533-1+4 exactly reaches the one-page minimum, so this check is
	// elided while the load remains correct.
	res, err := mod.ExportedFunction("safe").Call(ctx, 0x40)
	require.NoError(t, err)
	require.Equal(t, uint64(0xcafef00d), res[0])
	res, err = mod.ExportedFunction("scaled").Call(ctx, 0x10)
	require.NoError(t, err)
	require.Equal(t, uint64(0xcafef00d), res[0])

	// These ranges exceed the memory minimum and must retain their checks.
	_, err = mod.ExportedFunction("unsafe").Call(ctx, 65533)
	require.Error(t, err)
	_, err = mod.ExportedFunction("wide").Call(ctx, 2_000_000)
	require.Error(t, err)
	_, err = mod.ExportedFunction("scaled_unsafe").Call(ctx, 16384)
	require.Error(t, err)
	_, err = mod.ExportedFunction("scaled_overflow").Call(ctx, 1_000_000)
	require.Error(t, err)

	// A zero divisor has no range proof and must still trap at the remainder.
	_, err = mod.ExportedFunction("zero").Call(ctx, 1)
	require.Error(t, err)
}

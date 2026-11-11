package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/derived_address.wasm
var derivedAddressWasm []byte

// TestDerivedAddress runs accesses through x + k, which the compiler leaves unchecked when an earlier
// access proved x safe far enough, against the interpreter, at both ends of the memory: results and
// traps must be the same.
func TestDerivedAddress(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	run := func(config wazy.RuntimeConfig, name string, x uint64) ([]uint64, string) {
		r := wazy.NewRuntimeWithConfig(ctx, config)
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, derivedAddressWasm)
		require.NoError(t, err)
		require.True(t, mod.Memory().WriteUint32Le(uint32(x+8)&0xffff, 0x01020304))
		res, err := mod.ExportedFunction(name).Call(ctx, x)
		if err != nil {
			return nil, err.Error()
		}
		return res, ""
	}
	for _, name := range []string{"within", "past", "below", "store_then_load", "after_call"} {
		for _, x := range []uint64{0, 4, 100, 65536 - 24, 65536 - 23, 65536 - 28, 65536} {
			wantRes, wantErr := run(wazy.NewRuntimeConfigInterpreter(), name, x)
			gotRes, gotErr := run(wazy.NewRuntimeConfigCompiler(), name, x)
			require.Equal(t, wantErr, gotErr, "%s(%d)", name, x)
			require.Equal(t, wantRes, gotRes, "%s(%d)", name, x)
		}
	}
}

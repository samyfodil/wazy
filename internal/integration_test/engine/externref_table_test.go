package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/externref_table.wasm
var externrefTableWasm []byte

// TestExternrefTableRoundTrip stores a host externref with its high 32 bits set in a table and reads it back.
// A table entry was a uintptr, so on a 32-bit target the store kept only the low half of a reference that is
// 64 bits everywhere else: an externref, and a GC or exception handle, whose tag and generation live up there.
func TestExternrefTableRoundTrip(t *testing.T) {
	configs := map[string]wazy.RuntimeConfig{"interpreter": wazy.NewRuntimeConfigInterpreter()}
	if platform.CompilerSupported() {
		configs["compiler"] = wazy.NewRuntimeConfigCompiler()
	}
	for name, config := range configs {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntimeWithConfig(ctx, config)
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, externrefTableWasm)
			require.NoError(t, err)

			const ref = uint64(0xdead_beef_0000_0001)
			res, err := mod.ExportedFunction("round_trip").Call(ctx, ref)
			require.NoError(t, err)
			require.Equal(t, ref, res[0])
		})
	}
}

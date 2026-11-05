package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/band_flags.wasm
var bandFlagsWasm []byte

// TestBandFlags runs branches on comparisons of an and with zero, which arm64 makes a tst, against the
// interpreter, for every condition, and the and of two such tests as one branch.
func TestBandFlags(t *testing.T) {
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
		m, err := r.Instantiate(ctx, bandFlagsWasm)
		require.NoError(t, err)
		mods[interp] = m
	}
	vals := []uint64{0, 1, 0xff, 0xf0f0, 0x0f0f, 0x80000000, 0xffffffff, 0x12345678}
	for _, fn := range []string{"gtu", "geu", "ltu", "leu", "gts", "les", "zgtu", "subset"} {
		for _, a := range vals {
			for _, b := range vals {
				want, err := mods[true].ExportedFunction(fn).Call(ctx, a, b)
				require.NoError(t, err)
				got, err := mods[false].ExportedFunction(fn).Call(ctx, a, b)
				require.NoError(t, err)
				require.Equal(t, want, got, "%s(%#x, %#x)", fn, a, b)
			}
		}
	}
}

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

//go:embed testdata/notfold.wasm
var notFoldWasm []byte

// TestNotFold runs bitwise operations of an inverted value, which arm64 makes one bic, bics, orn, eon or
// mvn, against the interpreter.
func TestNotFold(t *testing.T) {
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
		m, err := r.Instantiate(ctx, notFoldWasm)
		require.NoError(t, err)
		mods[interp] = m
	}
	vals := []uint64{0, 1, 0xff, 0xff00, 0xffffffff, 0x1_0000_0000, 0x8000_0000_0000_0000, ^uint64(0), 0x0123_4567_89ab_cdef}
	for _, fn := range []string{"bic", "bic_left", "orn", "eon", "mvn", "subset", "shared", "constant"} {
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

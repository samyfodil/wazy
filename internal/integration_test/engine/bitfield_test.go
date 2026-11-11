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

//go:embed testdata/bitfield.wasm
var bitfieldWasm []byte

// TestBitfield runs bit fields and shifted operands, which arm64 makes ubfx and shifted-register
// forms, against the interpreter.
func TestBitfield(t *testing.T) {
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
		m, err := r.Instantiate(ctx, bitfieldWasm)
		require.NoError(t, err)
		mods[interp] = m
	}
	vals := []uint64{0, 1, 0x80000000, 0xffffffff, 0x12345678, 0xdeadbeef, 1 << 63, ^uint64(0), 0x0123456789abcdef}
	for _, fn := range []string{"f32", "f64", "notfield", "first", "low", "low64", "mismatch"} {
		for _, x := range vals {
			for _, y := range vals {
				args := []uint64{uint64(uint32(x)), y}
				want, err := mods[true].ExportedFunction(fn).Call(ctx, args...)
				require.NoError(t, err)
				got, err := mods[false].ExportedFunction(fn).Call(ctx, args...)
				require.NoError(t, err)
				require.Equal(t, want, got, "%s(%#x, %#x)", fn, args[0], args[1])
			}
		}
	}
}

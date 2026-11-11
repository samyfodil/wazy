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

//go:embed testdata/zext_param.wasm
var zextParamWasm []byte

// TestZextParam runs loops whose addresses are block parameters, which arm64 uses without zero-extending
// where every value coming in is, against the interpreter, with the function's i32 arguments passed with
// their top halves set, as a host may.
func TestZextParam(t *testing.T) {
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
		m, err := r.Instantiate(ctx, zextParamWasm)
		require.NoError(t, err)
		mods[interp] = m
	}
	// The loops run n times; ops takes any n.
	for _, fn := range []string{"param", "masked", "ops"} {
		ps, ns := []uint64{0, 3, 20, 65530}, []uint64{0, 1, 5, 9}
		if fn == "ops" {
			ps, ns = append(ps, 0x80000000, 0xffffffff), append(ns, 0x80000001, 0xfffffff9)
		}
		for _, top := range []uint64{0, 1 << 32, 0xffffffff_00000000} {
			for _, p := range ps {
				for _, n := range ns {
					args := []uint64{top | p, top | n}
					want, wantErr := mods[true].ExportedFunction(fn).Call(ctx, args...)
					got, err := mods[false].ExportedFunction(fn).Call(ctx, args...)
					require.Equal(t, wantErr == nil, err == nil, "%s(%#x, %#x): %v", fn, args[0], args[1], err)
					require.Equal(t, want, got, "%s(%#x, %#x)", fn, args[0], args[1])
				}
			}
		}
	}
}

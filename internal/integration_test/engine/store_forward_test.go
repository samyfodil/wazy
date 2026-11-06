package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/store_forward.wasm
var storeForwardWasm []byte

// TestStoreForward runs loads of what a store just wrote against the interpreter: the compiler takes
// the stored value where nothing between can have changed it, and must read memory everywhere else.
func TestStoreForward(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	type outcome struct {
		results []uint64
		err     string
	}
	run := func(config wazy.RuntimeConfig, name string, params ...uint64) outcome {
		r := wazy.NewRuntimeWithConfig(ctx, config)
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, storeForwardWasm)
		require.NoError(t, err)
		var o outcome
		if o.results, err = mod.ExportedFunction(name).Call(ctx, params...); err != nil {
			o.err = err.Error()
		}
		return o
	}
	for _, c := range []struct {
		name   string
		params []uint64
	}{
		{"same", []uint64{100, 0xdeadbeef}},
		{"alias", []uint64{100, 100}},
		{"alias", []uint64{100, 200}},
		{"narrow", []uint64{64}},
		{"call", []uint64{8}},
		{"fill", []uint64{8}},
		{"block", []uint64{8, 0}},
		{"block", []uint64{8, 1}},
		{"nan", []uint64{8}},
		{"oob", []uint64{65532}},
	} {
		want := run(wazy.NewRuntimeConfigInterpreter(), c.name, c.params...)
		got := run(wazy.NewRuntimeConfigCompiler(), c.name, c.params...)
		require.Equal(t, want, got, "%s%v", c.name, c.params)
	}
}

package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/select_sink.wasm
var selectSinkWasm []byte

// TestSelectSink runs selects the compiler may lower as a branch around one side's work against the
// interpreter: the kept side first or second, set or teed, i32 or i64, and ones whose work something
// else needs too.
func TestSelectSink(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	run := func(config wazy.RuntimeConfig, name string, params ...uint64) []uint64 {
		r := wazy.NewRuntimeWithConfig(ctx, config)
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, selectSinkWasm)
		require.NoError(t, err)
		res, err := mod.ExportedFunction(name).Call(ctx, params...)
		require.NoError(t, err)
		return res
	}
	for _, c := range []struct {
		name   string
		params []uint64
	}{
		{"hashbits", []uint64{1000, 0}},
		{"hashbits", []uint64{3, 0x9e3779b9}},
		{"hashbits", []uint64{5000, 12345}},
		{"hashbits64", []uint64{3000}},
		{"shared", []uint64{1000}},
		{"other", []uint64{1000}},
	} {
		want := run(wazy.NewRuntimeConfigInterpreter(), c.name, c.params...)
		got := run(wazy.NewRuntimeConfigCompiler(), c.name, c.params...)
		require.Equal(t, want, got, "%s%v", c.name, c.params)
	}
}

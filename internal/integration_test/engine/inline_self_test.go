package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/inline_self.wasm
var inlineSelfWasm []byte

// TestInlineSelf runs recursive functions, whose calls to themselves the compiler lowers as a copy of
// their body, against the interpreter, which never inlines: results, and the backtraces of traps in a
// copy and in a callee called from one, must be the same.
func TestInlineSelf(t *testing.T) {
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
		mod, err := r.Instantiate(ctx, inlineSelfWasm)
		require.NoError(t, err)
		for a := uint32(0); a < 65536; a += 4 {
			require.True(t, mod.Memory().WriteUint32Le(a, a*2654435761))
		}
		var o outcome
		o.results, err = mod.ExportedFunction(name).Call(ctx, params...)
		if err != nil {
			o.err = err.Error()
		}
		return o
	}
	type call struct {
		name   string
		params []uint64
	}
	var calls []call
	for n := uint64(0); n <= 22; n++ {
		calls = append(calls, call{"fib", []uint64{n}})
	}
	for _, p := range []uint64{0, 65536 - 4*40, 65536 - 4*9, 65536 - 4*10, 65536 - 2} {
		calls = append(calls, call{"sumdown", []uint64{p, 30}})
	}
	for n := uint64(0); n <= 7; n++ {
		calls = append(calls, call{"deep", []uint64{n}})
	}
	for a := uint64(0); a <= 6; a++ {
		calls = append(calls, call{"pair", []uint64{a, 3}})
	}
	for _, c := range calls {
		want := run(wazy.NewRuntimeConfigInterpreter(), c.name, c.params...)
		got := run(wazy.NewRuntimeConfigCompiler(), c.name, c.params...)
		require.Equal(t, want, got, "%s%v", c.name, c.params)
	}
}

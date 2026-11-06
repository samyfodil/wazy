package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/boundswiden.wasm
var boundsWidenWasm []byte

// TestBoundsWiden runs loads of one address at growing offsets, whose bounds checks the compiler merges
// into the first, against the interpreter, at and past the end of memory: results, traps, and memory
// after a trap must agree, also with a store or a division between the loads.
func TestBoundsWiden(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	type outcome struct {
		results []uint64
		err     string
		mem     []byte
	}
	run := func(config wazy.RuntimeConfig, name string, params ...uint64) outcome {
		r := wazy.NewRuntimeWithConfig(ctx, config)
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, boundsWidenWasm)
		require.NoError(t, err)
		for a := uint32(0); a < 65536; a += 4 {
			require.True(t, mod.Memory().WriteUint32Le(a, a*2654435761+7))
		}
		var o outcome
		if o.results, err = mod.ExportedFunction(name).Call(ctx, params...); err != nil {
			o.err = err.Error()
		}
		m, _ := mod.Memory().Read(0, 65536)
		o.mem = append([]byte(nil), m...)
		return o
	}
	const end = 65536
	var calls []struct {
		name   string
		params []uint64
	}
	add := func(name string, params ...uint64) {
		calls = append(calls, struct {
			name   string
			params []uint64
		}{name, params})
	}
	for _, p := range []uint64{0, 100, end - 8, end - 7, end - 5, end - 4, end - 1, end, 0xfffffffc} {
		add("pair", p)
		add("four", p, 200)
		add("four", 200, p)
		add("store", p, 300)
		add("div", p, 3)
		add("div", p, 0)
	}
	for _, c := range calls {
		want := run(wazy.NewRuntimeConfigInterpreter(), c.name, c.params...)
		got := run(wazy.NewRuntimeConfigCompiler(), c.name, c.params...)
		require.Equal(t, want, got, "%s%v", c.name, c.params)
	}
}

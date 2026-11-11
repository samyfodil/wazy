package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/inline_leaf.wasm
var inlineLeafWasm []byte

// TestInlineLeaf runs calls the compiler lowers as the callee's body against the interpreter, which
// never inlines: results, memory effects, and the backtrace of a trap inside the inlined body or
// after it must all be the same.
func TestInlineLeaf(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	type outcome struct {
		results []uint64
		mem     []byte
		err     string
	}
	run := func(config wazy.RuntimeConfig, name string, params ...uint64) outcome {
		r := wazy.NewRuntimeWithConfig(ctx, config)
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, inlineLeafWasm)
		require.NoError(t, err)
		require.True(t, mod.Memory().WriteUint32Le(4, 0xdeadbeef))
		var o outcome
		o.results, err = mod.ExportedFunction(name).Call(ctx, params...)
		if err != nil {
			o.err = err.Error()
		}
		o.mem, _ = mod.Memory().Read(0, 16)
		o.mem = append([]byte(nil), o.mem...)
		return o
	}
	for _, tc := range []struct {
		name   string
		params []uint64
	}{
		{name: "load_via_leaf", params: []uint64{0}},
		{name: "load_via_leaf", params: []uint64{65532}},
		{name: "mix_via_leaf", params: []uint64{0, 3}},
		{name: "mix_via_leaf", params: []uint64{8, 0}},
		{name: "mix_via_leaf", params: []uint64{65530, 3}},
		{name: "trap_after_leaf", params: []uint64{0}},
		{name: "trap_after_leaf", params: []uint64{65534}},
		{name: "load_via_far_leaf", params: []uint64{8}},
		{name: "load_via_far_leaf", params: []uint64{65533}},
	} {
		want := run(wazy.NewRuntimeConfigInterpreter(), tc.name, tc.params...)
		got := run(wazy.NewRuntimeConfigCompiler(), tc.name, tc.params...)
		require.Equal(t, want, got, "%s%v", tc.name, tc.params)
	}
}

package wazy_test

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// Real producer output, embedded rather than read from disk because some CI
// targets run the test binary from the repo root rather than the package dir.
var (
	//go:embed cmd/wazy/testdata/cat/cat-tinygo.wasm
	catTinyGoWasm []byte
	//go:embed examples/allocation/rust/testdata/greet.wasm
	greetRustWasm []byte
	//go:embed imports/wasi_snapshot_preview1/testdata/zig-cc/wasi.wasm
	wasiZigCCWasm []byte
)

// TestCloseOnContextDoneCompilesRealModules compiles real producer output with
// WithCloseOnContextDone on.
//
// The hand-written fixtures the rest of the suite uses are too small to build
// the control-flow shape this needs: a loop whose `end` finds nothing branching
// out of it takes the fall-through path in OpcodeEnd, where lowering continues
// in the block it is already in rather than in the frame's continuation.
// Emitting the module-closed slow path there without restoring the builder's
// current block sent the remainder of the enclosing frame into a block that
// already had a terminator, and the block-layout pass then dereferenced a nil
// instruction. Nothing else in the suite reaches that shape.
func TestCloseOnContextDoneCompilesRealModules(t *testing.T) {
	requireCompiler(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		bin  []byte
	}{
		{"tinygo cat", catTinyGoWasm},
		{"rust greet", greetRustWasm},
		{"zig-cc wasi", wasiZigCCWasm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithCloseOnContextDone(true))
			defer r.Close(ctx)
			_, err := r.CompileModule(ctx, tc.bin)
			require.NoError(t, err)
		})
	}
}

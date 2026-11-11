package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
)

// testdata/async/start_context.wat: a core start function using context
// storage, as wasi-sdk's WASI 0.3 libc does for its stack pointer.
//
//go:embed testdata/async/start_context.wasm
var startContextWasm []byte

// A core start function runs in a synchronous task of its own, with context
// storage of its own: context.get and context.set work in it, and a later
// call's task starts from zero (#126).
func TestStartFunctionHasTask(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	in, err := Instantiate(ctx, r, startContextWasm)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close(ctx)
	for export, want := range map[string]uint32{"f": 42, "g": 0} {
		res, err := in.Call(ctx, export)
		if err != nil {
			t.Fatal(err)
		}
		if res[0] != want {
			t.Errorf("%s = %v, want %d", export, res[0], want)
		}
	}
	if in.activeTask != nil || in.syncBase != nil {
		t.Error("the start function's task outlives it")
	}
}

package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
)

//go:embed testdata/async/lift_async_callback.wasm
var liftAsyncCallbackWasm []byte

// TestAsyncCallbackLiftBinds pins the Phase 1b boundary that superseded the
// old Phase 0 guard here: a component whose export is an async lift WITH a
// callback option now binds successfully (routed to invokeAsyncCallback --
// see bindFuncExportGraph), rather than failing loud. This fixture (unlike
// testdata/async/first_light.wasm, TestFirstLightAsync's zero-param
// fixture) declares a 1-param callback lift whose core funcs are all
// `unreachable`, so it exists to pin BINDING succeeding independent of
// whether calling it would trap -- see TestAsyncLiftFailsLoud's git history
// for the Phase 0 shape this replaces. Fixture is real wasm-tools 1.253
// output (see binary/testdata/async/lift_async_callback.wat).
func TestAsyncCallbackLiftBinds(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, liftAsyncCallbackWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)
}

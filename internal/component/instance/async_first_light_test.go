package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
)

//go:embed testdata/async/first_light.wasm
var firstLightAsyncWasm []byte

//go:embed testdata/async/yield_then_return.wasm
var yieldThenReturnAsyncWasm []byte

//go:embed testdata/async/exit_without_return.wasm
var exitWithoutReturnAsyncWasm []byte

//go:embed testdata/async/wait_unsupported.wasm
var waitUnsupportedAsyncWasm []byte

//go:embed testdata/async/invalid_callback_code.wasm
var invalidCallbackCodeAsyncWasm []byte

//go:embed testdata/async/stackful_no_callback.wasm
var stackfulNoCallbackAsyncWasm []byte

// TestFirstLightAsync is Phase 1b's milestone: a callback-async export that
// resolves without ever WAITing runs end to end. first_light.wasm's
// "run-async" export is `async func(result u32)`; its core "run" calls
// task.return(42) then returns EXIT(0) on the very first callback-lift
// call -- no WAIT, no waitable sets, no host imports, no scheduler drive are
// exercised (those are Phase 1c). See
// internal/component/instance/testdata/async/first_light.wat for the source
// and docs/component-model-async-runtime-design.md §1.3 for
// invokeAsyncCallback, the driver this test exercises.
func TestFirstLightAsync(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, firstLightAsyncWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "run-async")
	if err != nil {
		t.Fatalf("Call(run-async): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Call(run-async): got %d result(s), want 1: %v", len(results), results)
	}
	got, ok := results[0].(uint32)
	if !ok {
		t.Fatalf("Call(run-async): result is %T, want uint32", results[0])
	}
	if got != 42 {
		t.Fatalf("Call(run-async): got %d, want 42", got)
	}
}

// TestYieldThenReturnAsync exercises the callback loop's YIELD branch: "run"
// returns YIELD (code=1) on its first call, invokeAsyncCallback pumps the
// (empty) scheduler and re-invokes the callback with EventCode.NONE, which
// calls task.return(7) and then returns EXIT. This is the one first-light
// doesn't reach (it exits on the very first call): the run/callback
// round-trip and pumpSnapshot's zero-thunk case.
func TestYieldThenReturnAsync(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, yieldThenReturnAsyncWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "run-async")
	if err != nil {
		t.Fatalf("Call(run-async): %v", err)
	}
	if len(results) != 1 || results[0].(uint32) != 7 {
		t.Fatalf("Call(run-async) = %v, want [7]", results)
	}
}

// TestExitWithoutReturnAsync pins unregister_thread's trap_if(state !=
// RESOLVED): "run" returns EXIT on its very first call without ever calling
// task.return.
func TestExitWithoutReturnAsync(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, exitWithoutReturnAsyncWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run-async")
	requireErrContains(t, err, "callback returned EXIT before task.return resolved")
}

// TestWaitUnsupportedAsync pins canon_lift's WAIT branch trap_if(not
// isinstance(wset, WaitableSet)): "run" returns WAIT (code=2, si=2) on its
// first call, but si=2 never names a live waitable set (nothing in this
// fixture ever creates one) -- the WAIT driver (invokeAsyncCallback's
// callbackWait arm, docs/component-model-async-runtime-design.md §1.3) must
// reject that with a clear kind-mismatch trap, not attempt to drive a
// scheduler against a bogus handle. (Named for the pre-Phase-1c era when
// WAIT itself was unimplemented; kept as-is since the fixture and its
// assertion are still the same shape -- TestAwaitAsyncImport, in
// async_await_import_test.go, exercises the REAL WAIT path end to end,
// against a genuine waitable set.)
func TestWaitUnsupportedAsync(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, waitUnsupportedAsyncWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run-async")
	requireErrContains(t, err, "handle 2 is not a waitable set")
}

// TestInvalidCallbackCodeAsync pins unpack_callback_result's
// trap_if(code > MAX): "run" returns a packed value whose low nibble (3) is
// not one of EXIT/YIELD/WAIT.
func TestInvalidCallbackCodeAsync(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, invalidCallbackCodeAsyncWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run-async")
	requireErrContains(t, err, "invalid callback code")
}

// TestStackfulAsyncNoCallbackFailsLoud pins the OTHER half of Phase 1b's
// bind-time routing: an async lift with NO callback option (stackful async,
// which suspends mid-frame via a fiber/continuation) is out of scope for
// this milestone and must keep failing loud at bind time -- see
// bindFuncExportGraph. TestBindFuncExportGraph_AsyncNoCallback covers the
// same trap against a hand-built binary.Component; this is the real
// wasm-tools-encoded end-to-end confirmation.
func TestStackfulAsyncNoCallbackFailsLoud(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	_, err := Instantiate(ctx, r, stackfulNoCallbackAsyncWasm)
	requireErrContains(t, err, "stackful async lift (no callback) is not yet supported")
}

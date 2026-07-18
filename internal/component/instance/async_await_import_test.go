package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// await_import.wasm's "run-async" export calls an async-lowered import
// ("get", a top-level func import -- see testdata/async/await_import.wat),
// gets back [STARTED|subtaski<<4] (the host defers its Resolve), does
// waitable-set.new + waitable.join, and returns WAIT(wset); its callback,
// invoked once the deferred Resolve installs the SUBTASK pending event,
// reads the result the host wrote into memory, calls task.return, and
// returns EXIT. This is THE Phase 1 MVP: an async export that AWAITS an
// async host import runs end to end -- docs/component-model-async-runtime
// -design.md §2.5's "Deferred" trace.
//
//go:embed testdata/async/await_import.wasm
var awaitImportAsyncWasm []byte

// await_import_immediate.wasm's "run-async" is the same shape, but its host
// import Resolves SYNCHRONOUSLY (before the AsyncHostFunc returns): the
// wrapper takes the reference's immediate fast path (packed RETURNED,
// subtaski 0, no table entry), so "run" reads the already-written result and
// calls task.return + EXIT on the very first call, with no WAIT and no
// waitable set at all -- docs/component-model-async-runtime-design.md
// §2.5's "Immediate" trace.
//
//go:embed testdata/async/await_import_immediate.wasm
var awaitImportImmediateAsyncWasm []byte

// getImportParamsResults is the (params, results) WIT signature shared by
// both fixtures' "get" import: () -> u32.
func getImportParamsResults() (params, results []binary.TypeDesc) {
	return nil, []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}}
}

// TestAwaitAsyncImport is the Phase 1 MVP acceptance test: an async export
// that AWAITS an async host import whose result arrives via a Deferred
// (later-scheduled) Resolve runs to completion and returns the host's value.
func TestAwaitAsyncImport(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	params, results := getImportParamsResults()
	opt := WithAsyncImport("get", "", func(_ context.Context, args []abi.Value, call *AsyncCall) error {
		if len(args) != 0 {
			t.Errorf("host import %q: got %d arg(s), want 0", "get", len(args))
		}
		call.Defer(func() { call.Resolve([]abi.Value{uint32(99)}) })
		return nil
	}, params, results)

	inst, err := Instantiate(ctx, r, awaitImportAsyncWasm, opt)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	res, err := inst.Call(ctx, "run-async")
	if err != nil {
		t.Fatalf("Call(run-async): %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("Call(run-async): got %d result(s), want 1: %v", len(res), res)
	}
	got, ok := res[0].(uint32)
	if !ok {
		t.Fatalf("Call(run-async): result is %T, want uint32", res[0])
	}
	if got != 99 {
		t.Fatalf("Call(run-async) = %d, want 99", got)
	}
}

// TestAwaitAsyncImportImmediate is the immediate-resolve variant: the host
// import Resolves before returning, so the wrapper takes the RETURNED fast
// path (no table entry, no WAIT, no scheduler drive) and the guest's async
// export still resolves correctly on the first callback-loop iteration.
func TestAwaitAsyncImportImmediate(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	params, results := getImportParamsResults()
	opt := WithAsyncImport("get", "", func(_ context.Context, args []abi.Value, call *AsyncCall) error {
		if len(args) != 0 {
			t.Errorf("host import %q: got %d arg(s), want 0", "get", len(args))
		}
		call.Resolve([]abi.Value{uint32(7)})
		return nil
	}, params, results)

	inst, err := Instantiate(ctx, r, awaitImportImmediateAsyncWasm, opt)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	res, err := inst.Call(ctx, "run-async")
	if err != nil {
		t.Fatalf("Call(run-async): %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("Call(run-async): got %d result(s), want 1: %v", len(res), res)
	}
	got, ok := res[0].(uint32)
	if !ok {
		t.Fatalf("Call(run-async): result is %T, want uint32", res[0])
	}
	if got != 7 {
		t.Fatalf("Call(run-async) = %d, want 7", got)
	}
}

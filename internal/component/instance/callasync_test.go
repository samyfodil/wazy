package instance

import (
	"context"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// TestCallAsyncExternalCompletion is the CallAsync acceptance test: an async
// export awaits an async host import that completes from a DIFFERENT goroutine,
// after the import call returned -- the case a blocking Call cannot express (it
// would deadlock, "an import resolved externally requires CallAsync"). CallAsync
// returns a pending handle; Await drives it to completion once the external
// goroutine calls Resolve. Reuses the Phase-1 await_import fixture (run-async
// awaits the "get" () -> u32 import).
func TestCallAsyncExternalCompletion(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	params, results := getImportParamsResults()
	release := make(chan struct{})
	opt := WithAsyncImport("get", "", func(_ context.Context, args []abi.Value, call *AsyncCall) error {
		// Complete on another goroutine, after this returns -- truly external.
		go func() {
			<-release
			call.Resolve([]abi.Value{uint32(42)})
		}()
		return nil
	}, params, results)

	inst, err := Instantiate(ctx, r, awaitImportAsyncWasm, opt)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	p, err := inst.CallAsync(ctx, "run-async")
	if err != nil {
		t.Fatalf("CallAsync: %v", err)
	}

	// The call must be parked -- not resolved -- until the external import fires.
	select {
	case <-p.Done():
		t.Fatal("call resolved before the external import completed")
	case <-time.After(20 * time.Millisecond):
	}

	close(release) // let the external goroutine Resolve(42)

	res, err := p.Await(ctx)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("Await: got %d result(s), want 1: %v", len(res), res)
	}
	got, ok := res[0].(uint32)
	if !ok {
		t.Fatalf("Await: result is %T, want uint32", res[0])
	}
	if got != 42 {
		t.Fatalf("Await = %d, want 42", got)
	}
}

// TestCallAsyncImmediate: an async export whose import Resolves synchronously
// (no external goroutine) still completes through CallAsync -- CallAsync's
// initial pump drains the queued completion and returns a finished handle.
func TestCallAsyncImmediate(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	params, results := getImportParamsResults()
	opt := WithAsyncImport("get", "", func(_ context.Context, args []abi.Value, call *AsyncCall) error {
		call.Resolve([]abi.Value{uint32(7)})
		return nil
	}, params, results)

	inst, err := Instantiate(ctx, r, awaitImportImmediateAsyncWasm, opt)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	p, err := inst.CallAsync(ctx, "run-async")
	if err != nil {
		t.Fatalf("CallAsync: %v", err)
	}
	res, err := p.Await(ctx)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if len(res) != 1 || res[0].(uint32) != 7 {
		t.Fatalf("Await = %v, want [7]", res)
	}
}

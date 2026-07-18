package instance

import (
	"context"
	"path"
	"runtime"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// deadlockGoroutineFixture loads deadlock.0.wasm (testdata/wast-async/
// deadlock): a two-component fixture whose $C.f (a callback lift) waits
// forever on a permanently-empty waitable set -- there is no unblocker at
// all in this fixture, by design (deadlock.wast's own doc), so a stackful
// task that reaches $D.f's inline waitable-set.wait parks and stays parked
// until something aborts it. $D.f is exactly the stackful-lift shape
// (docs/component-model-async-stackful-design.md): a sync-opts stackful
// export whose core code sync-lowers to a callback lift, then blocks
// synchronously on its OWN waitable set.
func deadlockGoroutineFixture(t *testing.T) (*Instance, *boundExport) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	wasm, err := wastAsyncFS.ReadFile(path.Join("testdata", "wast-async", "deadlock", "deadlock.0.wasm"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	inst, err := Instantiate(ctx, r, wasm, WithWASI(WASIConfig{})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	be, ok := inst.exports["f"]
	if !ok {
		t.Fatalf("fixture has no %q export", "f")
	}
	return inst, be
}

// numGoroutineStable waits (bounded) for runtime.NumGoroutine() to settle,
// then returns it -- goroutine teardown after a channel close/send is not
// synchronous with the send itself (the Go scheduler may take a moment to
// actually unschedule the exiting goroutine), so a single immediate read is
// flaky. Polls instead of sleeping a fixed amount.
func numGoroutineStable(t *testing.T) int {
	t.Helper()
	var n, prev int
	for i := 0; i < 200; i++ {
		n = runtime.NumGoroutine()
		if i > 0 && n == prev {
			return n
		}
		prev = n
		time.Sleep(2 * time.Millisecond)
	}
	return n
}

// TestStackfulReap_CloseReapsParkedGoroutine is the design's §8 acceptance
// test: a stackful task started but never driven to completion (the
// guest<->guest delegation shape -- startStackfulExportTask returns as soon
// as the callee parks, per its doc, without ever driving the shared
// scheduler itself) leaves a real parked goroutine behind; Instance.Close
// MUST reap it -- runtime.NumGoroutine() must return to its pre-call
// baseline, not just "the call returned no error".
func TestStackfulReap_CloseReapsParkedGoroutine(t *testing.T) {
	inst, be := deadlockGoroutineFixture(t)
	home := be.home // boundExport.home's doc: $D, the export's real async-state owner
	if home == nil {
		home = inst
	}

	before := numGoroutineStable(t)

	onStart := func(*task) ([]abi.Value, error) { return nil, nil }
	onResolve := func([]abi.Value, bool) error { return nil }
	calleeTask, err := home.startStackfulExportTask(context.Background(), be, "f", onStart, onResolve)
	if err != nil {
		t.Fatalf("startStackfulExportTask: %v", err)
	}
	if calleeTask == nil {
		t.Fatal("startStackfulExportTask: calleeTask is nil")
	}

	during := numGoroutineStable(t)
	if during <= before {
		t.Fatalf("goroutine count = %d after starting a task that should have parked mid-wait, want > baseline %d", during, before)
	}

	if err := inst.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after := numGoroutineStable(t)
	if after > before {
		t.Fatalf("goroutine count after Close = %d, want <= pre-call baseline %d (parked stackful goroutine leaked)", after, before)
	}
}

// TestStackfulReap_InvokeStackfulReapsOnDeadlockTrap proves the OTHER reap
// site (§8 point 1, invokeStackful's own error path): a host-entry call to
// a stackful export that genuinely deadlocks traps with the exact spec text
// AND leaves no goroutine behind, even without Close ever being called
// (Close is exercised separately below via t.Cleanup, proving idempotency:
// a second reap call after everything is already gone is a no-op, not a
// hang).
func TestStackfulReap_InvokeStackfulReapsOnDeadlockTrap(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	wasm, err := wastAsyncFS.ReadFile(path.Join("testdata", "wast-async", "deadlock", "deadlock.0.wasm"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	inst, err := Instantiate(ctx, r, wasm, WithWASI(WASIConfig{})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	before := numGoroutineStable(t)
	_, callErr := inst.Call(ctx, "f")
	requireErrContains(t, callErr, "wasm trap: deadlock detected: event loop cannot make further progress")

	after := numGoroutineStable(t)
	if after > before {
		t.Fatalf("goroutine count after a deadlock-trapping call = %d, want <= pre-call baseline %d (invokeStackful's own reap did not clean up)", after, before)
	}

	// A second Close (this test's own defer, on top of Instantiate's t
	// already having implicitly reaped everything via the trap) must not
	// hang -- reapStackful's loop terminates the instant sched.parked has
	// no more *stackfulTask entries.
	done := make(chan struct{})
	go func() {
		inst.Close(ctx) //nolint:errcheck // exercising idempotency, not error handling
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second Close hung -- reapStackful is not idempotent")
	}
}

// TestStackfulTask_CancelAtSparkEntry pins §7's stackful taskInitial arm: a
// stackful task parked at sparkEntry (backpressure -- no goroutine spawned
// yet) that is cancelled resolves via cancelUnentered without ever running
// its core code, exactly mirroring guestTask's existing parkEntry+cancelWake
// landing. Built directly against deadlock.0.wasm's $D.f (needs_exclusive,
// sync-opts stackful): starting it TWICE against the same Instance forces
// the second start to hit backpressure (the first is parked mid-wait,
// holding the instance's exclusive) -- see task.go's hasBackpressure/
// tryEnter.
func TestStackfulTask_CancelAtSparkEntry(t *testing.T) {
	inst, be := deadlockGoroutineFixture(t)
	home := be.home
	if home == nil {
		home = inst
	}
	defer inst.Close(context.Background())

	onStart := func(*task) ([]abi.Value, error) { return nil, nil }
	onResolve := func([]abi.Value, bool) error { return nil }

	// First call: enters immediately, parks mid-wait (sparkBlock), holding
	// the instance's exclusive (needs_exclusive() for a sync-opts stackful
	// task -- task.go's enterRun/suspendRun doc).
	if _, err := home.startStackfulExportTask(context.Background(), be, "f", onStart, onResolve); err != nil {
		t.Fatalf("first startStackfulExportTask: %v", err)
	}

	// Second call: instance is busy (exclusiveHeld, no other task waiting),
	// so it must park at sparkEntry rather than spawn a goroutine.
	var secondCancelled bool
	onStart2 := func(*task) ([]abi.Value, error) { return nil, nil }
	onResolve2 := func(vals []abi.Value, cancelled bool) error {
		secondCancelled = cancelled
		return nil
	}
	calleeTask2, err := home.startStackfulExportTask(context.Background(), be, "f", onStart2, onResolve2)
	if err != nil {
		t.Fatalf("second startStackfulExportTask: %v", err)
	}

	// Find the second task directly to assert its park state before
	// cancelling (defensive: proves the test actually built the sparkEntry
	// situation it claims to, not just "cancel happened to work").
	var second *stackfulTask
	for _, p := range home.sched.parked {
		if st, ok := p.(*stackfulTask); ok && st.park == sparkEntry {
			second = st
		}
	}
	if second == nil {
		t.Fatal("BUG in test setup: no stackfulTask parked at sparkEntry")
	}

	if err := calleeTask2.requestCancellation(); err != nil {
		t.Fatalf("requestCancellation: %v", err)
	}
	if !secondCancelled {
		t.Fatal("second task's onResolve was not called with cancelled=true")
	}
	if !second.done {
		t.Fatal("cancelled sparkEntry task should be done")
	}
	if home.sched.isParked(second) {
		t.Fatal("cancelled sparkEntry task is still in sched.parked")
	}
}

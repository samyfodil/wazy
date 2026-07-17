package instance

import (
	"bytes"
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file is the Phase 3 acceptance suite for guest<->guest async-lowered
// calls (docs/component-model-async-phase3-design.md §3): a real
// component's async export A async-lowers a real sibling export B, wired
// directly at the hostImport level (bypassing full multi-component
// composition-binary authoring -- constructing the actual .wasm for a
// 2-component composed tree with async canon options across the boundary
// is a substantial wasm-tools-fixture-engineering effort; this instead
// drives the exact SAME production code path -- computeCanonHostFunc's
// lower case with hi.asyncTarget set, buildAsyncHostWrapper's callee arm,
// startAsyncExportTask -- the way instantiateNestedInstances wires it, just
// constructing the two Instances and their shared cfg by hand). Both A and
// B are real, wasm-tools-validated .wasm (the existing Phase 1 async
// fixtures, plus cancel_ack.wasm for the cancel trace), run through the
// real graph engine (instantiateGraph) and the real callback-loop
// interpreter (guestTask) end to end.

func decodeAsync(t *testing.T, wasm []byte) *binary.Component {
	t.Helper()
	comp, err := binary.Decode(bytes.NewReader(wasm))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return comp
}

// TestGuestGuestAsyncLower_ImmediateFastPath is the "immediate-EXIT fast
// path (no table entry)" acceptance shape (§3.3/§7): A (await_import_
// immediate.wasm) async-lowers B (first_light.wasm) through a guestAsyncTarget
// instead of a Go AsyncHostFunc; B resolves synchronously (task.return then
// EXIT, never parking), so A's wrapper takes the immediate path and A's own
// "run" proceeds without ever touching a waitable set.
func TestGuestGuestAsyncLower_ImmediateFastPath(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	bComp := decodeAsync(t, firstLightAsyncWasm)
	bInst, err := instantiateGraph(ctx, r, bComp, firstLightAsyncWasm, newConfig(nil))
	if err != nil {
		t.Fatalf("instantiate B (first_light): %v", err)
	}
	defer bInst.Close(ctx)
	bBE, ok := bInst.exports["run-async"]
	if !ok {
		t.Fatal(`B exports["run-async"] missing`)
	}

	aComp := decodeAsync(t, awaitImportImmediateAsyncWasm)
	aCfg := newConfig(nil)
	aCfg.sharedSched = bInst.sched // composition-tree invariant (Instance.sched's doc)
	aCfg.imports[mkImportKey("get", "")] = &hostImport{
		asyncTarget: &guestAsyncTarget{sub: bInst, be: bBE, exportName: "run-async"},
		results:     []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}},
	}
	aInst, err := instantiateGraph(ctx, r, aComp, awaitImportImmediateAsyncWasm, aCfg)
	if err != nil {
		t.Fatalf("instantiate A (await_import_immediate): %v", err)
	}
	defer aInst.Close(ctx)

	res, err := aInst.Call(ctx, "run-async")
	if err != nil {
		t.Fatalf("Call(run-async): %v", err)
	}
	if len(res) != 1 || res[0].(uint32) != 42 {
		t.Fatalf("Call(run-async) = %v, want [42] (first_light.wat hardcodes task.return(42))", res)
	}
}

// TestGuestGuestAsyncLower_ParkThenCrossInstanceResume is the flagship
// "A->B happy path" trace (§3.3): both A and B are await_import.wasm (WAIT-
// parking shape); B's own "get" import goes to a Go AsyncHostFunc that
// Defers, so B parks at WAIT waiting on IT; A async-lowers B's "run-async"
// through a guestAsyncTarget, so A ALSO parks at WAIT waiting on B. Both
// guestTasks are parked simultaneously on the ONE shared *sched -- proving
// cross-instance resumption (Instance.sched's doc, forced change #1):
// resolving the innermost Go import wakes B (a scheduler parked-task scan,
// not anything A drives directly), and B's own EXIT then wakes A the same
// way, all inside A's single top-level Call's sched.drive.
func TestGuestGuestAsyncLower_ParkThenCrossInstanceResume(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	bComp := decodeAsync(t, awaitImportAsyncWasm)
	bCfg := newConfig(nil)
	bCfg.imports[mkImportKey("get", "")] = &hostImport{
		asyncFn: func(_ context.Context, args []abi.Value, call *AsyncCall) error {
			if len(args) != 0 {
				t.Errorf("innermost host import: got %d arg(s), want 0", len(args))
			}
			call.Defer(func() { call.Resolve([]abi.Value{uint32(99)}) })
			return nil
		},
		results: []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}},
	}
	bInst, err := instantiateGraph(ctx, r, bComp, awaitImportAsyncWasm, bCfg)
	if err != nil {
		t.Fatalf("instantiate B (await_import, innermost): %v", err)
	}
	defer bInst.Close(ctx)
	bBE, ok := bInst.exports["run-async"]
	if !ok {
		t.Fatal(`B exports["run-async"] missing`)
	}

	aComp := decodeAsync(t, awaitImportAsyncWasm)
	aCfg := newConfig(nil)
	aCfg.sharedSched = bInst.sched
	aCfg.imports[mkImportKey("get", "")] = &hostImport{
		asyncTarget: &guestAsyncTarget{sub: bInst, be: bBE, exportName: "run-async"},
		results:     []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}},
	}
	aInst, err := instantiateGraph(ctx, r, aComp, awaitImportAsyncWasm, aCfg)
	if err != nil {
		t.Fatalf("instantiate A (await_import, outer): %v", err)
	}
	defer aInst.Close(ctx)

	if bInst.sched != aInst.sched {
		t.Fatal("precondition: A and B must share one *sched")
	}

	res, err := aInst.Call(ctx, "run-async")
	if err != nil {
		t.Fatalf("Call(run-async): %v", err)
	}
	if len(res) != 1 || res[0].(uint32) != 99 {
		t.Fatalf("Call(run-async) = %v, want [99]", res)
	}
	if len(aInst.sched.parked) != 0 {
		t.Fatalf("sched.parked non-empty after completion: %d entries", len(aInst.sched.parked))
	}
}

// TestGuestGuestAsyncLower_CancelAcksViaTaskCancel is the flagship CANCEL
// trace (§2.5/§3.3): A calls the REAL subtask.cancel builtin
// (subtaskCancelHostFuncGraph, sync form) on the subtask representing its
// call into B (cancel_ack.wasm); B is parked at WAIT (cancellable) on its
// OWN never-resolving import, so requestCancellation's STARTED branch
// resumes it INLINE with TASK_CANCELLED; B's callback acks via `canon
// task.cancel`; the ack flows back through startAsyncExportTask's onResolve
// (mapResultsFromProvider is a no-op for a cancelled resolve) and
// installSubtaskEvent, and subtask.cancel's own getPendingEvent delivery
// returns CANCELLED_BEFORE_RETURNED to A -- all synchronously, no scheduler
// drive needed (the sync form's `if !st.resolved()` block is skipped
// because the inline resume already resolved it).
func TestGuestGuestAsyncLower_CancelAcksViaTaskCancel(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	bComp := decodeAsync(t, cancelAckWasm)
	bCfg := newConfig(nil)
	bCfg.imports[mkImportKey("get", "")] = &hostImport{
		asyncFn: func(context.Context, []abi.Value, *AsyncCall) error {
			return nil // never resolves: B genuinely parks at WAIT, cancellable
		},
		results: []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}},
	}
	bInst, err := instantiateGraph(ctx, r, bComp, cancelAckWasm, bCfg)
	if err != nil {
		t.Fatalf("instantiate B (cancel_ack): %v", err)
	}
	defer bInst.Close(ctx)
	bBE, ok := bInst.exports["run-async"]
	if !ok {
		t.Fatal(`B exports["run-async"] missing`)
	}

	// A-side: build the callee-arm wrapper directly (the same func
	// computeCanonHostFunc's lower case would build for a REAL A component
	// lowering "get" with the async option -- see buildAsyncHostWrapper's
	// hi.asyncTarget branch) so the test can drive it with a hand-built
	// core stack and inspect the resulting subtask by handle, exactly like
	// a real A's canon-lowered "get" call and subsequent subtask.cancel
	// canon would.
	aResources := newHandleTable()
	aInst := &Instance{sched: bInst.sched, resources: aResources, mayEnter: true, mayLeave: true}
	hi := &hostImport{
		asyncTarget: &guestAsyncTarget{sub: bInst, be: bBE, exportName: "run-async"},
		results:     []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}},
	}
	fn, _, _, err := buildAsyncHostWrapper(aInst, "test", "get", hi, aResources, nil, nil)
	if err != nil {
		t.Fatalf("buildAsyncHostWrapper: %v", err)
	}

	// (retptr) -- the sole core param for a 0-arg/1-result async lower.
	stack := []uint64{0}
	fn.Call(ctx, nil, stack)
	packed := uint32(stack[0])
	state, subtaskHandle := subtaskState(packed&0xf), packed>>4
	if state != subtaskStarted || subtaskHandle == 0 {
		t.Fatalf("packed = %#x (state=%v handle=%d), want (STARTED, nonzero) -- B should have parked", packed, state, subtaskHandle)
	}

	// A's guest code would now do waitable-set.new + waitable.join + return
	// WAIT; instead, call the REAL subtask.cancel canon directly against
	// the subtask handle -- exactly what a cancel-aware A's core code would
	// do.
	cancelDef := subtaskCancelHostFuncGraph(aInst, binary.Canon{Async: false})
	cancelStack := []uint64{uint64(subtaskHandle)}
	cancelDef.fn.Call(ctx, nil, cancelStack)
	gotState := subtaskState(uint32(cancelStack[0]))
	if gotState != subtaskCancelledBeforeReturned {
		t.Fatalf("subtask.cancel result = %v, want subtaskCancelledBeforeReturned", gotState)
	}

	// B's own task must have resolved (leaveRun cleared its running state);
	// its guestTask is no longer parked anywhere.
	if bInst.activeTask != nil {
		t.Fatalf("B.activeTask = %v, want nil (B's task resolved)", bInst.activeTask)
	}
	for _, p := range bInst.sched.parked {
		if p.t.inst == bInst {
			t.Fatal("B still has a parked guestTask after being cancelled and acking")
		}
	}
}

// TestGuestGuestAsyncLower_BlockedThenLaterResolves is the "BLOCKED then
// eventual SUBTASK event" trace (§2.5/§7): an ASYNC subtask.cancel on a
// callee that does not resolve synchronously in response (cancel_ack.wasm's
// callback always acks immediately, so this test uses a callee that
// ignores cancellation instead -- a plain never-resolving host import
// standing in for "B" directly, since what's under test here is
// subtask.cancel's BLOCKED return, not B's own callback logic) returns
// BLOCKED, and the subtask resolves normally later via its own eventual
// completion.
func TestGuestGuestAsyncLower_BlockedThenLaterResolves(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	aResources := newHandleTable()
	sh := &sched{}
	aInst := &Instance{sched: sh, resources: aResources, mayEnter: true, mayLeave: true}

	st := newSubtask()
	st.state = subtaskStarted
	resolved := false
	st.onCancel = func() error {
		// The callee ignores the cancellation request (spec-legal): it
		// stays unresolved for now, and resolves later via its own normal
		// completion path (a Deferred thunk here, standing in for whatever
		// eventually completes it).
		sh.enqueue(func() error {
			resolved = true
			st.resolve(subtaskReturned, nil)
			installSubtaskEvent(st, st.subtaski())
			return nil
		})
		return nil
	}
	h := aResources.addEntry(st)
	st.setSubtaski(h)

	cancelDef := subtaskCancelHostFuncGraph(aInst, binary.Canon{Async: true})
	stack := []uint64{uint64(h)}
	cancelDef.fn.Call(ctx, nil, stack)
	if stack[0] != blockedSentinel {
		t.Fatalf("subtask.cancel(async) = %#x, want BLOCKED", stack[0])
	}
	if resolved {
		t.Fatal("the deferred completion should not have run yet")
	}

	// The eventual completion arrives later (e.g. the next WAIT drive);
	// simulate that by pumping the shared scheduler's one queued thunk
	// directly.
	progressed, err := sh.step()
	if err != nil {
		t.Fatalf("sched.step: %v", err)
	}
	if !progressed || !resolved {
		t.Fatal("the deferred completion never ran")
	}
	if !st.hasPendingEvent() {
		t.Fatal("expected a pending SUBTASK event after the deferred completion")
	}
	ev := st.getPendingEvent()
	if ev.code != eventSubtask || subtaskState(ev.p2) != subtaskReturned {
		t.Fatalf("event = %+v, want (SUBTASK, _, RETURNED)", ev)
	}
}

// TestSchedPumpSnapshot_ResumesAllReadyParkedTasksOnce pins sched.go's
// Phase 3 extension to pumpSnapshot (§0): "after the runq snapshot, resume
// each currently-ready parked task once" -- two independent guestTasks
// (separate Instances, ONE shared *sched, exactly the guest<->guest
// invariant) both parked at YIELD are both ready immediately
// (!exclusiveHeld), so one pumpSnapshot call resumes both to completion,
// not just the first one sched.step's plain FIFO scan would have picked.
func TestSchedPumpSnapshot_ResumesAllReadyParkedTasksOnce(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	comp := decodeAsync(t, yieldThenReturnAsyncWasm)
	sh := &sched{}

	newYieldTask := func() *guestTask {
		cfg := newConfig(nil)
		cfg.sharedSched = sh
		inst, err := instantiateGraph(ctx, r, comp, yieldThenReturnAsyncWasm, cfg)
		if err != nil {
			t.Fatalf("instantiateGraph: %v", err)
		}
		be, ok := inst.exports["run-async"]
		if !ok {
			t.Fatal(`exports["run-async"] missing`)
		}
		tk := &task{inst: inst, be: be}
		tk.onResolve = func(vals []abi.Value, cancelled bool) { tk.result, tk.cancelled = vals, cancelled }
		gt := &guestTask{
			t: tk, in: inst, be: be, ctx: ctx, exportName: "run-async",
			onStart: func() ([]abi.Value, error) { return nil, nil },
		}
		tk.gt = gt
		if err := gt.start(); err != nil {
			t.Fatalf("gt.start: %v", err)
		}
		return gt
	}

	gtA := newYieldTask()
	gtB := newYieldTask()
	if gtA.park != parkYield || gtB.park != parkYield {
		t.Fatalf("precondition: both tasks should be parked at YIELD (A=%v B=%v)", gtA.park, gtB.park)
	}
	if len(sh.parked) != 2 {
		t.Fatalf("sched.parked = %d, want 2", len(sh.parked))
	}

	if err := sh.pumpSnapshot(); err != nil {
		t.Fatalf("pumpSnapshot: %v", err)
	}
	if !gtA.done || !gtB.done {
		t.Fatalf("pumpSnapshot did not resume both ready parked tasks (A.done=%v B.done=%v)", gtA.done, gtB.done)
	}
	if len(sh.parked) != 0 {
		t.Fatalf("sched.parked = %d after both resolved, want 0", len(sh.parked))
	}
	if len(gtA.t.result) != 1 || gtA.t.result[0].(uint32) != 7 {
		t.Fatalf("A's result = %v, want [7] (yield_then_return.wat hardcodes task.return(7))", gtA.t.result)
	}
}

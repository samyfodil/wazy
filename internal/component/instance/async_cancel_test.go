package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file is the Phase 3 named-trap inventory (docs/component-model-
// async-phase3-design.md §7): direct Go-level tests for the cancellation
// state machine (task.cancel / subtask.cancel builtins, task.go's
// requestCancellation/deliverPendingCancel) that don't need a real wasm
// callback loop to exercise. guest_guest_async_test.go covers the
// guestTask/sched machinery these builtins sit on top of with real wasm.

// cancel_ack.wasm's "run-async" export awaits its own async-lowered "get"
// import (WAIT-parks, exactly like await_import.wasm), but its callback
// branches on the delivered event code: a TASK_CANCELLED (6) event acks via
// canon task.cancel instead of task.return -- see testdata/async/
// cancel_ack.wat. Built with `wasm-tools parse cancel_ack.wat -o
// cancel_ack.wasm` (verified: `wasm-tools validate --features
// component-model` and a `wasm-tools dump` byte-for-byte check that its
// canon task.cancel entry decodes as CanonKindTaskCancel with no payload,
// matching this package's decoder).
//
//go:embed testdata/async/cancel_ack.wasm
var cancelAckWasm []byte

// ------- task.cancel builtin -------

func TestTaskCancelHostFunc_NoCancellationDeliveredTraps(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{state: taskStarted}}
	def := taskCancelHostFuncGraph(in)
	requirePanicContains(t, "no cancellation has been delivered", func() {
		def.fn.Call(context.Background(), nil, nil)
	})
}

func TestTaskCancelHostFunc_NumBorrowsTraps(t *testing.T) {
	tk := &task{state: taskCancelDelivered, numBorrows: 1}
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: tk}
	def := taskCancelHostFuncGraph(in)
	requirePanicContains(t, "borrow handles still remain", func() {
		def.fn.Call(context.Background(), nil, nil)
	})
}

func TestTaskCancelHostFunc_Success(t *testing.T) {
	var gotVals []abi.Value
	var gotCancelled bool
	tk := &task{state: taskCancelDelivered, onResolve: func(vals []abi.Value, cancelled bool) {
		gotVals, gotCancelled = vals, cancelled
	}}
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: tk}
	def := taskCancelHostFuncGraph(in)
	def.fn.Call(context.Background(), nil, nil)
	if !gotCancelled || gotVals != nil {
		t.Fatalf("onResolve(vals=%v, cancelled=%v), want (nil, true)", gotVals, gotCancelled)
	}
	if tk.state != taskResolved || !tk.cancelled {
		t.Fatalf("task.state=%v cancelled=%v, want (taskResolved, true)", tk.state, tk.cancelled)
	}
}

func TestTaskCancelHostFunc_MayLeaveFalseTraps(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: false}
	def := taskCancelHostFuncGraph(in)
	requirePanicContains(t, "may not be left", func() {
		def.fn.Call(context.Background(), nil, nil)
	})
}

// ------- task.go: deliverPendingCancel / cancelDeliverable -------

func TestDeliverPendingCancel(t *testing.T) {
	tk := &task{state: taskPendingCancel}
	if !tk.cancelDeliverable() {
		t.Fatal("cancelDeliverable() = false for a PENDING_CANCEL task")
	}
	if !tk.deliverPendingCancel(true) {
		t.Fatal("deliverPendingCancel(true) = false, want true")
	}
	if tk.state != taskCancelDelivered {
		t.Fatalf("state = %v, want taskCancelDelivered", tk.state)
	}
	// Second call: no longer PENDING_CANCEL, returns false.
	if tk.deliverPendingCancel(true) {
		t.Fatal("deliverPendingCancel: second call should return false")
	}
}

func TestDeliverPendingCancel_NotCancellable(t *testing.T) {
	tk := &task{state: taskPendingCancel}
	if tk.deliverPendingCancel(false) {
		t.Fatal("deliverPendingCancel(false) should never fire")
	}
	if tk.state != taskPendingCancel {
		t.Fatalf("state changed to %v despite cancellable=false", tk.state)
	}
}

// ------- task.go: requestCancellation -------

func TestRequestCancellation_InitialParkedEntry(t *testing.T) {
	in := &Instance{sched: &sched{}, mayEnter: true}
	tk := &task{inst: in, state: taskInitial}
	var resolvedCancelled bool
	tk.onResolve = func(_ []abi.Value, cancelled bool) { resolvedCancelled = cancelled }
	gt := &guestTask{t: tk, in: in, park: parkEntry, cancellable: true}
	tk.gt = gt
	in.numWaitingToEnter = 1
	in.sched.park(gt)

	if err := tk.requestCancellation(); err != nil {
		t.Fatalf("requestCancellation: %v", err)
	}
	if !resolvedCancelled {
		t.Fatal("onResolve was not called with cancelled=true")
	}
	if tk.state != taskResolved {
		t.Fatalf("state = %v, want taskResolved", tk.state)
	}
	if !gt.done {
		t.Fatal("gt.done = false after a parked-entry cancel")
	}
	if in.numWaitingToEnter != 0 {
		t.Fatalf("numWaitingToEnter = %d, want 0", in.numWaitingToEnter)
	}
}

// TestRequestCancellation_StartedInlineResume is a REAL end-to-end pin of
// the STARTED branch's inline resume (§2.1) -- including the bugfix this
// design implementation needed (cancelWake must be set on the STARTED arm
// too, not just INITIAL, or resumeReady's parkWait arm wrongly falls
// through to deliverPendingCancel/getPendingEvent and panics with "BUG:
// getPendingEvent with no pending event" instead of delivering
// TASK_CANCELLED). See guest_guest_async_test.go for the full guest<->guest
// composition trace this same mechanism drives; this test isolates just
// task.requestCancellation against a real parked callback task built via
// startAsyncExportTask, using the cancel_ack.wasm fixture (its callback
// acks a TASK_CANCELLED event with task.cancel).
func TestRequestCancellation_StartedInlineResume(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	// cancel_ack.wasm's own "get" import: never resolves (parks it at
	// WAIT), so the outer task is genuinely STARTED+parked when we cancel.
	getOpt := WithAsyncImport("get", "", func(context.Context, []abi.Value, *AsyncCall) error {
		return nil // AsyncCall never Resolved/Deferred: the callee just never completes on its own
	}, nil, []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}})

	b, err := Instantiate(ctx, r, cancelAckWasm, getOpt)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer b.Close(ctx)
	be := b.exports["run-async"]

	var resolvedVals []abi.Value
	var resolvedCancelled bool
	onCancel, err := b.startAsyncExportTask(ctx, be, "run-async",
		func(*task) ([]abi.Value, error) { return nil, nil },
		func(vals []abi.Value, cancelled bool) error {
			resolvedVals, resolvedCancelled = vals, cancelled
			return nil
		})
	if err != nil {
		t.Fatalf("startAsyncExportTask: %v", err)
	}
	if resolvedCancelled {
		t.Fatal("task resolved before any cancel was requested")
	}

	if err := onCancel(); err != nil {
		t.Fatalf("onCancel (requestCancellation): %v", err)
	}
	if !resolvedCancelled || resolvedVals != nil {
		t.Fatalf("onResolve(vals=%v, cancelled=%v), want (nil, true)", resolvedVals, resolvedCancelled)
	}
}

func TestRequestCancellation_StartedNotResumableParksAsPendingCancel(t *testing.T) {
	in := &Instance{sched: &sched{}, mayEnter: true, exclusiveHeld: true} // a sibling task is running
	tk := &task{inst: in, state: taskStarted}
	gt := &guestTask{t: tk, in: in, park: parkWait, cancellable: true}
	tk.gt = gt
	if err := tk.requestCancellation(); err != nil {
		t.Fatalf("requestCancellation: %v", err)
	}
	if tk.state != taskPendingCancel {
		t.Fatalf("state = %v, want taskPendingCancel", tk.state)
	}
}

func TestRequestCancellation_ResolvedPanics(t *testing.T) {
	tk := &task{state: taskResolved}
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic (BUG) requesting cancellation on a resolved task")
		}
	}()
	_ = tk.requestCancellation()
}

// ------- subtask.cancel builtin -------

// newCancelTestInstance's activeTask is a benign non-nil, non-syncImplicit
// task (t.blocker() == nil, matching the "unpromoted callback task" arm).
// subtaskCancelHostFuncGraph's SYNC (non-async) variant traps a REAL
// syncImplicit task with "cannot block a synchronous task before returning"
// (requireNotSyncImplicit, design docs/component-model-async-threads-design-
// fable.md §8.3, trap-if-block-and-sync's trap-if-sync-cancel) but leaves a
// genuinely task-less caller on the pre-existing nested-drive fallback --
// this benign task exercises that same fallback arm (kept explicit, rather
// than relying on activeTask's zero value, so the tests below read the same
// regardless of which arm is currently wired). See
// TestSubtaskCancelHostFunc_SyncTaskCannotBlock below for the actual trap.
func newCancelTestInstance() *Instance {
	return &Instance{sched: &sched{}, mayLeave: true, resources: newHandleTable(), activeTask: &task{}}
}

func TestSubtaskCancelHostFunc_UnknownHandleTraps(t *testing.T) {
	in := newCancelTestInstance()
	def := subtaskCancelHostFuncGraph(in, binary.Canon{})
	requirePanicContains(t, "is not a subtask", func() {
		def.fn.Call(context.Background(), nil, []uint64{999})
	})
}

func TestSubtaskCancelHostFunc_NonSubtaskEntryTraps(t *testing.T) {
	in := newCancelTestInstance()
	h := in.resources.addEntry(&resourceEntry{})
	def := subtaskCancelHostFuncGraph(in, binary.Canon{})
	requirePanicContains(t, "is not a subtask", func() {
		def.fn.Call(context.Background(), nil, []uint64{uint64(h)})
	})
}

func TestSubtaskCancelHostFunc_ResolveDeliveredTraps(t *testing.T) {
	in := newCancelTestInstance()
	st := newSubtask()
	st.resolve(subtaskReturned, nil)
	st.deliverResolve()
	h := in.resources.addEntry(st)
	def := subtaskCancelHostFuncGraph(in, binary.Canon{})
	requirePanicContains(t, "resolve delivered", func() {
		def.fn.Call(context.Background(), nil, []uint64{uint64(h)})
	})
}

func TestSubtaskCancelHostFunc_DoubleCancelTraps(t *testing.T) {
	in := newCancelTestInstance()
	st := newSubtask()
	st.cancellationRequested = true
	h := in.resources.addEntry(st)
	def := subtaskCancelHostFuncGraph(in, binary.Canon{})
	requirePanicContains(t, "cancellation already requested", func() {
		def.fn.Call(context.Background(), nil, []uint64{uint64(h)})
	})
}

func TestSubtaskCancelHostFunc_InWaitableSetAndNotAsyncTraps(t *testing.T) {
	in := newCancelTestInstance()
	st := newSubtask()
	st.join(&waitableSet{})
	h := in.resources.addEntry(st)
	def := subtaskCancelHostFuncGraph(in, binary.Canon{Async: false})
	requirePanicContains(t, "joined to a waitable set", func() {
		def.fn.Call(context.Background(), nil, []uint64{uint64(h)})
	})
}

func TestSubtaskCancelHostFunc_ResolvedUndeliveredFastPath(t *testing.T) {
	in := newCancelTestInstance()
	st := newSubtask()
	st.resolve(subtaskReturned, nil)
	h := in.resources.addEntry(st)
	installSubtaskEvent(st, h) // the real production shape (async_host_import.go), not a bare setPendingEvent
	def := subtaskCancelHostFuncGraph(in, binary.Canon{})
	stack := []uint64{uint64(h)}
	def.fn.Call(context.Background(), nil, stack)
	if subtaskState(uint32(stack[0])) != subtaskReturned {
		t.Fatalf("result = %d, want subtaskReturned", stack[0])
	}
	if !st.resolveDelivered() {
		t.Fatal("resolve should now be delivered")
	}
}

func TestSubtaskCancelHostFunc_AsyncBlocked(t *testing.T) {
	in := newCancelTestInstance()
	st := newSubtask()
	st.state = subtaskStarted
	onCancelCalled := false
	st.onCancel = func() error { onCancelCalled = true; return nil } // callee ignores the request, stays unresolved
	h := in.resources.addEntry(st)
	def := subtaskCancelHostFuncGraph(in, binary.Canon{Async: true})
	stack := []uint64{uint64(h)}
	def.fn.Call(context.Background(), nil, stack)
	if !onCancelCalled {
		t.Fatal("onCancel was not invoked")
	}
	if stack[0] != blockedSentinel {
		t.Fatalf("result = %#x, want BLOCKED (%#x)", stack[0], uint64(blockedSentinel))
	}
	if !st.cancellationRequested {
		t.Fatal("cancellationRequested should be set even though the callee ignored it")
	}
}

func TestSubtaskCancelHostFunc_AsyncResolvesImmediately(t *testing.T) {
	in := newCancelTestInstance()
	st := newSubtask()
	st.state = subtaskStarted
	h := in.resources.addEntry(st)
	st.setSubtaski(h)
	st.onCancel = func() error {
		// A real onCancel (host import ResolveCancelled, or the guest<->
		// guest callee arm's onResolve) always installs the pending event
		// once resolved -- see installSubtaskEvent's callers.
		st.resolve(subtaskCancelledBeforeReturned, nil)
		installSubtaskEvent(st, st.subtaski())
		return nil
	}
	def := subtaskCancelHostFuncGraph(in, binary.Canon{Async: true})
	stack := []uint64{uint64(h)}
	def.fn.Call(context.Background(), nil, stack)
	if subtaskState(uint32(stack[0])) != subtaskCancelledBeforeReturned {
		t.Fatalf("result = %d, want subtaskCancelledBeforeReturned", stack[0])
	}
}

func TestSubtaskCancelHostFunc_NilOnCancelIgnored(t *testing.T) {
	// A host import that registered no OnCancel hook (AsyncCall.OnCancel's
	// doc): the request is recorded but otherwise ignored -- spec-legal.
	in := newCancelTestInstance()
	st := newSubtask()
	st.state = subtaskStarted
	h := in.resources.addEntry(st)
	def := subtaskCancelHostFuncGraph(in, binary.Canon{Async: true})
	stack := []uint64{uint64(h)}
	def.fn.Call(context.Background(), nil, stack)
	if stack[0] != blockedSentinel {
		t.Fatalf("result = %#x, want BLOCKED", stack[0])
	}
	if !st.cancellationRequested {
		t.Fatal("cancellationRequested should still be set")
	}
}

func TestSubtaskCancelHostFunc_SyncBlocksThenResolves(t *testing.T) {
	in := newCancelTestInstance()
	st := newSubtask()
	st.state = subtaskStarted
	h := in.resources.addEntry(st)
	st.setSubtaski(h)
	st.onCancel = func() error {
		// Defer the resolution onto the scheduler -- the sync cancel's
		// drive(st.hasPendingEvent) must pump it.
		in.sched.enqueue(func() error {
			st.resolve(subtaskCancelledBeforeReturned, nil)
			installSubtaskEvent(st, st.subtaski())
			return nil
		})
		return nil
	}
	def := subtaskCancelHostFuncGraph(in, binary.Canon{Async: false})
	stack := []uint64{uint64(h)}
	def.fn.Call(context.Background(), nil, stack)
	if subtaskState(uint32(stack[0])) != subtaskCancelledBeforeReturned {
		t.Fatalf("result = %d, want subtaskCancelledBeforeReturned", stack[0])
	}
}

func TestSubtaskCancelHostFunc_SyncDeadlockTraps(t *testing.T) {
	in := newCancelTestInstance()
	st := newSubtask()
	st.state = subtaskStarted
	st.onCancel = func() error { return nil } // never resolves, nothing queued
	h := in.resources.addEntry(st)
	def := subtaskCancelHostFuncGraph(in, binary.Canon{Async: false})
	requirePanicContains(t, "deadlock", func() {
		def.fn.Call(context.Background(), nil, []uint64{uint64(h)})
	})
}

// TestSubtaskCancelHostFunc_SyncTaskCannotBlock pins the early
// requireNotSyncImplicit classification added alongside thread.go's Stage C
// (design §8.3): the SYNC (non-async) variant of subtask.cancel traps
// "cannot block a synchronous task before returning" for a REAL syncImplicit
// task (this instance's syncTaskNeeded was set by some other canon, e.g.
// binding a thread.* canon -- invokeEntered's syncImplicit task, instance.go
// ~1651) BEFORE ever resolving the handle -- matching trap-if-block-and-
// sync's trap-if-sync-cancel, whose $D binds thread.* canons and deliberately
// passes an INVALID handle (0xdeadbeef), yet still expects this exact trap,
// not a handle-validity one. A genuinely TASK-LESS caller (no such canon
// anywhere in the instance) is a DIFFERENT case -- see
// TestSubtaskCancelHostFunc_UnknownHandleTraps, which keeps the pre-existing
// nested-drive-eligible behavior via newCancelTestInstance's benign task.
func TestSubtaskCancelHostFunc_SyncTaskCannotBlock(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, resources: newHandleTable(), activeTask: &task{syncImplicit: true}}
	def := subtaskCancelHostFuncGraph(in, binary.Canon{Async: false})
	requirePanicContains(t, "cannot block a synchronous task before returning", func() {
		def.fn.Call(context.Background(), nil, []uint64{0xdeadbeef})
	})
}

func TestSubtaskCancelHostFunc_MayLeaveFalseTraps(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: false, resources: newHandleTable()}
	def := subtaskCancelHostFuncGraph(in, binary.Canon{})
	requirePanicContains(t, "may not be left", func() {
		def.fn.Call(context.Background(), nil, []uint64{0})
	})
}

// ------- waitable.join: sync-waiter trap (Phase 3 retires the Phase-1/2
// "no sync waiters" collapse) -------

func TestWaitableJoin_SyncWaiterTraps(t *testing.T) {
	in := newCancelTestInstance()
	st := newSubtask()
	st.syncWaiter = true
	h := in.resources.addEntry(st)
	set := in.resources.addEntry(&waitableSet{})
	def := waitableJoinHostFunc(in)
	requirePanicContains(t, "synchronous subtask.cancel", func() {
		def.fn.Call(context.Background(), nil, []uint64{uint64(h), uint64(set)})
	})
}

func TestWaitableJoin_NoSyncWaiterSucceeds(t *testing.T) {
	in := newCancelTestInstance()
	st := newSubtask()
	h := in.resources.addEntry(st)
	set := in.resources.addEntry(&waitableSet{})
	def := waitableJoinHostFunc(in)
	def.fn.Call(context.Background(), nil, []uint64{uint64(h), uint64(set)})
	if st.wset == nil {
		t.Fatal("waitable was not joined to the set")
	}
}

// ------- guestTask.ready(): the PENDING_CANCEL edge (§2.5) -------

// TestGuestTaskReady_ParkWaitPendingCancelEdge pins ready()'s middle
// parkWait disjunct (gt.t.cancelDeliverable()) directly: a task parked at
// WAIT with a PENDING_CANCEL state (set by requestCancellation's
// not-immediately-resumable branch, e.g. a sibling task's exclusive was
// held at request time) becomes ready once exclusiveHeld clears, without
// cancelWake ever being set and without the underlying waitable set having
// any real pending event -- the edge-reachable delivery site §2.3 lists.
func TestGuestTaskReady_ParkWaitPendingCancelEdge(t *testing.T) {
	in := &Instance{sched: &sched{}}
	tk := &task{inst: in, state: taskPendingCancel}
	ws := &waitableSet{}
	gt := &guestTask{t: tk, in: in, park: parkWait, wset: ws, cancellable: true}
	tk.gt = gt

	in.exclusiveHeld = true // a sibling task is currently running
	if gt.ready() {
		t.Fatal("ready() = true while exclusiveHeld is held by a sibling")
	}

	in.exclusiveHeld = false
	if !gt.ready() {
		t.Fatal("ready() = false, want true (cancelDeliverable via PENDING_CANCEL)")
	}
}

func TestGuestTaskReady_ParkNoneIsNeverReady(t *testing.T) {
	gt := &guestTask{t: &task{}, in: &Instance{}, park: parkNone}
	if gt.ready() {
		t.Fatal("ready() = true for parkNone")
	}
}

// ------- startAsyncExportTask: on_start error propagation -------

func TestStartAsyncExportTask_OnStartErrorPropagates(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	b, err := Instantiate(ctx, r, firstLightAsyncWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer b.Close(ctx)
	be := b.exports["run-async"]

	_, err = b.startAsyncExportTask(ctx, be, "run-async",
		func(*task) ([]abi.Value, error) { return nil, errBoom },
		func([]abi.Value, bool) error { t.Fatal("onResolve should not run"); return nil })
	// startAsyncExportTask's onStart failure returns an error from
	// gt.start() directly (not a panic -- the callback loop never even ran).
	requireErrContains(t, err, "boom")
}

func TestStartAsyncExportTask_NotReenterableTraps(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	b, err := Instantiate(ctx, r, firstLightAsyncWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer b.Close(ctx)
	be := b.exports["run-async"]
	b.mayEnter = false

	_, err = b.startAsyncExportTask(ctx, be, "run-async",
		func(*task) ([]abi.Value, error) { return nil, nil },
		func([]abi.Value, bool) error { return nil })
	requireErrContains(t, err, "cannot enter component instance")
}

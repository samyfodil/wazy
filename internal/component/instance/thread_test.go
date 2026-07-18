package instance

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// callThreadYield builds threadYieldHostFunc(in, canon) and invokes it,
// returning the i32 result (Cancelled: FALSE=0, TRUE=1).
func callThreadYield(in *Instance, canon binary.Canon) uint64 {
	def := threadYieldHostFunc(in, canon)
	stack := []uint64{0}
	def.fn.Call(context.Background(), nil, stack)
	return stack[0]
}

// TestThreadYieldHostFunc_BindSetsPromotionGate pins design §4.1/§5.4:
// binding thread.yield unconditionally sets syncTaskNeeded (so a plain sync
// lift that calls it still gets a syncImplicit task) and mayBlockSync (the
// promotion gate a bound-but-not-yet-called thread canon must widen, per
// §5.4 -- checked at BIND time, not call time, so this must hold even before
// the returned host func is ever invoked).
func TestThreadYieldHostFunc_BindSetsPromotionGate(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	if in.syncTaskNeeded || in.mayBlockSync {
		t.Fatal("BUG in test setup: flags already set before bind")
	}
	threadYieldHostFunc(in, binary.Canon{})
	if !in.syncTaskNeeded {
		t.Fatal("threadYieldHostFunc: syncTaskNeeded not set at bind time")
	}
	if !in.mayBlockSync {
		t.Fatal("threadYieldHostFunc: mayBlockSync not set at bind time")
	}
}

// TestThreadYieldHostFunc_MayLeaveFalsePanics: like every other async
// builtin, the may_leave prologue traps first, before any task/blocker
// resolution.
func TestThreadYieldHostFunc_MayLeaveFalsePanics(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: false}
	requirePanicContains(t, "may not be left", func() {
		callThreadYield(in, binary.Canon{})
	})
}

// TestThreadYieldHostFunc_TasklessSyncDoesNotTrap pins design §4.1's default
// arm / §11.2: a task-less synchronous context (t == nil -- e.g. a core
// module's instantiation-time start function) is allowed to call
// thread.yield, unlike thread.suspend/waitable-set.wait's "cannot block a
// synchronous task" trap. It runs one scheduler round and reports NOT
// cancelled (a task-less context can never observe a delivered
// cancellation).
func TestThreadYieldHostFunc_TasklessSyncDoesNotTrap(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	if got := callThreadYield(in, binary.Canon{}); got != 0 {
		t.Fatalf("thread.yield (task-less sync) = %d, want 0 (not cancelled)", got)
	}
}

// TestThreadYieldHostFunc_SyncImplicitDoesNotTrap: the syncImplicit task
// (invokeEntered's implicit task for a plain sync lift) hits the same
// yield-is-fine default arm as the task-less case -- this is exactly what
// trap-if-block-and-sync's "yield-is-fine" assertion needs (that suite
// itself stays skipped in Stage A pending thread.index/new-indirect/suspend
// binds, but the yield semantics it will exercise are pinned here directly).
func TestThreadYieldHostFunc_SyncImplicitDoesNotTrap(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{syncImplicit: true}}
	if got := callThreadYield(in, binary.Canon{}); got != 0 {
		t.Fatalf("thread.yield (syncImplicit task) = %d, want 0 (not cancelled)", got)
	}
}

// TestThreadYieldHostFunc_UnpromotedCallbackTask pins the middle arm: a
// non-syncImplicit task with no live taskBlocker (t.blocker() == nil --
// t.st and t.gt.seg both nil, i.e. a callback task's guest code on the
// driving goroutine, not currently inside a live segment). Unexercised by
// any Stage A .wast suite (design §4.1's comment: cancellable's promotion
// gate means a bound thread.yield always gets a promoted task in practice),
// but the reference never distinguishes this from the stackful case at
// Thread.yield_'s level, so it must behave the same: one scheduler round,
// not cancelled.
func TestThreadYieldHostFunc_UnpromotedCallbackTask(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{}}
	if got := callThreadYield(in, binary.Canon{}); got != 0 {
		t.Fatalf("thread.yield (unpromoted callback task) = %d, want 0 (not cancelled)", got)
	}
}

// TestThreadYieldHostFunc_UnpromotedCallbackTask_PendingCancelDelivers pins
// the middle arm's deliverPendingCancel prologue (design §4.1, mirroring
// wait_until's ~404): a PENDING_CANCEL task with cancel?=true yielding must
// flip to CANCEL_DELIVERED and report Cancelled.TRUE immediately, without
// running a scheduler round at all.
func TestThreadYieldHostFunc_UnpromotedCallbackTask_PendingCancelDelivers(t *testing.T) {
	tk := &task{state: taskPendingCancel}
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: tk}
	if got := callThreadYield(in, binary.Canon{Cancellable: true}); got != 1 {
		t.Fatalf("thread.yield (pending cancel, cancellable) = %d, want 1 (Cancelled.TRUE)", got)
	}
	if tk.state != taskCancelDelivered {
		t.Fatalf("task state = %v, want taskCancelDelivered", tk.state)
	}
}

// TestThreadYieldHostFunc_UnpromotedCallbackTask_NonCancellableIgnoresPendingCancel
// pins that a non-cancellable yield never consults cancelDeliverable/
// deliverPendingCancel -- a PENDING_CANCEL task yielding without cancel?=true
// just runs its scheduler round and returns NOT cancelled, staying
// PENDING_CANCEL (the pending cancellation is only observed at a
// cancellable suspension point).
func TestThreadYieldHostFunc_UnpromotedCallbackTask_NonCancellableIgnoresPendingCancel(t *testing.T) {
	tk := &task{state: taskPendingCancel}
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: tk}
	if got := callThreadYield(in, binary.Canon{Cancellable: false}); got != 0 {
		t.Fatalf("thread.yield (pending cancel, non-cancellable) = %d, want 0 (not cancelled)", got)
	}
	if tk.state != taskPendingCancel {
		t.Fatalf("task state = %v, want unchanged taskPendingCancel", tk.state)
	}
}

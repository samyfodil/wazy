package instance

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/abi"
)

// taskState is the Canonical ABI's Task.State (testdata/definitions.py's
// Task.State). Phase 3 makes every state reachable -- see
// docs/component-model-async-phase3-design.md §2.1/§2.5.
type taskState uint8

const (
	taskInitial taskState = iota
	taskStarted
	taskPendingCancel
	taskCancelDelivered
	taskResolved
)

// task mirrors the Canonical ABI's Task (definitions.py's Task, ~450) for
// exactly one shape: a callback lift's implicit thread, which never has a
// live suspended core frame -- every suspension is a normal core function
// return, remembered as parked guestTask state instead (see guest_task.go)
// -- so "implicit thread" and "task" collapse into one Go value, unlike the
// reference's separate Task/Thread objects
// (docs/component-model-async-runtime-design.md §1.2,
// docs/component-model-async-phase3-design.md §0).
type task struct {
	inst  *Instance
	be    *boundExport // the async export's binding; task.return validates its result against be's declared result type
	state taskState

	// Exactly one of gt/st is non-nil for a task this package creates: gt
	// for a callback lift (guest_task.go), st for a stackful lift
	// (stackful_task.go, docs/component-model-async-stackful-design.md §2).
	// The oracle's hand-built task keeps gt non-nil, so every existing
	// branch it exercises is unchanged.
	gt *guestTask
	st *stackfulTask

	// numBorrows mirrors Task.num_borrows: incremented by
	// handleTable.NewBorrowScoped for every borrow<T> handle minted, scoped
	// to this task, by a cross-instance composition call (composition.go);
	// decremented by handleTable.Drop's borrow arm. task.return/task.cancel
	// trap_if(num_borrows > 0) below enforce the exit-time invariant.
	numBorrows int

	// cancelled records whether this task resolved via a delivered
	// cancellation (task.cancel) rather than a normal task.return -- Phase
	// 3's forced onResolve signature change (§6 #2): resolving-as-cancelled
	// must be distinguishable from "returned with an empty result".
	cancelled bool

	// ctxStorage mirrors the one load-bearing field of the reference's
	// Thread.storage: wit-bindgen's generated async executor stashes its
	// state pointer here via context.set and reads it back via context.get
	// (canon kinds 0x0a/0x0b). Two slots, per the spec's context.get/set
	// slot argument (canon.Slot, bounds-checked against len(ctxStorage) by
	// the context.get/set builtins -- see async_builtins.go).
	ctxStorage [2]uint64

	// onResolve, when non-nil, receives the result lifted by the task.return
	// builtin (or nil + cancelled=true from task.cancel), so a guest->guest
	// lower or a future CallAsync can plug in without reshaping task.return
	// -- startAsyncExportTask's onResolve bridges into the caller's subtask
	// this way (async_lift.go). nil is the default sink used by
	// invokeAsyncCallback's plain host-entry call (no forwarding needed):
	// returnValues/cancelResolve write straight into result/cancelled below
	// instead, saving the forwarding closure's allocation on that hot path.
	onResolve func(vals []abi.Value, cancelled bool)
	result    []abi.Value

	// syncImplicit marks the implicit Task of a PLAIN SYNC canon lift
	// (invokeEntered): the reference's Task for a not-opts.async_ lift
	// (definitions.py:2155-2164). It has no gt/st (blocker() == nil), never
	// enters the scheduler, and is torn down when invokeEntered returns. Its
	// ONE semantic difference from a callback task between segments: it may
	// NEVER block -- blockingTask traps on it (the spec's sync-task-block
	// trap, "cannot block a synchronous task before returning") instead of
	// taking the nested-drive arm.
	syncImplicit bool
}

// taskBlocker is the mid-call suspension capability (Feature 1,
// docs/component-model-async-final3-fable.md §1.4): implemented by
// *stackfulTask (goroutine always live once started) and by a PROMOTED
// *guestTask while a segment goroutine is live (t.gt.seg != nil). Blocking
// builtins and the sync-lower delegate use it instead of testing t.st
// directly, so both suspension flavors share one call site.
type taskBlocker interface {
	block(ready func() bool, cancellable bool) (cancelled bool)
}

// blocker returns t's live mid-call suspension primitive, or nil (then the
// caller keeps the Phase 1-3 nested drive / trap behavior it already had).
func (t *task) blocker() taskBlocker {
	if t.st != nil {
		return t.st
	}
	if t.gt != nil && t.gt.seg != nil {
		return t.gt
	}
	return nil
}

// returnValues is the reference Task.return_, called by the task.return
// builtin (canon_task_return, definitions.py ~2380). Unchanged by Phase 3
// besides the onResolve signature: after CANCEL_DELIVERED, task.return
// remains legal (the guest may finish normally instead of acking a
// cancellation) -- there is no state guard here besides RESOLVED, matching
// the reference.
func (t *task) returnValues(vals []abi.Value) error {
	if t.state == taskResolved { // trap_if(state == RESOLVED)
		return fmt.Errorf("task.return: task already resolved")
	}
	if t.numBorrows > 0 { // trap_if(num_borrows > 0)
		return fmt.Errorf("task.return: borrow handles still remain at the end of the call (%d still lent to subtasks)", t.numBorrows)
	}
	if t.onResolve != nil {
		t.onResolve(vals, false)
	} else {
		t.result, t.cancelled = vals, false
	}
	t.state = taskResolved
	return nil
}

// cancelDeliverable mirrors the reference's inline check at every cancellable
// suspension point (definitions.py ~404): a PENDING_CANCEL task is ready to
// have its cancellation delivered.
func (t *task) cancelDeliverable() bool { return t.state == taskPendingCancel }

// deliverPendingCancel mirrors Task.deliver_pending_cancel (~545): if
// cancellable and a cancellation is pending, flip to CANCEL_DELIVERED and
// report true (the caller then delivers TASK_CANCELLED instead of the
// suspension's normal event).
func (t *task) deliverPendingCancel(cancellable bool) bool {
	if cancellable && t.state == taskPendingCancel {
		t.state = taskCancelDelivered
		return true
	}
	return false
}

// cancelResolve is Task.cancel (~563): the guest ACKs a delivered
// cancellation via the task.cancel builtin.
func (t *task) cancelResolve() error {
	if t.state != taskCancelDelivered {
		return fmt.Errorf("task.cancel: no cancellation has been delivered to this task")
	}
	if t.numBorrows > 0 {
		return fmt.Errorf("task.cancel: borrow handles still remain at the end of the call (%d still held)", t.numBorrows)
	}
	if t.onResolve != nil {
		t.onResolve(nil, true)
	} else {
		t.result = nil
	}
	t.cancelled = true
	t.state = taskResolved
	return nil
}

// cancelUnentered is the parkEntry+cancelWake landing (guest_task.go's
// resumeReady): request_cancellation already advanced the task to
// CANCEL_DELIVERED without ever running guest code (the task was still
// parked at entry, INITIAL). Resolve it the same way task.cancel would.
func (t *task) cancelUnentered() error { return t.cancelResolve() }

// requestCancellation transliterates Task.request_cancellation
// (definitions.py ~527-543): called by canon_subtask_cancel via
// subtask.onCancel (the only Phase-3 caller), and it is exactly the func a
// future public CallAsync's cancel handle would call.
//
// Two collapse notes (docs/component-model-async-phase3-design.md §2.1):
//
//   - The reference excludes the implicit thread from candidates when
//     another thread holds the instance's exclusive (~535-536); with exactly
//     one thread per task and exclusiveHeld false at every park, the
//     "!t.inst.exclusiveHeld" conjunct below is the whole surviving check --
//     it can only be true here if a DIFFERENT task of the same instance is
//     running, in which case the parked target isn't resumable and PENDING_
//     CANCEL is correct, exactly as the reference would take.
//   - Deviation (blocking-builtin park): a task suspended inside the
//     blocking waitable-set.wait builtin has live Go frames beneath it, so
//     it cannot be resumed synchronously from here -- its drive predicate
//     (async_builtins.go) includes cancelDeliverable(), so PENDING_CANCEL is
//     set and it wakes at the drive loop's next check. Parked CALLBACK tasks
//     (the only shape wit-bindgen/the oracle's deterministic scenarios
//     drive) resume inline here, matching the reference exactly.
//
// The st arms generalize the two above for a stackfulTask
// (docs/component-model-async-stackful-design.md §7): sparkEntry's landing
// is identical (cancelUnentered via cancelWake+resumeReady); the sparkBlock
// arm's readiness conjunct widens from "!exclusiveHeld" to "not held by a
// DIFFERENT task" (heldByOther) -- the reference's "exclusive_thread not in
// {None, implicit_thread}" (~535): a stackful task parked while holding ITS
// OWN exclusive (needs_exclusive, held across its own suspensions -- task.go
// enterRun/suspendRun's doc) is still cancellable in place; only a
// DIFFERENT task's ownership blocks the synchronous resume.
func (t *task) requestCancellation() error {
	switch t.state {
	case taskInitial: // parked at entry, on_start never ran (~529-531)
		t.state = taskCancelDelivered
		if t.st != nil {
			t.st.cancelWake = true
			return t.st.resumeReady() // -> cancelUnentered -> on_resolve(nil,true)
		}
		t.gt.cancelWake = true
		return t.gt.resumeReady() // -> cancelUnentered -> on_resolve(nil,true)
	case taskStarted:
		if t.st != nil {
			heldByOther := t.inst.exclusiveHeld && t.inst.exclusiveOwner != t
			if t.st.park == sparkBlock && t.st.cancellable && !heldByOther && t.inst.mayEnter {
				t.state = taskCancelDelivered
				t.st.cancelWake = true
				return t.st.resumeReady() // baton -> goroutine with resumeCancelled
			}
			t.state = taskPendingCancel
			return nil
		}
		if t.gt.park == parkBlocked {
			// Feature 1 (docs/component-model-async-final3-fable.md §1.6):
			// a parkBlocked resume is a BATON HANDOFF (the segment goroutine
			// is alive at <-seg.resumeCh), exactly like the t.st arm above
			// -- it must NOT be resumed the way a frame-free WAIT/YIELD is
			// (those just re-run inline via resumeReady's own dispatch).
			// Mirrors the t.st arm's heldByOther/cancellable/mayEnter gate.
			heldByOther := t.inst.exclusiveHeld && t.inst.exclusiveOwner != t
			if t.gt.cancellable && !heldByOther && t.inst.mayEnter {
				t.state = taskCancelDelivered
				t.gt.cancelWake = true
				return t.gt.resumeReady() // -> gt.seg.handoff(resumeCancelled)
			}
			t.state = taskPendingCancel
			return nil
		}
		if t.gt.park != parkNone && t.gt.cancellable && !t.inst.exclusiveHeld && t.inst.mayEnter {
			t.state = taskCancelDelivered
			// cancelWake forces an unconditional TASK_CANCELLED delivery on
			// this resume (reference: thread.resume(Cancelled.TRUE) hands
			// wait_until its return value directly, bypassing the normal
			// ready_and_has_event predicate entirely -- ~538-541); without
			// it resumeReady's parkWait/parkYield arms would fall through to
			// deliverPendingCancel, which only fires from a PENDING_CANCEL
			// state and would wrongly try to read a real (nonexistent)
			// pending event here.
			t.gt.cancelWake = true
			return t.gt.resumeReady()
		}
		t.state = taskPendingCancel // (~543)
		return nil
	default:
		panic("BUG: request_cancellation on a resolved task")
	}
}

// hasBackpressure mirrors the reference's inst.has_backpressure() (~489):
// needs_exclusive() is always true for a callback lift, so the "or
// needs_exclusive() and exclusive_thread is not None" disjunct collapses to
// just exclusiveHeld.
func (in *Instance) hasBackpressure() bool {
	return in.backpressure > 0 || in.exclusiveHeld
}

// tryEnter is the reference Task.enter_implicit_thread's non-blocking
// attempt (~485-503): returns entered=false when the task must park at
// parkEntry instead (see guestTask.start, guest_task.go). numWaitingToEnter
// mirrors the reference's FIFO fairness (~492-495): a later caller may not
// jump ahead of an earlier one already parked waiting to enter.
func (in *Instance) tryEnter(t *task) (entered bool) {
	if in.hasBackpressure() || in.numWaitingToEnter > 0 {
		return false
	}
	in.activeTask = t
	in.exclusiveHeld, in.exclusiveOwner = true, t
	return true
}

// enterRun/leaveRun bracket a task's guest code actually being on the stack
// (docs/component-model-async-phase3-design.md §1.2): mayEnter is false only
// between an enterRun and its matching leaveRun, unlike Phase 1/2's
// invokeAsyncCallback which held it false for the whole call. This is what
// lets a second entry into a busy-but-PARKED instance succeed (gated by
// exclusive/backpressure parking, not by mayEnter).
func (in *Instance) enterRun(t *task) {
	in.mayEnter = false
	in.activeTask = t
	in.exclusiveHeld, in.exclusiveOwner = true, t
}

func (in *Instance) leaveRun() {
	in.exclusiveHeld, in.exclusiveOwner = false, nil
	in.activeTask = in.syncBase // nil pre-implicit-task: identical to the old `= nil`
	in.mayEnter = true
}

// suspendRun is a STACKFUL park (docs/component-model-async-stackful-
// design.md §5): guest frames stay live on the parked goroutine, but the
// task is not RUNNING -- entry gating falls to exclusiveHeld/backpressure
// (tryEnter parks newcomers), not to a mayEnter trap. Unlike leaveRun,
// exclusiveHeld/exclusiveOwner are deliberately KEPT: a sync-opts stackful
// task needs_exclusive() and holds it across its OWN suspensions (the
// reference's wait_until never touches exclusive_thread; only the callback
// loop releases it around WAIT/YIELD).
func (in *Instance) suspendRun() {
	in.activeTask = in.syncBase // nil pre-implicit-task: identical to the old `= nil`
	in.mayEnter = true
}

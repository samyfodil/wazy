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

	// resultBuf is an inline 1-element backing array for the common
	// single-result task.return call (taskReturnHostFuncGraph,
	// async_builtins.go): a task.return with a declared result type always
	// produces exactly one abi.Value (multi-result task.return is rejected
	// at bind time -- taskReturnHostFuncGraph's "multiple task.return
	// results are not supported" error), so `t.resultBuf[0] = val;
	// result = t.resultBuf[:1]` avoids a `[]abi.Value{val}` heap allocation
	// per call. Aliasing the task's own field is safe: a task is never
	// reused/pooled and task.return traps if called twice
	// (trap_if(state==RESOLVED) in returnValues below), so nothing ever
	// overwrites resultBuf after the one call that populates it, and the
	// returned slice header keeps this task (hence resultBuf's backing
	// array) reachable for exactly as long as anything still holds it.
	resultBuf [1]abi.Value

	// syncImplicit marks the implicit Task of a PLAIN SYNC canon lift
	// (invokeEntered): the reference's Task for a not-opts.async_ lift
	// (definitions.py:2155-2164). It has no gt/st (blocker() == nil), never
	// enters the scheduler, and is torn down when invokeEntered returns. Its
	// ONE semantic difference from a callback task between segments: it may
	// NEVER block -- blockingTask traps on it (the spec's sync-task-block
	// trap, "cannot block a synchronous task before returning") instead of
	// taking the nested-drive arm.
	syncImplicit bool

	// implicitThreadIdx is this task's lazily-allocated slot in
	// in.threads (design §4.3): 0 means "never registered" (thread.index
	// was never called on this task); non-zero is the index
	// threadIndexHostFunc returned and will keep returning. See thread.go's
	// threadIndexHostFunc doc for why registration is lazy rather than
	// eager (deviation §11.4).
	implicitThreadIdx uint32

	// liveThreads counts this task's currently-registered SPAWNED
	// guestThreads (design §3/§12 Stage D: the reference's Task.threads list
	// length, definitions.py:467, :505-525) -- incremented by
	// thread.new-indirect's registration, decremented by guestThread.run's
	// unregister tail (or reapParkedGoroutines' never-spawned reap arm) --
	// used only for the reference's last-thread-unresolved trap (a spawned
	// thread's run() checks it; see thread.go's doc). Always 0 for a task
	// that never spawns a thread.
	liveThreads int

	// syncThread is a sync (non-async-typed) task's implicit thread, created on
	// first use and kept for the task's lifetime so a switch built-in can
	// publish a suspension on it before control moves (see switchWindow). Nil
	// for an async task, whose implicit thread IS its guestTask/stackfulTask.
	syncThread *syncImplicitThread
}

// implicitThreadAlive reports whether this task's implicit thread is still in
// the reference's Task.threads: register_thread runs in enter_implicit_thread
// and the entry is removed by exit_implicit_thread, so the implicit thread is a
// member for as long as the task's own body has not finished
// (definitions.py ~431-461).
//
// liveThreads counts only SPAWNED threads, so the last-thread-unresolved trap
// (unregister_thread's trap_if(state != RESOLVED), ~465-468) must consult this
// too: a spawned thread exiting while the task's own body is merely PARKED is
// not the last thread of the task. trap-if-block-and-sync's
// yield-then-resume-is-fine and during-sync-scheduling-candidates both hinge on
// it -- their spawned thread is created precisely to run and exit while the
// implicit thread waits to be resumed.
func (t *task) implicitThreadAlive() bool {
	switch {
	case t.st != nil:
		return !t.st.done
	case t.gt != nil:
		return !t.gt.done
	}
	// A sync task has neither: its implicit thread is the goroutine whose frames
	// the whole call is running on, so it is alive for the call's duration --
	// which is exactly as long as any thread it spawned can be running.
	return true
}

// taskBlocker is the mid-call suspension capability (Feature 1,
// docs/component-model-async-final3-fable.md §1.4): implemented by
// *stackfulTask (goroutine always live once started) and by a PROMOTED
// *guestTask while a segment goroutine is live (t.gt.seg != nil). Blocking
// builtins and the sync-lower delegate use it instead of testing t.st
// directly, so both suspension flavors share one call site.
type taskBlocker interface {
	block(ready func() bool) (cancelled bool)
	// window is this blocker's publish-before-park window (see switchWindow):
	// the switch built-ins open it so the thread they switch to observes the
	// switcher as already suspended, and latch a resume that arrives before the
	// park actually happens.
	window() *switchWindow
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

// cancelDeliverable mirrors the reference's inline check at every suspension
// point: a PENDING_CANCEL task is ready to have its cancellation delivered.
//
// Every suspension is one. The canonical ABI used to let a built-in opt out via
// a `cancellable` immediate; component-model#716 removed it after #707 made
// cancellation always delivered, and the reference's wait_until no longer takes
// the flag at any call site.
func (t *task) cancelDeliverable() bool { return t.state == taskPendingCancel }

// deliverPendingCancel mirrors Task.deliver_pending_cancel: if a cancellation
// is pending, flip to CANCEL_DELIVERED and report true (the caller then delivers
// TASK_CANCELLED instead of the suspension's normal event).
func (t *task) deliverPendingCancel() bool {
	if t.state == taskPendingCancel {
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
// (definitions.py ~472-485): called by canon_subtask_cancel via
// subtask.onCancel (the only Phase-3 caller), and it is exactly the func a
// future public CallAsync's cancel handle would call.
//
//	def request_cancellation(self):
//	  if self.state == Task.State.INITIAL:
//	    self.state = Task.State.PENDING_CANCEL
//	    self.implicit_thread.resume()
//	    assert(self.state == Task.State.RESOLVED)
//	  else:
//	    assert(self.state == Task.State.STARTED)
//	    self.state = Task.State.PENDING_CANCEL
//	    while self.state != Task.State.RESOLVED:
//	      candidates = { t for t in self.inst.threads if t.ready() }
//	      if candidates:
//	        random.choice(list(candidates)).resume()
//	      if not candidates or DETERMINISTIC_PROFILE or random.randint(0,1):
//	        break
//
// Two things this revision made simpler than the one wazy was written
// against, and both are asserted by suites:
//
//   - The candidate set is the whole INSTANCE's ready threads, not the
//     cancelled task's. cancel-instance-wide-resume.wast arranges for the only
//     ready thread in the callee's instance to belong to a DIFFERENT task and
//     requires it to run.
//   - There is no "resume with Cancelled.TRUE" any more. A resumed thread
//     learns of the cancellation through the ordinary delivery sites (the
//     callback loop and wait_from_callback), so readiness is what selects the
//     thread and nothing bypasses a ready_func. The only wake that still
//     carries a flag is the INITIAL one below, which resumes a thread that has
//     never run rather than one that is waiting on a predicate.
//
// DETERMINISTIC_PROFILE takes the `break` on the first iteration, so this is
// exactly one resume attempt: if no thread of the instance is ready (say the
// exclusive lock is held, or the task is blocked mid-core-call), the request
// stays PENDING_CANCEL and subtask.cancel reports BLOCKED, with delivery
// happening whenever the instance next drives a ready thread.
func (t *task) requestCancellation() error {
	switch t.state {
	case taskInitial: // parked at entry, on_start never ran (~473-476)
		// The reference resumes the implicit thread unconditionally here (it is
		// parked in enter_implicit_thread's backpressure wait, whose post-check
		// delivers the cancellation and resolves the task), so this is the one
		// resume that does not go through a readiness scan. cancelWake routes
		// resumeReady's parkEntry arm to cancelUnentered, which is that
		// post-check's `deliver_pending_cancel() -> self.cancel()`.
		t.state = taskCancelDelivered
		if t.st != nil {
			t.st.cancelWake = true
			return t.st.resumeReady()
		}
		t.gt.cancelWake = true
		return t.gt.resumeReady()
	case taskStarted:
		t.state = taskPendingCancel
		_, err := t.inst.sched.resumeOneReady(t.inst)
		return err
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

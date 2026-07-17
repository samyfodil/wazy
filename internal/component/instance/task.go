package instance

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/abi"
)

// taskState is the Canonical ABI's Task.State (testdata/definitions.py's
// Task.State), restricted to what a callback-lift task can actually reach in
// this milestone: INITIAL -> STARTED -> RESOLVED. PENDING_CANCEL and
// CANCEL_DELIVERED are reserved (Phase 3 cancellation) purely so the
// numbering already matches the reference when that lands -- nothing in this
// package ever sets a task to either value yet.
type taskState uint8

const (
	taskInitial taskState = iota
	taskStarted
	taskPendingCancel   // Phase 3; unreachable in this milestone
	taskCancelDelivered // Phase 3; unreachable in this milestone
	taskResolved
)

// task mirrors the Canonical ABI's Task (definitions.py's Task, ~450) for
// exactly one shape: a callback lift's implicit thread, which never has a
// live suspended core frame -- every suspension is a normal core function
// return (see invokeAsyncCallback) -- so "implicit thread" and "task"
// collapse into one Go value, unlike the reference's separate Task/Thread
// objects (docs/component-model-async-runtime-design.md §1.2).
type task struct {
	inst  *Instance
	be    *boundExport // the async export's binding; task.return validates its result against be's declared result type
	state taskState

	// numBorrows mirrors Task.num_borrows: always 0 until Phase 3 borrow
	// scopes exist, but task.return's trap_if(num_borrows > 0) and the
	// exit-time assert are wired against it now (see returnValues and
	// invokeAsyncCallback) so Phase 3 only has to start incrementing it.
	numBorrows int

	// ctxStorage mirrors the one load-bearing field of the reference's
	// Thread.storage: wit-bindgen's generated async executor stashes its
	// state pointer here via context.set and reads it back via context.get
	// (canon kinds 0x0a/0x0b). Two slots, per the spec's context.get/set
	// slot argument (canon.Slot, bounds-checked against len(ctxStorage) by
	// the context.get/set builtins -- see async_builtins.go).
	ctxStorage [2]uint64

	// onResolve receives the result lifted by the task.return builtin. Kept
	// as a closure (not an inlined result field) so a later guest->guest
	// lower or CallAsync can plug in without reshaping task.return --
	// invokeAsyncCallback sets this to capture into result.
	onResolve func(vals []abi.Value)
	result    []abi.Value
}

// returnValues is the reference Task.return_, called by the task.return
// builtin (canon_task_return, definitions.py ~2380).
func (t *task) returnValues(vals []abi.Value) error {
	if t.state == taskResolved { // trap_if(state == RESOLVED)
		return fmt.Errorf("task.return: task already resolved")
	}
	if t.numBorrows > 0 { // trap_if(num_borrows > 0)
		return fmt.Errorf("task.return: %d borrowed handle(s) still lent to subtasks", t.numBorrows)
	}
	t.onResolve(vals)
	t.state = taskResolved
	return nil
}

// enterTask is the reference Task.enter_implicit_thread, minus the green
// thread: with exactly one task active at a time (Instance.activeTask),
// every condition that would BLOCK entry in the reference (backpressure
// held, exclusive held by another task, waiters queued) is a permanent
// deadlock -- nothing concurrent exists to ever clear it -- so it traps
// instead of parking. Phase 3/CallAsync, which makes concurrent entry
// meaningful, replaces this trap with the reference's real wait.
func (in *Instance) enterTask(t *task) error {
	if in.activeTask != nil || in.exclusiveHeld {
		return fmt.Errorf("reentrant async call while a task is active (needs CallAsync, not yet implemented)")
	}
	if in.backpressure > 0 {
		return fmt.Errorf("async export entry blocked by backpressure (%d) with no concurrent task to clear it", in.backpressure)
	}
	in.activeTask = t
	in.exclusiveHeld = true // needs_exclusive() is always true for a callback lift (reference Task.needs_exclusive)
	return nil
}

// exitTask is the reference Task.exit_implicit_thread's state-clearing half.
// The unregister_thread "trap_if(state != RESOLVED)" check lives in
// invokeAsyncCallback instead, since it needs to return an error rather than
// just clear state.
func (in *Instance) exitTask() {
	in.exclusiveHeld = false
	in.activeTask = nil
}

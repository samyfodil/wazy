package instance

import (
	"context"
	"fmt"

	"github.com/samyfodil/wazy/internal/component/abi"
)

// This file implements guestTask, the Phase 3 structural addition
// (docs/component-model-async-phase3-design.md §0/§1): a callback-lift task
// as a genuinely PARKABLE state machine, replacing invokeAsyncCallback's
// Phase-1/2 "drive the whole callback loop inside one Go frame" shape. This
// is free precisely because of the callback-ABI decision (plan §2): every
// suspension is already a normal core function return, so "park" is just
// remembering the loop position in a struct instead of holding it in a Go
// frame.
//
//   - run:    call the core func / callback, interpret the packed code.
//   - park:   store (waitableSet, cancellable) on the guestTask and RETURN
//     to whoever called us (the scheduler, or an async-lower wrapper that
//     started us) -- see sched.park.
//   - resume: sched.step (or task.requestCancellation) calls gt.resumeReady,
//     which calls the callback core func and continues the loop.

// guestParkKind names the site a guestTask is currently suspended at.
type guestParkKind uint8

const (
	parkNone  guestParkKind = iota
	parkEntry               // Task.enter_implicit_thread's backpressure wait (~492-498)
	parkWait                // callback WAIT on a waitable set (~2185-2188)
	parkYield               // callback YIELD (~2179-2184)
)

// guestTask is the reference's Task + its implicit Thread, collapsed for
// wazy's one-callback-loop-per-task shape (task.go's doc), with the thread's
// stack replaced by explicit park state.
type guestTask struct {
	t          *task
	in         *Instance // == t.inst; the instance whose guest code this task runs
	be         *boundExport
	ctx        context.Context
	exportName string // for error messages only

	park        guestParkKind
	wset        *waitableSet // parkWait only
	cancellable bool         // is the current park a cancellable suspension?
	cancelWake  bool         // request_cancellation resumed us with Cancelled.TRUE (§2.2)

	onStart func() ([]abi.Value, error) // reference OnStart: produce lifted args (lazily!)
	done    bool
	err     error           // a trap during a scheduler-driven resume, reported to the finisher
	finish  func(err error) // notifies whoever started us (host entry or async lower)
}

// ready mirrors the reference's per-park wait_for_event_and/wait_until
// predicate (docs/component-model-async-phase3-design.md §1.1): whether
// sched.step may resume this parked task right now.
func (gt *guestTask) ready() bool {
	switch gt.park {
	case parkEntry:
		return !gt.in.hasBackpressure()
	case parkWait:
		// wait_for_event_and(lambda: not exclusive, ...) -- the conjunct is
		// load-bearing now (another task of the same instance may hold the
		// callee's exclusive).
		return !gt.in.exclusiveHeld &&
			(gt.cancelWake || gt.t.cancelDeliverable() || gt.wset.hasPendingEvent())
	case parkYield:
		return !gt.in.exclusiveHeld
	}
	return false
}

// start is canon_lift's thread_func front half (~2145-2173): attempt entry;
// if the task must park (backpressure/exclusive held), register it and
// return -- the caller (an async-lower wrapper, or invokeAsyncCallback's
// host-entry drive) sees the subtask/call still unresolved. If entry
// succeeds immediately, run to the first suspension or EXIT.
func (gt *guestTask) start() error {
	if !gt.in.tryEnter(gt.t) {
		gt.park, gt.cancellable = parkEntry, true // entry wait is cancellable (~494)
		gt.in.numWaitingToEnter++
		gt.in.sched.park(gt)
		return nil
	}
	return gt.firstRun()
}

// firstRun is task.start() -> lower params -> core call -> advance(packed),
// the Phase-1/2 invokeAsyncCallback prologue verbatim (arg lowering, pooled
// stacks, packed i32), reparented here. on_start is called HERE, not at
// canon_lower/bind time: the reference lifts caller args lazily (~2250-
// 2254), observable when a parked entry reads the caller's memory only once
// the callee actually starts (the caller may have mutated its arg buffer in
// the meantime under backpressure -- §3.3's acceptance trace).
func (gt *guestTask) firstRun() error {
	gt.in.enterRun(gt.t)
	be, t := gt.be, gt.t

	args, err := gt.onStart()
	if err != nil {
		gt.in.leaveRun()
		return gt.fail(err)
	}

	mem, memAvailable := memoryBytesOf(be.mod)
	realloc := cachedReallocOf(gt.ctx, be)

	coreArgsPtr := coreValueSlicePool.Get().(*[]abi.CoreValue)
	*coreArgsPtr = (*coreArgsPtr)[:0]
	t.state = taskStarted
	coreArgs, err := gt.in.lowerParams(be, args, mem, memAvailable, realloc, gt.exportName, *coreArgsPtr)
	if err != nil {
		coreValueSlicePool.Put(coreArgsPtr)
		gt.in.leaveRun()
		return gt.fail(err)
	}
	*coreArgsPtr = coreArgs
	if len(coreArgs) != len(be.coreParamsWant) {
		putCoreValueSlice(coreArgsPtr)
		gt.in.leaveRun()
		return gt.fail(fmt.Errorf("component/instance: export %q: parameter list flattens to %d core value(s) but the core signature expects %d; whole-parameter-list spilling to memory is not supported by this milestone", gt.exportName, len(coreArgs), len(be.coreParamsWant)))
	}

	numResults := be.coreResultCount
	stackLen := len(coreArgs)
	if numResults > stackLen {
		stackLen = numResults
	}
	stackPtr := getUint64Slice(stackLen)
	stack := *stackPtr
	for i, cv := range coreArgs {
		stack[i] = cv.Bits
	}
	putCoreValueSlice(coreArgsPtr)

	if err := be.coreFn.CallWithStack(gt.ctx, stack); err != nil {
		putUint64Slice(stackPtr)
		gt.in.leaveRun()
		return gt.fail(fmt.Errorf("component/instance: export %q: call core func %q: %w", gt.exportName, be.funcName, err))
	}
	packed := uint32(stack[0])
	putUint64Slice(stackPtr)

	return gt.advance(packed)
}

// resumeReady is called only by sched.step/pumpSnapshot (or
// task.requestCancellation's inline resume) when ready() is true (or a
// cancel forces an out-of-band wake via cancelWake).
func (gt *guestTask) resumeReady() error {
	switch gt.park {
	case parkEntry:
		gt.in.numWaitingToEnter--
		gt.in.sched.unpark(gt)
		if gt.cancelWake { // request_cancellation hit us at INITIAL (§2.2)
			gt.cancelWake = false
			if err := gt.t.cancelUnentered(); err != nil { // -> on_resolve(nil,true)
				return gt.fail(err)
			}
			gt.done = true
			if gt.finish != nil {
				gt.finish(nil)
			}
			return nil
		}
		return gt.firstRun()

	case parkWait:
		ws := gt.wset
		ws.numWaiting--
		ev := eventTuple{code: eventTaskCancelled}
		if !gt.cancelWake && !gt.t.deliverPendingCancel(true) {
			ev = ws.getPendingEvent()
		}
		gt.cancelWake = false
		gt.wset = nil
		gt.in.sched.unpark(gt)
		return gt.runLoop(ev)

	case parkYield:
		ev := eventTuple{code: eventNone}
		if gt.cancelWake || gt.t.deliverPendingCancel(true) {
			ev = eventTuple{code: eventTaskCancelled}
		}
		gt.cancelWake = false
		gt.in.sched.unpark(gt)
		return gt.runLoop(ev)
	}
	return fmt.Errorf("component/instance: BUG: resumeReady on a guestTask with no park state")
}

// runLoop delivers ev to the callback core func, then hands the packed
// result to advance -- the Phase-1/2 callback-invocation half of the loop
// body, reparented here (docs/component-model-async-phase3-design.md §1.3).
func (gt *guestTask) runLoop(ev eventTuple) error {
	gt.in.enterRun(gt.t)
	var cbStack [3]uint64
	cbStack[0], cbStack[1], cbStack[2] = uint64(ev.code), uint64(ev.p1), uint64(ev.p2)
	if err := gt.be.callbackFn.CallWithStack(gt.ctx, cbStack[:]); err != nil {
		gt.in.leaveRun()
		return gt.fail(fmt.Errorf("component/instance: export %q: callback %q: %w", gt.exportName, gt.be.callbackFuncName, err))
	}
	return gt.advance(uint32(cbStack[0]))
}

// advance interprets one packed callback-loop result: EXIT finishes the
// task, YIELD/WAIT park (never an inline nested drive -- Phase 3's
// structural change is that sched.step now knows how to resume a parked
// task on its own, so there is nothing left to drive inline here).
func (gt *guestTask) advance(packed uint32) error {
	code, si, cerr := unpackCallbackResult(packed)
	if cerr != nil {
		gt.in.leaveRun()
		return gt.fail(fmt.Errorf("component/instance: export %q: %w", gt.exportName, cerr))
	}
	if code == callbackExit {
		return gt.finishExit()
	}

	switch code {
	case callbackYield:
		gt.park = parkYield
		gt.cancellable = true
		gt.in.leaveRun()
		gt.in.sched.park(gt)
		return nil

	case callbackWait:
		e, ok := gt.in.resources.getEntry(si)
		ws, isWS := e.(*waitableSet)
		if !ok || !isWS { // trap_if(not isinstance(wset, WaitableSet))
			gt.in.leaveRun()
			return gt.fail(fmt.Errorf("component/instance: export %q: callback WAIT: handle %d is not a waitable set", gt.exportName, si))
		}
		gt.park = parkWait
		gt.wset = ws
		gt.cancellable = true
		ws.numWaiting++ // held across the park; released by resumeReady's parkWait arm
		gt.in.leaveRun()
		gt.in.sched.park(gt)
		return nil

	default:
		gt.in.leaveRun()
		return gt.fail(fmt.Errorf("component/instance: export %q: invalid callback code %d", gt.exportName, code))
	}
}

// finishExit is EXIT observed: trap_if(state != RESOLVED) (unregister_thread
// ~522), preserving Phase 1's exact trap text ("callback returned EXIT
// before task.return resolved the task").
func (gt *guestTask) finishExit() error {
	gt.in.leaveRun()
	if gt.t.state != taskResolved {
		return gt.fail(fmt.Errorf("component/instance: export %q: callback returned EXIT before task.return resolved the task", gt.exportName))
	}
	gt.done = true
	if gt.finish != nil {
		gt.finish(nil)
	}
	return nil
}

// fail records a trap and reports it to whoever started this task -- a
// synchronous failure (during gt.start()'s inline first run, propagated
// straight back to the caller) or a scheduler-driven one (propagated out of
// sched.step as its error return, exactly like a failing runq thunk today).
func (gt *guestTask) fail(err error) error {
	gt.done = true
	gt.err = err
	if gt.finish != nil {
		gt.finish(err)
	}
	return err
}

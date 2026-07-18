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

	// parkBlocked: suspended MID-CORE-CALL inside a blocking builtin (a sync
	// stream/future copy, or a sync-lowered call to an async callee) --
	// Feature 1, docs/component-model-async-final3-fable.md §1. Live guest
	// frames sit on a segment goroutine at <-seg.resumeCh; the counterpart
	// of stackfulTask's sparkBlock. Reachable only for a PROMOTED guestTask
	// (gt.promoted), i.e. one started by startAsyncExportTask on an
	// instance flagged mayBlockSync -- see gt.block.
	parkBlocked
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

	// onStart, when non-nil, is the reference OnStart: produce lifted args
	// (lazily!) -- used by startAsyncExportTask, where args must be lifted
	// from the CALLER's memory only once the callee actually starts (see the
	// package doc). nil is the default sink used by invokeAsyncCallback's
	// plain host-entry call, whose args are already lifted component-level
	// values by the time invokeAsyncCallback runs (no laziness needed):
	// firstRun reads them straight from args instead, saving the trivial
	// forwarding closure's allocation on that hot path.
	onStart func() ([]abi.Value, error)
	args    []abi.Value // used when onStart == nil
	done    bool
	err     error           // a trap during a scheduler-driven resume, reported to the finisher
	finish  func(err error) // notifies whoever started us (host entry or async lower)

	// promoted: core/callback invocations run via runSegment on a fresh
	// baton goroutine, so a blocking builtin reached INSIDE the call can
	// park this task (parkBlocked) with live frames while the caller's
	// goroutine (and eventually the root driver) continues. Set at
	// construction (startAsyncExportTask only, gated on the starting
	// instance's mayBlockSync), never mutated after. Left false for
	// invokeAsyncCallback's host-entry call -- see docs/component-model-
	// async-final3-fable.md §1.9's honest flag #1.
	promoted bool

	// seg is the live segment, non-nil exactly while a segment goroutine
	// exists: from runSegment's spawn until the segment's fn returns
	// (frame-free park, EXIT, or trap). It stays non-nil across a
	// parkBlocked suspension (the goroutine is alive inside gt.block).
	// Always nil for an unpromoted task.
	seg *guestSegment
}

// guestSegment is one run of a promoted guestTask's core code: a goroutine
// executing exactly one fn (firstRunBody's body, or one runLoopBody
// invocation), plus the same baton-channel pair stackfulTask uses
// (resumeMode/errStackfulAbort, stackful_task.go). Segments are
// per-invocation: a task that parks frame-free (WAIT/YIELD) costs no
// goroutine while parked; only a parkBlocked task pins one -- see
// docs/component-model-async-final3-fable.md §1.2.
type guestSegment struct {
	gt       *guestTask
	resumeCh chan resumeMode // driver -> goroutine
	yieldCh  chan struct{}   // goroutine -> driver
	err      error           // fn's result, valid once gt.seg == nil
	panicVal any             // non-trap panic on the goroutine; re-panicked on the driver

	// parkReady is parkBlocked's predicate (mirrors stackfulTask.parkReady),
	// set by gt.block for the duration of one parkBlocked suspension.
	parkReady func() bool
}

// runSegment executes fn inline (unpromoted task -- bit-identical to every
// currently-green suite) or on a fresh baton goroutine (promoted). Returns
// fn's error once the segment finishes, or nil if the segment PARKED
// mid-call (gt is then in sched.parked at parkBlocked and the goroutine is
// alive at <-resumeCh inside gt.block).
func (gt *guestTask) runSegment(fn func() error) error {
	if !gt.promoted {
		return fn()
	}
	seg := &guestSegment{
		gt:       gt,
		resumeCh: make(chan resumeMode), yieldCh: make(chan struct{}),
	}
	gt.seg = seg
	go seg.main(fn)
	return seg.handoff(resumeNormal)
}

// main is the segment goroutine body -- mirrors stackfulTask.main.
func (s *guestSegment) main(fn func() error) {
	mode := <-s.resumeCh // first baton
	defer func() {
		if r := recover(); r != nil && r != any(errStackfulAbort) {
			s.panicVal = r // real bug: surface on the driver, never swallow
		}
		s.gt.seg = nil          // segment over (fn returned or panicked)
		s.yieldCh <- struct{}{} // final handoff
	}()
	if mode == resumeAbort {
		return // reaped before ever running
	}
	s.err = fn()
}

// handoff hands the baton to the segment goroutine and waits for it back.
// Runs on whatever goroutine currently owns the baton (the starter on the
// first handoff; sched.step's caller on resumes).
func (s *guestSegment) handoff(mode resumeMode) error {
	s.resumeCh <- mode
	<-s.yieldCh
	if s.gt.seg == nil { // finished
		if s.panicVal != nil {
			panic(s.panicVal)
		}
		return s.err // nil on success/frame-free park; non-nil = trap (already gt.fail-ed)
	}
	return nil // parked at parkBlocked; block() already re-registered gt in sched.parked
}

// block suspends the calling PROMOTED guestTask mid-core-call until ready()
// holds (or a cancel wake). MUST be called on the segment goroutine (i.e.
// from a builtin invoked by this task's guest code). Counterpart of
// stackfulTask.block; the exclusive stays HELD across the park (reference
// wait_until never releases it -- only the callback loop's frame-free WAIT
// does, via leaveRun in advance).
func (gt *guestTask) block(ready func() bool, cancellable bool) (cancelled bool) {
	if gt.t.deliverPendingCancel(cancellable) {
		return true
	}
	seg := gt.seg
	gt.park, gt.cancellable = parkBlocked, cancellable
	seg.parkReady = ready
	gt.in.suspendRun()   // activeTask=nil, mayEnter=true; exclusive KEPT
	gt.in.sched.park(gt) // safe: we hold the baton
	seg.yieldCh <- struct{}{}
	mode := <-seg.resumeCh
	if mode == resumeAbort {
		panic(errStackfulAbort) // unwinds guest frames via the engine's recover
	}
	gt.park, gt.cancellable = parkNone, false
	seg.parkReady = nil
	gt.in.enterRun(gt.t)
	return mode == resumeCancelled
}

// ready mirrors the reference's per-park wait_for_event_and/wait_until
// predicate (docs/component-model-async-phase3-design.md §1.1): whether
// sched.step may resume this parked task right now.
func (gt *guestTask) ready() bool {
	// held mirrors the reference's "exclusive_thread not in {None, self}"
	// (docs/component-model-async-stackful-design.md §5): exclusiveOwner
	// distinguishes "this instance's exclusive is free" from "held by a
	// DIFFERENT task" now that a stackful task can hold its own exclusive
	// across its own park. Identity-preserving for a pure-callback
	// composition (exclusiveOwner is always either nil or the one running
	// task there, so this evaluates exactly like the old bare
	// !gt.in.exclusiveHeld check).
	held := gt.in.exclusiveHeld && gt.in.exclusiveOwner != gt.t
	switch gt.park {
	case parkEntry:
		return !gt.in.hasBackpressure()
	case parkWait:
		// wait_for_event_and(lambda: not exclusive, ...) -- the conjunct is
		// load-bearing now (another task of the same instance may hold the
		// callee's exclusive).
		return !held &&
			(gt.cancelWake || gt.t.cancelDeliverable() || gt.wset.hasPendingEvent())
	case parkYield:
		return !held
	case parkBlocked:
		// No !held conjunct, mirroring stackfulTask.ready's sparkBlock case:
		// gt itself still holds its instance's exclusive across its own
		// parkBlocked suspension (suspendRun deliberately keeps
		// exclusiveHeld/exclusiveOwner==gt.t), so held would always
		// evaluate false here anyway -- gating on it would only risk a
		// self-deadlock if that invariant ever shifted.
		return gt.cancelWake || gt.t.cancelDeliverable() || gt.seg.parkReady()
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

// firstRun runs firstRunBody inline (unpromoted task) or on a fresh segment
// goroutine (promoted) via runSegment -- see guestTask.promoted's doc.
func (gt *guestTask) firstRun() error { return gt.runSegment(gt.firstRunBody) }

// firstRunBody is task.start() -> lower params -> core call -> advance(packed),
// the Phase-1/2 invokeAsyncCallback prologue verbatim (arg lowering, pooled
// stacks, packed i32), reparented here. on_start is called HERE, not at
// canon_lower/bind time: the reference lifts caller args lazily (~2250-
// 2254), observable when a parked entry reads the caller's memory only once
// the callee actually starts (the caller may have mutated its arg buffer in
// the meantime under backpressure -- §3.3's acceptance trace). For a
// PROMOTED task this runs on the segment goroutine (see runSegment) --
// onStart's caller-memory read is then legal because the baton serializes
// it: the starter is blocked in seg.handoff while this goroutine runs.
func (gt *guestTask) firstRunBody() error {
	gt.in.enterRun(gt.t)
	be, t := gt.be, gt.t

	args := gt.args
	if gt.onStart != nil {
		var err error
		if args, err = gt.onStart(); err != nil {
			gt.in.leaveRun()
			return gt.fail(err)
		}
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
		gt.in.poisoned = true // guest code actually ran and trapped -- see fail's doc
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

	case parkBlocked:
		// Feature 1: the segment goroutine is still alive, blocked inside
		// gt.block's <-seg.resumeCh -- hand the baton back directly (no new
		// segment) rather than starting a fresh invocation, mirroring
		// stackfulTask.resumeReady's sparkBlock arm.
		gt.in.sched.unpark(gt)
		mode := resumeNormal
		if gt.cancelWake {
			gt.cancelWake, mode = false, resumeCancelled
		}
		return gt.seg.handoff(mode)
	}
	return fmt.Errorf("component/instance: BUG: resumeReady on a guestTask with no park state")
}

// runLoop runs runLoopBody(ev) inline (unpromoted task) or on a fresh
// segment goroutine (promoted) via runSegment -- avoids the closure
// allocation on the unpromoted hot path.
func (gt *guestTask) runLoop(ev eventTuple) error {
	if !gt.promoted {
		return gt.runLoopBody(ev)
	}
	return gt.runSegment(func() error { return gt.runLoopBody(ev) })
}

// runLoopBody delivers ev to the callback core func, then hands the packed
// result to advance -- the Phase-1/2 callback-invocation half of the loop
// body, reparented here (docs/component-model-async-phase3-design.md §1.3).
// For a PROMOTED task this runs on a fresh segment goroutine each
// invocation (see runLoop/runSegment).
func (gt *guestTask) runLoopBody(ev eventTuple) error {
	gt.in.enterRun(gt.t)
	// Pooled (getUint64Slice/putUint64Slice, instance.go), not a stack
	// array: CallWithStack's interface call makes escape analysis treat a
	// local [3]uint64 as heap-escaping, exactly the allocation invoke's own
	// coreArgs/stack buffers already avoid this way.
	cbStackPtr := getUint64Slice(3)
	cbStack := *cbStackPtr
	cbStack[0], cbStack[1], cbStack[2] = uint64(ev.code), uint64(ev.p1), uint64(ev.p2)
	if err := gt.be.callbackFn.CallWithStack(gt.ctx, cbStack); err != nil {
		putUint64Slice(cbStackPtr)
		gt.in.leaveRun()
		gt.in.poisoned = true // guest code actually ran and trapped -- see fail's doc
		return gt.fail(fmt.Errorf("component/instance: export %q: callback %q: %w", gt.exportName, gt.be.callbackFuncName, err))
	}
	packed := uint32(cbStack[0])
	putUint64Slice(cbStackPtr)
	return gt.advance(packed)
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
//
// fail itself does NOT poison gt.in -- some of its callers are host-side ABI
// validation failures that never actually ran guest code (onStart/
// lowerParams, an unknown waitable-set handle in a callback WAIT/YIELD
// result, ...), analogous to invokeEntered's lowerParams/liftResult (see its
// doc): poisoning those would wrongly brick the instance for every LATER,
// unrelated call, exactly the regression invokeEntered's doc describes for
// resolveArgHandles on a dropped resource handle. Only firstRun's
// be.coreFn.CallWithStack and runLoop's be.callbackFn.CallWithStack --
// guest code that actually ran and trapped -- set gt.in.poisoned = true
// themselves, right where they call fail.
func (gt *guestTask) fail(err error) error {
	gt.done = true
	gt.err = err
	if gt.finish != nil {
		gt.finish(err)
	}
	return err
}

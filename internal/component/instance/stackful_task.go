package instance

import (
	"context"
	"errors"
	"fmt"

	"github.com/samyfodil/wazy/internal/component/abi"
)

// This file implements stackfulTask, the last unhandled canon_lift shape
// (docs/component-model-async-stackful-design.md, the "stackful lift"): an
// async-TYPED func lifted with NO callback option, whose core code calls a
// blocking builtin (waitable-set.wait, a blocking stream/future op, a
// sync-lowered async import) DIRECTLY and must suspend WITH LIVE WASM FRAMES
// beneath it -- be.coreFn.CallWithStack is blocked mid-execution inside a
// hostFuncDef closure. Unlike guestTask (guest_task.go), whose every
// suspension is a normal core function return (so "park" is just Go state),
// a stackful task's suspended stack is a REAL parked goroutine.
//
// The model (design doc §1): a stackful task is a goroutine, and the baton
// is a pair of unbuffered channels. At any instant, at most one goroutine in
// a composition tree executes component-runtime code; every transfer of
// control is a channel send/recv pair, so -race sees ordinary happens-before
// edges and nothing else needs new synchronization.

// stackfulParkKind names the site a stackfulTask is suspended at.
type stackfulParkKind uint8

const (
	sparkNone  stackfulParkKind = iota
	sparkEntry                  // backpressure wait before first run (~492-498); goroutine NOT yet spawned
	sparkBlock                  // parked inside a blocking builtin, goroutine live at <-resumeCh
)

// resumeMode is the value carried driver->goroutine on resumeCh.
type resumeMode uint8

const (
	resumeNormal    resumeMode = iota
	resumeCancelled            // reference thread.resume(Cancelled.TRUE) (~538-541)
	resumeAbort                // reap: unwind the goroutine (Close / driver error path)
)

// errStackfulAbort is the sentinel block() panics with on resumeAbort; it
// unwinds the goroutine's guest frames (recovered by the engine's
// callWithStack recover and re-surfaced as a call error) and is finally
// swallowed by the goroutine's own top-level recover (main). Never escapes
// to users.
var errStackfulAbort = errors.New("component/instance: stackful task aborted")

// stackfulTask is the reference's Task + implicit Thread for a lift with NO
// callback option: the thread's suspended stack is a REAL parked goroutine
// (guest frames live inside be.coreFn.CallWithStack), and Thread.resume/
// block's lock handoff is a pair of unbuffered channels.
type stackfulTask struct {
	sw         switchWindow // publish-before-park window (see switchWindow)
	t          *task
	in         *Instance
	be         *boundExport
	ctx        context.Context
	exportName string

	// asyncOpts: false = sync-opts sub-shape (results lifted from flat core
	// results, needs_exclusive; the only shape the stackful conformance
	// suites use), true = async-no-callback sub-shape (results via the
	// task.return builtin).
	asyncOpts bool

	// The baton. Created by firstRun(); nil until first real run (an
	// entry-parked task has no goroutine yet).
	resumeCh chan resumeMode // driver -> goroutine: run until next suspension
	yieldCh  chan struct{}   // goroutine -> driver: suspended-or-done
	spawned  bool

	park       stackfulParkKind
	parkReady  func() bool // sparkBlock only: the blocking site's predicate
	cancelWake bool        // requestCancellation resumed us with Cancelled.TRUE

	onStart func() ([]abi.Value, error) // lazy caller-arg lift (delegated calls); nil => args
	args    []abi.Value
	done    bool
	err     error
	// panicVal is a non-trap panic on the goroutine (a real bug in our own
	// lowering code, not a guest trap), re-panicked on the driver.
	panicVal any
	finish   func(err error) // notifies whoever started us (host entry or async lower)
}

// startTask mirrors guestTask.start (guest_task.go ~90): attempt entry; park
// at sparkEntry (no goroutine yet) under backpressure, else spawn + first run.
func (st *stackfulTask) startTask() error {
	if !st.in.tryEnter(st.t) {
		st.park = sparkEntry
		st.in.numWaitingToEnter++
		st.in.sched.park(st)
		return nil
	}
	return st.firstRun()
}

// firstRun spawns the goroutine and performs the first baton handoff. On
// return the task has either finished (st.done) or parked (in sched.parked).
func (st *stackfulTask) firstRun() error {
	st.resumeCh, st.yieldCh = make(chan resumeMode), make(chan struct{})
	st.spawned = true
	go st.main()
	return st.handoff(resumeNormal)
}

// main is the goroutine body -- the reference cont_new's thread_base (~279).
func (st *stackfulTask) main() {
	mode := <-st.resumeCh // wait for the first baton
	defer func() {
		if r := recover(); r != nil && r != any(errStackfulAbort) {
			st.panicVal = r // driver re-panics; never swallow a real bug
		}
		st.done = true
		st.yieldCh <- struct{}{} // final handoff; driver observes done
	}()
	if mode == resumeAbort {
		return // reaped before ever running (only via reap of a just-spawned task)
	}
	st.err = st.run()
}

// run is canon_lift's no-callback body (~2144-2171): guestTask.firstRun's
// exact prologue (enterRun, lazy onStart, lowerParams, pooled stacks),
// followed by the shape split (sync-opts vs async-no-callback). Runs ON the
// goroutine; CallWithStack may suspend/resume any number of times via
// block() before it returns -- when it does, THIS goroutine holds the baton
// again and enterRun's brackets are re-established (block()'s epilogue).
func (st *stackfulTask) run() error {
	in, be, t := st.in, st.be, st.t
	in.enterRun(t)

	args := st.args
	if st.onStart != nil {
		var err error
		if args, err = st.onStart(); err != nil {
			in.leaveRun()
			return st.fail(err)
		}
	}

	mem, memAvailable := memoryBytesOf(be.mod)
	realloc := cachedReallocOf(st.ctx, be)

	coreArgsPtr := coreValueSlicePool.Get().(*[]abi.CoreValue)
	*coreArgsPtr = (*coreArgsPtr)[:0]
	t.state = taskStarted
	coreArgs, err := in.lowerParams(be, args, mem, memAvailable, realloc, st.exportName, *coreArgsPtr)
	if err != nil {
		coreValueSlicePool.Put(coreArgsPtr)
		in.leaveRun()
		return st.fail(err)
	}
	*coreArgsPtr = coreArgs
	if len(coreArgs) != len(be.coreParamsWant) {
		putCoreValueSlice(coreArgsPtr)
		in.leaveRun()
		return st.fail(fmt.Errorf("component/instance: export %q: parameter list flattens to %d core value(s) but the core signature expects %d; whole-parameter-list spilling to memory is not supported by this milestone", st.exportName, len(coreArgs), len(be.coreParamsWant)))
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

	if err := be.coreFn.CallWithStack(st.ctx, stack); err != nil {
		putUint64Slice(stackPtr)
		in.leaveRun()
		in.poisoned = true // guest code actually ran and trapped -- see fail's doc
		return st.fail(fmt.Errorf("component/instance: export %q: call core func %q: %w", st.exportName, be.funcName, err))
	}

	if st.asyncOpts {
		// async-no-callback: core returns nothing; exit_implicit_thread's
		// trap_if(state != RESOLVED) (~522), same text as guestTask's
		// finishExit.
		putUint64Slice(stackPtr)
		in.leaveRun()
		if t.state != taskResolved {
			return st.fail(fmt.Errorf("component/instance: export %q: stackful async export returned before task.return resolved the task", st.exportName))
		}
		st.finishOK()
		return nil
	}

	// sync-opts: lift flat results -> task.return_ -> post-return (~2156-2163).
	rawResults := stack[:be.coreResultCount]
	// Re-fetch the memory view: the guest may have grown memory while the task
	// ran, replacing the backing array (and possibly pooling the old one), so
	// the pre-call slice can no longer be trusted for lifting. Same reasoning
	// as invoke's re-fetch in instance.go.
	mem, memAvailable = memoryBytesOf(be.mod)
	results, err := in.liftResult(be, rawResults, mem, memAvailable, st.exportName)
	if err != nil {
		putUint64Slice(stackPtr)
		in.leaveRun()
		return st.fail(err)
	}
	if err := t.returnValues(results); err != nil { // traps if guest already resolved
		putUint64Slice(stackPtr)
		in.leaveRun()
		return st.fail(fmt.Errorf("component/instance: export %q: %w", st.exportName, err))
	}
	if be.postReturnFuncName != "" {
		if be.postReturnFn == nil {
			putUint64Slice(stackPtr)
			in.leaveRun()
			return st.fail(fmt.Errorf("component/instance: export %q: post-return core func %q not found", st.exportName, be.postReturnFuncName))
		}
		if err := be.postReturnFn.CallWithStack(st.ctx, rawResults); err != nil {
			putUint64Slice(stackPtr)
			in.leaveRun()
			in.poisoned = true // guest code actually ran and trapped -- see fail's doc
			return st.fail(fmt.Errorf("component/instance: export %q: post-return %q: %w", st.exportName, be.postReturnFuncName, err))
		}
	}
	putUint64Slice(stackPtr)
	in.leaveRun()
	st.finishOK()
	return nil
}

// fail records a trap. Like guestTask.fail, it does NOT itself poison st.in
// -- see that doc; only run's be.coreFn.CallWithStack and be.postReturnFn.
// CallWithStack (guest code that actually ran) set st.in.poisoned = true.
func (st *stackfulTask) fail(err error) error {
	st.err = err
	if st.finish != nil {
		st.finish(err)
	}
	return err
}

func (st *stackfulTask) finishOK() {
	if st.finish != nil {
		st.finish(nil)
	}
}

// block suspends the calling stackful goroutine until ready() holds (or a
// cancel wake). It is Thread.suspend/wait_until (~396-409) with the lock
// handoff replaced by the baton channels. MUST be called with the baton held
// (i.e. from a builtin invoked by this task's guest code, ON the goroutine).
func (st *stackfulTask) block(ready func() bool) (cancelled bool) {
	// No cancellation delivery here. The reference's Thread.wait_until (~353)
	// does not deliver: delivery happens at exactly four sites, all of them in
	// the callback dispatch loop or async entry -- the loop top, its YIELD and
	// WAIT dispositions (WaitableSet.wait_from_callback, ~803), and
	// Task.enter_implicit_thread's backpressure wait (~441). An older revision
	// delivered in wait_until's prologue, which is where this used to be; the
	// re-vendor to 07afb8145b80 dropped it, and cancel-delivery.wast asserts
	// the difference directly ("a pending cancellation is not reported by the
	// thread.yield and waitable-set.poll built-ins ... it survives them and is
	// delivered at the event loop"). Every builtin that parks through here --
	// thread.yield, thread.suspend, the four switch builtins, waitable-set.wait
	// -- must leave a PENDING_CANCEL pending.
	ready = st.sw.effectiveReady(ready)
	if st.sw.closeWindow() { // already resumed inside the window: do not park
		return false
	}
	st.park, st.parkReady = sparkBlock, ready
	st.in.suspendRun()       // activeTask=nil, mayEnter=true; exclusive KEPT
	st.in.sched.park(st)     // safe: we hold the baton
	st.yieldCh <- struct{}{} // baton -> driver
	mode := <-st.resumeCh    // baton <- driver (sched.step, canceller, or reaper)
	if mode == resumeAbort {
		panic(errStackfulAbort)
	}
	st.park, st.parkReady = sparkNone, nil
	st.in.enterRun(st.t) // re-establish running brackets
	return mode == resumeCancelled
}

func (st *stackfulTask) instance() *Instance { return st.in }

// ready mirrors guestTask.ready (guest_task.go ~69) for the two park kinds.
// No !exclusiveHeld conjunct at sparkBlock: a sync-opts stackful task HOLDS
// its instance's exclusive across its own suspension (reference: wait_until
// never releases exclusive_thread; only the callback loop does, ~2177), so
// gating on it would deadlock against ourselves. Cross-task exclusion is
// per-instance and handled by exclusiveOwner (task.go/instance.go).
func (st *stackfulTask) window() *switchWindow { return &st.sw }

func (st *stackfulTask) ready() bool {
	if st.sw.open {
		return st.sw.publishedReady()
	}
	switch st.park {
	case sparkEntry:
		return !st.in.hasBackpressure()
	case sparkBlock:
		// See guestTask.ready's parkBlocked arm for why there is no
		// cancellation disjunct here. A nil parkReady is the reference's
		// ready_func of None: SUSPENDED, so never ready (see suspendedState).
		return st.cancelWake || (st.parkReady != nil && st.parkReady())
	}
	return false
}

// suspendedState/startWaiting/stopWaiting make a stackful task's implicit
// thread a first-class Thread for the switch built-ins (§4.5): the reference
// draws no distinction between a task's implicit thread and a spawned one, so
// thread.{resume-later,suspend-then-resume,yield-then-resume,*-then-promote}
// may target either. SUSPENDED is "parked mid-call with ready_func None" --
// exactly what a thread.suspend leaves behind, since it installs no predicate
// (Thread.suspend is a bare block(), definitions.py ~347) -- which is what
// during-sync-scheduling-candidates and reentrance's outer-sync/inner-async
// cases resume.
func (st *stackfulTask) suspendedState() bool {
	if st.sw.open {
		return st.sw.publishedSuspended()
	}
	return st.park == sparkBlock && st.parkReady == nil
}

// startWaiting is Thread.start_waiting(ready_func = lambda: True), the body of
// resume_later; stopWaiting is Thread.stop_waiting, which clears ready_func and
// so makes the thread SUSPENDED again.
func (st *stackfulTask) startWaiting() {
	if !st.sw.setReady(alwaysReady) {
		st.parkReady = alwaysReady
	}
}

func (st *stackfulTask) stopWaiting() {
	if !st.sw.setReady(nil) {
		st.parkReady = nil
	}
}

// resumeReady is called only with st in sched.parked (goroutine at
// <-resumeCh, or not yet spawned for sparkEntry), by sched.step/pumpSnapshot
// or task.requestCancellation -- on whatever goroutine currently holds the
// baton.
func (st *stackfulTask) resumeReady() error {
	if st.sw.catchResume() {
		return nil
	}
	switch st.park {
	case sparkEntry:
		st.in.numWaitingToEnter--
		st.in.sched.unpark(st)
		st.park = sparkNone
		if st.cancelWake { // cancelled at INITIAL: same landing as guestTask (~170-180)
			st.cancelWake = false
			if err := st.t.cancelUnentered(); err != nil {
				st.done = true
				return st.fail(err)
			}
			st.done = true
			st.finishOK()
			return nil
		}
		return st.firstRun()

	case sparkBlock:
		st.in.sched.unpark(st)
		mode := resumeNormal
		if st.cancelWake {
			st.cancelWake, mode = false, resumeCancelled
		}
		return st.handoff(mode)
	}
	return fmt.Errorf("component/instance: BUG: resumeReady on a stackfulTask with no park state")
}

// handoff gives the baton to the goroutine and waits for it back.
func (st *stackfulTask) handoff(mode resumeMode) error {
	// The baton is strictly nested: this call blocks until it comes back, so
	// whoever we hand it to (and anything IT hands it to) runs entirely inside
	// this frame. Save and restore our own current-thread identity around that,
	// or a spawned thread that merely runs and exits -- guestThread.run clears
	// activeThread on the way out -- leaves the instance with no current thread
	// at all, and the next builtin on THIS thread resolves the wrong blocker.
	// during-sync-call-may-block-if-other-ready-threads arms exactly such a
	// short-lived thread before blocking, and its switch back then parked the
	// sync task's implicit thread instead of itself.
	prevThread := st.in.activeThread
	defer func() { st.in.activeThread = prevThread }()
	st.resumeCh <- mode
	<-st.yieldCh
	if st.done {
		if st.panicVal != nil {
			panic(st.panicVal) // a real bug on the goroutine: surface on the driver
		}
		return st.err // nil on success; sched.step propagates non-nil exactly like a failing thunk
	}
	return nil // parked again: block() already re-registered st in sched.parked
}

// reapParkedGoroutines aborts every parked task in the shared scheduler that
// pins a live goroutine -- a *stackfulTask (docs/component-model-async-
// stackful-design.md §8), a PROMOTED *guestTask parked at parkBlocked
// (Feature 1, docs/component-model-async-final3-fable.md §1.5: a promoted
// composition can now strand parked segment goroutines under a
// callback-rooted invoke too, not just a stackful-rooted one), or a
// *guestThread (Stage D, docs/component-model-async-threads-design-fable.md
// §6.4: a spawned thread.new-indirect thread -- SUSPENDED (never resumed,
// nothing to unwind) or parked WAITING inside a real blocking builtin).
// Runs on the driver (the baton is necessarily free here: nothing is
// running). Abort makes block() panic errStackfulAbort on the goroutine; the
// engine's recover degrades it to a call error through the guest frames;
// main()'s recover swallows the sentinel; the final yield completes the
// handoff -- so this func never returns until every parked goroutine has
// fully unwound and exited. Not called concurrently with a live driver by
// construction (Close and invoke*'s error paths only).
func (in *Instance) reapParkedGoroutines() {
	for {
		var vst *stackfulTask
		var vgt *guestTask
		var vth *guestThread
		for _, p := range in.sched.parked {
			switch v := p.(type) {
			case *stackfulTask:
				vst = v
			case *guestTask:
				if v.seg != nil { // parkBlocked with a live goroutine; frame-free parks hold nothing
					vgt = v
				}
			case *guestThread:
				vth = v
			}
			if vst != nil || vgt != nil || vth != nil {
				break
			}
		}
		switch {
		case vst != nil:
			in.sched.unpark(vst)
			if vst.park == sparkEntry { // no goroutine yet -- nothing to unwind
				vst.done = true
				vst.park = sparkNone
				continue
			}
			vst.resumeCh <- resumeAbort
			<-vst.yieldCh // goroutine has fully unwound; done == true

		case vgt != nil:
			in.sched.unpark(vgt)
			seg := vgt.seg
			seg.resumeCh <- resumeAbort // block() panics errStackfulAbort -> engine recover ->
			<-seg.yieldCh               // fn returns an error -> seg.main's deferred yield
			// seg is nil now; goroutine fully unwound.

		case vth != nil:
			in.sched.unpark(vth)
			if !vth.spawned { // never resumed: no goroutine exists to unwind, and
				// run() (which normally does this bookkeeping) will never execute.
				//
				// vth.in, NOT in: the sched is shared across the whole
				// composition tree, so the reaper is a different instance from
				// the thread's owner whenever a parent reaps a sub's thread.
				// Freeing vth.index on the reaper's own table would both leak
				// the owner's slot AND hand the reaper's unrelated slot at that
				// index out twice (threadTable.remove pushes it onto the free
				// list). Same receiver as run's unregister tail (thread.go).
				// vth.t is already the OWNING task (guestThread.t == the task
				// that called thread.new-indirect, never anything reached
				// through the reaper), so liveThreads-- needs no such fix.
				//
				// vth.parked stays true (SUSPENDED-looking) deliberately: the
				// freed owner slot is the gate -- thread.yield-then-resume is
				// the only path that can resume a suspended thread and it
				// resolves through in.threads.getThread first, which now misses;
				// the scheduler never sees it either (unparked above, and
				// ready() is false for a nil parkReady). Clearing it here would
				// also diverge from the spawned arm below, which leaves exactly
				// the same residue on a reaped thread (its abort goes straight
				// to resumeCh, bypassing resumeReady's clear). done is the
				// terminal marker for both.
				vth.in.threads.remove(vth.index)
				vth.t.liveThreads--
				vth.done = true
				continue
			}
			vth.resumeCh <- resumeAbort // block() panics errStackfulAbort -> engine recover ->
			<-vth.yieldCh               // run() returns an error, doing its own threads.remove/
			// liveThreads-- in its (shared, err-or-not) tail -> main's deferred final yield

		default:
			return
		}
	}
}

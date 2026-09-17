package instance

import (
	"errors"
	"fmt"
)

// A sync task's implicit thread, made schedulable.
//
// The reference puts no restriction on which thread may suspend:
// canon_thread_suspend is `thread.suspend(); return [0]`, and canon_lift's
// sync-caller loop then resumes whatever candidate is ready -- including the
// implicit thread of an async callback task, whose callback runs in the middle
// of the sync call. That is exactly what the during-sync-call-* suites assert,
// and wazy used to trap instead ("cannot block a synchronous task before
// returning"), which was correct for an older revision of the specification.
//
// A sync call holds live guest frames on the calling goroutine, so it cannot be
// switched away from. It does not have to be: suspending drives the shared
// scheduler *nested* on that goroutine until something resumes this thread,
// which runs every other ready thread of the instance in the meantime. The
// guest frame stays exactly where it is.
type syncImplicitThread struct {
	t  *task
	in *Instance

	// readyFn is the reference's Thread.ready_func, and nil means None: this
	// thread is SUSPENDED and only an explicit resume-later or switch can wake
	// it. Non-nil makes it WAITING, and the predicate is re-evaluated on every
	// scheduler scan -- a thread parked on `waitable-set has an event` becomes
	// ready when the event arrives, which a boolean snapshot taken at park time
	// could never notice.
	readyFn func() bool
	// parked is true only while this thread is actually suspended: the record
	// outlives any one suspension (task.syncThread), and a RUNNING thread is
	// neither suspended nor ready however its ready_func happens to read.
	parked bool
	// resumed is set when the driver actually resumes this thread, which is what
	// the nested drive below waits for. Only the goroutine-less path uses it.
	resumed bool

	// The baton, when this thread has a goroutine of its own (runSyncCore).
	// A sync lift whose instance can suspend runs its core call on that
	// goroutine so the thread is a real continuation: parking hands the baton
	// back to the driver rather than nesting a drive inside the guest's frames,
	// which is what lets ANYTHING resume it later -- including a reentrant call
	// running on top of the thread it switched to (reentrance.wast's last case).
	// Instances that bind no suspending canon never spawn one and keep running
	// the core call inline.
	spawned  bool
	resumeCh chan resumeMode
	yieldCh  chan struct{}
	done     bool
	err      error
	panicVal any
}

// main is the sync thread's goroutine body -- shape-identical to
// stackfulTask.main.
func (s *syncImplicitThread) main(fn func() error) {
	mode := <-s.resumeCh
	defer func() {
		if r := recover(); r != nil && r != any(errStackfulAbort) {
			s.panicVal = r // a real trap or bug: re-panicked on the driver
		}
		s.done = true
		s.yieldCh <- struct{}{}
	}()
	if mode == resumeAbort {
		return
	}
	s.err = fn()
}

// handoff gives the baton to the goroutine and waits for it back -- shape-
// identical to stackfulTask.handoff.
func (s *syncImplicitThread) handoff(mode resumeMode) error {
	prevThread := s.in.activeThread
	defer func() { s.in.activeThread = prevThread }()
	s.resumeCh <- mode
	<-s.yieldCh
	if s.done {
		if s.panicVal != nil {
			panic(s.panicVal)
		}
		return s.err
	}
	return nil // parked again
}

func (s *syncImplicitThread) instance() *Instance { return s.in }

func (s *syncImplicitThread) ready() bool {
	return s.parked && s.readyFn != nil && s.readyFn()
}

func (s *syncImplicitThread) resumeReady() error {
	s.in.sched.unpark(s)
	s.readyFn, s.parked, s.resumed = nil, false, true
	if s.spawned {
		s.in.enterRun(s.t)
		return s.handoff(resumeNormal)
	}
	return nil
}

// suspendedState is the reference's Thread.suspended(): not running, with no
// ready_func.
func (s *syncImplicitThread) suspendedState() bool {
	return s.parked && s.readyFn == nil
}

// startWaiting is start_waiting(ready_func = lambda: True) -- resume_later's
// body; stopWaiting clears ready_func, making the thread SUSPENDED again.
func (s *syncImplicitThread) startWaiting() { s.readyFn = alwaysReady }
func (s *syncImplicitThread) stopWaiting()  { s.readyFn = nil }

// suspendSelf parks the caller with the given readiness, whichever kind of task
// it belongs to: an async task hands its blocker the predicate, and a sync
// task's implicit thread parks and drives the scheduler nested (see
// suspendSyncThread). Every switch built-in goes through here, because the
// reference draws no distinction -- Thread.suspend/yield_ work the same for an
// implicit thread of a sync task as for a spawned one.
func suspendSelf(in *Instance, name string, selfReady func() bool) {
	captureSuspender(in, name)(selfReady)
}

// captureSuspender resolves WHO is about to park, before anything else runs.
//
// It matters for the switch built-ins: those resume the other thread first, and
// that thread's code runs -- moving in.activeTask -- before the caller parks. A
// suspendSelf evaluated after the switch would read whoever is active by then
// and park the wrong task, which showed up as a working example suddenly
// reporting "cannot block a synchronous task before returning".
func captureSuspender(in *Instance, name string) func(selfReady func() bool) {
	// A SPAWNED thread of a sync task must park itself, not its task's implicit
	// thread: only the goroutine holding the baton may park, and the implicit
	// thread may be separately parked already (design §5.2, the same preference
	// currentBlocker makes). trap-if-block-and-sync's $thread-start-resume-back
	// runs on such a thread and switches back to the sync implicit thread.
	if th := in.activeThread; th != nil {
		return func(selfReady func() bool) { th.block(selfReady) }
	}
	if t := in.activeTask; t != nil && t.syncImplicit {
		return syncBlocker{in: in, t: t, name: name}.blockVoid
	}
	_, blk := blockingTask(in, name)
	if blk == nil {
		panic(fmt.Errorf("component/instance: %s: not supported from an unpromoted callback task", name))
	}
	return func(selfReady func() bool) { blk.block(selfReady) }
}

// suspendSyncThread parks this sync task's implicit thread and drives the
// scheduler until something resumes it, which is the whole point: while it is
// parked, every other ready thread of the instance gets to run.
//
// selfReady is the caller's own ready_func: nil is a suspend (no predicate, so
// only an explicit resume-later or switch wakes it), always-ready is a yield
// (the driver may pick it straight back up, behind anything already ready), and
// anything else is a wait_until on that predicate.
func suspendSyncThread(in *Instance, t *task, name string, selfReady func() bool) {
	sit := syncBlocker{in: in, t: t, name: name}.sit()
	// selfReady nil is the reference's ready_func of None: a thread.suspend
	// installs no predicate and the thread is SUSPENDED (only an explicit
	// resume-later or switch wakes it). Anything else is a wait_until.
	sit.readyFn, sit.parked, sit.resumed = selfReady, true, false
	in.sched.park(sit)

	if sit.spawned {
		// This thread owns a goroutine: hand the baton back to whoever resumed
		// us (a driver's step) and wait there. No nested drive, so the thread's
		// frames sit on a channel receive and anything can resume them.
		prevTask, prevMayEnter := in.activeTask, in.mayEnter
		in.suspendRun()
		sit.yieldCh <- struct{}{}
		mode := <-sit.resumeCh
		in.activeTask, in.mayEnter = prevTask, prevMayEnter
		if mode == resumeAbort {
			panic(errStackfulAbort)
		}
		return
	}

	// The sync call's guest frames stay live on this goroutine, but the task is
	// not RUNNING while parked: release the run state exactly as a stackful
	// park does, or the work we are about to drive cannot re-enter the instance
	// ("cannot enter component instance") and the whole point of suspending --
	// letting other threads run -- is defeated.
	prevTask, prevMayEnter := in.activeTask, in.mayEnter
	in.suspendRun()
	defer func() {
		in.activeTask, in.mayEnter = prevTask, prevMayEnter
	}()

	if err := in.sched.driveScoped(in, func() bool { return sit.resumed }); err != nil {
		in.sched.unpark(sit)
		sit.parked = false
		if errors.Is(err, errAsyncDeadlock) {
			// Nothing in the instance can ever resume this thread, so the
			// sync call can never return. That is the specification's
			// sync-task-block trap, and the wording the suites assert.
			panic(fmt.Errorf("component/instance: %s: cannot block a synchronous task before returning", name))
		}
		panic(fmt.Errorf("component/instance: %s: %w", name, err))
	}
}

// syncBlocker is a sync task's implicit thread presented as a taskBlocker, so
// every blocking built-in can park it through the one interface it already uses
// for a stackful task or a promoted callback segment.
//
// It exists because whether a non-async-typed call may block is NOT a static
// property of the call. canon_lift's sync-caller loop is
//
//	while task.state != Task.State.RESOLVED:
//	  candidates = { t for t in inst.threads if t.ready() }
//	  trap_if(not candidates)
//	  random.choice(list(candidates)).resume()
//
// so blocking traps only when, at the moment of blocking, no other thread of
// this instance is ready to run -- "Whether a thread may synchronously call an
// async-typed function does not depend on the thread's task's function type ...
// Rather, it depends on whether, at the point of blocking, there are any other
// threads that are ready to run" (during-sync-call-may-block-if-other-ready-
// threads.wast). wazy used to trap eagerly at every blocking built-in the
// moment it saw a syncImplicit task, which is the older revision's rule.
//
// suspendSyncThread performs exactly that loop, instance-scoped, and raises the
// spec's sync-block trap if and only if it finds no candidate.
type syncBlocker struct {
	in   *Instance
	t    *task
	name string
}

// sit returns this task's implicit-thread record, creating and registering it on
// first use. Registration replaces the implicitThreadMarker in the task's
// thread-table slot (allocating the slot if thread.index never did), so every
// switch built-in that resolves that index reaches the real thread object.
func (b syncBlocker) sit() *syncImplicitThread {
	if b.t.syncThread == nil {
		b.t.syncThread = &syncImplicitThread{t: b.t, in: b.in}
		if b.t.implicitThreadIdx == 0 {
			b.t.implicitThreadIdx = b.in.threads.add(b.t.syncThread)
		} else {
			b.in.threads.replace(b.t.implicitThreadIdx, b.t.syncThread)
		}
	}
	return b.t.syncThread
}

// block never reports a cancellation: the reference delivers one only in the
// callback dispatch loop and wait_from_callback, neither of which a sync task
// has (see guestThread.block's note).
func (b syncBlocker) block(ready func() bool) (cancelled bool) {
	b.blockVoid(ready)
	return false
}

// blockVoid is block without the never-true cancellation result, so
// captureSuspender can hand it straight back as its park func.
func (b syncBlocker) blockVoid(ready func() bool) {
	suspendSyncThread(b.in, b.t, b.name, ready)
}

// runSyncCore runs a non-async-typed lift's core call.
//
// canon_lift gives EVERY call its own Thread with its own continuation, and for
// a non-async-typed one it then drives
//
//	while task.state != Task.State.RESOLVED:
//	  candidates = { t for t in inst.threads if t.ready() }
//	  trap_if(not candidates)
//	  random.choice(list(candidates)).resume()
//
// (definitions.py ~2116-2122). Two properties matter and wazy only had the
// second: the call's thread is independently resumable, and blocking traps only
// when no thread of this instance is ready.
//
// Running the core call inline on the caller's goroutine gives up the first one.
// Its frames are then pinned beneath whatever it switches to, and the only way
// to resume it is a scheduler drive nested inside those frames -- which cannot
// reach the thread when something ELSE is mid-call on top of it. That is
// reentrance.wast's last case: run suspends into an explicit thread, the
// explicit thread reenters the same instance, and the reentrant call's own
// thread switches back to run. So when this instance binds a canon that can
// suspend, the core call gets its own baton goroutine and this function is the
// sync-caller loop above.
//
// Instances that bind no such canon (the overwhelming majority: no thread.*, no
// waitable-set.wait, no sync stream/future copy, no sync lower of an async lift)
// keep calling straight through with no goroutine and no scheduler at all.
// The gate is mayThreadSwitch, not mayBlockSync: a continuation is only needed
// when something can resume THIS thread, which means a cooperative-thread canon
// is bound in this instance. A sync lift that merely blocks (a sync stream copy,
// a sync lower of an async lift, waitable-set.wait) is resumed by its own nested
// drive and needs no goroutine.
func (in *Instance) runSyncCore(t *task, exportName string, call func() error) error {
	// t == nil means this instance never needed task tracking, so there is no
	// thread to give a continuation to (and nothing in it can suspend).
	// Callers test the gate themselves (invokeEntered) so the closure they pass
	// is only built when it is actually needed; this is belt and braces.
	if t == nil || !in.mayThreadSwitch {
		return call()
	}
	sit := syncBlocker{in: in, t: t, name: exportName}.sit()
	sit.spawned = true
	sit.resumeCh, sit.yieldCh = make(chan resumeMode), make(chan struct{})
	go sit.main(call)

	if err := sit.handoff(resumeNormal); err != nil {
		return err
	}
	if sit.done {
		return sit.err
	}
	// Parked: drive this instance's own ready threads until the call finishes.
	// driveScoped is the `candidates = { t for t in inst.threads ... }`
	// restriction (during-sync-call-no-sibling-resume), and its no-candidate
	// result is the loop's trap_if.
	if err := in.sched.driveScoped(in, func() bool { return sit.done }); err != nil {
		in.sched.unpark(sit)
		sit.parked = false
		in.abortSyncThread(sit)
		if errors.Is(err, errAsyncDeadlock) {
			return fmt.Errorf("cannot block a synchronous task before returning")
		}
		return err
	}
	return sit.err
}

// abortSyncThread unwinds a sync thread's goroutine that can never be resumed,
// the way reapParkedGoroutines unwinds a stranded stackful task: hand it
// resumeAbort so block() panics errStackfulAbort, the engine's recover turns
// that into a returned error, and the goroutine exits.
func (in *Instance) abortSyncThread(sit *syncImplicitThread) {
	if !sit.spawned || sit.done {
		return
	}
	sit.resumeCh <- resumeAbort
	<-sit.yieldCh
}

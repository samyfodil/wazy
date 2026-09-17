package instance

import "errors"

// errAsyncDeadlock is the single-threaded translation of canon_lift's
// sync-caller loop's trap_if(not candidates) (definitions.py ~2200): the
// guest is suspended on a condition no queued work can ever satisfy. See
// sched.drive.
var errAsyncDeadlock = errors.New("async deadlock: no runnable work")

// sched replaces the reference's Store.waiting + Store.tick() and
// canon_lift's sync-caller resume loop
// (docs/component-model-async-runtime-design.md §0/§1.1,
// docs/component-model-async-phase3-design.md §0). It is not a full thread
// scheduler: runq entries are host-side completion thunks (deferred async
// import resolutions); parked entries are tasks suspended mid callback-loop
// (guest_task.go) or mid stackful-lift core call (stackful_task.go) waiting
// on backpressure, a waitable set, or a yield round. Phase 3 shares ONE
// *sched per composition tree (Instance.sched's doc) so a guest<->guest
// async lower's WAIT drive on the caller side can resume the callee's
// parked task.
type sched struct {
	runq []schedThunk

	// pumping is true while a thunk from runq -- or a parked task's
	// resumeReady -- is executing. AsyncCall.Resolve checks it to tell
	// "called from inside the scheduler" (legal) apart from "called from an
	// arbitrary goroutine" (not, without CallAsync).
	pumping bool

	// parked holds every task currently suspended -- a callback lift's
	// *guestTask (guest_task.go) or a stackful lift's *stackfulTask
	// (stackful_task.go's docs/component-model-async-stackful-design.md §3),
	// in registration order -- step resumes the first one whose ready()
	// predicate holds, which is the deterministic profile's own resume
	// order (plan §6's random.choice -> first monkeypatch).
	parked []parkedTask

	// pendingSwitch is the reference's Handler.switch_to (definitions.py
	// ~269-277): the thread a switch built-in asked to run next.
	//
	// suspend(switch_to = other) captures the switcher's continuation and hands
	// the scheduler the target in one step -- the switcher is parked BEFORE
	// other runs, and other is resumed by the scheduler rather than from inside
	// the switcher's own frame. wazy used to resume other inline instead, which
	// left the switcher's goroutine blocked in a nested baton handoff: fine
	// until something wanted to resume the switcher itself, which a reentrant
	// call legitimately does (reentrance.wast's last case). Parking first and
	// letting the next step perform the switch keeps every suspended thread
	// resumable from anywhere, which is the whole point of a continuation.
	pendingSwitch parkedTask

	// instantiating is true while the root Instantiate is running core-module
	// start functions (docs/component-model-async-stackful-design.md §4.3).
	// One flag on the SHARED sched (one per composition tree) so every
	// sub-Instance's builtins/wrappers see it -- a sync-lowered call from a
	// start function to an async lift must trap eagerly, before the
	// callee's core code ever runs, only during this window.
	instantiating bool
}

// parkedTask is what sched can park/resume: a callback guestTask (its parked
// state is plain Go fields) or a stackfulTask (its parked state is a live
// goroutine blocked on its resume channel) --
// docs/component-model-async-stackful-design.md §3.
type parkedTask interface {
	ready() bool
	resumeReady() error
	// instance is which component instance this park belongs to, so a sync
	// call's drive can restrict its candidates to that instance's own threads
	// -- see stepScoped.
	instance() *Instance
}

// schedThunk is one runq entry: exactly one of fe/fv is set. Storing both as
// plain fields (rather than a single func() error) lets AsyncCall.Defer
// (async_host_import.go) enqueue its func() directly -- fv -- with no
// wrapping closure; enqueue's generic func() error callers (mostly tests
// exercising sched's own error-propagation contract) still get fe. A
// schedThunk value lives inline in runq's backing array, so appending one
// costs no allocation beyond the slice's own (amortized, see step's reuse of
// the backing array).
type schedThunk struct {
	fe func() error
	fv func()
}

func (t schedThunk) run() error {
	if t.fv != nil {
		t.fv()
		return nil
	}
	return t.fe()
}

// enqueue appends a completion thunk to the FIFO run queue.
func (s *sched) enqueue(f func() error) { s.runq = append(s.runq, schedThunk{fe: f}) }

// enqueueVoid appends a completion thunk that never fails -- AsyncCall.Defer's
// case, and the only production caller. Distinct from enqueue purely to avoid
// wrapping fn in a func() error adapter closure (a real per-call allocation
// on the async-import hot path): fn is stored directly in the schedThunk
// value, not captured by a new closure.
func (s *sched) enqueueVoid(f func()) { s.runq = append(s.runq, schedThunk{fv: f}) }

// park registers pt as suspended; step/pumpSnapshot may resume it once its
// ready() predicate holds.
func (s *sched) park(pt parkedTask) { s.parked = append(s.parked, pt) }

// unpark removes pt from the parked set (by identity). A no-op if pt is not
// currently parked (defensive; every caller unparks exactly once).
func (s *sched) unpark(pt parkedTask) {
	for i, p := range s.parked {
		if p == pt {
			s.parked = append(s.parked[:i], s.parked[i+1:]...)
			return
		}
	}
}

// step pops and runs one runq thunk, or -- if the queue is empty -- resumes
// the first ready parked task. progressed=false means neither had any
// work to do.
func (s *sched) step() (progressed bool, err error) { return s.stepScoped(nil) }

// stepScoped is step restricted to one instance's parked threads.
//
// canon_lift's sync-caller loop draws its candidates from `inst.threads`, not
// from the whole store (definitions.py ~2119) -- while a non-async-typed export
// call is in progress, the runtime resumes threads of THAT instance only, and
// ready threads of sibling instances wait for the top-level event loop
// (during-sync-call-no-sibling-resume.wast). Passing only == nil is the
// top-level loop: every instance is a candidate.
//
// runq thunks are not threads: they are host-side completions (a deferred async
// import resolving), which in the reference happen outside this loop entirely,
// so they are never scoped out.
func (s *sched) stepScoped(only *Instance) (progressed bool, err error) {
	// An explicit switch runs first and is never scoped out: the switcher named
	// this thread, so it is not a candidate being chosen but a transfer already
	// decided (see pendingSwitch).
	if pt := s.pendingSwitch; pt != nil {
		s.pendingSwitch = nil
		s.pumping = true
		err = pt.resumeReady()
		s.pumping = false
		return true, err
	}
	if len(s.runq) > 0 {
		th := s.runq[0]
		// Shift the remaining entries down in place (not s.runq[1:]) so the
		// backing array's capacity survives the pop: a re-slice-from-front
		// would advance the slice header past freed capacity, forcing the
		// next enqueue to grow (reallocate) even though this array has
		// plenty of room. The queue is typically 0-1 entries deep (one
		// deferred completion at a time), so the shift is O(1) in practice.
		n := copy(s.runq, s.runq[1:])
		s.runq[n] = schedThunk{} // drop the trailing duplicate's func refs
		s.runq = s.runq[:n]
		s.pumping = true
		err = th.run()
		s.pumping = false
		return true, err
	}
	for _, pt := range s.parked {
		if only != nil && pt.instance() != only {
			continue
		}
		if pt.ready() {
			s.pumping = true
			err = pt.resumeReady()
			s.pumping = false
			return true, err
		}
	}
	return false, nil
}

// drive pumps the queue FIFO until pred holds. An empty queue with pred
// still false, and no parked task ready, is a guaranteed-permanent deadlock
// (nothing else can ever run to make it true) => errAsyncDeadlock; callers
// wrap it with export/waitable-set context.
func (s *sched) drive(pred func() bool) error { return s.driveScoped(nil, pred) }

// driveScoped is drive with stepScoped's instance restriction.
func (s *sched) driveScoped(only *Instance, pred func() bool) error {
	for !pred() {
		progressed, err := s.stepScoped(only)
		if err != nil {
			return err
		}
		if !progressed {
			return errAsyncDeadlock
		}
	}
	return nil
}

// driveToQuiescence pumps the queue and ready parked tasks until pred holds
// (returns done=true) or no work remains (done=false: the task is parked
// awaiting input a future step cannot supply -- for CallAsync, an external
// import completion). Unlike drive, an empty runq with pred still false is NOT
// an error here (that "deadlock" is exactly the point where CallAsync hands
// control back to the host).
func (s *sched) driveToQuiescence(pred func() bool) (done bool, err error) {
	for {
		if pred() {
			return true, nil
		}
		progressed, e := s.step()
		if e != nil {
			return false, e
		}
		if !progressed {
			return false, nil
		}
	}
}

// drainReady runs every already-ready parked task (and any thunks their
// progress enqueues) to quiescence -- the reference Store.tick loop's
// "keep ticking after the sync caller's own task completes" behavior
// (Feature 1, docs/component-model-async-final3-fable.md §1.5), so e.g.
// sync-streams' promoted $C.get/$C.set run to EXIT (and their segment
// goroutines exit) before the host-entry invoke that drove $D returns.
// Stops at the first no-progress round; permanently-unready tasks stay
// parked (they may legitimately wake on a LATER invoke -- reference threads
// persist across ticks) and are reaped only at Close/an invoke error path.
// A no-op whenever nothing is ready, so every currently-green suite where
// the drive already quiesced everything sees zero behavior change.
func (s *sched) drainReady() error {
	// Bounded by the parked set as it stands on entry, not run to a fixed
	// point: a callback loop that re-parks itself ready on every resume (a
	// bare YIELD) is ready again the instant it is stepped, so an unbounded
	// drain would never return. One pass per parked task is what "drain what
	// is already ready" means.
	for n := len(s.parked) + len(s.runq); n > 0; n-- {
		progressed, err := s.step()
		if err != nil {
			return err
		}
		if !progressed {
			return nil
		}
	}
	return nil
}

// pumpSnapshot runs exactly the thunks queued at entry, not ones they
// enqueue, THEN resumes each guestTask that was already ready at entry,
// once each: one deterministic scheduler round, used by CallbackCode.YIELD
// (parkYield). Mirrors the reference's deterministic profile, where a
// yielded thread re-enters the waiting list BEHIND already-ready threads, so
// all of them -- runq thunks and other ready parked tasks alike -- run once
// before the yielder resumes.
func (s *sched) pumpSnapshot() error {
	for n := len(s.runq); n > 0; n-- {
		if _, err := s.step(); err != nil {
			return err
		}
	}
	readyNow := make([]parkedTask, 0, len(s.parked))
	for _, pt := range s.parked {
		if pt.ready() {
			readyNow = append(readyNow, pt)
		}
	}
	for _, pt := range readyNow {
		// pt may have been unparked/reparked by an earlier iteration of
		// this very loop (e.g. a resumed task's own progress unparks a
		// third task) -- re-check membership+ready before resuming.
		if !s.isParked(pt) || !pt.ready() {
			continue
		}
		s.pumping = true
		err := pt.resumeReady()
		s.pumping = false
		if err != nil {
			return err
		}
	}
	return nil
}

// resumeOneReady resumes the FIRST ready parked task of only (every instance
// when only is nil) and reports whether it found one. It is
// Task.request_cancellation's body under DETERMINISTIC_PROFILE
// (definitions.py ~477-484):
//
//	while self.state != Task.State.RESOLVED:
//	  candidates = { t for t in self.inst.threads if t.ready() }
//	  if candidates: random.choice(list(candidates)).resume()
//	  if not candidates or DETERMINISTIC_PROFILE or ...: break
//
// -- one resume, from the whole INSTANCE's ready threads and not just the
// cancelled task's (cancel-instance-wide-resume.wast turns that distinction
// into an assertion), then out of the loop. Unlike step it never runs a runq
// thunk: the reference resumes a thread here, nothing else.
func (s *sched) resumeOneReady(only *Instance) (resumed bool, err error) {
	for _, pt := range s.parked {
		if only != nil && pt.instance() != only {
			continue
		}
		if pt.ready() {
			s.pumping = true
			err = pt.resumeReady()
			s.pumping = false
			return true, err
		}
	}
	return false, nil
}

// isParked reports whether pt is currently in s.parked.
func (s *sched) isParked(pt parkedTask) bool {
	for _, p := range s.parked {
		if p == pt {
			return true
		}
	}
	return false
}

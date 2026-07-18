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
func (s *sched) step() (progressed bool, err error) {
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
func (s *sched) drive(pred func() bool) error {
	for !pred() {
		progressed, err := s.step()
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
	for {
		progressed, err := s.step()
		if err != nil {
			return err
		}
		if !progressed {
			return nil
		}
	}
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

// isParked reports whether pt is currently in s.parked.
func (s *sched) isParked(pt parkedTask) bool {
	for _, p := range s.parked {
		if p == pt {
			return true
		}
	}
	return false
}

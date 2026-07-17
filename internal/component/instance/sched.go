package instance

import "errors"

// errAsyncDeadlock is the single-threaded translation of canon_lift's
// sync-caller loop's trap_if(not candidates) (definitions.py ~2200): the
// guest is suspended on a condition no queued work can ever satisfy. See
// sched.drive.
var errAsyncDeadlock = errors.New("async deadlock: no runnable work")

// sched replaces the reference's Store.waiting + Store.tick() and
// canon_lift's sync-caller resume loop
// (docs/component-model-async-runtime-design.md §0/§1.1). It is NOT a
// thread scheduler: entries are host-side completion thunks (deferred async
// import resolutions -- Phase 1c). Nothing enqueues a thunk yet in this
// milestone (first-light never WAITs or calls a host import), so runq stays
// empty and every method here is exercised only by direct sched tests --
// it exists now so Phase 1c's WAIT/host-import wiring drops in without a
// scheduler redesign. One sched per Instance, used only while an async task
// is active.
type sched struct {
	runq []func() error

	// pumping is true while a thunk from runq is executing. Phase 1c's
	// AsyncCall.Resolve checks it to tell "called from inside the scheduler"
	// (legal) apart from "called from an arbitrary goroutine" (not, without
	// CallAsync). Unused by anything in this milestone.
	pumping bool
}

// enqueue appends a completion thunk to the FIFO run queue.
func (s *sched) enqueue(f func() error) { s.runq = append(s.runq, f) }

// step pops and runs one thunk. progressed=false means the queue was empty.
func (s *sched) step() (progressed bool, err error) {
	if len(s.runq) == 0 {
		return false, nil
	}
	f := s.runq[0]
	s.runq[0] = nil
	s.runq = s.runq[1:]
	s.pumping = true
	err = f()
	s.pumping = false
	return true, err
}

// drive pumps the queue FIFO until pred holds. An empty queue with pred
// still false is a guaranteed-permanent deadlock (nothing else can ever run
// to make it true) => errAsyncDeadlock; callers wrap it with
// export/waitable-set context.
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

// pumpSnapshot runs exactly the thunks queued at entry, not ones they
// enqueue: one deterministic scheduler round, used by CallbackCode.YIELD.
// Mirrors the reference's deterministic profile, where a yielded thread
// re-enters the waiting list BEHIND already-ready threads, so all of them
// run once before the yielder resumes.
func (s *sched) pumpSnapshot() error {
	for n := len(s.runq); n > 0; n-- {
		if _, err := s.step(); err != nil {
			return err
		}
	}
	return nil
}

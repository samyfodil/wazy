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

	// waiting is the reference's start_waiting: resume-later sets it, making
	// this thread ready for the driver to pick. resumed is set when the driver
	// actually resumes it, which is what the nested drive below waits for.
	waiting bool
	resumed bool
}

func (s *syncImplicitThread) ready() bool { return s.waiting }

func (s *syncImplicitThread) resumeReady() error {
	s.in.sched.unpark(s)
	s.waiting, s.resumed = false, true
	return nil
}

// suspendedState is the reference's Thread.suspended(): parked with no
// ready_func. A sync implicit thread is parked for its whole suspension, so
// this is just "not yet made ready".
func (s *syncImplicitThread) suspendedState() bool { return !s.waiting }

func (s *syncImplicitThread) startWaiting() { s.waiting = true }
func (s *syncImplicitThread) stopWaiting()  { s.waiting = false }

// suspendSyncThread parks this sync task's implicit thread and drives the
// scheduler until something resumes it, which is the whole point: while it is
// parked, every other ready thread of the instance gets to run.
func suspendSyncThread(in *Instance, t *task, name string) {
	sit := &syncImplicitThread{t: t, in: in}
	if t.implicitThreadIdx == 0 {
		t.implicitThreadIdx = in.threads.add(sit)
	} else {
		in.threads.replace(t.implicitThreadIdx, sit)
	}
	in.sched.park(sit)
	if err := in.sched.drive(func() bool { return sit.resumed }); err != nil {
		in.sched.unpark(sit)
		if errors.Is(err, errAsyncDeadlock) {
			// Nothing in the instance can ever resume this thread, so the
			// sync call can never return. That is the specification's
			// sync-task-block trap, and the wording the suites assert.
			panic(fmt.Errorf("component/instance: %s: cannot block a synchronous task before returning", name))
		}
		panic(fmt.Errorf("component/instance: %s: %w", name, err))
	}
}

package instance

import (
	"errors"
	"testing"
)

func TestSched_DriveDeadlockOnEmptyQueue(t *testing.T) {
	var s sched
	err := s.drive(func() bool { return false })
	if !errors.Is(err, errAsyncDeadlock) {
		t.Fatalf("drive on empty queue = %v, want errAsyncDeadlock", err)
	}
}

func TestSched_DrivePredAlreadyTrue(t *testing.T) {
	var s sched
	ran := false
	s.enqueue(func() error { ran = true; return nil })
	if err := s.drive(func() bool { return true }); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if ran {
		t.Fatal("drive ran a thunk even though pred was already true")
	}
}

func TestSched_DriveRunsThunksFIFOUntilPredTrue(t *testing.T) {
	var s sched
	var order []int
	done := false
	s.enqueue(func() error { order = append(order, 1); return nil })
	s.enqueue(func() error { order = append(order, 2); done = true; return nil })
	s.enqueue(func() error { order = append(order, 3); return nil }) // never runs: pred goes true after thunk 2

	if err := s.drive(func() bool { return done }); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("order = %v, want [1 2]", order)
	}
	if len(s.runq) != 1 {
		t.Fatalf("runq len = %d, want 1 (thunk 3 left queued)", len(s.runq))
	}
}

func TestSched_DrivePropagatesThunkError(t *testing.T) {
	var s sched
	boom := errors.New("boom")
	s.enqueue(func() error { return boom })
	if err := s.drive(func() bool { return false }); !errors.Is(err, boom) {
		t.Fatalf("drive = %v, want boom", err)
	}
}

func TestSched_DriveDeadlockAfterQueueDrains(t *testing.T) {
	var s sched
	s.enqueue(func() error { return nil })
	err := s.drive(func() bool { return false }) // pred never true, queue drains after 1 thunk
	if !errors.Is(err, errAsyncDeadlock) {
		t.Fatalf("drive = %v, want errAsyncDeadlock", err)
	}
}

func TestSched_StepEmptyQueue(t *testing.T) {
	var s sched
	progressed, err := s.step()
	if progressed || err != nil {
		t.Fatalf("step on empty queue = (%v, %v), want (false, nil)", progressed, err)
	}
}

func TestSched_StepSetsPumpingDuringThunk(t *testing.T) {
	var s sched
	var pumpingDuring bool
	s.enqueue(func() error { pumpingDuring = s.pumping; return nil })
	if _, err := s.step(); err != nil {
		t.Fatalf("step: %v", err)
	}
	if !pumpingDuring {
		t.Fatal("s.pumping was false while the thunk was executing")
	}
	if s.pumping {
		t.Fatal("s.pumping stayed true after step returned")
	}
}

// TestSched_PumpSnapshotIgnoresThunksEnqueuedDuringTheRound pins the YIELD
// semantics (docs/component-model-async-runtime-design.md §4 point 1): a
// thunk enqueued BY a thunk that's running during pumpSnapshot must NOT run
// in the same round -- it waits for the yielder's NEXT yield.
func TestSched_PumpSnapshotIgnoresThunksEnqueuedDuringTheRound(t *testing.T) {
	var s sched
	var ran []int
	s.enqueue(func() error {
		ran = append(ran, 1)
		s.enqueue(func() error { ran = append(ran, 2); return nil }) // queued for the NEXT round
		return nil
	})
	if err := s.pumpSnapshot(); err != nil {
		t.Fatalf("pumpSnapshot: %v", err)
	}
	if len(ran) != 1 || ran[0] != 1 {
		t.Fatalf("ran = %v, want [1] (thunk 2 must not run this round)", ran)
	}
	if len(s.runq) != 1 {
		t.Fatalf("runq len = %d, want 1 (thunk 2 still queued)", len(s.runq))
	}

	// A second round picks up the deferred thunk.
	if err := s.pumpSnapshot(); err != nil {
		t.Fatalf("pumpSnapshot (round 2): %v", err)
	}
	if len(ran) != 2 || ran[1] != 2 {
		t.Fatalf("ran = %v, want [1 2] after round 2", ran)
	}
}

func TestSched_PumpSnapshotPropagatesError(t *testing.T) {
	var s sched
	boom := errors.New("boom")
	s.enqueue(func() error { return nil })
	s.enqueue(func() error { return boom })
	if err := s.pumpSnapshot(); !errors.Is(err, boom) {
		t.Fatalf("pumpSnapshot = %v, want boom", err)
	}
}

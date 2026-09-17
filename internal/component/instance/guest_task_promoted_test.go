package instance

import "testing"

// This file exercises Feature 1 (callback-task parking,
// docs/component-model-async-final3-fable.md §1) at the guestTask/sched
// level directly, hand-building a PROMOTED guestTask the way
// stackful_task_test.go's TestStackfulReap_* suite does for stackfulTask --
// no wasm needed, per §1.8 item 1's own prescription ("a hand-built
// promoted guestTask whose segment fn calls gt.block(pred); assert
// park/resume/abort transitions + goroutine delta 0").

// newPromotedGuestTask builds a minimal (*task, *guestTask) pair wired the
// way startAsyncExportTask wires a promoted callback task, without any real
// wasm core/callback funcs -- callers drive it via gt.runSegment(fn)
// directly instead of gt.start()/gt.firstRun().
func newPromotedGuestTask(in *Instance) (*task, *guestTask) {
	t := &task{inst: in, state: taskStarted}
	gt := &guestTask{t: t, in: in, promoted: true}
	t.gt = gt
	return t, gt
}

// TestPromotedGuestTask_BlockParkResumeAbort is Feature 1's §1.8 item 1
// acceptance test: a promoted guestTask's segment calls gt.block, parking
// with a live goroutine (parkBlocked); resuming it once ready() holds hands
// the baton back and the segment goroutine exits (goroutine count returns
// to baseline).
func TestPromotedGuestTask_BlockParkResumeAbort(t *testing.T) {
	in := newAsyncInst()
	_, gt := newPromotedGuestTask(in)

	before := numGoroutineStable(t)

	ready := false
	blockResult := make(chan bool, 1)
	if err := gt.runSegment(func() error {
		cancelled := gt.block(func() bool { return ready })
		blockResult <- cancelled
		return nil
	}); err != nil {
		t.Fatalf("runSegment: %v", err)
	}

	// The segment parked mid-call: gt is registered in sched.parked at
	// parkBlocked, with a live goroutine -- unlike an unpromoted task's
	// frame-free parks, this one pins a real goroutine until resumed.
	if gt.park != parkBlocked {
		t.Fatalf("park = %v, want parkBlocked", gt.park)
	}
	if gt.seg == nil {
		t.Fatal("seg is nil, want a live segment while parked")
	}
	if !in.sched.isParked(gt) {
		t.Fatal("gt not registered in sched.parked")
	}
	if gt.ready() {
		t.Fatal("ready() true before the predicate is satisfied")
	}

	during := waitGoroutines(t, func(n int) bool { return n > before })
	if during <= before {
		t.Fatalf("goroutine count = %d during park, want > baseline %d (segment goroutine should be live)", during, before)
	}

	ready = true
	if !gt.ready() {
		t.Fatal("ready() false after the predicate is satisfied")
	}
	if err := gt.resumeReady(); err != nil {
		t.Fatalf("resumeReady: %v", err)
	}

	select {
	case cancelled := <-blockResult:
		if cancelled {
			t.Fatal("block() reported cancelled, want false (a plain ready-predicate resume)")
		}
	default:
		t.Fatal("segment did not run to completion synchronously within resumeReady")
	}
	if gt.seg != nil {
		t.Fatal("seg should be nil once the segment finishes (frame-free again)")
	}
	if in.sched.isParked(gt) {
		t.Fatal("gt should be unparked after resumeReady")
	}

	after := waitGoroutines(t, func(n int) bool { return n <= before })
	if after > before {
		t.Fatalf("goroutine count after resume = %d, want <= baseline %d (segment goroutine leaked)", after, before)
	}
}

// TestPromotedGuestTask_ReapAbortsParkedSegment mirrors
// TestStackfulReap_CloseReapsParkedGoroutine for a promoted guestTask:
// reapParkedGoroutines (Instance.Close's own mechanism) must abort a
// parkBlocked guestTask's live segment goroutine, not just a stackfulTask's.
func TestPromotedGuestTask_ReapAbortsParkedSegment(t *testing.T) {
	in := newAsyncInst()
	_, gt := newPromotedGuestTask(in)

	before := numGoroutineStable(t)

	blockResult := make(chan bool, 1)
	if err := gt.runSegment(func() error {
		cancelled := gt.block(func() bool { return false }) // never ready on its own
		blockResult <- cancelled
		return nil
	}); err != nil {
		t.Fatalf("runSegment: %v", err)
	}

	during := waitGoroutines(t, func(n int) bool { return n > before })
	if during <= before {
		t.Fatalf("goroutine count = %d during park, want > baseline %d", during, before)
	}

	in.reapParkedGoroutines()

	if gt.seg != nil {
		t.Fatal("seg should be nil after reap (goroutine fully unwound)")
	}
	if in.sched.isParked(gt) {
		t.Fatal("gt should be unparked after reap")
	}
	// block() panicked errStackfulAbort inside the segment (recovered by
	// seg.main, never reaching the "cancelled <- blockResult" send below
	// it) -- the closure's own send is unreachable on the abort path,
	// matching stackfulTask's reap (block() never returns on abort, it
	// panics).
	select {
	case <-blockResult:
		t.Fatal("blockResult received a value -- gt.block returned normally instead of panicking on abort")
	default:
	}

	after := waitGoroutines(t, func(n int) bool { return n <= before })
	if after > before {
		t.Fatalf("goroutine count after reap = %d, want <= baseline %d (parked segment goroutine leaked)", after, before)
	}

	// Idempotent: reaping again (nothing left parked) must not hang.
	in.reapParkedGoroutines()
}

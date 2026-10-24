package wasip2

import (
	"context"
	"testing"
	"time"

	"github.com/samyfodil/wazy/component/componenttest"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// newTestPoll returns a wasiPoll wired to a fresh handle table, as Instantiate
// would, with the wall clock pinned to `when`.
func newTestPoll(when time.Time) *wasiPoll {
	p := newWasiPoll(func() time.Time { return when })
	p.setResources(componenttest.New().Resources())
	return p
}

// pollHandle mints an own<pollable> handle for rep in p's resource table.
func pollHandle(t *testing.T, p *wasiPoll, rep uint32) uint32 {
	t.Helper()
	res, err := p.getResources()
	if err != nil {
		t.Fatal(err)
	}
	return res.NewOwn(wasiPollableResType, rep)
}

func TestPoll_WallClock(t *testing.T) {
	when := time.Unix(1_700_000_123, 456)
	p := newTestPoll(when)
	res, err := p.wallNow(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	dt := res[0].([]abi.Value)
	if dt[0].(uint64) != uint64(when.Unix()) || dt[1].(uint32) != uint32(when.UTC().Nanosecond()) {
		t.Fatalf("wallNow = %v, want seconds=%d nanos=%d", dt, when.Unix(), when.Nanosecond())
	}
	// resolution -> datetime {0, 1}
	rres, _ := p.wallResolution(context.Background(), nil)
	rdt := rres[0].([]abi.Value)
	if rdt[0].(uint64) != 0 || rdt[1].(uint32) != 1 {
		t.Fatalf("wallResolution = %v, want {0,1}", rdt)
	}
}

func TestPoll_MonotonicNowAndResolution(t *testing.T) {
	p := newTestPoll(time.Now())
	res, err := p.monotonicNow(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res[0].(uint64); !ok {
		t.Fatalf("monotonicNow returned %T, want uint64", res[0])
	}
	rres, _ := p.monotonicResolution(context.Background(), nil)
	if rres[0].(uint64) != 1 {
		t.Fatalf("monotonicResolution = %v, want 1", rres[0])
	}
}

func TestPoll_SubscribeArgErrors(t *testing.T) {
	p := newTestPoll(time.Now())
	if _, err := p.subscribeDuration(context.Background(), nil); err == nil {
		t.Fatal("expected arg-count error")
	}
	if _, err := p.subscribeDuration(context.Background(), []abi.Value{"x"}); err == nil {
		t.Fatal("expected type error")
	}
	if _, err := p.subscribeInstant(context.Background(), []abi.Value{uint32(1)}); err == nil {
		t.Fatal("expected type error (needs uint64)")
	}
}

func TestPoll_BlockTimerSleeps(t *testing.T) {
	p := newTestPoll(time.Now())
	// subscribe-duration for 40ms -> a timer pollable rep; block must sleep.
	res, err := p.subscribeDuration(context.Background(), []abi.Value{uint64(40 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	rep := res[0].(uint32)
	deadline, ok := p.deadlineOf(rep)
	if !ok {
		t.Fatal("timer deadline not found")
	}
	if _, err := p.block(context.Background(), []abi.Value{rep}); err != nil {
		t.Fatal(err)
	}
	if returnedAt := time.Now(); returnedAt.Before(deadline) {
		t.Fatalf("block returned at %v, before timer deadline %v", returnedAt, deadline)
	}
}

func TestPoll_BlockAlwaysReadyNoOp(t *testing.T) {
	p := newTestPoll(time.Now())
	start := time.Now()
	// wasiPollableRep is the always-ready singleton: block returns immediately.
	if _, err := p.block(context.Background(), []abi.Value{wasiPollableRep}); err != nil {
		t.Fatal(err)
	}
	// The always-ready singleton carries no deadline, so a broken fast path
	// would not sleep for a bounded time -- it would block until the context
	// died, i.e. hang. That, not a few milliseconds, is the failure this
	// guards against, so the budget only has to sit clear of scheduler noise
	// on a loaded shared runner. The original 20ms did not: it flaked at
	// 24.6ms on windows-2025 CI.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("block on always-ready pollable took %v, want ~0", elapsed)
	}
	// arg errors
	if _, err := p.block(context.Background(), nil); err == nil {
		t.Fatal("expected arg-count error")
	}
	if _, err := p.block(context.Background(), []abi.Value{"x"}); err == nil {
		t.Fatal("expected type error")
	}
}

func TestPoll_BlockCtxCancel(t *testing.T) {
	p := newTestPoll(time.Now())
	// An hour-long timer, cancelled immediately, must return before it is
	// due: woken by ctx, not by the timer, however slow the machine.
	res, _ := p.subscribeInstant(context.Background(), []abi.Value{uint64(p.base.Add(time.Hour).Sub(p.base))})
	rep := res[0].(uint32)
	deadline, _ := p.deadlineOf(rep)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.block(ctx, []abi.Value{rep}); err != nil {
		t.Fatal(err)
	}
	if returnedAt := time.Now(); !returnedAt.Before(deadline) {
		t.Fatalf("cancelled block returned at %v, not before the timer's deadline %v", returnedAt, deadline)
	}
}

func TestPoll_PollReadyImmediate(t *testing.T) {
	p := newTestPoll(time.Now())
	h1 := pollHandle(t, p, wasiPollableRep)
	h2 := pollHandle(t, p, wasiPollableRep)
	res, err := p.poll(context.Background(), []abi.Value{[]abi.Value{h1, h2}})
	if err != nil {
		t.Fatal(err)
	}
	out := res[0].([]abi.Value)
	if len(out) != 2 || out[0].(uint32) != 0 || out[1].(uint32) != 1 {
		t.Fatalf("poll ready indices = %v, want [0 1]", out)
	}
}

// TestPoll_PollBlocksUntilTimer pins that poll, with nothing ready, sleeps
// until the earliest timer and reports exactly the timers then due.
//
// The later timer is an hour out, not a few milliseconds: poll's wakeup can
// be late by however long the scheduler takes, and a later timer only tens
// of milliseconds after the sooner one was due too by the time a loaded
// machine woke the test -- which then saw two timers ready, not one. An
// hour cannot pass that way, so exactly one is due. ctx bounds the wait
// instead: a poll that slept to the later deadline is woken by it with
// nothing due, which fails as surely as the near timer once did.
func TestPoll_PollBlocksUntilTimer(t *testing.T) {
	p := newTestPoll(time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r1, _ := p.subscribeDuration(ctx, []abi.Value{uint64(time.Hour)})
	r2, _ := p.subscribeDuration(ctx, []abi.Value{uint64(20 * time.Millisecond)})
	h1 := pollHandle(t, p, r1[0].(uint32))
	soonerRep := r2[0].(uint32)
	h2 := pollHandle(t, p, soonerRep)
	soonerDeadline, ok := p.deadlineOf(soonerRep)
	if !ok {
		t.Fatal("sooner timer deadline not found")
	}
	res, err := p.poll(ctx, []abi.Value{[]abi.Value{h1, h2}})
	if err != nil {
		t.Fatal(err)
	}
	if returnedAt := time.Now(); returnedAt.Before(soonerDeadline) {
		t.Fatalf("poll returned at %v, before sooner timer deadline %v", returnedAt, soonerDeadline)
	}
	if ctx.Err() != nil {
		t.Fatal("poll slept past the sooner timer, until ctx woke it")
	}
	out := res[0].([]abi.Value)
	// The sooner timer (index 1) must be ready; the later one (index 0) not.
	if len(out) != 1 || out[0].(uint32) != 1 {
		t.Fatalf("poll ready indices = %v, want [1] (only the sooner timer due)", out)
	}
}

func TestPoll_PollArgErrors(t *testing.T) {
	p := newTestPoll(time.Now())
	if _, err := p.poll(context.Background(), nil); err == nil {
		t.Fatal("expected arg-count error")
	}
	if _, err := p.poll(context.Background(), []abi.Value{"notalist"}); err == nil {
		t.Fatal("expected list type error")
	}
	if _, err := p.poll(context.Background(), []abi.Value{[]abi.Value{"nothandle"}}); err == nil {
		t.Fatal("expected handle type error")
	}
	if _, err := p.poll(context.Background(), []abi.Value{[]abi.Value{uint32(9999)}}); err == nil {
		t.Fatal("expected bogus-handle error")
	}
}

func TestPoll_GetResourcesUninitialized(t *testing.T) {
	p := newWasiPoll(time.Now) // no setResources
	if _, err := p.poll(context.Background(), []abi.Value{[]abi.Value{}}); err == nil {
		t.Fatal("expected error when resources not initialized")
	}
}

// TestPoll_DropPollableReleasesDeadline pins that dropping a timer pollable
// releases its deadline.
//
// Every subscribe-duration/subscribe-instant records one, and nothing removed
// it: a guest that waits on a timer -- the ordinary way to wait -- grew
// deadlines by one entry per wait for the life of the instance, without bound
// and at the guest's choosing. Subscribing repeatedly and dropping each handle
// is what separates "released" from "accumulates".
func TestPoll_DropPollableReleasesDeadline(t *testing.T) {
	p := newTestPoll(time.Now())
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		res, err := p.subscribeDuration(ctx, []abi.Value{uint64(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		rep := res[0].(uint32)
		if _, ok := p.deadlineOf(rep); !ok {
			t.Fatalf("subscribe %d: deadline not recorded", i)
		}
		if err := p.dropPollable(ctx, rep); err != nil {
			t.Fatalf("dropPollable: %v", err)
		}
		if _, ok := p.deadlineOf(rep); ok {
			t.Fatalf("subscribe %d: deadline still present after drop", i)
		}
	}

	p.mu.Lock()
	n := len(p.deadlines)
	p.mu.Unlock()
	if n != 0 {
		t.Errorf("deadlines holds %d entries after 50 subscribe/drop rounds, want 0", n)
	}
}

// TestPoll_DropAlwaysReadyPollable pins that the same destructor is harmless
// for the always-ready singleton, which every non-timer pollable shares and
// which is not in the deadlines map at all.
func TestPoll_DropAlwaysReadyPollable(t *testing.T) {
	p := newTestPoll(time.Now())
	if err := p.dropPollable(context.Background(), wasiPollableRep); err != nil {
		t.Fatalf("dropPollable(always-ready): %v", err)
	}
}

// TestSleepUntilPast pins that a deadline already past returns without
// waiting. A bound on the elapsed time would measure the scheduler too, so
// ctx bounds the wait instead: a sleep that waited out the hour's distance is
// woken by it, which then has an error.
func TestSleepUntilPast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	wasiSleepUntil(ctx, time.Now().Add(-time.Hour))
	if ctx.Err() != nil {
		t.Fatal("past deadline waited, until ctx woke it")
	}
}

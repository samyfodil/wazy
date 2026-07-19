package instance

import (
	"context"
	"testing"
	"time"

	"github.com/samyfodil/wazy/internal/component/abi"
)

// newTestPoll returns a wasiPoll wired to a fresh handle table, as Instantiate
// would, with the wall clock pinned to `when`.
func newTestPoll(when time.Time) *wasiPoll {
	p := newWasiPoll(func() time.Time { return when })
	p.setResources(newHandleTable())
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
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
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
	// A 10s timer, cancelled immediately, must return promptly (not hang).
	res, _ := p.subscribeInstant(context.Background(), []abi.Value{uint64(p.base.Add(10 * time.Second).Sub(p.base))})
	rep := res[0].(uint32)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := p.block(ctx, []abi.Value{rep}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled block took %v, want prompt return", elapsed)
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

func TestPoll_PollBlocksUntilTimer(t *testing.T) {
	p := newTestPoll(time.Now())
	// Two timers: 60ms and 20ms. poll should block ~20ms and report the sooner.
	r1, _ := p.subscribeDuration(context.Background(), []abi.Value{uint64(60 * time.Millisecond)})
	r2, _ := p.subscribeDuration(context.Background(), []abi.Value{uint64(20 * time.Millisecond)})
	h1 := pollHandle(t, p, r1[0].(uint32))
	soonerRep := r2[0].(uint32)
	h2 := pollHandle(t, p, soonerRep)
	soonerDeadline, ok := p.deadlineOf(soonerRep)
	if !ok {
		t.Fatal("sooner timer deadline not found")
	}
	res, err := p.poll(context.Background(), []abi.Value{[]abi.Value{h1, h2}})
	if err != nil {
		t.Fatal(err)
	}
	if returnedAt := time.Now(); returnedAt.Before(soonerDeadline) {
		t.Fatalf("poll returned at %v, before sooner timer deadline %v", returnedAt, soonerDeadline)
	}
	out := res[0].([]abi.Value)
	// The 20ms timer (index 1) must be ready; the 60ms one (index 0) not yet.
	if len(out) != 1 || out[0].(uint32) != 1 {
		t.Fatalf("poll ready indices = %v, want [1] (only the 20ms timer due)", out)
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

func TestSleepUntilPast(t *testing.T) {
	start := time.Now()
	wasiSleepUntil(context.Background(), time.Now().Add(-time.Hour))
	if time.Since(start) > 10*time.Millisecond {
		t.Fatalf("past deadline should return immediately, took %v", time.Since(start))
	}
}

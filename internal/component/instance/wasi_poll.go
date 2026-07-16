package instance

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// wasiPoll is the shared wasi:io/poll + wasi:clocks host. It owns the single
// timer-aware block/poll implementation -- replacing the former per-interface
// no-op copies in wasi_sockets.go (TCP+UDP) and wasi_http.go -- plus the
// wasi:clocks monotonic-clock and wall-clock funcs.
//
// # Pollable model
//
// Socket / stream / http `subscribe` methods all return the always-ready
// singleton wasiPollableRep: their real blocking happens inside
// read/receive/handle, so the pollable itself is immediately ready and
// block/poll on it is a no-op (the pre-existing model -- see wasiPollableRep's
// doc in wasi_sockets.go). wasi:clocks timer subscribes
// (subscribe-duration/subscribe-instant) instead mint DISTINCT reps carrying a
// real wall-clock deadline; block/poll on those genuinely sleep until the
// deadline. That deadline is the ONLY thing that produces a
// std::thread::sleep's delay -- unlike a socket read there is no other blocking
// operation to hang the delay on -- so, uniquely for clocks, the pollable must
// really wait.
//
// # Monotonic vs wall time
//
// base is captured at instance start; monotonic-clock.now returns nanoseconds
// since base (a real, monotonic reading via time.Since). subscribe-duration's
// deadline is now+when; subscribe-instant's is base+when (the absolute
// monotonic instant). wall-clock.now reads wallClock (WASIConfig.WallClock,
// defaulted to time.Now) -- the one injectable surface, so a test can pin the
// wall time for a deterministic assertion while real monotonic sleeps still
// elapse for real.
type wasiPoll struct {
	mu        sync.Mutex
	resources *handleTable
	base      time.Time
	wallClock func() time.Time
	deadlines map[uint32]time.Time
	nextRep   uint32
}

// wasiPollTimerRepBase is wasiPoll.nextRep's start: any value but the
// always-ready singleton wasiPollableRep (1) works, since timer reps only ever
// need to be distinct from that one and from each other (all pollables share
// wasiPollableResType, and every non-timer pollable is rep 1).
const wasiPollTimerRepBase uint32 = 0x1000

// wasiIfaceClocksMonotonic / wasiIfaceClocksWall are the wasi:clocks interface
// names (version-stripped by mkImportKey, so the @x.y.z suffix is tolerant --
// see mkImportKey's doc).
const (
	wasiIfaceClocksMonotonic = "wasi:clocks/monotonic-clock@0.2.3"
	wasiIfaceClocksWall      = "wasi:clocks/wall-clock@0.2.3"
)

// newWasiPoll builds the shared poll/clocks host. wallClock is never nil by the
// time WithWASI calls this (defaulted to time.Now there).
func newWasiPoll(wallClock func() time.Time) *wasiPoll {
	return &wasiPoll{
		base:      time.Now(),
		wallClock: wallClock,
		deadlines: make(map[uint32]time.Time),
		nextRep:   wasiPollTimerRepBase,
	}
}

// setResources implements withResourcesHook's callback -- mirrors
// wasiFS.setResources's doc.
func (p *wasiPoll) setResources(t *handleTable) {
	p.mu.Lock()
	p.resources = t
	p.mu.Unlock()
}

func (p *wasiPoll) getResources() (*handleTable, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resources == nil {
		return nil, fmt.Errorf("wasi:io/poll: resources handle table not yet initialized (setResources not called)")
	}
	return p.resources, nil
}

// newTimer mints a fresh timer pollable rep with the given absolute deadline.
func (p *wasiPoll) newTimer(deadline time.Time) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	rep := p.nextRep
	p.nextRep++
	p.deadlines[rep] = deadline
	return rep
}

// deadlineOf returns rep's timer deadline, or ok=false if rep is not a timer
// pollable (i.e. an always-ready socket/stream/http pollable).
func (p *wasiPoll) deadlineOf(rep uint32) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.deadlines[rep]
	return d, ok
}

// monotonicNow implements wasi:clocks/monotonic-clock.now() -> instant (u64 ns
// since base).
func (p *wasiPoll) monotonicNow(context.Context, []abi.Value) ([]abi.Value, error) {
	return []abi.Value{uint64(time.Since(p.base))}, nil
}

// monotonicResolution implements monotonic-clock.resolution() -> duration.
// Go's time.Since resolves to the nanosecond, so 1ns is reported.
func (p *wasiPoll) monotonicResolution(context.Context, []abi.Value) ([]abi.Value, error) {
	return []abi.Value{uint64(1)}, nil
}

// subscribeDuration implements monotonic-clock.subscribe-duration(when:
// duration) -> pollable: a timer that fires `when` nanoseconds from now. The
// bare rep is auto-wrapped into an own<pollable> handle (top-level own result
// -- see host_import.go's allocHandleResult).
func (p *wasiPoll) subscribeDuration(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	when, err := wasiPollU64Arg("subscribe-duration", args)
	if err != nil {
		return nil, err
	}
	return []abi.Value{p.newTimer(time.Now().Add(time.Duration(when)))}, nil
}

// subscribeInstant implements monotonic-clock.subscribe-instant(when: instant)
// -> pollable: a timer that fires at the absolute monotonic instant `when`
// (base + when).
func (p *wasiPoll) subscribeInstant(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	when, err := wasiPollU64Arg("subscribe-instant", args)
	if err != nil {
		return nil, err
	}
	return []abi.Value{p.newTimer(p.base.Add(time.Duration(when)))}, nil
}

// wallNow implements wasi:clocks/wall-clock.now() -> datetime { seconds: u64,
// nanoseconds: u32 } from wallClock.
func (p *wasiPoll) wallNow(context.Context, []abi.Value) ([]abi.Value, error) {
	t := p.wallClock().UTC()
	return []abi.Value{wasiDatetimeValue(t)}, nil
}

// wallResolution implements wall-clock.resolution() -> datetime. Reported as
// 1ns, matching monotonicResolution.
func (p *wasiPoll) wallResolution(context.Context, []abi.Value) ([]abi.Value, error) {
	return []abi.Value{[]abi.Value{uint64(0), uint32(1)}}, nil
}

// block implements wasi:io/poll [method]pollable.block(self: borrow<pollable>)
// -> (): for a timer pollable it sleeps until the deadline; for an always-ready
// pollable it returns immediately. self is a top-level borrow, already resolved
// to a rep by liftHostArgs.
func (p *wasiPoll) block(ctx context.Context, args []abi.Value) ([]abi.Value, error) {
	rep, err := wasiPollU32Arg("[method]pollable.block", args)
	if err != nil {
		return nil, err
	}
	if deadline, ok := p.deadlineOf(rep); ok {
		wasiSleepUntil(ctx, deadline)
	}
	return nil, nil
}

// poll implements the free wasi:io/poll.poll(in: list<borrow<pollable>>) ->
// list<u32>. It resolves each handle to a live pollable rep (trapping loud on a
// bogus handle, matching every other borrow resolution). Always-ready pollables
// and already-due timers are reported ready immediately; if none are ready it
// blocks until the earliest timer deadline, then reports whatever is due --
// exactly poll's contract (block until >=1 ready, return all currently ready).
func (p *wasiPoll) poll(ctx context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("wasi:io/poll.poll: expected 1 arg (in), got %d", len(args))
	}
	list, ok := args[0].([]abi.Value)
	if !ok {
		return nil, fmt.Errorf("wasi:io/poll.poll: in: expected list<borrow<pollable>> ([]abi.Value), got %T", args[0])
	}
	resources, err := p.getResources()
	if err != nil {
		return nil, err
	}
	// Resolve every handle to a rep once, up front (also validates them).
	reps := make([]uint32, len(list))
	for i, v := range list {
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("wasi:io/poll.poll: in[%d]: expected uint32 handle, got %T", i, v)
		}
		rep, err := resources.Rep(wasiPollableResType, h)
		if err != nil {
			return nil, fmt.Errorf("wasi:io/poll.poll: in[%d]: %w", i, err)
		}
		reps[i] = rep
	}
	if out := p.readyIndices(reps); len(out) > 0 {
		return []abi.Value{out}, nil
	}
	// Nothing ready: every input is a not-yet-due timer. Sleep to the earliest.
	if earliest, ok := p.earliestDeadline(reps); ok {
		wasiSleepUntil(ctx, earliest)
	}
	return []abi.Value{p.readyIndices(reps)}, nil
}

// readyIndices returns the indices of reps that are ready now: a non-timer
// (always-ready) rep, or a timer whose deadline has passed.
func (p *wasiPoll) readyIndices(reps []uint32) []abi.Value {
	now := time.Now()
	out := make([]abi.Value, 0, len(reps))
	for i, rep := range reps {
		deadline, isTimer := p.deadlineOf(rep)
		if !isTimer || !now.Before(deadline) {
			out = append(out, uint32(i))
		}
	}
	return out
}

// earliestDeadline returns the soonest timer deadline among reps.
func (p *wasiPoll) earliestDeadline(reps []uint32) (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, rep := range reps {
		if d, ok := p.deadlineOf(rep); ok {
			if !found || d.Before(earliest) {
				earliest, found = d, true
			}
		}
	}
	return earliest, found
}

// wasiSleepUntil sleeps until deadline, waking early if ctx is cancelled (so a
// cancelled guest call does not hang on a long timer). A non-positive remaining
// duration returns immediately.
func wasiSleepUntil(ctx context.Context, deadline time.Time) {
	d := time.Until(deadline)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// wasiDatetimeValue builds the wasi:clocks/wall-clock `datetime` record value
// (seconds: u64, nanoseconds: u32) from a time.Time.
func wasiDatetimeValue(t time.Time) abi.Value {
	return []abi.Value{uint64(t.Unix()), uint32(t.Nanosecond())}
}

// wasiPollU64Arg parses a single-u64-arg func's args.
func wasiPollU64Arg(method string, args []abi.Value) (uint64, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("wasi:clocks/monotonic-clock.%s: expected 1 arg (when), got %d", method, len(args))
	}
	when, ok := args[0].(uint64)
	if !ok {
		return 0, fmt.Errorf("wasi:clocks/monotonic-clock.%s: when: expected uint64, got %T", method, args[0])
	}
	return when, nil
}

// wasiPollU32Arg parses a single-u32-rep-arg func's args (a resolved borrow
// self).
func wasiPollU32Arg(method string, args []abi.Value) (uint32, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("%s: expected 1 arg (self), got %d", method, len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return 0, fmt.Errorf("%s: self: expected uint32 rep, got %T", method, args[0])
	}
	return rep, nil
}

// wasiClockPollOptions returns the centralized wasi:io/poll (block, poll,
// pollable resource tag) + wasi:clocks (monotonic + wall-clock) Options,
// registered unconditionally by WithWASI so any guest -- sockets, http, clocks,
// or a bare stream-poller -- shares one timer-aware implementation.
func wasiClockPollOptions(p *wasiPoll) []Option {
	blockFD, blockR := wasiPollableBlockSig()
	pollFD, pollR := wasiPollSig()
	monoNowFD, monoNowR := wasiMonotonicNowSig()
	monoSubFD, monoSubR := wasiMonotonicSubscribeSig()
	monoResFD, monoResR := wasiMonotonicNowSig() // () -> u64, same shape as now
	wallNowFD, wallNowR := wasiWallClockNowSig()
	wallResFD, wallResR := wasiWallClockNowSig() // () -> datetime, same shape as now

	return []Option{
		withResourcesHook(p.setResources),
		withResourceTag(wasiIfacePoll, "pollable", wasiPollableResType),

		withImportCustom(wasiIfacePoll, "[method]pollable.block", p.block, blockFD, blockR),
		withImportCustom(wasiIfacePoll, "poll", p.poll, pollFD, pollR),

		withImportCustom(wasiIfaceClocksMonotonic, "now", p.monotonicNow, monoNowFD, monoNowR),
		withImportCustom(wasiIfaceClocksMonotonic, "resolution", p.monotonicResolution, monoResFD, monoResR),
		withImportCustom(wasiIfaceClocksMonotonic, "subscribe-duration", p.subscribeDuration, monoSubFD, monoSubR),
		withImportCustom(wasiIfaceClocksMonotonic, "subscribe-instant", p.subscribeInstant, monoSubFD, monoSubR),

		withImportCustom(wasiIfaceClocksWall, "now", p.wallNow, wallNowFD, wallNowR),
		withImportCustom(wasiIfaceClocksWall, "resolution", p.wallResolution, wallResFD, wallResR),
	}
}

// wasiMonotonicNowSig builds the FuncDesc/resolver for monotonic-clock.now() ->
// instant (u64) -- also reused for resolution() -> duration (u64), same shape.
func wasiMonotonicNowSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	ref := binary.TypeRef{Primitive: "u64"}
	fd := binary.FuncDesc{Results: binary.FuncResults{Unnamed: &ref}}
	return fd, tbl.resolver()
}

// wasiMonotonicSubscribeSig builds the FuncDesc/resolver for a subscribe-*(when:
// u64) -> own<pollable> method (subscribe-duration and subscribe-instant share
// it).
func wasiMonotonicSubscribeSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	pollRef := tbl.add(binary.OwnDesc{ResourceType: wasiPollableResType})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "when", Type: binary.TypeRef{Primitive: "u64"}}},
		Results: binary.FuncResults{Unnamed: &pollRef},
	}
	return fd, tbl.resolver()
}

// wasiWallClockNowSig builds the FuncDesc/resolver for wall-clock.now() ->
// datetime -- also reused for resolution() -> datetime, same shape.
func wasiWallClockNowSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	dtRef := wasiDatetimeType(tbl)
	fd := binary.FuncDesc{Results: binary.FuncResults{Unnamed: &dtRef}}
	return fd, tbl.resolver()
}

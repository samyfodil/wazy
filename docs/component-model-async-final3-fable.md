# Component Model async — the final 3 deferred features (design)

Target branch: `feat/component-model-async`. Goal: unblock the last async `.wast` suites gated on
runtime features (as opposed to decoder/binder gaps tracked elsewhere):

| # | Feature | Suites unblocked | Risk |
|---|---------|------------------|------|
| 1 | Callback-task parking ("promotion") | `sync-streams`, `async-calls-sync` (run2), and — **already passing, stale skip** — `zero-length` | **HIGH** |
| 3 | Per-element resource transfer in streams | `passing-resources` | MEDIUM |
| 2 | Cross-core-instance realloc on `canon lift` | `cross-abi-calls` | LOW |

Ground-truth probes run on this branch (2026-07-17, `runAsyncWastSuite` driven directly):

- **`zero-length` PASSES today.** Its skip text predates the committed §4.4
  `startDelegatedFromStackful` work: `$Parent.run` is a *stackful* lift (async type, no callback), so
  its sync-lowers of `produce`/`consume` already park `$Parent`'s goroutine instead of nested-driving,
  and `produce`/`consume` themselves only ever park frame-free (async 0-length copies return
  `BLOCKED`, then packed `WAIT`). **Action: delete the skip, no code.** Keep it in Feature 1's
  regression set anyway (promotion must not regress it).
- **`sync-streams` fails** exactly as designed-for:
  `host import "get": export "get": call core func "get": stream.write: deadlock: no pending event
  and the run queue is empty` — `$C.get` is a CALLBACK lift whose core body calls **sync**
  `stream.write` mid-core-call; `activeStackfulTask($C)` is nil (the active task is a callback task),
  so the builtin takes the nested `sched.drive(e.hasPendingEvent)` arm, which frame-holds `$D.run`'s
  continuation (the only possible rendezvous peer) beneath it. Feature 1's exact target.
- **`cross-abi-calls` fails at Instantiate**: `export "sync-17-param": realloc core func targets a
  different core instance than the lift's own core func; cross-instance realloc is not supported`
  (`resolveReallocFuncGraph`, graph.go). The *lower*-side twin (`canonMemoryAndRealloc`) already
  supports cross-core-instance memory/realloc; only the lift side over-restricts. Feature 2.
- **`passing-resources` fails at Instantiate**: `stream/future element type contains an own/borrow
  resource handle, which is not supported by this milestone` (`resolveStreamOrFutureElem`'s
  `elemContainsResourceHandle` ceiling). Feature 3.

**Concurrent-work caution:** at design time the working tree carries uncommitted edits
(resource-identity canonicalization for `drop-cross-task-borrow`: `composition.go`'s
`resourceIdentity`/`originOf`, `Instance.resourceOrigin`, plus a `copyElementsMemmove` nil-memMod
fix). Feature 3 deliberately *builds on* `originOf`; line numbers cited here may shift. Rebase before
implementing.

---

## Feature 1 — CALLBACK-TASK PARKING (the crux)

### 1.0 The gap, precisely

Committed state: a **stackful** task suspends mid-core-call by parking its goroutine
(`stackfulTask.block`, baton channels). A **callback** `guestTask` has no goroutine — its only legal
suspensions are frame-free (`parkEntry`/`parkWait`/`parkYield`, i.e. the core call has *returned* a
packed code). When a callback task's core body blocks **mid-call** — a sync stream/future copy
(`stream_builtins.go` sync arms), a sync-lowered call to an async callee (`host_import.go` →
`delegatingHostImport.fn` → `sub.invoke` → `invokeAsyncCallback`'s `drive(gt.done)`), or
`waitable-set.wait` — the builtin nested-drives the shared scheduler *on the current goroutine*,
frame-holding everything beneath: in particular the **caller's own continuation**, which for
`sync-streams`/`async-calls-sync` run2 is the only thing that can satisfy the wait. The nested drive
can never converge → `errAsyncDeadlock`. This is stackful-design §12 honest-flag #1.

Frames cannot be migrated to a goroutine after the fact, so the decision "may this core call need to
suspend with live frames?" must be made **before** `CallWithStack`. The design: run a callback task's
core/callback invocations on a short-lived **segment goroutine** — but only for tasks that can
actually need it (see the gate), so every currently-green path stays bit-identical.

### 1.1 The promotion gate (bind-time + start-time, both deterministic)

```go
// instance.go — new Instance field (alongside activeTask/exclusiveHeld):

	// mayBlockSync is set at graph-bind time when this instance's core
	// module(s) can reach a MID-CORE-CALL blocking site from a callback
	// task: a SYNC (no async opt) stream/future copy canon, or a sync
	// canon-lowered import whose target is an async lift (callback or
	// stackful). Callback tasks of such an instance, when started by a
	// GUEST caller, run their core/callback invocations on a per-segment
	// goroutine (guestTask.promoted) so those sites can park instead of
	// nested-driving. Instances without such sites (the overwhelmingly
	// common case: async lowers + callback WAITs only) never pay a
	// goroutine. See docs/component-model-async-final3-fable.md §1.
	mayBlockSync bool
```

Set it in `graph.go`:

- wherever `streamCopyHostFunc`/`futureCopyHostFunc` are bound with `async == false` **and a non-nil
  element or not — unconditionally for sync copies** → `in.mayBlockSync = true`;
- wherever a delegated import is wired with `hi.asyncTarget != nil` whose target
  `be.asyncCallback || be.stackful` **and the lower is sync** (the same condition
  `buildHostWrapper`'s §4.3/§4.4 block tests) → `in.mayBlockSync = true` on the **importing**
  instance.

Deliberately **excluded** from the gate (v1): `waitable-set.wait`, sync `subtask.cancel`. No target
suite blocks a *guest-called callback task* inside those builtins, and excluding them keeps
`wait-during-callback`, `deadlock`, `cross-task-future` etc. on today's exact nested-drive paths. The
gate is a one-line lever if a future suite needs them promoted.

Promotion decision per task, in `startAsyncExportTask`'s callback arm (`async_lift.go`):

```go
	gt := &guestTask{
		t: t, in: in, be: be, ctx: ctx, exportName: exportName,
		onStart:  func() ([]abi.Value, error) { return onStart(t) },
		promoted: in.mayBlockSync, // guest caller ⇒ its continuation may be load-bearing
	}
```

`invokeAsyncCallback` (host entry) leaves `promoted == false`: the host has no guest continuation
beneath the drive, so the nested-drive deadlock detection there is already *correct*, and every
currently-green host-entry suite stays byte-for-byte on today's path. (Residual honest flag, §1.9.)

`sync-streams` check: `$C` binds sync `stream.read`/`stream.write` canons ⇒ `mayBlockSync` ⇒ `$C.get`
/`$C.set` (started via `startDelegatedFromStackful` → `startAsyncExportTask`) are promoted.
`async-calls-sync` run2: `$AsyncMiddle` sync-lowers `blocking-call` (an async lift) ⇒ flagged ⇒ its
`sync-func` (async-lowered by `$AsyncOuter2` ⇒ guest caller) is promoted. `zero-length`:
`$Producer`/`$Consumer` bind only **async** copies ⇒ NOT flagged ⇒ zero new goroutines, provably
unchanged.

### 1.2 guestTask additions (`guest_task.go`)

```go
const (
	parkNone  guestParkKind = iota
	parkEntry
	parkWait
	parkYield
	parkBlocked // NEW: suspended MID-CORE-CALL inside a blocking builtin;
	            // live guest frames on a segment goroutine at <-seg.resumeCh.
	            // Promoted tasks only. Counterpart of stackful sparkBlock.
)

type guestTask struct {
	// ... existing fields unchanged ...

	// promoted: core/callback invocations run via runSegment on a fresh
	// baton goroutine, so a blocking builtin reached INSIDE the call can
	// park this task with live frames while the caller's goroutine (and
	// eventually the root driver) continues. Set at construction
	// (startAsyncExportTask only), never mutated after.
	promoted bool

	// seg is the live segment, non-nil exactly while a segment goroutine
	// exists: from runSegment's spawn until the segment's fn returns
	// (frame-free park, EXIT, or trap). It stays non-nil across a
	// parkBlocked suspension (the goroutine is alive inside block()).
	seg *guestSegment
}

// guestSegment is one run of a promoted guestTask's core code: a goroutine
// executing exactly one fn (firstRun's body, or one runLoop callback
// invocation), plus the same baton-channel pair stackfulTask uses. Segments
// are per-invocation: a task that parks frame-free (WAIT/YIELD) costs no
// goroutine while parked; only a parkBlocked task pins one.
type guestSegment struct {
	gt       *guestTask
	resumeCh chan resumeMode // driver -> goroutine (reuses stackful_task.go's resumeMode)
	yieldCh  chan struct{}   // goroutine -> driver
	err      error           // fn's result, valid once gt.seg == nil
	panicVal any             // non-trap panic on the goroutine; re-panicked on the driver
	parkReady func() bool    // parkBlocked's predicate (mirrors stackfulTask.parkReady)
}
```

Segment runner — the ONLY structural change to the run path; unpromoted tasks take the first return
and are bit-identical to today:

```go
// runSegment executes fn inline (unpromoted) or on a fresh baton goroutine
// (promoted). Returns fn's error once the segment finishes, or nil if the
// segment PARKED mid-call (gt is then in sched.parked at parkBlocked and
// the goroutine is alive at <-resumeCh inside gt.block).
func (gt *guestTask) runSegment(fn func() error) error {
	if !gt.promoted {
		return fn()
	}
	seg := &guestSegment{gt: gt,
		resumeCh: make(chan resumeMode), yieldCh: make(chan struct{})}
	gt.seg = seg
	go seg.main(fn)
	return seg.handoff(resumeNormal)
}

func (s *guestSegment) main(fn func() error) {
	mode := <-s.resumeCh // first baton
	defer func() {
		if r := recover(); r != nil && r != any(errStackfulAbort) {
			s.panicVal = r // real bug: surface on the driver, never swallow
		}
		s.gt.seg = nil           // segment over (fn returned or panicked)
		s.yieldCh <- struct{}{}  // final handoff
	}()
	if mode == resumeAbort {
		return // reaped before ever running
	}
	s.err = fn()
}

// handoff hands the baton to the segment goroutine and waits for it back.
// Runs on whatever goroutine currently owns the baton (the starter on the
// first handoff; sched.step's caller on resumes).
func (s *guestSegment) handoff(mode resumeMode) error {
	s.resumeCh <- mode
	<-s.yieldCh
	if s.gt.seg == nil { // finished
		if s.panicVal != nil {
			panic(s.panicVal)
		}
		return s.err // nil on success/frame-free park; non-nil = trap (already gt.fail-ed)
	}
	return nil // parked at parkBlocked; block() already re-registered gt in sched.parked
}
```

Rewire the two invocation sites (mechanical: rename current bodies to `firstRunBody`/`runLoopBody`,
zero logic changes inside them):

```go
func (gt *guestTask) firstRun() error { return gt.runSegment(gt.firstRunBody) }

func (gt *guestTask) runLoop(ev eventTuple) error {
	if !gt.promoted { // avoid the closure alloc on the hot path
		return gt.runLoopBody(ev)
	}
	return gt.runSegment(func() error { return gt.runLoopBody(ev) })
}
```

Note `firstRunBody` includes `onStart` (the lazy caller-arg lift): it now runs on the segment
goroutine, reading the caller's memory — legal, the baton serializes it (§1.7). `advance` also runs
inside the segment: a frame-free park (`parkWait`/`parkYield`) makes `fn` return nil *after*
`sched.park(gt)`, the goroutine exits, and the next resume spawns a fresh segment. `finishExit`/
`fail` likewise run on the goroutine; their `finish(err)` callbacks are baton-serialized.

### 1.3 The mid-call suspension primitive

`gt.block` is `stackfulTask.block` transliterated (same baton discipline, same abort sentinel):

```go
// block suspends the calling PROMOTED guestTask mid-core-call until ready()
// holds (or a cancel wake). MUST be called on the segment goroutine (i.e.
// from a builtin invoked by this task's guest code). Counterpart of
// stackfulTask.block; the exclusive stays HELD across the park (reference
// wait_until never releases it — only the callback loop's frame-free WAIT
// does, via leaveRun in advance).
func (gt *guestTask) block(ready func() bool, cancellable bool) (cancelled bool) {
	if gt.t.deliverPendingCancel(cancellable) {
		return true
	}
	seg := gt.seg
	gt.park, gt.cancellable = parkBlocked, cancellable
	seg.parkReady = ready
	gt.in.suspendRun()       // activeTask=nil, mayEnter=true; exclusive KEPT
	gt.in.sched.park(gt)     // safe: we hold the baton
	seg.yieldCh <- struct{}{}
	mode := <-seg.resumeCh
	if mode == resumeAbort {
		panic(errStackfulAbort) // unwinds guest frames via the engine's recover
	}
	gt.park, gt.cancellable = parkNone, false
	seg.parkReady = nil
	gt.in.enterRun(gt.t)
	return mode == resumeCancelled
}
```

`ready`/`resumeReady` gain one arm each, mirroring `sparkBlock` exactly:

```go
// in guestTask.ready():
	case parkBlocked:
		return gt.cancelWake || gt.t.cancelDeliverable() || gt.seg.parkReady()

// in guestTask.resumeReady():
	case parkBlocked:
		gt.in.sched.unpark(gt)
		mode := resumeNormal
		if gt.cancelWake {
			gt.cancelWake, mode = false, resumeCancelled
		}
		return gt.seg.handoff(mode)
```

**No `sched.go` / `parkedTask` changes.** `guestTask` already implements `parkedTask`; `parkBlocked`
is just new internal state. (Explicit answer to "does this force changes to the committed
sched/parkedTask core": **no** — the core survives untouched. `resumeMode`/`errStackfulAbort` are
reused from `stackful_task.go` as-is.)

### 1.4 Generalizing "who can park mid-call": `task.blocker`

```go
// task.go:

// taskBlocker is the mid-call suspension capability: implemented by
// *stackfulTask (goroutine always live) and by a promoted *guestTask while
// a segment goroutine is live. Blocking builtins and the sync-lower
// delegate use it instead of testing t.st directly.
type taskBlocker interface {
	block(ready func() bool, cancellable bool) (cancelled bool)
}

// blocker returns t's live mid-call suspension primitive, or nil (then the
// caller keeps the Phase 1-3 nested drive / trap behavior).
func (t *task) blocker() taskBlocker {
	if t.st != nil {
		return t.st
	}
	if t.gt != nil && t.gt.seg != nil {
		return t.gt
	}
	return nil
}
```

Call-site changes (all mechanical, each keeps its existing else-arm verbatim):

1. **`async_builtins.go`** — `activeStackfulTask(in) *stackfulTask` becomes
   `activeBlocker(in) taskBlocker` (same doc, same nil semantics); `blockingTask` returns
   `(t *task, blk taskBlocker)` instead of `(t, st)`. Sites: `waitableSetWaitHostFunc` (~176:
   `if blk != nil { ws.numWaiting++; cancelled := blk.block(pred, cancellable); ... }`), the sync
   `subtask.cancel` wait (~399-406). *(With `waitable-set.wait` excluded from the gate, `blk` is
   non-nil there only for stackful tasks today — behavior unchanged — but the code is already
   general for the future lever.)*
2. **`stream_builtins.go`** — the three sync-copy waits (stream ~399, future ~489, cancel ~626):
   `if blk := activeBlocker(in); blk != nil { blk.block(e.hasPendingEvent, false) } else { nested
   drive as today }`. This is THE `sync-streams` fix: `$C.get`'s promoted task has a live segment ⇒
   `blk != nil` ⇒ park instead of nested-drive.
3. **`host_import.go`** (~607-636) — the §4.4 routing generalizes from
   `stackfulCaller = t.st` to `blk := t.blocker()`; call `startDelegatedFromBlocker(ctx, blk,
   hi.asyncTarget, args)` when non-nil. This is the `async-calls-sync` run2 fix: `$AsyncMiddle`'s
   promoted `sync-func` sync-lowers `blocking-call` from inside its segment ⇒ parks (reference
   `thread.wait_until(subtask.resolved)`, ~2273-2275) ⇒ `$AsyncOuter2.run` continues ⇒ `unblock`
   eventually runs ⇒ convergence.
4. **`graph.go`** — `startDelegatedFromStackful(ctx, st *stackfulTask, ...)` renames to
   `startDelegatedFromBlocker(ctx, blk taskBlocker, ...)`; the single `st.block(...)` call becomes
   `blk.block(...)`. Body otherwise unchanged.

### 1.5 End-of-invoke drain + generalized reap

Two lifecycle additions in `async_lift.go` / `stackful_task.go`:

```go
// sched.go or async_lift.go: run every already-ready parked task (and any
// thunks their progress enqueues) to quiescence — the reference Store.tick
// loop that keeps ticking after the sync caller's own task completes, so
// e.g. sync-streams' $C.get/$C.set run to EXIT (and their segment
// goroutines exit) before the host-entry invoke returns. Stops at the
// first no-progress round; permanently-unready tasks stay parked (they may
// legitimately wake on a LATER invoke — reference threads persist across
// ticks) and are reaped only at Close.
func (s *sched) drainReady() error {
	for {
		progressed, err := s.step()
		if err != nil {
			return err // a leftover task's trap fails the invoke (fail-loud; deviation noted §1.9)
		}
		if !progressed {
			return nil
		}
	}
}
```

Call it after the successful `drive` in **both** `invokeAsyncCallback` and `invokeStackful` (before
reading `t.result`). It is a no-op whenever nothing is ready — currently-green suites where the
drive already quiesced everything see zero behavior change; suites that previously stranded
resolved-but-unfinished frame-free tasks now (correctly) run them to EXIT.

`reapStackful` (`stackful_task.go` ~333) generalizes to reap promoted guestTasks too — rename to
`reapParkedGoroutines`, keep the old name as a thin alias if churn matters:

```go
func (in *Instance) reapParkedGoroutines() {
	for {
		var vst *stackfulTask
		var vgt *guestTask
		for _, p := range in.sched.parked {
			switch v := p.(type) {
			case *stackfulTask:
				vst = v
			case *guestTask:
				if v.seg != nil { // parkBlocked with a live goroutine; frame-free parks hold nothing
					vgt = v
				}
			}
			if vst != nil || vgt != nil {
				break
			}
		}
		switch {
		case vst != nil:
			// ... existing stackful arm verbatim ...
		case vgt != nil:
			in.sched.unpark(vgt)
			seg := vgt.seg
			seg.resumeCh <- resumeAbort // block() panics errStackfulAbort → engine recover →
			<-seg.yieldCh               // fn returns an error → seg.main's deferred yield
			// seg is nil now; goroutine fully unwound.
		default:
			return
		}
	}
}
```

Callers: `Instance.Close` (already calls the reap), `invokeStackful`'s three error paths (already),
**plus** `invokeAsyncCallback`'s error paths (new — a promoted composition can now strand parked
goroutines under a callback-rooted invoke too).

### 1.6 Cancellation interplay (`task.requestCancellation`, task.go ~172)

- `taskInitial` arm: unchanged (`parkEntry` promotion doesn't exist — promotion affects run
  segments, not entry parks).
- `taskStarted`, `t.gt` arm: today's condition `gt.park != parkNone && gt.cancellable &&
  !t.inst.exclusiveHeld && ...` must NOT resume a `parkBlocked` task synchronously the way frame-free
  parks are resumed — a parkBlocked resume is a *baton handoff*, which is exactly what the `t.st`
  arm already does. Restructure the gt arm to mirror the st arm:

```go
	if t.gt.park == parkBlocked {
		heldByOther := t.inst.exclusiveHeld && t.inst.exclusiveOwner != t
		if t.gt.cancellable && !heldByOther && t.inst.mayEnter {
			t.state = taskCancelDelivered
			t.gt.cancelWake = true
			return t.gt.resumeReady() // handoff(resumeCancelled)
		}
		t.state = taskPendingCancel
		return nil
	}
	// existing frame-free arm unchanged below
```

  In v1 every `parkBlocked` site passes `cancellable == false` (sync copies, sync-lower resolution
  wait — reference ~2275), so only the `taskPendingCancel` branch is reachable; the delivered-cancel
  branch is wired for the `waitable-set.wait` lever. `gt.block`'s prologue `deliverPendingCancel`
  and `ready()`'s `cancelDeliverable()` conjunct then deliver the pending cancel at the next
  legal point, exactly like `sparkBlock`.

### 1.7 Invariants

**Single-runnable / `-race`.** Identical argument to the stackful design §1: at any instant at most
one goroutine in a composition tree executes component-runtime code; every control transfer is an
unbuffered channel send/recv pair (`resumeCh`/`yieldCh`), so -race sees plain happens-before edges.
Segments nest correctly: when `$D`'s stackful goroutine starts `$C.get`'s segment, `$D` blocks in
`seg.handoff` (`<-yieldCh`) while `G_C` runs — the baton simply moves down and back. All guestTask
fields (`park`, `seg`, `err`, `done`, the `resolved` closures in delegates) are only ever touched by
the baton holder. New test: `-race` over the whole package (already standard) + a dedicated
promoted-composition test (§1.8).

**No hang.** Every mid-call park registers in `sched.parked` *before* yielding the baton
(`sched.park(gt)` precedes `yieldCh <-` in `block`), so the root `drive` always sees it; `drive`'s
no-progress detection is untouched, and a permanently-unready promoted task surfaces as
`errAsyncDeadlock` at the outermost drive → the existing spec trap texts (`invokeStackful`'s
`"wasm trap: deadlock detected: event loop cannot make further progress"` — which is what
`deadlock.wast` asserts, via its stackful root, unchanged). `drainReady` terminates: each round
either pops a runq entry or resumes a ready task; a resumed task either finishes, re-parks
not-ready, or re-parks ready-again only after making progress (event consumed) — the reference's
own tick-loop termination argument.

**No goroutine leak.** A segment goroutine exists only (a) while a resumer is blocked in `handoff`
(bounded), or (b) parked at `parkBlocked` in `sched.parked` — drained at invoke end if ready, reaped
at `Close`/invoke-error otherwise. Tests: `runtime.NumGoroutine()` deltas around (i) a full
`sync-streams` run, (ii) a deliberately-deadlocked promoted call (trap → reap), (iii) `Close` with a
parked promoted task.

### 1.8 Verification (in order)

1. `sched_test.go`-level unit: a hand-built promoted guestTask whose segment fn calls
   `gt.block(pred, false)`; assert park/resume/abort transitions + goroutine delta 0.
2. Delete `zero-length`'s skip → green (no code yet — do this first as its own commit).
3. Implement, then delete `sync-streams`' skip → green. Its internal asserts double as the
   event-payload oracle (`0x41 = DROPPED|4<<4` on both parked sides — exercises the
   event-overwrite-at-delivery path across a parkBlocked resume).
4. Delete `async-calls-sync`'s skip; run under the timeout guard (stackful §12.4's warning stands:
   if run2 still livelocks the residue is YIELD-fairness in `pumpSnapshot`, a bounded follow-up —
   re-skip ONLY that suite with an updated narrow reason, don't block the rest).
5. Full `TestAsyncWastConformance` + `TestWastConformance` + package tests, then `-race`, then
   `benchmarks/vs-wazero` guest-guest async benches (expect zero delta: unflagged instances take
   `!promoted` fast paths; flag any instance-flagging false positives).

### 1.9 Trap edges + oracle + honest flags

Trap edges (all must have tests): invalid callback code / EXIT-before-resolved from a *promoted*
task (trap flows `fn` → `seg.err` → `handoff` → driver, same text); deadlock with a parked promoted
task (outermost-drive text, `deadlock.wast` regression-guards the stackful flavor); trap *inside* a
segment while OTHER tasks are parked (reap leaves no goroutine); cancel of a parkBlocked
non-cancellable task lands as PENDING_CANCEL (unit test).

Trace-oracle: the differential oracle (`async_oracle_test.go` + `gen_async_oracle.py`) drives
builtins on a hand-built task — extend `async_scenarios.json` with a scenario where a callback
task's op sequence contains a sync `stream.write` that blocks then rendezvouses (the reference
interprets it with its real threads; wazy's side needs the oracle harness to construct a promoted
task). If the harness can't express "mid-call" ops (it calls builtins between core returns), cover
via the Go unit tests above instead and note it in the scenario file — do not force the schema.

Honest flags / deviations (all deliberate, single-threaded-host):
1. **Host-entry callback tasks stay unpromoted.** A host-entry callback task that sync-lowers a
   callee whose convergence needs the *host-entry task's own* continuation still deadlock-traps
   (detected, never hangs). No suite exercises this; the reference has no such asymmetry (every task
   is a thread). Lever: set `promoted = in.mayBlockSync` unconditionally in `invokeAsyncCallback`
   too — costs ordering-risk on green host-entry suites, so only pull it when a suite demands.
2. **`drainReady` error = invoke error.** The reference traps the whole instance when a leftover
   thread traps after the caller's task resolved; wazy fails the invoke that drove it (fail-loud,
   observable difference only for already-trapping guests).
3. **Exclusive held across parkBlocked** (matches reference `wait_until`; the frame-free WAIT
   releasing it also matches — both sides keep their current semantics; `sync-streams`' `$C.set`
   entering while `$C.get` is parkBlocked relies on this: `$C.set` parks at `parkEntry` until
   `$C.get` EXITs — trace it in the acceptance test).
4. Gate excludes `waitable-set.wait`/sync `subtask.cancel` (v1) — documented lever, not spec
   divergence (the nested drive there is still convergence-complete for every existing suite).

---

## Feature 2 — CROSS-CORE-INSTANCE REALLOC on `canon lift`

### 2.1 What's actually wrong

`cross-abi-calls`' `$C` puts memory AND `realloc` on a dedicated `$Memory` core instance; `$Core`
*imports* the memory (so `memoryBytesOf(be.mod)` already sees the right bytes — imported memories
are in the importer's index space) but does NOT import `realloc` — and `finalizeBoundExport`
resolves `be.reallocFn = be.mod.ExportedFunction(name)` against the *lift's* core instance, which
doesn't export it. `resolveReallocFuncGraph` (graph.go ~1741) therefore hard-errors at bind. The
allocation itself is coherent by construction: `$Memory.realloc` grows/allocates in the same memory
object `be.mod` reads. Note the *lower* side (`canonMemoryAndRealloc`, graph.go ~1470) already
resolves realloc cross-core-instance — this change only brings the lift side to parity. (The brief's
"callee-instance realloc" framing is satisfied automatically: the delegated 17-param spill path
allocates via `cachedReallocOf(be)` = the CALLEE's own canon-declared realloc, which after this fix
is the right function regardless of which core instance hosts it.)

### 2.2 Change (pure plumbing, 4 sites)

```go
// instance.go, boundExport: new field next to reallocFuncName:

	// reallocMod is the core instance that exports reallocFuncName when the
	// canon lift's realloc option targets a DIFFERENT core instance than
	// the lift's own core func (legal per the ABI: the option is a plain
	// core func index; cross-abi-calls' $Memory/$Core split). nil ⇒ be.mod.
	// The memory itself needs no twin field: be.mod reaches the shared
	// memory through its own (imported) memory index space, and lowering
	// reads bytes via memoryBytesOf(be.mod) as today.
	reallocMod api.Module
```

```go
// graph.go: resolveReallocFuncGraph returns the module instead of erroring:
func resolveReallocFuncGraph(canon binary.Canon, coreFuncTarget func(int) (api.Module, string, error),
	liftMod api.Module) (reallocMod api.Module, name string, err error) {
	for _, opt := range canon.Opts {
		if opt.Kind != 0x04 {
			continue
		}
		mod, name, err := coreFuncTarget(int(opt.Idx))
		if err != nil {
			return nil, "", fmt.Errorf("realloc %w", err)
		}
		if mod == liftMod {
			mod = nil // same-instance: keep the be.mod fallback (status quo)
		}
		return mod, name, nil
	}
	return nil, "", nil
}
// caller (bindFuncExportGraph ~1683): be.reallocMod, reallocName from the new return;
// set be.reallocMod before finalizeBoundExport.
```

```go
// instance.go, finalizeBoundExport (~939):
	reallocName := be.reallocFuncName
	if reallocName == "" {
		reallocName = "cabi_realloc" // fallback stays be.mod-only by construction (no opt ⇒ no reallocMod)
	}
	rmod := be.mod
	if be.reallocMod != nil {
		rmod = be.reallocMod
	}
	be.reallocFn = rmod.ExportedFunction(reallocName)
	be.reallocCall = coreReallocCall(be.reallocFn)
```

`cachedReallocOf`, `lowerParams`, `guestTask.firstRunBody`, `stackfulTask.run` — all consume
`be.reallocFn`/`reallocCall` and need **zero** changes. The non-graph "trivial single-module" path
never decodes canon opts, so it can't reach this shape — untouched. Also mirror the same relaxation
ONLY if a suite demands it for `post-return`/`callback` (they keep their same-instance errors: the
runtime genuinely calls those on `be.mod`, and `cross-abi-calls` doesn't cross them).

### 2.3 Invariants, traps, verification

No concurrency surface (bind-time only): nothing to say for -race/hang/leak beyond "unchanged".
Trap edges: (a) realloc name unresolvable on the target instance → the existing lazy
`cachedReallocOf` nil-check text fires at first use (test: a lift whose realloc opt names a missing
export); (b) same-instance path byte-identical (regression: full sync+async conformance).
Oracle: no ABI-value change ⇒ no oracle delta; `cross-abi-calls`' own 24 sync/async×{4,5,17-param,
1,16,17-result} asserts ARE the verification matrix (they exercise the 17-param spill through
`paramsSpill` + delegated custom-FD arities, both existing machinery). Delete the suite's skip; if a
*post-bind* failure surfaces it is a separate pre-existing arity gap — document it in the skip
rather than growing this feature. Reference ambiguity: none — the ABI never restricted the option's
target instance; the restriction was wazy's own bind-time conservatism.

---

## Feature 3 — PER-ELEMENT RESOURCE TRANSFER IN STREAMS

### 3.1 Semantics to match (from `passing-resources` + reference `Buffer` lift/lower)

- `stream.write` of a buffer of `own<R>` handles that BLOCKS transfers **nothing** (writer still
  owns both; asserted via `resource.rep` post-block).
- Transfer happens per element **at rendezvous**, for exactly the elements copied: reader takes 1 of
  2 ⇒ element 0's handle is REMOVED from the writer's table (`resource.rep` later ⇒ trap
  `"unknown handle index 3"`) and a fresh own handle is minted in the reader's table; element 1
  stays writer-owned.
- `stream.cancel-write` then reports `DROPPED | 1<<4` (progress bookkeeping already correct —
  `guestBuffer.progress` advances only for copied elements).

### 3.2 Bind-time ceiling: split, don't delete (`stream_builtins.go`)

`resolveStreamOrFutureElem`:

- **Allow** a TOP-LEVEL `binary.OwnDesc` element (`stream<own R>` / `future<own R>`): return it with
  `elemSz=4, align=4, numeric=false` (`noneOrNumberType(OwnDesc)` is already false ⇒ the memmove
  fast path is structurally unreachable for resource elements — the brief's fast-path interaction is
  answered by construction, no new guard needed).
- **Keep rejecting** `BorrowDesc` anywhere (spec: borrow is invalid in stream/future element types —
  upgrade the message to say so: `"stream/future element type contains a borrow handle, which the
  component-model spec disallows"`), and `OwnDesc` at depth > 0 (keep the existing
  `"...not supported by this milestone"` text — nested transfer is a real deferral, see §3.6).

Implementation: change `elemContainsResourceHandle(elem, resolve, 0)`'s call site to
`if _, isOwn := elem.(binary.OwnDesc); !isOwn && elemContainsResourceHandle(...)` plus a preceding
borrow-specific walk (`elemContainsBorrowHandle`, same recursion, Borrow-only) so top-level own
passes, borrow-anywhere and nested-own still fail loud.

### 3.3 Element-type identity across instances (`stream.go`, uses the NEW `originOf`)

`sharedStream`/`sharedFuture`.elem is the *creator's* desc; `requireStreamEnd`/`requireFutureEnd`/
`peekReadable{Stream,Future}End` compare with `reflect.DeepEqual` — which false-mismatches
`own<R>` descs whose `ResourceType` indices are different local names for the same resource
(Producer's `$R` vs its export alias `$R'` vs Consumer's imported `$R`). Fix:

```go
// stream.go: sharedStream/sharedFuture gain the creator instance:
	elemIn *Instance // instance whose resolver/table elem's type indices are relative to; nil = host

// set in streamNewHostFunc/futureNewHostFunc: shared := &sharedStream{elem: elemDesc, elemIn: in, ...}

// elemTypesCompatible replaces DeepEqual at the four compare sites. Fast
// path: DeepEqual (covers every non-handle element bit-identically to
// today). Slow path: both sides are OwnDesc ⇒ compare resource IDENTITY
// via the composition-wide key (composition.go's resourceIdentity):
func elemTypesCompatible(aIn *Instance, a binary.TypeDesc, bIn *Instance, b binary.TypeDesc) bool {
	if reflect.DeepEqual(a, b) && (aIn == bIn || !descContainsHandle(a)) {
		return true
	}
	ao, aOk := a.(binary.OwnDesc)
	bo, bOk := b.(binary.OwnDesc)
	if !aOk || !bOk || aIn == nil || bIn == nil {
		return false
	}
	return aIn.originOf(aIn.canonTag(ao.ResourceType)) == bIn.originOf(bIn.canonTag(bo.ResourceType))
}
```

`peekReadable*End` and `takeReadable*End` need the comparing instance: add an `in *Instance` first
parameter (callers all have one in scope: `resolveHandleArg` (`in`), `providerHandleToRep` (`sub`),
`liftResult` (`in`), `requireStreamEnd` (`in`)). Flag: this touches committed Phase-2 signatures —
mechanical, compiler-enforced, no behavior change for handle-free elements (DeepEqual fast path
first).

*Watch:* if the same-instance alias case (`$R` vs `$R'`) makes even Producer-side `DeepEqual` fail
at `canon stream.write $ST` (its `$ST` uses `$R` while the export type uses `$R'`), the identity
compare above already reconciles it (`aIn == bIn`, tags canonicalize to one origin). This is the
main integration point with the in-flight resource-identity work — coordinate at rebase.

### 3.4 The per-element transfer itself (`stream.go` guestBuffer)

`guestBuffer` gains the owning instance + a precomputed flag:

```go
type guestBuffer struct {
	// ... existing ...
	inst   *Instance // owning instance: handle table + canonTag for own-elements; nil in host-only tests
	ownElem *binary.OwnDesc // non-nil ⇔ elem is a top-level own — the per-element transfer arm
}
// newGuestBuffer gains `inst *Instance`; derive ownElem inside from elem.
// Callers: streamCopyHostFunc/futureCopyHostFunc pass `in` (they close over it already).
```

Transfer happens exactly where the reference does it — inside the buffer's `read`/`write`, so the
rendezvous copy (`copyElements` general path: `vs, _ := src.read(n); dst.write(vs)`) is untouched
and laziness/partiality are structural:

```go
// guestBuffer.read — after abi.Load of element i (own is loaded as a bare
// uint32 handle per abi's own-as-i32 mapping):
	if b.ownElem != nil {
		h, _ := v.(uint32)
		rep, err := b.inst.resources.TakeOwn(b.inst.canonTag(b.ownElem.ResourceType), h)
		if err != nil {
			return nil, fmt.Errorf("stream/future buffer read: element %d: own<%d>: %w", i, b.ownElem.ResourceType, err)
		}
		v = rep // the host-level intermediate for own is the REP (repToProviderHandle's existing convention)
	}

// guestBuffer.write — before abi.Store of element i:
	if b.ownElem != nil {
		rep, ok := v.(uint32)
		if !ok {
			return fmt.Errorf("stream/future buffer write: element %d: expected a uint32 rep, got %T", i, v)
		}
		v = b.inst.resources.NewOwn(b.inst.canonTag(b.ownElem.ResourceType), rep)
	}
```

Each side uses **its own** canon-bound desc + table (writer's buffer lifts with the writer's tag,
reader's lowers with the reader's) — the same rep-intermediate convention `repToProviderHandle`/
`providerHandleToRep` use for direct call args, applied per element. `hostBuffer` needs no change:
host-side values are reps by the same convention (host streams of resources remain
by-convention/undocumented — deviation note §3.6). `TakeOwn` already enforces lend accounting
(`"cannot remove owned resource while borrowed"`) and own-vs-borrow kind — mid-copy failure
propagates as `copyElements`' error → the copy builtin's panic → a trap, with elements 0..k-1
already transferred (reference behavior: the trap poisons the offending component; partial transfer
is observable and correct).

### 3.5 Trap-text alignment (`resource.go` ~266)

`handleTable.lookup`'s `"unknown handle %d"` → `"unknown handle index %d"` — required verbatim by
`passing-resources`' `(assert_trap ... "unknown handle index 3")`; every existing test matches the
substring `"unknown handle"` and stays green (verify with a grep + full run).

### 3.6 Invariants, suites, oracle, deviations

Single-runnable: transfers run inside the rendezvous copy, which always executes on the baton
holder (a copy builtin's frame) — no new concurrency. `handleTable` is already mutex-guarded
besides. No-hang/no-leak: no new parks or goroutines.

Verification: delete `passing-resources`' skip → both asserts green (run=42 exercises: block-no-
transfer, partial transfer, cross-table remint, borrow-lower of the transferred handle via
`[method]R.foo`, drop-readable→DROPPED, cancel-write progress); then full conformance + `-race`.
Regression focus: every existing stream/future suite (`drop-stream`, `partial-stream-copies`,
`cross-task-future`, zero-length) — they take the DeepEqual fast path and unchanged `read`/`write`
(nil `ownElem`).

Trace-oracle: this feature is the best oracle fit of the three — add an `async_scenarios.json`
scenario: `stream<own R>` write-2/read-1/rep-after-transfer/cancel; the vendored `definitions.py`
implements the reference transfer natively; regenerate `async_oracle_golden.json` via
`gen_async_oracle.py` and let the deep-diff pin: table-removal timing (at copy, not at write-call),
minted-index determinism, TakeOwn-on-lent trap. If the scenario schema lacks `resource.new`/`rep`
ops, extend `gen_async_oracle.py`'s op set (it's project-owned) rather than skipping oracle
coverage.

Deviations/ambiguity:
- **Nested own (e.g. `stream<record{own R}>`)**: still bind-rejected. The reference supports it via
  its recursive lower/lift context; wazy's `resolveHandleArg`-style top-level-only convention is
  kept (the suite only needs top-level). Recorded as the residual ceiling in the (updated) bind
  error text.
- **Borrow-in-element**: rejected at bind with a spec-grounded message; the reference treats it as
  validation-impossible (never reaches runtime) — same observable.
- **Host-side streams of resources**: reps cross the host boundary raw (no host handle table) —
  documented convention, matches delegated-call args today.

---

## Risk ranking & implementation order

**1 (highest) — Feature 1.** Touches the committed async core's *call sites* (guest_task.go,
async_builtins.go, stream_builtins.go, host_import.go, graph.go delegate, reap, both invoke entry
points) even though `sched`/`parkedTask` themselves are untouched. Behavioral risk is fenced by the
two-condition gate (`mayBlockSync` ∧ guest-caller) — unflagged instances provably execute today's
code — but the gate computation itself (graph-bind sweep) and the `drainReady` addition are the two
places a green suite could shift. `async-calls-sync` run2 additionally carries stackful-§12.4's
pre-declared YIELD-fairness residue risk (bounded fallback: narrow re-skip). Implement in §1.8's
order; commit the zero-length unskip separately first.

**2 — Feature 3.** New semantics in a well-tested subsystem; the risky part is not the transfer
(local, convention-following) but the elem-identity compare threading (`peek/take` signature change,
four compare sites) and its coupling to the uncommitted resource-identity work — rebase first,
implement `elemTypesCompatible` on top of `originOf`.

**3 (lowest) — Feature 2.** ~30 lines of bind-time plumbing with an existing lower-side precedent;
residual risk is only that `cross-abi-calls` reveals a *further* pre-existing arity gap post-bind,
which becomes a documented narrow skip, not scope growth.

Suggested landing order: **2 → 3 → 1** (rising risk, each independently green), or 1 first if the
parking work should soak longest — the three are file-disjoint except `graph.go`/`instance.go`
touch-points and compose without ordering constraints.

# wazy async Phase 3 — cancellation, guest↔guest async, borrow scopes

Successor to `component-model-async-runtime-design.md` (Phase 1) and
`component-model-async-phase2-design.md` (Phase 2), keeping their conventions verbatim:
transliterate `internal/component/abi/testdata/definitions.py`, keep every guest-reachable
`trap_if`, collapse green-thread machinery onto the FIFO `sched` + `errAsyncDeadlock`, flip
state **at delivery** inside `pendingEvent` closures, wire every builtin as a `hostFuncDef`
`canonToDef` case (fixed i32 sigs, no reflection). Reference line numbers below are into the
vendored definitions.py.

Phase 3 scope (plan §3, Phase 3): **task/subtask cancellation** (`canon_task_cancel` ~2393,
`canon_subtask_cancel` ~2465, `Task.request_cancellation`/`deliver_pending_cancel` ~527-549,
`Subtask` cancel states ~847-903), **async-lowered calls to guest exports** (composition A→B,
`canon_lower`'s callee-is-a-lift shape ~2231-2295 + `canon_lift` ~2144-2207), **borrow
scopes** (`ResourceHandle.borrow_scope` ~725, `lower_borrow` ~1809, `canon_resource_drop`'s
borrow arm ~2324, `Task.num_borrows` traps ~523/558/565), and **`resource.drop async`**
(canon kind 0x07). Public `CallAsync` (external concurrency from arbitrary goroutines) stays
deferred; everything here still runs on the one goroutine that called `Call`/`CallExport` —
but the *internal* cancel entry (`task.requestCancellation`) is exactly the func a future
`CallAsync` returns, so nothing below is reshaped by it later.

---

## 0. The one new mechanism, stated once

Phases 1–2 have exactly one task alive, driven to completion inside `invokeAsyncCallback`'s
Go frame: WAIT = a nested `sched.drive`. That is why guest↔guest async and cancellation are
ceilings today: a subtask over a guest callee can never be observed unresolved (the callee is
driven to completion before the caller regains control), so `STARTING`/`STARTED` subtask
states, `BLOCKED`, `TASK_CANCELLED`, and `CANCELLED_BEFORE_*` are all unreachable.

Phase 3's single structural addition: **a callback-lift task becomes a parkable state
machine** (`guestTask`, §1). This is free precisely because of the callback-ABI decision
(plan §2): every suspension is already a normal core return, so "park" is just *remembering
the loop position in a struct instead of holding it in a Go frame*. The reference's
`Thread.suspend/resume` becomes:

- **run**: call the core func / callback, interpret the packed code;
- **park**: store `(waitableSet, cancellable)` on the `guestTask` and *return* to whoever
  called us (the scheduler, or the async-lower wrapper that started us);
- **resume**: `sched.step` (or `request_cancellation`) calls `gt.resume(event)`, which calls
  the callback core func and continues the loop.

`Store.tick`'s "pick a ready thread" becomes: `sched.step` pops the FIFO runq first, else
resumes the first-registered parked task whose `ready()` predicate holds. The deterministic
profile's `random.choice → first` monkeypatch (plan §6) makes this the oracle's own order.

Everything else in Phase 3 is transliteration on top of this: cancellation is "resume a
parked task with `(TASK_CANCELLED,0,0)`", guest↔guest async is "start a `guestTask` in the
callee and return to the caller while it's parked", entry backpressure is "park before the
first run".

**Scheduler sharing (forced change #1, §6).** Cross-instance resumption only works if A's
WAIT drive can resume B's parked task. The reference has one `Store`; wazy gets one `*sched`
per composition **tree**: `Instance.sched` changes from a value to a pointer, created by the
root instantiation and inherited by `instantiateNestedInstances` via `subCfg.sharedSched`.
A flat instance's behavior is unchanged (its tree is itself).

```go
// sched.go (extended)
type sched struct {
	runq    []func() error
	pumping bool
	parked  []*guestTask // registration order == deterministic resume order
}

func (s *sched) park(gt *guestTask)   { s.parked = append(s.parked, gt) }
func (s *sched) unpark(gt *guestTask) { /* remove by identity */ }

// step: FIFO runq first (unchanged), else resume the first ready parked task.
func (s *sched) step() (progressed bool, err error) {
	if len(s.runq) > 0 { /* exactly the Phase-1 body */ }
	for _, gt := range s.parked {
		if gt.ready() {
			s.pumping = true
			err = gt.resumeReady() // §1.3; re-parks or finishes; may mutate s.parked
			s.pumping = false
			return true, err
		}
	}
	return false, nil
}
```

`drive(pred)` and `errAsyncDeadlock` are unchanged: runq empty **and** no parked task ready
**and** pred false is still a guaranteed-permanent deadlock (nothing external exists).
`pumpSnapshot` (YIELD) gains one clause to stay faithful to the deterministic profile
("everything ready runs once, then the yielder resumes"): after the runq snapshot, resume
each *currently-ready* parked task once (snapshot the parked slice first).

---

## 1. The resumable guest task

### 1.1 guestTask (new file `instance/guest_task.go`)

```go
type guestParkKind uint8

const (
	parkNone  guestParkKind = iota
	parkEntry               // enter_implicit_thread's backpressure wait (~492-498)
	parkWait                // callback WAIT on a waitable set (~2185-2188)
	parkYield               // callback YIELD (~2179-2184)
)

// guestTask is the reference (Task + its implicit Thread) for a callback lift,
// with the thread's stack replaced by explicit park state. Phase 1's
// invokeAsyncCallback becomes "start + drive until done" over this (§1.4).
type guestTask struct {
	t   *task
	in  *Instance // == t.inst; the CALLEE instance
	be  *boundExport
	ctx context.Context

	park        guestParkKind
	wset        *waitableSet // parkWait only
	cancellable bool         // is the current park a cancellable suspension?
	cancelWake  bool         // request_cancellation resumed us with Cancelled.TRUE (§2.2)

	onStart func() ([]abi.Value, error) // reference OnStart: produce lifted args (lazily!)
	done    bool
	err     error // a trap during a scheduler-driven resume, reported to the finisher
	finish  func(err error) // notifies whoever started us (host entry or async lower)
}

func (gt *guestTask) ready() bool {
	switch gt.park {
	case parkEntry:
		return !gt.in.hasBackpressure() // §1.2
	case parkWait:
		// wait_for_event_and(lambda: not exclusive, ...) — the conjunct is
		// load-bearing now (another task may hold the callee's exclusive).
		return !gt.in.exclusiveHeld &&
			(gt.cancelWake || gt.t.cancelDeliverable() || gt.wset.hasPendingEvent())
	case parkYield:
		return !gt.in.exclusiveHeld
	}
	return false
}
```

### 1.2 Task entry — backpressure parks instead of trapping

Phase 1's `enterTask` trapped on any concurrency because none could exist. Phase 3
transliterates `Task.enter_implicit_thread` (~485-503) for real:

```go
// instance.go additions
func (in *Instance) hasBackpressure() bool { // reference has_backpressure (~489)
	return in.backpressure > 0 || in.exclusiveHeld // needs_exclusive() is always true (callback lift)
}

// task.go — replaces enterTask's trap arm
// tryEnter: returns entered=false when the task must park at parkEntry.
func (in *Instance) tryEnter(t *task) (entered bool) {
	if in.hasBackpressure() || in.numWaitingToEnter > 0 { // FIFO fairness (~492)
		return false
	}
	in.activeTask = t
	in.exclusiveHeld = true
	return true
}
```

`Instance` gains `numWaitingToEnter int` (incremented while a `guestTask` sits at
`parkEntry`, mirroring ~493-495). `activeTask` keeps its Phase-1 meaning narrowed one
notch: **the task whose guest code is running right now** (nil while everything is
parked) — the builtins' `requireActiveTask` is untouched. `exclusiveHeld` follows the
reference exactly: held while running, released across every suspension (the Phase-1 loop
already set/cleared it at the right sites; those sites move into §1.3's brackets).

`mayEnter` moves from "cleared for the whole invokeAsyncCallback" to the reference's
bracket: cleared by `enterRun`, restored by `leaveRun` (below) — i.e. false only while
guest code of this instance is actually on the stack, true while parked. This is what makes
a second entry into a busy-but-parked instance legal (gated by exclusive/backpressure
parking, not a trap), and it is the Phase-1-committed-code change with the widest blast
radius (§6 #3).

```go
func (in *Instance) enterRun(t *task) { in.mayEnter = false; in.activeTask = t; in.exclusiveHeld = true }
func (in *Instance) leaveRun()        { in.exclusiveHeld = false; in.activeTask = nil; in.mayEnter = true }
```

### 1.3 start / resume — the callback loop as a state machine

`startGuestTask` is `canon_lift`'s thread_func front half (~2145-2173); `resumeReady` +
`runLoop` are the loop body (~2175-2197). One function holds the loop so the packing,
trap texts, and exclusive brackets exist exactly once:

```go
// start: attempt entry; if parked, register and return (the caller — an async
// lower — sees the subtask still STARTING). If entered, run on_start (lower
// params), the first core call, then the loop until EXIT or the first park.
func (gt *guestTask) start() error {
	if !gt.in.tryEnter(gt.t) {
		gt.park, gt.cancellable = parkEntry, true // entry wait is cancellable (~494)
		gt.in.numWaitingToEnter++
		gt.in.sched.park(gt)
		return nil
	}
	return gt.firstRun()
}

// firstRun: task.start() -> lower params -> core call -> runLoop(packed).
// Bodies are the Phase-1 invokeAsyncCallback prologue verbatim (arg lowering,
// pooled stacks, packed i32), reparented here; on_start is called HERE, not at
// canon_lower time — the reference lifts caller args lazily (~2250-2254), so a
// parked entry reads the caller's memory when the callee actually starts.
func (gt *guestTask) firstRun() error

// resumeReady: called only by sched.step / pumpSnapshot when ready() is true.
func (gt *guestTask) resumeReady() error {
	switch gt.park {
	case parkEntry:
		gt.in.numWaitingToEnter--
		gt.in.sched.unpark(gt)
		if gt.cancelWake { // request_cancellation hit us at INITIAL (§2.2)
			return gt.t.cancelUnentered() // on_resolve(nil,cancelled) — no guest code runs
		}
		return gt.firstRun()
	case parkWait:
		ev := eventTuple{code: eventTaskCancelled}
		if !gt.cancelWake && !gt.t.deliverPendingCancel(true) {
			ev = gt.wset.getPendingEvent()
		}
		gt.cancelWake = false
		gt.in.sched.unpark(gt)
		return gt.runLoop(ev)
	case parkYield:
		ev := eventTuple{code: eventNone}
		if gt.cancelWake || gt.t.deliverPendingCancel(true) { ev = eventTuple{code: eventTaskCancelled} }
		gt.cancelWake = false
		gt.in.sched.unpark(gt)
		return gt.runLoop(ev)
	}
}

// runLoop: deliver ev to the callback, then interpret packed codes until EXIT
// or a park. The Phase-1 loop body moves here byte-for-byte, with two changes:
// (a) brackets: gt.in.enterRun(gt.t) before each callback call, gt.in.leaveRun()
//     at each park/exit (the reference's exclusive_thread = None / = thread and
//     Store.tick's enter_from/leave_to, ~2176-2192 + ~613-615);
// (b) WAIT with no pending event PARKS (park=parkWait, wset=ws, cancellable=true,
//     sched.park, ws.numWaiting++ held across the park) instead of driving a
//     nested sched loop; YIELD parks at parkYield (cancellable=true).
// deliver_pending_cancel(true) is checked at each suspension's prologue exactly
// where wait_until does (~404).
func (gt *guestTask) runLoop(ev eventTuple) error

// finishExit: EXIT observed. trap_if(state != RESOLVED) (unregister_thread ~522)
// keeps Phase 1's exact text ("callback returned EXIT before task.return resolved
// the task"); then leaveRun, done=true, finish(nil).
```

A trap during a *scheduler-driven* resume (the callback panics or returns garbage) sets
`gt.err`/`done` and propagates out of `sched.step` as the error return, exactly like a
failing thunk today — the outermost drive surfaces it.

### 1.4 invokeAsyncCallback — preserved surface, new engine

```go
func (in *Instance) invokeAsyncCallback(ctx context.Context, be *boundExport, exportName string, args []abi.Value) ([]abi.Value, error) {
	// prologue checks identical to Phase 1 (arity, coreFn, callbackFn, flatten,
	// mayEnter trap text unchanged)
	t := &task{inst: in, be: be}
	t.onResolve = func(vals []abi.Value, cancelled bool) { t.result = vals } // §2.1 signature change
	gt := &guestTask{t: t, in: in, be: be, ctx: ctx,
		onStart: func() ([]abi.Value, error) { return args, nil },
		finish:  func(err error) { /* record */ }}
	if err := gt.start(); err != nil { return nil, err }
	if err := in.sched.drive(func() bool { return gt.done }); err != nil {
		if errors.Is(err, errAsyncDeadlock) { /* Phase-1 deadlock text, verbatim */ }
		return nil, err
	}
	return t.result, nil
}
```

Host-entry behavior is bit-identical for every Phase-1/2 test: a first-light export never
parks (start runs to EXIT inside `gt.start`); a WAITing export parks and the `drive` here
pumps the same FIFO the nested drive pumped, delivering the same events in the same order;
the deadlock and "EXIT before task.return" error strings are preserved (tests assert them).

---

## 2. Cancellation

### 2.1 task side — states, request_cancellation, the task.cancel builtin

`taskState` already has the reference numbering; Phase 3 makes `taskPendingCancel` and
`taskCancelDelivered` real. **Forced signature change** (§6 #2): resolving-as-cancelled must
be distinguishable from "returned with an empty result" (task.return of a no-result func
already passes a nil slice), so:

```go
// task.go
type task struct {
	// ... Phase 1 fields unchanged ...
	onResolve func(vals []abi.Value, cancelled bool) // was func([]abi.Value)
	cancelled bool
}

func (t *task) cancelDeliverable() bool { return t.state == taskPendingCancel }

// deliverPendingCancel mirrors Task.deliver_pending_cancel (~545).
func (t *task) deliverPendingCancel(cancellable bool) bool {
	if cancellable && t.state == taskPendingCancel {
		t.state = taskCancelDelivered
		return true
	}
	return false
}

// cancelResolve is Task.cancel (~563): the guest ACKS a delivered cancel.
func (t *task) cancelResolve() error {
	if t.state != taskCancelDelivered {
		return fmt.Errorf("task.cancel: no cancellation has been delivered to this task")
	}
	if t.numBorrows > 0 {
		return fmt.Errorf("task.cancel: %d borrowed handle(s) still held", t.numBorrows)
	}
	t.onResolve(nil, true)
	t.cancelled = true
	t.state = taskResolved
	return nil
}

// cancelUnentered: the INITIAL arm's landing (§1.3 parkEntry+cancelWake):
// state is already taskCancelDelivered; resolve without running guest code.
func (t *task) cancelUnentered() error { return t.cancelResolve() }
```

`returnValues` keeps its two traps and calls `t.onResolve(vals, false)`. The task.return
builtin is otherwise untouched. **Note**: after `CANCEL_DELIVERED`, `task.return` remains
legal (the guest may finish normally instead of acking) — `returnValues` has no state guard
besides RESOLVED, matching the reference.

`requestCancellation` transliterates `Task.request_cancellation` (~527-543). It is called
by `canon_subtask_cancel` via `subtask.onCancel` (the only Phase-3 caller) and is the func
a future `CallAsync` returns:

```go
// requestCancellation: gt is the task's guestTask (task gains a `gt *guestTask`
// back-pointer, set by startGuestTask; nil never observed — every async task now
// has one).
func (t *task) requestCancellation() error {
	switch t.state {
	case taskInitial: // parked at entry, on_start never ran (~529-531)
		t.state = taskCancelDelivered
		t.gt.cancelWake = true
		// resume NOW, synchronously (the reference resumes the thread inline):
		return t.gt.resumeReady() // -> cancelUnentered -> on_resolve(nil,true)
	case taskStarted:
		if t.gt.park != parkNone && t.gt.cancellable && !t.inst.exclusiveHeld && t.inst.mayEnter {
			// candidates non-empty && may_enter_from(caller) (~537)
			t.state = taskCancelDelivered
			return t.gt.resumeReady() // delivers (TASK_CANCELLED,0,0) inline (~538-541)
		}
		t.state = taskPendingCancel // (~543)
		return nil
	default:
		panic("BUG: request_cancellation on a resolved task") // reference assert
	}
}
```

Two collapse notes, kept as comments at the site:

- The reference excludes the implicit thread from candidates when another thread holds the
  instance's exclusive (~535-536); with exactly one thread per task and `exclusiveHeld`
  false at every park, the `!t.inst.exclusiveHeld` conjunct is the whole surviving check —
  it can only be true here if a *different* task of the same instance is running, in which
  case the parked target isn't resumable and we take PENDING_CANCEL, exactly as the
  reference would.
- **Deviation (blocking-builtin park)**: a task suspended inside the *blocking*
  `waitable-set.wait` builtin has live Go frames beneath it, so it cannot be resumed
  synchronously from here. Its drive predicate (§2.3) includes `t.cancelDeliverable()`,
  so we set PENDING_CANCEL and it wakes at the drive loop's next check — same events, the
  cancelled guest just runs a few thunks later than the reference's inline resume. Parked
  *callback* tasks (the wit-bindgen shape, and the only shape the oracle's deterministic
  scenarios drive) resume inline, matching the reference exactly.

**The task.cancel builtin** (`CanonKindTaskCancel` 0x05, core sig `() -> ()`):

```go
// async_builtins.go
func taskCancelHostFuncGraph(in *Instance) hostFuncDef {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, _ []uint64) {
		t := requireActiveTask(in, "task.cancel") // may_leave + current_task (~2394-2395)
		// trap_if(not task.opts.async_) (~2396): every task this package creates
		// is an async callback lift, so the trap is structurally unreachable;
		// asserted via t.be.asyncCallback for oracle parity.
		if err := t.cancelResolve(); err != nil {
			panic(fmt.Errorf("component/instance: %w", err))
		}
	})
	return hostFuncDef{fn: fn, params: nil, results: nil}
}
```

### 2.2 subtask side — cancel states, on_cancel, the subtask.cancel builtin

`waitable.go` additions (the reference fields Phase 1 deliberately left out, ~856-866):

```go
type subtask struct {
	waitable
	state       subtaskState
	lenders     []*resourceEntry
	flatResults []uint64
	applyResolve func(results []abi.Value) error // unchanged (host-import arm)

	onCancel              func() error // reference Subtask.on_cancel: the callee's cancel entry
	cancellationRequested bool
}
```

`subtask.resolve` already accepts the CANCELLED_* states (flat-results assert holds). The
event closure (installed by `AsyncCall.Resolve` today, and by §3's callee bridge) is
unchanged: `p2` reads `st.state` at delivery, so a cancelled resolution automatically
delivers `SUBTASK` with `CANCELLED_BEFORE_{STARTED,RETURNED}`.

**The subtask.cancel builtin** (`CanonKindSubtaskCancel` 0x06, payload `Canon.Async`,
core sig `(i:i32) -> i32`) — `canon_subtask_cancel` (~2465-2486) line-for-line:

```go
const blockedSentinel = 0xffff_ffff // reference BLOCKED (~2463)

func subtaskCancelHostFuncGraph(in *Instance, canon binary.Canon) hostFuncDef {
	async := canon.Async
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "subtask.cancel")
		i := api.DecodeU32(stack[0])
		e, ok := in.resources.getEntry(i)
		st, isSub := e.(*subtask)
		if !ok || !isSub { panic(...) }                       // trap_if(not isinstance(...))
		if st.resolveDelivered() { panic(...) }               // trap_if(resolve_delivered)
		if st.cancellationRequested { panic(...) }            // trap_if(cancellation_requested)
		if st.inWaitableSet() && !async { panic(...) }        // trap_if(in_waitable_set and not async_)

		if !st.resolved() {
			st.cancellationRequested = true
			if st.onCancel != nil { // nil only for a host import with no OnCancel hook (§2.4)
				if err := st.onCancel(); err != nil { panic(...) }
			}
			if !st.resolved() {
				if async {
					stack[0] = blockedSentinel
					return
				}
				// wait_for_pending_event (~776): sync cancel blocks. Waitable
				// gains syncWaiter (retiring the Phase-1/2 "no sync waiters"
				// collapse) so waitable.join's trap_if(has_sync_waiter) is real.
				st.syncWaiter = true
				err := in.sched.drive(st.hasPendingEvent)
				st.syncWaiter = false
				if err != nil { /* errAsyncDeadlock -> "...cancel is waiting on a subtask that nothing can resolve" */ }
			}
		}
		ev := st.getPendingEvent() // asserts SUBTASK/i/state (~2483-2484); delivers lend release
		_ = ev
		stack[0] = uint64(uint32(st.state))
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}
}
```

Companion changes: `waitable` gains `syncWaiter bool`; `waitableJoinHostFunc` gains
`trap_if(w.syncWaiter)` (~2452), retiring that documented collapse; `graph.go` passes the
`canon` into the new case (it already has it in hand).

Resolved-with-undelivered-event fast path (`if subtask.resolved(): assert(has_pending_event)`
~2473-2474): falls through to `getPendingEvent` — cancel of an already-resolved-but-
undelivered subtask just delivers, returning `RETURNED`. Reachable and tested.

### 2.3 Delivery sites inventory

| Site | Reference | Phase 3 form |
|---|---|---|
| callback WAIT park | `wait_for_event_and(..., cancellable=True)` (~2188) | `resumeReady` parkWait arm: `cancelWake \|\| deliverPendingCancel(true)` → `(TASK_CANCELLED,0,0)` |
| callback YIELD park | `wait_until(..., cancellable=True)` (~2180) | parkYield arm, same check |
| blocking `waitable-set.wait` builtin | `wait_for_event(cancellable)` (~2415) | drive pred becomes `ws.hasPendingEvent() \|\| (cancellable && t.cancelDeliverable())`; on cancel wake, `deliverPendingCancel(true)` then store `(TASK_CANCELLED,0,0)`. `cancellable` is `canon.Cancellable`, finally threaded through (`waitableSetWaitHostFunc(in, canon)` — signature change, §6 #6) |
| `waitable-set.poll` builtin | `poll(cancellable)` prologue (~834) | `if cancellable && t.deliverPendingCancel(true) { return TASK_CANCELLED }` before the Phase-1 body |
| task entry park | `wait_until(..., cancellable=True)` (~494) | `requestCancellation` INITIAL arm → `cancelUnentered` |
| `yield` canon (0x0c) | `canon_yield(cancellable)` | **still not decoded** (Phase 0 left 0x0c out); wit-bindgen yields via the callback code. Unchanged omission, noted in decoder doc |

### 2.4 Host-import cancellation — the AsyncCall additions

```go
// async_host_import.go
// OnCancel registers fn as the reference's Subtask.on_cancel for this call: it
// runs synchronously inside the guest's subtask.cancel. fn may Resolve/
// ResolveCancelled synchronously (the guest then sees the final state, never
// BLOCKED) or later via Defer (async cancel returns BLOCKED; sync cancel
// blocks on the scheduler). Must be called before the AsyncHostFunc returns.
func (ac *AsyncCall) OnCancel(fn func()) { ac.st.onCancel = func() error { fn(); return nil } }

// ResolveCancelled resolves the call as cancelled (reference on_resolve(None),
// ~2258-2264): CANCELLED_BEFORE_RETURNED — the import had already started (the
// wrapper lifts args eagerly, so a host-import subtask is never STARTING).
// Same one-shot + same-goroutine discipline as Resolve; same parked-path
// pending-event installation.
func (ac *AsyncCall) ResolveCancelled() {
	// trap-shaped guards identical to Resolve; requires ac.st.cancellationRequested
	// (the reference asserts result==None implies cancellation_requested)
	ac.st.resolve(subtaskCancelledBeforeReturned, nil)
	// inCall fast path / parked pendingEvent installation — shared with Resolve
}
```

A host import that registers no `OnCancel`: `st.onCancel` stays nil, the cancel request is
recorded (`cancellationRequested`) and otherwise ignored — spec-legal (a callee may ignore
cancellation; the subtask resolves whenever the import completes). Documented on
`WithAsyncImport`.

### 2.5 Reachability — honest table (single-threaded, callback-only, no public CallAsync)

| Path | Reachable? | How / why kept |
|---|---|---|
| `subtask.cancel` on guest↔guest subtask, callee parked at WAIT/YIELD | **YES — the flagship** | inline `TASK_CANCELLED` resume; callee acks via `task.cancel` (→ `CANCELLED_BEFORE_RETURNED`) or completes (→ `RETURNED`) |
| `BLOCKED` return (`async_=true`) | **YES** | callee's cancel resume re-parks without resolving (callback returns WAIT again) |
| sync cancel's blocking drive | **YES** | callee resolves via its own import's `Defer` chain; empty queue → deadlock trap |
| `CANCELLED_BEFORE_STARTED` | **YES** | B holds `backpressure.inc`, A's async call parks at entry (STARTING), A cancels |
| `CANCELLED_BEFORE_RETURNED` | **YES** | ack path above; also host import `ResolveCancelled` |
| `task.cancel` builtin | **YES** | only for tasks with a cancelling caller (guest↔guest); trap arm ("no cancellation delivered") reachable by calling it uncancelled |
| `PENDING_CANCEL` → later delivery | **Edge-reachable** | needs the target parked non-cancellably (blocking builtin with `cancellable=false`, or exclusive held by a sibling task) — 3-instance / 2-task shapes; implemented (cheap), oracle-tier tests |
| `request_cancellation` `may_enter_from` false arm | **Dead-kept** | requires the target instance running while its caller runs — impossible on one thread; comment cites this |
| cancel of a **host-entry** task (`Call`) | **Unreachable** | no cancel API on `Call`; `requestCancellation` is exactly what `CallAsync`'s cancel func will call — no reshaping later |
| candidates-exclusion via `exclusive_thread not in {None, implicit}` (~535) | **Collapsed** | one thread per task; the surviving `!exclusiveHeld` conjunct covers the observable case |

---

## 3. Async-lowered calls to guest exports (guest↔guest)

### 3.1 Bind-time wiring

Today `instantiateNestedInstances` registers `delegatingHostImport(sib, name, be, provToImp)`
with only `hi.fn`, so an async lower over it fails at bind ("register it with
WithAsyncImport instead"). Phase 3 registers **both arms**:

```go
// graph.go, in the arg.Sort == 0x05 loop:
hi := delegatingHostImport(sib, name, be, provToImp) // sync arm, unchanged
hi.asyncTarget = &guestAsyncTarget{sub: sib, be: be, exportName: name, provToImp: provToImp}
subCfg.imports[mkImportKey(arg.Name, name)] = hi
```

`computeCanonHostFunc`'s lower case: `isAsyncLower && hi.asyncFn == nil && hi.asyncTarget ==
nil` is now the bind error; an `asyncTarget` routes to `buildAsyncHostWrapper` the same as
an `asyncFn` (the wrapper branches internally, §3.2). Sync lower → async export needs **no
change**: `delegatingHostImport.fn` calls `sub.invoke`, which routes to
`invokeAsyncCallback`'s drive-to-completion — precisely `canon_lower`'s sync arm
`wait_until(subtask.resolved)` (~2273-2278) collapsed, as Phase 1 established.

```go
type guestAsyncTarget struct {
	sub        *Instance
	be         *boundExport
	exportName string
	provToImp  func(uint32) (uint32, bool)
}
```

### 3.2 The wrapper's callee arm — the FuncInst protocol across two tables

`buildAsyncHostWrapper` keeps its whole shape (core sig `params... [retptr] -> packed i32`,
`MAX_FLAT_ASYNC_PARAMS`, epilogue packing `state | subtaski<<4`). The block that today does
"lift args eagerly → set STARTED → call `hi.asyncFn`" becomes, for `hi.asyncTarget != nil`,
the reference `canon_lower` callee protocol (~2246-2271) verbatim — **on_start and
on_resolve are real closures bridging the two instances' tables**, and arg lifting moves
*inside* on_start (the reference lifts lazily; observable when B's entry parks and A
mutates the arg buffer first):

```go
fn := api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
	st := newSubtask()
	flat := append([]uint64(nil), stack[:flatParamWidth]...) // lazy-lift snapshot (CoreValueIter)
	var retPtr uint32
	if outPtrIdx >= 0 { retPtr = api.DecodeU32(stack[outPtrIdx]) }

	// reference on_start (~2250): flip STARTING->STARTED, lift A-side args
	// (borrow<T> params addLender on st, as liftAsyncHostArgsPlanned already
	// does), then map importer handles/reps to PROVIDER handles — the same
	// repToProviderHandle walk the sync delegate does, but per the callee
	// task's borrow scope (§4: minted borrows carry borrowScope = callee task).
	onStart := func(calleeTask *task) ([]abi.Value, error) {
		args, err := liftAsyncHostArgsPlanned(in, paramPlans, resolve, flat, memMod, resources, st)
		if err != nil { return nil, err }
		st.state = subtaskStarted
		return mapArgsToProvider(tgt, args, calleeTask) // repToProviderHandle + scope minted borrows
	}

	// reference on_resolve (~2256): nil+cancelled -> CANCELLED_BEFORE_*;
	// else lower B's results back: provider handles -> reps -> A's importer
	// values (providerHandleToRep), abi.Store through retPtr with A's realloc,
	// memory fetched FRESH (Phase-1 applyResolve discipline), then RETURNED.
	onResolve := func(vals []abi.Value, cancelled bool) error {
		if cancelled {
			if st.state == subtaskStarting { st.resolve(subtaskCancelledBeforeStarted, nil)
			} else { st.resolve(subtaskCancelledBeforeReturned, nil) }
		} else {
			/* provider->importer result mapping + store via retPtr */
			st.resolve(subtaskReturned, nil)
		}
		if parked := st.subtaski(); parked != 0 { installSubtaskEvent(st, parked) } // == AsyncCall.Resolve's parked arm
		return nil
	}

	// reference: subtask.on_cancel = callee(on_start, on_resolve, caller) (~2270)
	onCancel, err := tgt.sub.startAsyncExportTask(ctx, tgt.be, tgt.exportName, onStart, onResolve)
	if err != nil { panic(...) }
	st.onCancel = onCancel

	if st.resolved() { // immediate path: B ran to EXIT without parking
		st.deliverResolve()
		stack[0] = uint64(uint32(st.state)) // RETURNED (or CANCELLED_* — impossible here, asserted)
		return
	}
	subtaski := resources.addEntry(st) // A's table
	st.setSubtaski(subtaski)
	stack[0] = uint64(uint32(st.state)) | uint64(subtaski)<<4 // STARTING or STARTED
})
```

`startAsyncExportTask` is the `Store.lift` func_inst + `canon_lift` front half (~587-595 +
~2144-2207) on the **callee** instance — the same machinery `invokeAsyncCallback` now sits
on, minus the drive:

```go
// async_lift.go
// startAsyncExportTask starts be (an async callback lift of THIS instance) as a
// guestTask whose results flow to onResolve instead of a host caller. Returns
// the task's requestCancellation as the subtask's on_cancel (reference ~2207).
func (in *Instance) startAsyncExportTask(ctx context.Context, be *boundExport, exportName string,
	onStart func(*task) ([]abi.Value, error), onResolve func([]abi.Value, bool) error,
) (onCancel func() error, err error) {
	// trap_if(not inst.may_enter_from(caller)) (~590): mayEnter is true here in
	// every reachable interleaving (the caller is running => this instance is
	// not), kept as a real trap for parity.
	if !in.mayEnter { return nil, fmt.Errorf("...instance is not reenterable") }
	t := &task{inst: in, be: be}
	t.onResolve = func(vals []abi.Value, cancelled bool) {
		if e := onResolve(vals, cancelled); e != nil { panic(...) }
	}
	gt := &guestTask{t: t, in: in, be: be, ctx: ctx,
		onStart: func() ([]abi.Value, error) { return onStart(t) },
		finish:  func(err error) { /* a trap in B propagates to whoever is driving */ }}
	t.gt = gt
	if err := gt.start(); err != nil { return nil, err } // may run to EXIT, may park at entry/WAIT
	return t.requestCancellation, nil
}
```

Enter/leave across the pair falls out of §1's brackets with no extra code: while B runs,
`B.mayEnter=false, B.exclusiveHeld=true`; the wrapper (A's frame) is suspended beneath —
matching the reference where `canon_lower`'s thread blocks in `callee(...)` until the
callee's first suspension. When B parks, `leaveRun` restores B's flags and control returns
through `gt.start` → the wrapper → A's core code, with the packed `STARTING|STARTED |
subtaski<<4` result. B thereafter resumes only via `sched.step` (A WAITing, or a blocking
builtin, or B's own import completions) or via `st.onCancel`.

### 3.3 Traces (the acceptance shapes)

**A→B happy path**: A's export (callback lift) calls the lowered B-import → wrapper →
`startAsyncExportTask` → B enters, `on_start` lifts A's args into B's table, B's core func
returns `WAIT(wsB)` (B awaits its own async host import) → B parks; wrapper returns
`STARTED | stA<<4` to A → A joins stA to wsA, returns `WAIT(wsA)` → A parks → host `drive`
(A was host-entered) pops the import's `Defer` thunk → resolves B's import → B's `ready()`
fires → B's callback runs → `task.return` (B's results lowered through `onResolve` into A's
memory via retptr) → B EXITs → subtask event installs → A's `ready()` fires → A's callback
gets `(SUBTASK, stA, RETURNED)` → A `task.return`s → EXIT → host `drive` sees done.

**Cancel**: as above until A parks; then A (resumed for some other event) calls
`subtask.cancel(stA)` → `st.onCancel` = B's `requestCancellation` → B parked at WAIT,
cancellable → inline resume with `(TASK_CANCELLED,0,0)` → B's callback calls `task.cancel`
→ `onResolve(nil, true)` → `CANCELLED_BEFORE_RETURNED` → builtin's `getPendingEvent`
delivers → returns `[4]` to A. If B instead returns `WAIT` again: builtin returns
`[BLOCKED]` (async form) and B's eventual resolution arrives as a normal SUBTASK event.

---

## 4. Borrow scopes — retiring resource.go's ceiling — and resource.drop async

### 4.1 The fields and the four sites

```go
// resource.go
type resourceEntry struct {
	typeIdx    uint32
	rep        uint32
	own        bool
	lendCount  int
	borrowScope *task // Phase 3: the callee task this borrow handle is scoped to;
	                  // nil for own handles and for host-minted unscoped borrows (§4.3)
}

// NewBorrowScoped mirrors lower_borrow's minting arm (~1813-1815): mint a borrow
// handle in this (the callee's) table scoped to the callee task; the caller
// increments scope.numBorrows. NewBorrow (scope-less) remains for the host-API
// deviation (§4.3) with its Drop rejection intact.
func (t *handleTable) NewBorrowScoped(typeIdx, rep uint32, scope *task) uint32 {
	scope.numBorrows++
	return t.addEntry(&resourceEntry{typeIdx: typeIdx, rep: rep, own: false, borrowScope: scope})
}
```

**Site 1 — mint (lower_borrow ~1809).** The only place wazy lowers a borrow *into a
callee's table* is the composition delegate (`repToProviderHandle`, composition.go:242 —
today `NewBorrow`, leaking an undroppable entry per call). It becomes scope-aware:

- the reference's same-instance exemption (`if cx.inst is t.rt.impl: return rep`, ~1811 —
  **no handle, no num_borrows++**) is already wazy's behavior for guest-defined resources
  (`resolveArgHandles` reduces to rep); getting this wrong (scoping the rep case) makes
  `task.return` trap on every borrow-taking async export — the #1 "what breaks";
- the cross-instance case mints via `NewBorrowScoped(tag, rep, calleeScope)`.

Since `repToProviderHandle` runs before the callee task exists on the sync path, the
delegate's borrow minting moves *after* scope creation: `delegatingHostImport` detects
borrow params at bind time (paramDescs walk); at call time it creates the scope first, then
maps args. Async path: minting happens inside `mapArgsToProvider(tgt, args, calleeTask)`
(§3.2), where the callee task already exists — matching the reference's on_start timing.

**Site 2 — drop (canon_resource_drop's else arm ~2323-2324).** `handleTable.Drop` gains the
borrow arm, replacing the milestone rejection:

```go
func (t *handleTable) Drop(typeIdx, h uint32) error {
	// lookup + typeIdx + lendCount guards unchanged (order: lends checked before
	// the own/borrow split, as the reference does at ~2314)
	if !e.own {
		if e.borrowScope == nil {
			return fmt.Errorf("handle %d is a borrow with no owning call scope; host-minted borrows cannot be dropped by the guest", h)
		}
		e.borrowScope.numBorrows--    // h.borrow_scope.num_borrows -= 1
		delete(t.entries, h)          // handles.remove(i) happens for borrows too (~2311)
		t.free = append(t.free, h)
		return nil
	}
	// own arm unchanged (dtor runs at the canon layer, as today)
}
```

`DropOwned`/`TakeOwn`/`Rep`/`Lend`/`Unlend`: **byte-identical**. The sync-lift trap texts
the `.wast` suites see are untouched.

**Site 3 — task-exit traps.** Already half-wired in Phase 1: `task.returnValues` and
`cancelResolve` trap on `numBorrows > 0` (~558/565). The remaining site is the **sync**
callee (reference: the sync arm of canon_lift calls `task.return_` itself, so the same trap
covers it). wazy's sync `invoke` has no task; the composition delegate supplies a
scope-only one:

```go
// composition.go — delegatingHostImport's fn, when bind-time analysis found
// cross-instance borrow params:
scope := &task{inst: sub}            // scope-only: no be, never entered, no gt
args  := mapArgs(..., scope)         // NewBorrowScoped increments scope.numBorrows
out, err := sub.invoke(ctx, be, exportName, in)
if err == nil && scope.numBorrows > 0 {
	return nil, fmt.Errorf("component/instance: %s: callee returned still holding %d borrowed handle(s)", exportName, scope.numBorrows)
}
```

For the async callee the scope IS the real task (`mapArgsToProvider` scopes to it), and
`task.return`'s existing trap fires with no new code.

**Site 4 — caller-side lending** is Phase-1/2 machinery unchanged: sync host-import wrappers
`Lend`/defer-`Unlend`; async wrappers `addLender`/`deliverResolve` (`lift_borrow` ~1505-1511
— note the reference asserts the caller-side scope is a *Subtask* and the callee-side a
*Task*; wazy's split between `st.addLender` and `NewBorrowScoped` mirrors that exactly).

### 4.2 Gates: what breaks in the shipped resource `.wast` suites if done wrong

Run `resources` / `multiple-resources` (+ the component-model value suites) before/after
every commit in this area — the highest-regression-risk surface in Phase 3 (plan §5):

- **Scoping the same-instance rep case** (see above): every borrow-param call inside one
  instance would start trapping at return. The suites pass borrows intra-component
  constantly — instant red.
- **Free-list ordering**: table indices are guest-observable (Phase-1 doc; oracle pins
  them). Borrow drops now push onto `free` — but only on paths that previously *trapped*,
  so no existing golden can shift. Any change to `add`/`remove` ordering itself is
  forbidden.
- **Trap-text drift**: `resource_test.go` and the harness assert current messages
  ("unknown handle %d", "has %d outstanding borrow(s)"). Only the borrow-drop message
  changes (it was "not supported by this milestone"; it becomes the scope-less variant
  above, and scoped drops succeed).
- **Lend-guard order**: checking own/borrow before `lendCount` would let a lent borrow be
  dropped — the reference checks lends first (~2314); keep that order and its test.
- **Double-drop**: the borrow arm must remove the entry, so a second drop is "unknown
  handle" — same observable as an own double-drop; a named test pins it.
- **Dtor non-run**: a borrow drop must NOT run the resource dtor (only the own arm does);
  the graph-layer `resourceCanonHostFuncGraph` drop case must skip its dtor lookup when the
  entry is a borrow — restructure: ask the table first (`entryIsOwn(typeIdx, h)`), then
  dtor+rep only for own. Getting this wrong double-frees host resources in every WASI
  fixture.

Verification note recorded with the change: current wit-bindgen callees *do* emit
`resource.drop` on cross-instance borrow params at function end — meaning today's shipped
composition fixtures cannot be passing cross-sibling borrows (they'd trap). `git grep` the
fixtures to confirm before enabling the exit trap; if one does and doesn't drop, it predates
the spec rule and gets regenerated, not accommodated.

### 4.3 The retained deviation — host-minted unscoped borrows

`handleTable.NewBorrow` (host-facing: WASI results, resource hooks) keeps minting
`borrowScope = nil` handles that the guest holds long-term and must not drop. This is
wazy's documented WASI-shape deviation (host resources passed as standing handles rather
than per-call borrows); Phase 3 keeps it and keeps its fail-loud drop (message above).
The doc comment ceiling-list entry in resource.go is rewritten from "no borrow scopes
exist" to "host-minted borrows are deliberately unscoped".

### 4.4 `resource.drop async` (canon kind 0x07)

Decoder: add `CanonKindResourceDropAsync byte = 0x07` (payload: `rt:typeidx`, identical to
0x03 — verify against fresh `wasm-tools` output per the Phase-0 discipline, and remove 0x07
from the "not in this list" doc note). Runtime: route to `resourceCanonHostFuncGraph`'s
drop case unchanged, with a comment stating the honest position: the vendored definitions.py
vintage has **no async branch in canon_resource_drop at all** (~2308-2325 lifts the dtor
with `async_ = False` unconditionally); the async form only changes behavior when a dtor
must suspend or the impl instance can't be entered synchronously — both impossible here
(dtors are sync core funcs / Go callbacks, and the single thread means the impl instance is
always enterable when the dropper runs). Decode + execute-as-sync is therefore
observationally correct against the reference we pin. A bind-time nothing; a decoder test
plus one execution test (drop-async of an own handle runs the dtor) covers it.

---

## 5. Keep / trap / omit — Phase 3 disposition table

| Reference item | Disposition | Why |
|---|---|---|
| `Task.State` 5 states | **KEEP — all reachable now** (host-entry tasks never leave INITIAL→STARTED→RESOLVED) | §2.5 table |
| `request_cancellation` INITIAL arm | **KEEP** | reachable via backpressure entry-park (`CANCELLED_BEFORE_STARTED` acceptance test) |
| `request_cancellation` candidates/exclusive set logic (~534-536) | **COLLAPSE** to `park != none && cancellable && !exclusiveHeld` | one thread per task; comment maps each conjunct |
| `deliver_pending_cancel` at every cancellable suspension | **KEEP** (WAIT/YIELD/wait-builtin/poll/entry) | §2.3 inventory; PENDING_CANCEL is edge-reachable |
| synchronous inline resume of the cancelled thread | **KEEP for parked callbacks; DEVIATE for blocking-builtin parks** (drive-loop wake) | live Go frames can't be re-entered; ordering-only difference, flagged §2.1 |
| `Subtask.cancellation_requested` + 4 cancel traps in canon_subtask_cancel | **KEEP verbatim** | all guest-reachable; named test each |
| `BLOCKED` sentinel | **KEEP** | reachable (§2.5) |
| `Waitable.has_sync_waiter` (+ join trap, + wait_for_pending_event) | **RETIRE the Phase-1/2 omission — now implemented** | sync `subtask.cancel` is a real sync waiter that survives across scheduler steps, so the join trap is no longer identically-false |
| `waitable-set.wait/poll` `cancellable` immediate | **KEEP — finally threaded** (was decoded, ignored) | §2.3 |
| `canon_yield` (0x0c) | **OMIT (still undecoded)** | wit-bindgen uses callback YIELD; revisit only on a real fixture |
| `Task.needs_exclusive` false arm (async-without-callback) | **OMIT** | stackful lifts still bind-fail (plan §2) |
| entry parking + `num_waiting_to_enter` FIFO | **KEEP** | required for overlapping A→B calls under guest backpressure; makes `backpressure.inc/dec` mean something |
| `lower_borrow` same-instance rep exemption | **KEEP (already the behavior)** | §4.2's #1 hazard |
| `borrow_scope` on ResourceHandle + `num_borrows` traps | **KEEP** | the ceiling being retired |
| host-minted standing borrows | **DEVIATE (retained)** | §4.3 |
| `resource.drop async` | **KEEP decode; execute as sync drop** | the pinned reference itself has no async branch (§4.4) |
| `Store.invoke` nesting_depth / `tick`'s asserts | **COLLAPSE** (already collapsed in Phase 1) | one driver |
| lazy `on_start` arg lift (~2250) | **KEEP for guest↔guest** (snapshot flat stack, lift at start) | observable when entry parks; host-import wrapper stays eager (its callee starts immediately, so timing is indistinguishable) |
| `on_resolve` result lowering at resolve time, fresh memory | **KEEP** | Phase-1 applyResolve discipline extended |

---

## 6. Forced changes to committed Phase 1/2 code (honest list)

1. **`Instance.sched` value → `*sched`, shared per composition tree** (`instance.go`,
   `graph.go` subCfg threading). Mechanical (`in.sched.` sites compile unchanged via the
   pointer); flat components observably identical.
2. **`task.onResolve` gains the `cancelled bool`** (`task.go`, `async_lift.go`,
   `async_builtins.go` task.return, Phase-2 nothing — streams never resolve tasks).
3. **`invokeAsyncCallback` reimplemented over `guestTask`** — the loop body relocates, the
   public surface, event order (FIFO), and every asserted error string are preserved;
   Phase-1/2 async tests are the gate, run before and after the refactor commit.
4. **`mayEnter`/`exclusiveHeld`/`activeTask` become per-run brackets** instead of
   per-invocation (§1.2). Observable only to programs that could not run before (second
   entry while parked); the Phase-1 "reentrant async call" trap text disappears in favor of
   entry parking — one Phase-1 test asserts that text and is updated to assert the new
   parked behavior instead.
5. **`subtask` gains `onCancel` + `cancellationRequested`; `waitable` gains `syncWaiter`;
   `waitableJoinHostFunc` gains the sync-waiter trap** (`waitable.go`,
   `async_builtins.go`). Zero effect on existing paths (fields idle until cancel is used).
6. **`waitableSetWaitHostFunc`/`waitableSetPollHostFunc` signatures gain `canon`**
   (`graph.go` call sites) to carry `Cancellable`.
7. **`resource.go`**: `resourceEntry.borrowScope`, `NewBorrowScoped`, `Drop`'s borrow arm;
   graph drop-canon learns own/borrow dispatch before dtor lookup (§4.2 last bullet).
   Gate: resource `.wast` suites + `resource_test.go` unchanged-text assertions.
8. **`composition.go` / `graph.go`**: delegate dual-arm (`asyncTarget`), scope-aware borrow
   minting, sync-exit borrow trap.
9. **`binary`**: `CanonKindResourceDropAsync = 0x07` + decoder case + kind-name — the only
   decoder change in Phase 3.
10. **`async_host_import.go`**: `AsyncCall.OnCancel`/`ResolveCancelled`; `buildAsyncHostWrapper`
    callee arm (§3.2). `abi` package: **no changes**.

---

## 7. Test hooks (the named-trap inventory this design implies)

Every trap above gets a named test; the non-obvious ones: subtask.cancel × {unknown handle,
non-subtask entry, resolve-delivered, double-cancel, in-set-and-sync}; cancel of an
already-resolved-undelivered subtask returns RETURNED; BLOCKED then eventual SUBTASK event;
sync cancel deadlock trap; task.cancel with no delivered cancel; task.cancel with
numBorrows>0; TASK_CANCELLED delivered at WAIT vs YIELD vs cancellable wait-builtin vs
poll; PENDING_CANCEL parked-noncancellable delivery; CANCELLED_BEFORE_STARTED via
backpressure.inc; entry-park FIFO fairness (two callers, one backpressured callee);
waitable.join on a sync-waited subtask traps; guest↔guest: immediate-EXIT fast path (no
table entry), STARTED packing, lazy on_start memory read (A mutates arg buffer before B
starts under backpressure), cross-table result handles (own<T> result minted in A, dtor
runs on A's drop), borrow param: same-instance rep (no scope), cross-sibling scoped mint +
drop + exit-trap-if-undropped + double-drop + lent-borrow-drop trap + dtor-not-run-on-
borrow-drop; resource.drop async decodes and runs the dtor; EXIT-before-resolve text
preserved; Phase-1/2 full async suites green across the §6 #3/#4 refactor commit,
`resources`/`multiple-resources` `.wast` green across every §4 commit; oracle scenarios:
scripted A→B cancel-at-each-state traces (deterministic monkeypatches) diffing event
tuples, packed results, and table indices.

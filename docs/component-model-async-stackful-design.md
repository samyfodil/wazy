# Component Model async — Phase 4: stackful lift (handoff-goroutine design)

Status: DESIGN (implements the last big deferred async feature on
`feat/component-model-async`). Builds directly on the Phase 1–3 conventions in
`docs/component-model-async-runtime-design.md` and
`docs/component-model-async-phase3-design.md`; file/symbol references are to
`internal/component/instance/` unless noted. The reference is
`internal/component/abi/testdata/definitions.py` (line refs `~NNN`).

## 0. Problem, shapes, and the seven suites

wazy's async runtime (Phases 1–3) handles only the **callback** lift: every
suspension is a normal core-function return carrying a packed code, so a
suspended task is plain Go state (`guestTask`, guest_task.go) with **no live
wasm frame**. Two lift shapes remain unhandled:

1. **Stackful lift** — a lift of an **async-TYPED** func (`fd.Async == true`)
   with **no `callback` canon option**. Its core code calls
   `waitable-set.wait` (or a blocking stream/future op, or a sync-lowered
   async import) DIRECTLY and must suspend **with live wasm frames beneath
   it** — `be.coreFn.CallWithStack` is blocked mid-execution inside a
   `hostFuncDef` closure. Two sub-shapes, distinguished by the lift's own
   canon opts (reference: `Task.needs_exclusive()` ~481,
   `canon_lift` ~2155/~2167):

   | sub-shape | canon opts | results | needs_exclusive | exercised by |
   |---|---|---|---|---|
   | **sync-opts** (`not opts.async_`) | *(none)* | lifted from flat core results; runtime calls `task.return_` itself (~2158-2159) | **true** | **all 7 suites** (every `(canon lift (core func $m "run"))` of an `async`-typed func) + `async-calls-sync`'s `$SyncMiddle.sync-func` |
   | **async-no-callback** (`opts.async_ && !opts.callback`) | `async` only | core returns `[]`; guest calls the `task.return` builtin; trap at exit if unresolved (~2168-2170, ~522) | false | none of the 7 (design covers it; graph.go currently rejects it — keep supporting it in the same struct behind one flag) |

2. **The sync-task-block trap** — a **sync-TYPED** context (a core module's
   instantiation-time `start` function, or a plain sync lift of a sync-typed
   func) whose code reaches a blocking builtin must TRAP with the spec text
   **`cannot block a synchronous task before returning`**
   (dont-block-start.json, both cases; matched by `strings.Contains`).

Seven vendored suites (`testdata/wast-async/`) unblock:
`cancel-subtask`, `partial-stream-copies`, `empty-wait`, `deadlock`,
`dont-block-start`, `sync-streams`, `zero-length`. Verified shapes:

- `empty-wait`/`deadlock`/`partial-stream-copies`/`sync-streams`/
  `zero-length`/`cancel-subtask`: the top-level `run`/`f` export is a
  sync-opts lift of an async-typed func, invoked from the host; its core code
  async-lowers callbacks-based callees and then calls its OWN
  `waitable-set.wait` inline.
- `cancel-subtask` additionally async-lowers `$C.g` — a sync-opts stackful
  export reached **across a guest↔guest delegation boundary**
  (`startAsyncExportTask` currently mis-drives it through callback machinery).
- `deadlock`: the stackful task blocks forever; expected trap text (substring,
  including the prefix!): **`wasm trap: deadlock detected: event loop cannot
  make further progress`**.
- `dont-block-start` case 1: `start` calls `waitable-set.wait` → trap during
  `Instantiate`. Case 2: `start` makes a **sync-lowered call to an async
  (callback) lifted import whose core body is `unreachable`** → the expected
  trap is still `cannot block a synchronous task before returning`, so the
  trap must fire **before the callee's core code runs** (an eager may-block
  check at the sync-lower site, wasmtime-style — the callee's `unreachable`
  must never be reached).

## 1. The model: reference OS-threads → one-baton goroutines

The reference's `Thread`/`Continuation` (~258-441) is a green thread built on
a real `threading.Thread` plus a lock handoff: at any instant **exactly one**
thread runs; `resume()` releases the suspendee's lock and blocks on the
handler lock; `block()` does the mirror image. `Store.tick` (~606) hands the
baton to one ready waiting thread.

wazy's translation: **a stackful task is a goroutine, and the baton is a pair
of unbuffered channels.** Everything else — `sched`, `task`, `waitable`,
handle tables, `Instance` flags — stays exactly as-is, unlocked, because:

> **Baton invariant.** At any instant, at most one goroutine in a composition
> tree executes component-runtime code (guest wasm, builtins, sched, tables).
> Every other goroutine is blocked on a channel op: a parked stackful task on
> `<-st.resumeCh`, a suspended driver on `<-st.yieldCh` of the task it
> resumed. Every transfer of control is a send/recv pair on an unbuffered
> channel, so every write on one side happens-before every read on the other:
> `go test -race` passes with zero new synchronization on existing state.

The "driver of the moment" forms a chain: host caller → (resumed) stackful
goroutine A → (A drives sched, resumes) stackful goroutine B → … Each link is
blocked in `<-yieldCh` of the next; the deepest link is the only runner. A
stackful task appears in `sched.parked` **iff** its goroutine is blocked on
`<-st.resumeCh` — that is the only state in which `resumeReady` may be called,
so a double-resume is structurally impossible (resume unparks first).

This is deliberately NOT `Store.tick`'s random choice: wazy keeps its
deterministic FIFO profile (established Phase 1, plan §6 — the oracle
monkeypatches `random` to match).

## 2. `stackfulTask` (new file `instance/stackful_task.go`)

```go
// stackfulParkKind names the site a stackfulTask is suspended at.
type stackfulParkKind uint8

const (
	sparkNone  stackfulParkKind = iota
	sparkEntry                  // backpressure wait before first run (~492-498); goroutine NOT yet spawned
	sparkBlock                  // parked inside a blocking builtin, goroutine live at <-resumeCh
)

// resumeMode is the value carried driver->goroutine on resumeCh.
type resumeMode uint8

const (
	resumeNormal    resumeMode = iota
	resumeCancelled            // reference thread.resume(Cancelled.TRUE) (~538-541)
	resumeAbort                // reap: unwind the goroutine (Close / driver error path)
)

// errStackfulAbort is the sentinel block() panics with on resumeAbort; it
// unwinds the goroutine's guest frames (recovered by the engine's
// callWithStack recover and re-surfaced as a call error) and is finally
// swallowed by the goroutine's own top-level recover. Never escapes to users.
var errStackfulAbort = errors.New("component/instance: stackful task aborted")

// stackfulTask is the reference's Task + implicit Thread for a lift with NO
// callback option: the thread's suspended stack is a REAL parked goroutine
// (guest frames live inside be.coreFn.CallWithStack), and Thread.resume/
// block's lock handoff is a pair of unbuffered channels.
type stackfulTask struct {
	t          *task
	in         *Instance
	be         *boundExport
	ctx        context.Context
	exportName string

	// asyncOpts: false = sync-opts sub-shape (results lifted from flat core
	// results, needs_exclusive; the only shape the 7 suites use), true =
	// async-no-callback sub-shape (results via task.return builtin).
	asyncOpts bool

	// The baton. Created by spawn(); nil until first real run (an
	// entry-parked task has no goroutine yet).
	resumeCh chan resumeMode // driver -> goroutine: run until next suspension
	yieldCh  chan struct{}   // goroutine -> driver: suspended-or-done
	spawned  bool

	park        stackfulParkKind
	parkReady   func() bool // sparkBlock only: the blocking site's predicate
	cancellable bool        // is the current park a cancellable suspension?
	cancelWake  bool        // requestCancellation resumed us with Cancelled.TRUE

	onStart func() ([]abi.Value, error) // lazy caller-arg lift (delegated calls); nil => args
	args    []abi.Value
	done    bool
	err      error
	panicVal any             // non-trap panic on the goroutine, re-panicked on the driver
	finish   func(err error) // notifies whoever started us (host entry or async lower)
}
```

`task` (task.go) gains one field and stays otherwise untouched:

```go
type task struct {
	// ... existing fields ...
	// Exactly one of gt/st is non-nil for a task this package creates: gt
	// for a callback lift (guest_task.go), st for a stackful lift. The
	// oracle's hand-built task keeps gt non-nil, so every existing branch
	// it exercises is unchanged.
	gt *guestTask
	st *stackfulTask
}
```

### 2.1 Start, first run, the goroutine body

```go
// startTask mirrors guestTask.start (guest_task.go ~90): attempt entry; park
// at sparkEntry (no goroutine yet) under backpressure, else spawn + first run.
func (st *stackfulTask) startTask() error {
	if !st.in.tryEnter(st.t) {
		st.park, st.cancellable = sparkEntry, true
		st.in.numWaitingToEnter++
		st.in.sched.park(st)
		return nil
	}
	return st.firstRun()
}

// firstRun spawns the goroutine and performs the first baton handoff. On
// return the task has either finished (st.done) or parked (in sched.parked).
func (st *stackfulTask) firstRun() error {
	st.resumeCh, st.yieldCh = make(chan resumeMode), make(chan struct{})
	st.spawned = true
	go st.main()
	return st.handoff(resumeNormal)
}

// main is the goroutine body — the reference cont_new's thread_base (~279).
func (st *stackfulTask) main() {
	mode := <-st.resumeCh // wait for the first baton
	defer func() {
		if r := recover(); r != nil && r != any(errStackfulAbort) {
			st.panicVal = r // driver re-panics; never swallow a real bug
		}
		st.done = true
		st.yieldCh <- struct{}{} // final handoff; driver observes done
	}()
	if mode == resumeAbort {
		return // reaped before ever running (only via reap of a just-spawned task)
	}
	st.err = st.run()
}
```

`run()` is `canon_lift`'s no-callback body (~2144-2171), the exact
`guestTask.firstRun` prologue (guest_task.go ~107-158: `enterRun`, lazy
`onStart`, `lowerParams`, pooled stacks) followed by the shape split — write
it by copying firstRun's body and changing the tail:

```go
func (st *stackfulTask) run() error {
	in, be, t := st.in, st.be, st.t
	in.enterRun(t)
	// [identical to guestTask.firstRun: onStart/args, lowerParams, core-arg
	//  count check, pooled stack build — same pools, same error wrapping,
	//  each error path: in.leaveRun(); return st.fail(err)]
	t.state = taskStarted
	if err := be.coreFn.CallWithStack(st.ctx, stack); err != nil {
		putUint64Slice(stackPtr)
		in.leaveRun()
		return st.fail(fmt.Errorf("component/instance: export %q: call core func %q: %w", st.exportName, be.funcName, err))
	}
	// NOTE: CallWithStack may have suspended/resumed any number of times in
	// between (block()/handoff below); when it returns, this goroutine holds
	// the baton and enterRun state is re-established (block()'s epilogue).

	if st.asyncOpts {
		// async-no-callback: core returns nothing; exit_implicit_thread's
		// trap_if(state != RESOLVED) (~522), same text as finishExit.
		putUint64Slice(stackPtr)
		in.leaveRun()
		if t.state != taskResolved {
			return st.fail(fmt.Errorf("component/instance: export %q: stackful async export returned before task.return resolved the task", st.exportName))
		}
		st.finishOK()
		return nil
	}

	// sync-opts: lift flat results -> task.return_ -> post-return (~2156-2163).
	rawResults := stack[:be.coreResultCount]
	results, err := in.liftResult(be, rawResults, mem, memAvailable, st.exportName)
	if err != nil { putUint64Slice(stackPtr); in.leaveRun(); return st.fail(err) }
	if err := t.returnValues(results); err != nil { // traps if guest already resolved
		putUint64Slice(stackPtr); in.leaveRun()
		return st.fail(fmt.Errorf("component/instance: export %q: %w", st.exportName, err))
	}
	if be.postReturnFuncName != "" { /* same as invoke()'s post-return, error => st.fail */ }
	putUint64Slice(stackPtr)
	in.leaveRun()
	st.finishOK()
	return nil
}

func (st *stackfulTask) fail(err error) error {
	st.err = err
	if st.finish != nil { st.finish(err) }
	return err
}
func (st *stackfulTask) finishOK() { if st.finish != nil { st.finish(nil) } }
```

(`st.done` is set only in `main`'s defer, after `run` returns — so the driver
observing the final yield always sees a fully-recorded outcome.)

### 2.2 block — the suspension primitive (runs ON the goroutine)

```go
// block suspends the calling stackful goroutine until ready() holds (or a
// cancel wake). It is Thread.suspend/wait_until (~396-409) with the lock
// handoff replaced by the baton channels. MUST be called with the baton held
// (i.e. from a builtin invoked by this task's guest code).
func (st *stackfulTask) block(ready func() bool, cancellable bool) (cancelled bool) {
	if st.t.deliverPendingCancel(cancellable) { // wait_until's prologue (~404)
		return true
	}
	st.park, st.parkReady, st.cancellable = sparkBlock, ready, cancellable
	st.in.suspendRun()      // §5: activeTask=nil, mayEnter=true; exclusive KEPT
	st.in.sched.park(st)    // safe: we hold the baton
	st.yieldCh <- struct{}{} // baton -> driver
	mode := <-st.resumeCh    // baton <- driver (sched.step, canceller, or reaper)
	if mode == resumeAbort {
		panic(errStackfulAbort)
	}
	st.park, st.parkReady, st.cancellable = sparkNone, nil, false
	st.in.enterRun(st.t)    // re-establish running brackets
	return mode == resumeCancelled
}
```

Deterministic-profile note: like the reference's `wait_until` under
`DETERMINISTIC_PROFILE` (~406: the early-return is random-gated off), block
**always parks even if ready() is already true** — the driver's very next
`sched.step` resumes it. This keeps resume ordering identical to the parked
callback tasks' FIFO discipline.

### 2.3 ready / resumeReady — the sched side

```go
// ready mirrors guestTask.ready (guest_task.go ~69) for the two park kinds.
// No !exclusiveHeld conjunct at sparkBlock: a sync-opts stackful task HOLDS
// its instance's exclusive across its own suspension (reference: wait_until
// never releases exclusive_thread; only the callback loop does, ~2177), so
// gating on it would deadlock against ourselves. Cross-task exclusion is
// per-instance and handled by exclusiveOwner (§5).
func (st *stackfulTask) ready() bool {
	switch st.park {
	case sparkEntry:
		return !st.in.hasBackpressure()
	case sparkBlock:
		return st.cancelWake || st.t.cancelDeliverable() || st.parkReady()
	}
	return false
}

// resumeReady is called only with st in sched.parked (goroutine at
// <-resumeCh, or not yet spawned for sparkEntry), by sched.step/pumpSnapshot
// or task.requestCancellation — on whatever goroutine currently holds the
// baton.
func (st *stackfulTask) resumeReady() error {
	switch st.park {
	case sparkEntry:
		st.in.numWaitingToEnter--
		st.in.sched.unpark(st)
		st.park = sparkNone
		if st.cancelWake { // cancelled at INITIAL: same landing as guestTask (~170-180)
			st.cancelWake = false
			if err := st.t.cancelUnentered(); err != nil {
				st.done = true
				return st.fail(err)
			}
			st.done = true
			st.finishOK()
			return nil
		}
		return st.firstRun()

	case sparkBlock:
		st.in.sched.unpark(st)
		mode := resumeNormal
		if st.cancelWake {
			st.cancelWake, mode = false, resumeCancelled
		}
		return st.handoff(mode)
	}
	return fmt.Errorf("component/instance: BUG: resumeReady on a stackfulTask with no park state")
}

// handoff gives the baton to the goroutine and waits for it back.
func (st *stackfulTask) handoff(mode resumeMode) error {
	st.resumeCh <- mode
	<-st.yieldCh
	if st.done {
		if st.panicVal != nil {
			panic(st.panicVal) // a real bug on the goroutine: surface on the driver
		}
		return st.err // nil on success; sched.step propagates non-nil exactly like a failing thunk
	}
	return nil // parked again: block() already re-registered st in sched.parked
}
```

Error propagation is byte-for-byte the `guestTask` convention: a trap during a
scheduler-driven resume returns from `resumeReady` → out of `sched.step` → to
the current driver, AND is reported to the task's starter via `finish(err)` /
`st.err` (async_lift.go's drive loop then prefers `gt.err`-style local state).
Guest traps and builtin-panic traps both arrive as `CallWithStack` errors on
the goroutine (the engine's `recover()` — call_engine.go ~498 — degrades host
-func panics to call errors), so `run()` funnels every trap through
`st.fail`; only non-trap panics in our own lowering code between engine frames
reach `main`'s recover and re-panic on the driver.

## 3. sched integration — `parked` becomes an interface

sched.go's only structural change (sched.go ~34, ~70-111, ~138-165):

```go
// parkedTask is what sched can park/resume: a callback guestTask (its parked
// state is plain Go fields) or a stackfulTask (its parked state is a live
// goroutine blocked on its resume channel).
type parkedTask interface {
	ready() bool
	resumeReady() error
}

type sched struct {
	runq    []schedThunk
	pumping bool
	parked  []parkedTask // was []*guestTask

	// instantiating is true while the root Instantiate is running core-module
	// start functions (§4). One flag on the SHARED sched (one per composition
	// tree) so every sub-Instance's builtins/wrappers see it.
	instantiating bool
}
```

`park`/`unpark`/`isParked` take `parkedTask` (identity comparison unchanged —
both implementations are pointer types). `step`, `drive`, `pumpSnapshot`, and
the deadlock detection (`progressed == false` → `errAsyncDeadlock`) are
**textually unchanged**: a parked stackful task is just another entry whose
`ready()` is consulted and whose `resumeReady()` runs it to its next
suspension. `pumpSnapshot`'s snapshot/re-check logic works as-is (its
`readyNow` slice becomes `[]parkedTask`).

The `sched.pumping` bracket already set around `resumeReady` in `step`/
`pumpSnapshot` now also covers stackful resumes — which is exactly right:
while a stackful goroutine holds the baton it IS "inside the scheduler", so
`AsyncCall.Resolve`/`requireSchedulable`'s existing proof-of-driving-goroutine
checks stay correct with zero changes. (`pumping` is written on the driver
before the channel send and read by builtins on the goroutine after the
receive — ordered by the handoff, race-free.)

## 4. Blocking builtins: the three-way context split + the sync trap

One new helper in async_builtins.go replaces the ad-hoc prologues at every
builtin that can block. "Current task" remains `in.activeTask` (per-instance,
like the reference's `current_task()`), so nesting is automatic: when a
stackful task of D drives a callback task of C, C's builtins see C's task.

```go
// blockingTask classifies the context of a blocking builtin:
//   - st != nil: a stackful task's guest code — suspend by parking the goroutine.
//   - st == nil, t != nil: a callback task's guest/callback code on the driving
//     goroutine — suspend by NESTED sched.drive (Phase 1-3 behavior, kept:
//     wait-during-callback stays green).
//   - t == nil: a synchronous context (a core start function, or a plain sync
//     lift of a sync-TYPED func) — the spec trap.
func blockingTask(in *Instance, builtin string) (t *task, st *stackfulTask) {
	if !in.mayLeave {
		panic(fmt.Errorf("component/instance: %s: instance may not be left right now", builtin))
	}
	t = in.activeTask
	if t == nil {
		panic(fmt.Errorf("component/instance: %s: cannot block a synchronous task before returning", builtin))
	}
	return t, t.st
}
```

### 4.1 waitable-set.wait (async_builtins.go ~118)

```go
fn := api.GoModuleFunc(func(_ context.Context, mod api.Module, stack []uint64) {
	si, ptr := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
	ws := requireWaitableSet(in, "waitable-set.wait", si)
	t, st := blockingTask(in, "waitable-set.wait")

	var ev eventTuple
	if st != nil {
		// Stackful arm: park the goroutine (wait_for_event_and, ~818).
		ws.numWaiting++
		cancelled := st.block(func() bool {
			return ws.hasPendingEvent() || (cancellable && t.cancelDeliverable())
		}, cancellable)
		ws.numWaiting--
		if cancelled || (cancellable && t.deliverPendingCancel(true)) {
			ev = eventTuple{code: eventTaskCancelled}
		} else {
			ev = ws.getPendingEvent()
		}
	} else {
		// Callback-task arm: UNCHANGED nested drive (pred/numWaiting/deadlock
		// wrap exactly as today, lines ~127-144).
	}
	stack[0] = uint64(storeEvent(mod, "waitable-set.wait", ptr, ev))
})
```

Note the stackful arm surfaces **no deadlock error itself**: if nothing can
ever wake it, the task simply stays parked and the deadlock is detected by
whoever is driving (§6's host drive maps `errAsyncDeadlock` to the spec text).

### 4.2 Other blocking sites

Apply the same split wherever the current code nested-drives on behalf of the
*calling guest*:

- **stream/future sync copy waits** (stream_builtins.go ~395, ~482, ~609):
  `t, st := blockingTask(...)`; if `st != nil` replace
  `in.sched.drive(e.hasPendingEvent)` with `st.block(e.hasPendingEvent,
  false)`; else keep the nested drive. (`sync-streams` exercises sync stream
  ops from a stackful task; parking, not nested-driving, is what lets the
  peer's task — resumable only by the outer driver once our frames yield the
  baton — make progress. `waitable.join`'s `syncWaiter` bookkeeping around the
  wait is kept identical in both arms.)
- **synchronous subtask.cancel's wait** (async_builtins.go ~331): same split
  (`st.block(st2.hasPendingEvent, false)` with `syncWaiter` bracketed).
- **waitable-set.poll**, context.get/set, task.return, etc.: NOT blocking —
  untouched (`requireActiveTask` keeps its existing message for them; only
  builtins that can *suspend* adopt `blockingTask`'s trap text).

### 4.3 The sync-lower eager trap (dont-block-start case 2)

A **sync** canon lower whose callee is an async lift (callback or stackful)
is a potential block. Reached from a start function it must trap **before the
callee runs**. Siting: the importer-side sync wrapper — `buildHostWrapper`'s
delegated arm (host_import.go) / `delegatingHostImport`'s consumer — right
before `sub.invoke(...)` is called on a `be.asyncCallback || be.stackful`
target:

```go
if in.sched.instantiating && in.activeTask == nil {
	panic(fmt.Errorf("component/instance: %s: cannot block a synchronous task before returning", exportName))
}
```

Scoped to the instantiation window (`sched.instantiating`, set/cleared by the
root `instantiateGraph` around the core-module instantiation loop — every
core `start` runs inside it) so host-entry sync invokes keep today's
drive-and-deadlock-detect behavior; zero blast radius outside `Instantiate`.
`waitable-set.wait` inside a start function needs no flag — `activeTask ==
nil` already classifies it (§4's trap), during instantiation or not.

### 4.4 Sync lower FROM a stackful task (async-calls-sync's actual bug)

`$SyncMiddle.sync-func` (a sync-opts **stackful** lift — verified: its
`canon lift` has no callback, contradicting the earlier session note) makes a
**sync-lowered** call to the async `blocking-call`. Today that path
(`delegatingHostImport.fn` → `sub.invoke` → `invokeAsyncCallback` →
`sched.drive(gt.done)`) nested-drives until the callee resolves — but
`blocking-call` YIELDs forever until `unblock` runs, and `unblock` can only
run after the outer `run` task continues, which is stuck beneath this very
nested drive: **the confirmed livelock** (step keeps "progressing" through
the YIELD park, the predicate never converges).

Fix: in the sync delegated wrapper, when the caller context is stackful, do
the reference's `thread.wait_until(subtask.resolved)` (~2273-2275) — park
instead of drive:

```go
// importer-side sync wrapper, callee is an async lift, caller is stackful:
t := in.activeTask
if t != nil && t.st != nil && (be.asyncCallback || be.stackful) {
	var resolved bool
	var res []abi.Value; var cancelled bool
	onStart := func(*task) ([]abi.Value, error) { return in2, nil } // args already lifted by this wrapper
	onResolve := func(vals []abi.Value, c bool) error { res, cancelled, resolved = vals, c, true; return nil }
	if _, err := sub.startAsyncExportTask(ctx, be, exportName, onStart, onResolve); err != nil { return nil, err }
	if !resolved {
		t.st.block(func() bool { return resolved }, false) // non-cancellable, per ~2275
	}
	// map results exactly as delegatingHostImport's tail does (providerHandleToRep etc.)
	...
}
// else: existing sub.invoke path (host-entry / callback-caller nested drive), unchanged
```

(`startAsyncExportTask` dispatches on the callee's shape — §7. `cancelled`
can't be true here: nothing requests cancellation of an anonymous sync-lower
subtask.)

## 5. Instance brackets: `exclusiveOwner`, suspendRun

A sync-opts stackful task `needs_exclusive` and — unlike the callback loop,
which releases the exclusive around every WAIT/YIELD (~2177) — **holds it
across its own suspensions** (reference `wait_until` never touches
`exclusive_thread`). While parked, though, its guest code is off the
(semantic) stack: re-entry attempts must PARK on backpressure, not trap on
`mayEnter`. So (task.go):

```go
// Instance gains:
//   exclusiveOwner *task // who holds exclusiveHeld; nil iff !exclusiveHeld

func (in *Instance) enterRun(t *task) {
	in.mayEnter = false
	in.activeTask = t
	in.exclusiveHeld, in.exclusiveOwner = true, t
}
func (in *Instance) leaveRun() {
	in.exclusiveHeld, in.exclusiveOwner = false, nil
	in.activeTask = nil
	in.mayEnter = true
}
// suspendRun is a stackful park: guest frames stay live on the parked
// goroutine, but the task is not RUNNING — entry gating falls to
// exclusiveHeld/backpressure (tryEnter parks newcomers), not to a mayEnter
// trap. exclusiveHeld/Owner are deliberately KEPT.
func (in *Instance) suspendRun() {
	in.activeTask = nil
	in.mayEnter = true
}
```

(`tryEnter` sets `exclusiveOwner = t` alongside `exclusiveHeld = true`.)
For the async-no-callback sub-shape (`needs_exclusive == false`), `enterRun`/
`suspendRun` calls are replaced by thin variants that skip the exclusive
fields; not needed for the 7 suites — gate it on `st.asyncOpts` and keep the
code path present but unexercised.

`hasBackpressure()` is unchanged (`backpressure > 0 || exclusiveHeld`) — a
parked sync-opts stackful task therefore back-pressures new entries into its
instance, which is precisely `async-calls-sync`'s "hitting the synchronous
backpressure case in 2 of the 4 calls".

`guestTask.ready()`'s two `!gt.in.exclusiveHeld` conjuncts (guest_task.go
~77, ~81) become "not held by another task":

```go
held := gt.in.exclusiveHeld && gt.in.exclusiveOwner != gt.t
```

(For pure-callback compositions `exclusiveOwner` is always either nil or the
running task — the conjunct evaluates exactly as before; only a same-instance
mix of stackful + callback tasks can observe the difference.)

## 6. Entry points

### 6.1 Host entry: `invokeStackful` (async_lift.go)

`invoke`'s dispatch (instance.go ~1317) becomes:

```go
if be.asyncCallback { return in.invokeAsyncCallback(ctx, be, exportName, args) }
if be.stackful     { return in.invokeStackful(ctx, be, exportName, args) }
// plain sync path (sync-TYPED funcs) unchanged
```

```go
func (in *Instance) invokeStackful(ctx context.Context, be *boundExport, exportName string, args []abi.Value) ([]abi.Value, error) {
	// [arg-count / coreFn-nil / flattenErr prologue: copy invokeAsyncCallback ~47-62,
	//  minus the callbackFn check; plus mayEnter trap ~70]
	t := &task{inst: in, be: be}
	st := &stackfulTask{t: t, in: in, be: be, ctx: ctx, exportName: exportName, args: args}
	t.st = st
	if err := st.startTask(); err != nil {
		in.reapStackful() // a trap may strand OTHER parked stackful tasks (delegated callees)
		return nil, err
	}
	if err := in.sched.drive(func() bool { return st.done }); err != nil {
		in.reapStackful()
		if errors.Is(err, errAsyncDeadlock) {
			return nil, fmt.Errorf("component/instance: export %q: wasm trap: deadlock detected: event loop cannot make further progress", exportName)
		}
		return nil, err
	}
	if st.err != nil {
		in.reapStackful()
		return nil, st.err
	}
	return t.result, nil
}
```

The `wasm trap: ` prefix is load-bearing: deadlock.json's expected substring
includes it. (`invokeAsyncCallback`'s own deadlock text stays as-is — no suite
requires changing it, and tests assert it.)

### 6.2 Guest↔guest: `startAsyncExportTask` dispatches on shape

`startAsyncExportTask` (async_lift.go ~113) is called unconditionally by the
delegated async lower (async_host_import.go ~354) — today it mis-drives a
stackful callee through callback machinery (cancel-subtask's `g`). Add the
branch at its top:

```go
if be.stackful {
	return in.startStackfulExportTask(ctx, be, exportName, onStart, onResolve)
}
```

`startStackfulExportTask` is `startAsyncExportTask` with `guestTask` swapped
for `stackfulTask`: same `mayEnter` trap, same `t.onResolve` panic-bridge,
`st.onStart` adapting `onStart(t)`, then `st.startTask()` (may run to
completion inline on the caller's goroutine — the immediate path async_host_
import.go ~360 already handles via `st.resolved()` — or park at
sparkEntry/sparkBlock), returning `t.requestCancellation` as `onCancel`.
Result values flow through `task.returnValues` → `t.onResolve` → the
wrapper's existing `onResolve` closure — for a sync-opts callee it's `run()`
itself that calls `returnValues` after lifting flat results, so the wrapper
needs zero changes.

## 7. Cancellation

`task.requestCancellation` (task.go ~159) generalizes its two arms; the gt
paths are untouched (oracle-compatible):

```go
case taskInitial:
	t.state = taskCancelDelivered
	if t.st != nil { t.st.cancelWake = true; return t.st.resumeReady() } // sparkEntry landing
	t.gt.cancelWake = true
	return t.gt.resumeReady()
case taskStarted:
	if t.st != nil {
		heldByOther := t.inst.exclusiveHeld && t.inst.exclusiveOwner != t
		if t.st.park == sparkBlock && t.st.cancellable && !heldByOther && t.inst.mayEnter {
			t.state = taskCancelDelivered
			t.st.cancelWake = true
			return t.st.resumeReady() // baton -> goroutine with resumeCancelled
		}
		t.state = taskPendingCancel
		return nil
	}
	// existing gt arm unchanged
```

The `heldByOther` relaxation (vs. today's `!exclusiveHeld`) is the reference's
`exclusive_thread not in {None, implicit_thread}` (~535): a stackful task
parked while holding **its own** exclusive is still cancellable in place. The
synchronous resume hands the baton to the parked goroutine, which returns
`Cancelled.TRUE` from `block()`; the blocking builtin (e.g. cancellable
`waitable-set.wait`) then delivers `TASK_CANCELLED`, and the guest eventually
acks via `task.cancel` (whose `!t.be.asyncCallback` parity-trap must widen to
`!t.be.asyncCallback && !t.be.stackfulAsync…` — precisely: trap only when the
lift is sync-OPTS, matching the reference's `trap_if(not task.opts.async_)`;
note a sync-opts stackful task therefore CANNOT `task.cancel`, it resolves by
returning). Non-cancellable parks pick the cancellation up via
`deliverPendingCancel` in `block()`'s prologue at the next cancellable
suspension — same as today's PENDING_CANCEL story.

## 8. Goroutine lifecycle — no leaks, ever

A stackful goroutine exists between `firstRun` and `main`'s final yield. It
is blocked on `<-st.resumeCh` iff `st` is in `sched.parked` with
`sparkBlock`. Leak sources and their reaps:

1. **Driver error path** (deadlock, another task's trap): `invokeStackful`
   calls `in.reapStackful()` on every error return (§6.1).
2. **`Instance.Close`** (instance.go ~1615): first line becomes
   `if in.sched != nil { in.reapStackful() }` — BEFORE closing core modules,
   since aborting unwinds guest frames still inside the engine. The sched is
   shared per composition tree, so whichever Close runs first reaps all;
   subsequent calls find nothing.

```go
// reapStackful aborts every parked stackful task in the shared scheduler.
// Runs on the driver (baton necessarily free: nothing is running). Abort
// makes block() panic errStackfulAbort on the goroutine; the engine's
// recover degrades it to a call error through the guest frames; main()'s
// recover swallows the sentinel; the final yield completes the handoff.
func (in *Instance) reapStackful() {
	for {
		var victim *stackfulTask
		for _, p := range in.sched.parked {
			if st, ok := p.(*stackfulTask); ok { victim = st; break }
		}
		if victim == nil { return }
		in.sched.unpark(victim)
		if victim.park == sparkEntry { // no goroutine yet
			victim.done = true
			victim.park = sparkNone
			continue
		}
		victim.resumeCh <- resumeAbort
		<-victim.yieldCh // goroutine has fully unwound; done == true
	}
}
```

The abort unwind may execute deferred cleanups in `run()` (pool puts,
`leaveRun`) — `block()`'s panic happens after `suspendRun`, and `run()`'s
error path via the CallWithStack error return handles the rest; `st.fail`
fires `finish(err)` with the abort-wrapped error, which is fine (the starter
already gave up). Reap is not called concurrently with a live driver by
construction (Close and error paths only).

Also note: a parked goroutine ignores `ctx` cancellation — context-based
interruption lands at the next `CallWithStack` boundary exactly as it does
for the sync path today; `Close` is the hard stop.

## 9. graph.go binding

Replace the rejection (graph.go ~1547-1553) with:

```go
if isAsyncLift && callbackName == "" {
	be.stackful, be.stackfulAsyncOpts = true, true // async-no-callback sub-shape
}
if !isAsyncLift && fdIsAsync { // sync-opts lift of an async-TYPED func: the 7 suites' shape
	be.stackful = true
}
if isAsyncLift && callbackName != "" { be.asyncCallback = true } // unchanged
```

(`fdIsAsync` = the component func type's Async bit, `binary.FuncDesc.Async` —
already decoded; this is the check the empty-wait skip note identified as
missing.) `boundExport` gains `stackful, stackfulAsyncOpts bool`; the
callback-option validation (~1612+) keeps applying only to `asyncCallback`.
`instantiateGraph` brackets its core-module instantiation loop with
`sched.instantiating = true/false` (§4.3).

## 10. What does NOT change (compat inventory)

- **Callback lift**: `guestTask`, `invokeAsyncCallback`,
  `startAsyncExportTask`'s callback path — untouched except
  `ready()`'s `exclusiveOwner` refinement (§5, identity-preserving for
  pure-callback trees).
- **Oracle** (`async_oracle_test.go`): builds `task{gt: &guestTask{}}` with
  `in.activeTask` set — `blockingTask` returns `(t, nil)` → every builtin
  takes the callback/nested-drive arm it takes today. `task.st` stays nil;
  `sched.parked`'s interface change is invisible (it stores the same
  pointers). **No adaptation needed.** Only if an oracle scenario asserts the
  old `waitable-set.wait: called outside an active async task` text (it
  doesn't — scenarios always set activeTask) would text drift matter; grep
  `called outside an active async task` in tests and update any hit that
  targets a BLOCKING builtin to the new spec text.
- **`-race`**: all cross-goroutine interaction is the two channels; every
  shared-state touch is baton-guarded (§1). Run the full package under
  `-race` as the acceptance gate; any report is a design violation (someone
  touched sched/Instance state without holding the baton), not a "flaky".

## 11. Expected suite outcomes (un-skip list)

| suite | expected result | trap text (substring-matched) |
|---|---|---|
| empty-wait | assert_return 44 | — |
| deadlock | trap | `wasm trap: deadlock detected: event loop cannot make further progress` |
| dont-block-start | 2× assert_uninstantiable | `cannot block a synchronous task before returning` |
| partial-stream-copies | assert_return | — |
| sync-streams | assert_return | — |
| zero-length | assert_return | — |
| cancel-subtask | assert_return | — |
| async-calls-sync | **attempt un-skip** after the rest are green (§4.4); keep the timeout guard while verifying | — |
| trap-if-block-and-sync | likely also unblocked by §4/§4.3 (same guards); verify opportunistically | `cannot block a synchronous task` |

Un-skip by deleting each suite's `skipReason` (wast_async_conformance_test.go
~308) so regressions red the build.

## 12. Honest flags — where the reference model resists Go

1. **Nested drives are still frame-held.** A CALLBACK task's
   `waitable-set.wait` and a host-entry sync lower keep Phase 1-3's nested
   `sched.drive` — those "threads" still cannot yield the baton upward. Any
   convergence that requires *their* caller's continuation to proceed still
   deadlocks (detected, not hung). The reference has no such asymmetry (every
   thread is parkable). Acceptable: every shape in the 8 suites above routes
   the blocking side through a stackful task; if a future suite blocks a
   callback task on its own caller, the fix is to run callback tasks' WAITs
   through `st.block`-style parking too — the machinery generalizes, it just
   isn't wired for that here.
2. **Goroutine cost per parked task**: a parked stackful task pins a
   goroutine stack + the engine call frames of the blocked `CallWithStack`
   (~10KB callEngine + guest frames). The reference pins a whole OS thread —
   we're strictly cheaper — but `Instance.Close` MUST reap (§8) or tests leak
   goroutines; add a `runtime.NumGoroutine()`-delta assertion to the new
   lifecycle test.
3. **No preemption**: `resumeAbort` only lands at a suspension point. A
   stackful task spinning in pure wasm (no builtin calls) is not reapable —
   same property as wazy's existing sync calls (Close doesn't interrupt a
   running `CallWithStack` either); not a regression, just stated.
4. **`async-calls-sync` is a read, not a promise** (§4.4): the livelock's
   root is confirmed to run through the stackful `$SyncMiddle.sync-func` and
   the nested-drive sync lower, both replaced here, and the backpressure
   handshake maps onto `sparkEntry`. High confidence, but it exercises 4
   concurrent tasks across 3 instances — verify under the timeout guard
   before deleting its skip; if it still livelocks, the residue is in
   YIELD-park fairness (`pumpSnapshot` rounds), a bounded follow-up, and the
   suite stays skipped without blocking this phase's other six.

## 13. Implementation order (each step compiles + full tests green)

1. sched `parkedTask` interface + `exclusiveOwner`/`suspendRun` (no behavior
   change; run full tests + `-race`).
2. `stackfulTask` + `invokeStackful` + graph.go binding; un-skip
   `empty-wait` (single-suite acceptance: exercises park/resume/subtask
   event/result lift end to end).
3. `blockingTask` trap + `sched.instantiating` + sync-lower eager trap;
   un-skip `dont-block-start`, `deadlock` (trap texts).
4. `startStackfulExportTask`; un-skip `cancel-subtask`,
   `partial-stream-copies`.
5. Stream sync-op `st.block` arms; un-skip `sync-streams`, `zero-length`.
6. §4.4 sync-lower-from-stackful parking; attempt `async-calls-sync` and
   `trap-if-block-and-sync` under timeout.
7. Reap tests: goroutine-count assertions around Close on a
   deliberately-deadlocked stackful call; cancellation of a goroutine-parked
   task (`sparkBlock` cancellable + non-cancellable + `sparkEntry`); `-race`
   over the whole package.

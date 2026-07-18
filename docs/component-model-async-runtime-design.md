# wazy async MVP — scheduler/wait design (callback lift, single top-level task)

Scope: Phase 1 of `docs/component-model-async-plan.md`. One top-level async task at a
time; `Call` drives it to completion; genuine concurrency (`CallAsync`) deferred.
Behavior source: `internal/component/abi/testdata/definitions.py` (canon_lift ~2144,
canon_lower ~2231, Task ~450, Waitable/WaitableSet ~756/799, Subtask ~847).

Design rule applied throughout: **transliterate every `trap_if`; collapse every
green-thread mechanism whose only job is inter-thread scheduling.** With exactly one
guest "thread" (the callback task's implicit thread, which is never a live suspended
frame — every suspension is a normal core return) and zero host threads, the entire
`Thread`/`Store.waiting`/`tick()` apparatus reduces to: *a FIFO queue of host
completion thunks, pumped between callback invocations, with a trap when the queue
runs dry while the guest is waiting.*

File layout (new): `instance/sched.go`, `instance/task.go`, `instance/waitable.go`
(waitable, waitableSet, subtask, eventTuple), `instance/async_lift.go`
(`invokeAsyncCallback`), `instance/async_builtins.go` (canonToDef cases). Modified:
`instance.go` (Instance fields, bind-time routing), `host_import.go`
(`WithAsyncImport` + async wrapper arm), `resource.go` (two new table kinds).

---

## 0. The core translation, stated once

The reference has three cooperating mechanisms:

1. **Blocking waits** — `thread.wait_until(pred)` / `wset.wait_for_event_and(pred)`
   park a green thread until *some other thread* makes `pred` true.
2. **Event sources** — other threads (host FuncInsts, other tasks, stream copies)
   call `Waitable.set_pending_event(closure)`.
3. **A driver** — either canon_lift's sync-caller loop
   (`while task.state != RESOLVED: candidates = {t ready}; trap_if(not candidates);
   resume(choice)`) or the embedder's `Store.tick()`.

In wazy-MVP all three collapse into one place because there is exactly one waiter
(the callback loop in `invokeAsyncCallback`) and one class of event source (host
import completions, which are plain Go closures):

- "Park until pred" ⇒ `sched.drive(pred)`: run queued thunks FIFO until pred is true.
- "Another thread makes pred true" ⇒ a thunk calls `Resolve`, which sets a
  `pendingEvent` closure on a subtask.
- `trap_if(not candidates)` ⇒ `drive` returns `errAsyncDeadlock` when the queue is
  empty and pred is still false. Nothing else in the process can ever make progress —
  single-threaded, no external completions until CallAsync — so trapping is exact,
  not conservative.

FIFO order is the determinism contract: the conformance oracle (§6 of the plan)
monkeypatches `random.choice→first` / `shuffle→no-op`, so the reference under the
deterministic profile and wazy's FIFO agree event-for-event.

---

## 1. Q1 — the callback-lift driver

### 1.1 Scheduler

```go
// sched.go

// errAsyncDeadlock is the single-threaded translation of canon_lift's
// trap_if(not candidates): the guest is suspended on a condition no queued
// work can ever satisfy.
var errAsyncDeadlock = errors.New("async deadlock: no runnable work")

// sched replaces the reference's Store.waiting + tick() and canon_lift's
// sync-caller resume loop. It is NOT a thread scheduler: entries are host-side
// completion thunks (deferred import resolutions; later: stream copy steps).
// One per Instance; used only while an async task is active.
type sched struct {
	runq    []func() error
	pumping bool // a thunk is executing => Resolver calls are legal (see §2)
}

func (s *sched) enqueue(f func() error) { s.runq = append(s.runq, f) }

// step pops and runs one thunk. progressed=false means the queue is empty.
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

// drive pumps FIFO until pred() holds. Queue empty with pred false is a
// guaranteed-permanent deadlock => errAsyncDeadlock (callers wrap it with the
// export/waitable-set context).
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

// pumpSnapshot runs exactly the thunks queued at entry (not ones they enqueue):
// one deterministic scheduler round, used by CallbackCode.YIELD and the yield
// builtin. Mirrors the reference where a yielded thread re-enters the waiting
// list BEHIND already-ready threads (deterministic profile => they all run
// once before the yielder resumes).
func (s *sched) pumpSnapshot() error {
	for n := len(s.runq); n > 0; n-- {
		if _, err := s.step(); err != nil {
			return err
		}
	}
	return nil
}
```

### 1.2 Task + Instance state

```go
// task.go

type taskState uint8

const (
	taskInitial taskState = iota
	taskStarted
	taskPendingCancel   // Phase 3; unreachable in MVP (values reserved so the
	taskCancelDelivered // state numbering matches the reference)
	taskResolved
)

// task mirrors reference Task for a callback lift. Because a callback task's
// implicit thread never has a live suspended frame, "implicit thread ≡ task"
// (the plan's Phase-4 note): the ONE load-bearing Thread field, storage[2]
// (context.get/set — wit-bindgen stashes its executor pointer there), lives
// here as ctxStorage.
type task struct {
	inst       *Instance
	be         *boundExport // ft + lift opts (task.return validates against these)
	state      taskState
	numBorrows int          // Phase 3 borrow scopes; MVP: always 0, trap sites kept
	ctxStorage [2]uint64    // reference Thread.storage; context.{get,set}

	// onResolve receives the lifted result from canon task.return. Kept as a
	// closure (not an inlined field write) so guest→guest lower and CallAsync
	// plug in without reshaping task.return. MVP: Call sets it to capture
	// result into the task.
	onResolve func(vals []abi.Value)
	result    []abi.Value
}

// returnValues is reference Task.return_ — called by the task.return builtin.
func (t *task) returnValues(vals []abi.Value) error {
	if t.state == taskResolved {
		return errors.New("task.return: task already resolved") // trap_if(state == RESOLVED)
	}
	if t.numBorrows > 0 { // trap_if(num_borrows > 0)
		return fmt.Errorf("task.return: %d borrowed handle(s) still lent to subtasks", t.numBorrows)
	}
	t.onResolve(vals)
	t.state = taskResolved
	return nil
}
```

```go
// Instance gains (async-free components never touch these — two flag inits):
type Instance struct {
	// ...
	sched         sched
	activeTask    *task // non-nil while invokeAsyncCallback is on the stack
	exclusiveHeld bool  // reference exclusive_thread, collapsed (see §3)
	backpressure  int   // canon backpressure.set/inc/dec state
	mayEnter      bool  // reference may_enter  (init true)
	mayLeave      bool  // reference may_leave  (init true)
}
```

Entry/exit — `enter_implicit_thread` / `exit_implicit_thread` with every
would-block turned into a trap (§3 has the field-by-field justification):

```go
// enterTask: reference Task.enter_implicit_thread minus the green thread.
// Any condition that would BLOCK entry in the reference (backpressure held,
// exclusive held, waiters queued) is a permanent deadlock with one task —
// nothing concurrent exists to clear it — so it traps instead of parking.
func (in *Instance) enterTask(t *task) error {
	if in.activeTask != nil || in.exclusiveHeld {
		return errors.New("reentrant async call while a task is active (needs CallAsync, not yet implemented)")
	}
	if in.backpressure > 0 {
		return fmt.Errorf("async export entry blocked by backpressure (%d) with no concurrent task to clear it", in.backpressure)
	}
	in.activeTask = t
	in.exclusiveHeld = true // needs_exclusive() is always true for callback lifts
	return nil
}

func (in *Instance) exitTask() { in.exclusiveHeld = false; in.activeTask = nil }
```

### 1.3 invokeAsyncCallback — the driver

Bind time (`finalizeBoundExport`): async+callback lift stores `be.asyncCallback =
true`, resolves `be.callbackFn` (the callback core export named by CanonOpt 0x07) the
same way `be.coreFn` is resolved, and precomputes the async flattening (Phase 0:
callee returns exactly one i32). `invoke` routes: `if be.asyncCallback { return
in.invokeAsyncCallback(...) }`. Sync exports pay one predicted-false branch.

```go
// async_lift.go

type callbackCode uint32

const (
	callbackExit  callbackCode = 0
	callbackYield callbackCode = 1
	callbackWait  callbackCode = 2
	callbackMax   callbackCode = 2
)

// unpackCallbackResult is the reference's unpack_callback_result:
// code = packed & 0xf (trap_if > MAX), waitable-set index = packed >> 4.
// (Table max length must stay < 2^28 so si<<4 can't be ambiguous — enforce in
// handleTable.add.)
func unpackCallbackResult(packed uint32) (callbackCode, uint32, error) {
	code := packed & 0xf
	if code > uint32(callbackMax) {
		return 0, 0, fmt.Errorf("invalid callback code %d (packed %#x)", code, packed)
	}
	return callbackCode(code), packed >> 4, nil
}

func (in *Instance) invokeAsyncCallback(ctx context.Context, be *boundExport, exportName string, args []abi.Value) ([]abi.Value, error) {
	// --- arity/bind checks: identical to invoke() ---
	if len(args) != len(be.fd.Params) { /* same error text as invoke */ }
	if be.coreFn == nil || be.callbackFn == nil { /* fail loud */ }

	// --- Store.lift prologue: trap_if(!may_enter_from) + enter_from ---
	if !in.mayEnter {
		return nil, fmt.Errorf("component/instance: export %q: instance is not reenterable", exportName)
	}
	in.mayEnter = false
	defer func() { in.mayEnter = true }() // leave_to

	// --- Task + enter_implicit_thread (backpressure/exclusive gate) ---
	t := &task{inst: in, be: be}
	t.onResolve = func(vals []abi.Value) { t.result = vals }
	if err := in.enterTask(t); err != nil {
		return nil, fmt.Errorf("component/instance: export %q: %w", exportName, err)
	}
	defer in.exitTask()

	// --- on_start: lower params exactly as the sync path ---
	mem, memAvailable := memoryBytesOf(be.mod)
	realloc := cachedReallocOf(ctx, be)
	t.state = taskStarted
	coreArgs, err := in.lowerParams(be, args, mem, memAvailable, realloc, exportName, scratch)
	// ... same pooling/arity discipline as invoke() ...

	// --- first core call: [packed] = callee(flat_args) ---
	// (async-lift-with-callback flattens results to one i32 — Phase 0)
	stack := /* pooled, len = max(len(coreArgs), 1) */
	if err := be.coreFn.CallWithStack(ctx, stack); err != nil {
		return nil, fmt.Errorf("component/instance: export %q: call core func %q: %w", exportName, be.funcName, err)
	}
	packed := uint32(stack[0])

	// --- the callback loop (canon_lift lines ~2172–2197, transliterated) ---
	var cbStack [3]uint64
	for {
		code, si, err := unpackCallbackResult(packed)
		if err != nil {
			return nil, fmt.Errorf("component/instance: export %q: %w", exportName, err)
		}
		if code == callbackExit {
			break
		}

		// Reference: assert(exclusive is us); release exclusive for the
		// duration of the suspension; re-acquire before invoking the callback.
		// With one task this is pure bookkeeping, but the set/clear SITES are
		// kept verbatim so CallAsync drops in without reshaping the loop.
		in.exclusiveHeld = false
		var ev eventTuple
		switch code {
		case callbackYield:
			// wait_until(λ: !exclusive) under the deterministic profile =
			// exactly one scheduler round: everything already queued runs,
			// then the yielder resumes with NONE.
			if err := in.sched.pumpSnapshot(); err != nil {
				return nil, err
			}
			ev = eventTuple{code: eventNone}
		case callbackWait:
			ws, err := in.handles.getWaitableSet(si) // trap_if(!isinstance(WaitableSet))
			if err != nil {
				return nil, fmt.Errorf("component/instance: export %q: callback WAIT: %w", exportName, err)
			}
			// wait_for_event_and(λ: !exclusive, ...): the !exclusive conjunct
			// is vacuously true (we just cleared it); pred = has_pending_event.
			ws.numWaiting++
			err = in.sched.drive(ws.hasPendingEvent)
			ws.numWaiting--
			if err != nil {
				if errors.Is(err, errAsyncDeadlock) {
					return nil, fmt.Errorf("component/instance: export %q: deadlock: guest is WAITing on waitable-set %d but it has no pending event and the run queue is empty; an import resolved externally requires CallAsync (not yet implemented)", exportName, si)
				}
				return nil, err
			}
			ev = ws.getPendingEvent() // FIFO first-with-event (see §1.4)
		}
		in.exclusiveHeld = true

		// [packed] = callback(event_code, p1, p2)
		cbStack[0], cbStack[1], cbStack[2] = uint64(ev.code), uint64(ev.p1), uint64(ev.p2)
		if err := be.callbackFn.CallWithStack(ctx, cbStack[:]); err != nil {
			return nil, fmt.Errorf("component/instance: export %q: callback %q: %w", exportName, be.callbackFuncName, err)
		}
		packed = uint32(cbStack[0])
	}

	// exit_implicit_thread → unregister_thread: trap_if(state != RESOLVED).
	// This is THE "async export forgot task.return" trap.
	if t.state != taskResolved {
		return nil, fmt.Errorf("component/instance: export %q: callback returned EXIT before task.return resolved the task", exportName)
	}
	// (assert: t.numBorrows == 0 — reference unregister_thread assert)
	return t.result, nil
}
```

Notes:
- **The result** comes from the `task.return` builtin (a canonToDef case), which
  lifts its flat args against the task's ft result type and calls `t.returnValues`
  — see §1.5. Any memory the lift reads is read inside that hostcall, so the usual
  "post-return frees it" hazard doesn't arise; async lifts have no post-return.
- **Memory growth**: nothing in the loop caches `mem` across core calls; builtins
  and resolve-time lowering fetch memory bytes fresh (see §2.3).
- **"How does WAIT drive pending events to readiness?"** — it doesn't poll guest
  state; readiness in the MVP has exactly one source: runq thunks calling
  `Resolve`, which install `pendingEvent` closures on subtasks that are joined to
  waitable sets. `drive` interleaves "run one thunk" with "re-check
  `ws.hasPendingEvent`". A thunk that resolves a subtask joined to a *different*
  set doesn't satisfy the pred; the loop keeps pumping; if the queue drains the
  guest is provably stuck ⇒ trap.

### 1.4 Waitable / WaitableSet

```go
// waitable.go

type eventCode uint32

const (
	eventNone          eventCode = 0
	eventSubtask       eventCode = 1
	eventStreamRead    eventCode = 2 // Phase 2
	eventStreamWrite   eventCode = 3 // Phase 2
	eventFutureRead    eventCode = 4 // Phase 2
	eventFutureWrite   eventCode = 5 // Phase 2
	eventTaskCancelled eventCode = 6 // Phase 3
)

type eventTuple struct {
	code   eventCode
	p1, p2 uint32
}

// waitable: reference Waitable, embedded by subtask (Phase 2: stream/future
// ends). pendingEvent is a CLOSURE evaluated at delivery, exactly as in the
// reference — the subtask's state is read (and deliver_resolve run) when the
// event is HANDED to the guest, not when it was set. has_sync_waiter is
// omitted (only reachable via stackful wait / sync subtask.cancel; §3).
type waitable struct {
	pendingEvent func() eventTuple
	wset         *waitableSet
}

func (w *waitable) setPendingEvent(f func() eventTuple) { w.pendingEvent = f }
func (w *waitable) hasPendingEvent() bool               { return w.pendingEvent != nil }

func (w *waitable) getPendingEvent() eventTuple { // clears BEFORE evaluating (reference order)
	f := w.pendingEvent
	w.pendingEvent = nil
	return f()
}

// join moves w between sets (nil = remove). Identity removal on *waitable.
func (w *waitable) join(ws *waitableSet) {
	if w.wset != nil {
		w.wset.remove(w)
	}
	w.wset = ws
	if ws != nil {
		ws.elems = append(ws.elems, w)
	}
}

type waitableSet struct {
	elems      []*waitable
	numWaiting int
}

func (ws *waitableSet) hasPendingEvent() bool {
	for _, w := range ws.elems {
		if w.hasPendingEvent() {
			return true
		}
	}
	return false
}

// getPendingEvent: first element with an event, in join order. The reference
// random.shuffle()s first; the oracle pins shuffle→no-op, so first-with-event
// is the conformance order.
func (ws *waitableSet) getPendingEvent() eventTuple {
	for _, w := range ws.elems {
		if w.hasPendingEvent() {
			return w.getPendingEvent()
		}
	}
	panic("getPendingEvent on set with no pending event") // caller checked
}

func (ws *waitableSet) drop() error {
	if len(ws.elems) > 0 { // trap_if
		return fmt.Errorf("waitable-set.drop: set still has %d member(s)", len(ws.elems))
	}
	if ws.numWaiting > 0 { // trap_if — unreachable in MVP, kept for oracle parity
		return errors.New("waitable-set.drop: set is being waited on")
	}
	return nil
}
```

Table integration (`resource.go`): two new kinds, same unified index space and
free list (guest-observable reuse):

```go
const (
	entryResource entryKind = iota
	entryWaitableSet
	entrySubtask
)

func (*waitableSet) entryKind() entryKind { return entryWaitableSet }
func (*subtask) entryKind() entryKind     { return entrySubtask }

// getWaitableSet / getSubtask / getWaitable: lookup + kind assert, the spec's
// trap_if(not isinstance(...)) with the existing fail-loud error style.
// getWaitable accepts any entry embedding waitable (MVP: subtask only).
```

### 1.5 MVP builtins (canonToDef cases, `async_builtins.go`)

All prefixed with `trap_if(!in.mayLeave)`. `current_task()` = `in.activeTask`,
trap if nil (builtin called outside an async export).

- **task.return**: trap opts/result-type mismatch vs `t.be` (the reference's
  `result_type != task.ft.result` + `LiftOptions.equal` — compare the canon's
  memory/realloc/encoding against the lift's at bind time where possible, at
  call time otherwise); lift flat args (`MAX_FLAT_PARAMS`) against the ft result
  type; `t.returnValues(result)`.
- **context.get/set** `(t, i)`: `t.ctxStorage[i]` (i < 2 validated at bind).
- **backpressure.set**: `in.backpressure = 0|1`; **inc/dec** (if the pinned
  wasm-tools vintage emits them): `++` with `trap_if == 2^16`, `--` with
  `trap_if < 0`.
- **waitable-set.new**: `in.handles.add(&waitableSet{})` → i32.
- **waitable-set.poll** `(si, ptr)`: kind-checked lookup; no event → `NONE`;
  else `getPendingEvent`; store p1/p2 at ptr/ptr+4 (unpack_event), return code.
- **waitable-set.wait** `(si, ptr)`: same, but no event ⇒ nested
  `in.sched.drive(ws.hasPendingEvent)` (numWaiting bracketed) with the same
  deadlock trap. Legal from callback code; single-threaded nested pumping is
  just re-entering the queue loop.
- **waitable.join** `(wi, si)`: `si==0` ⇒ `join(nil)`; else kind-checked set
  lookup, `join(ws)`.
- **waitable-set.drop** `(i)`: table remove + kind trap + `ws.drop()`.
- **subtask.drop** `(i)`: table remove + kind trap + `trap_if(!resolveDelivered)`
  + assert no pending event + `join(nil)`. Index returns to the free list
  (observable reuse — oracle asserts indices).
- **thread.yield** (`yield` in the plan; this vintage models it as a thread
  builtin returning `[cancelled]`): `pumpSnapshot()`, return `[0]` (cancellation
  can't be pending in MVP).

---

## 2. Q2 — async host-import completion

### 2.1 Host-facing API

```go
// host_import.go

// AsyncCall is the per-invocation handle an async host import receives.
// Single-threaded contract: Resolve/Defer may be called (a) synchronously
// inside the AsyncHostFunc, or (b) from a thunk running on the instance
// scheduler (i.e. from a Defer'd func, transitively). Anything else — another
// goroutine, after Call returned — panics fail-loud: external completion is
// CallAsync's job (deferred).
type AsyncCall struct {
	in       *Instance
	st       *subtask
	subtaski uint32 // 0 until parked in the table
	inCall   bool   // true while the AsyncHostFunc invocation is on the stack
	resolved bool
}

// Resolve delivers the import's results. One-shot: a second call panics.
//   - Called inside the AsyncHostFunc (inCall): the wrapper observes the
//     resolved subtask when the func returns and takes the reference's
//     immediate fast path — packed [RETURNED], subtask never enters the table,
//     no event, no WAIT needed.
//   - Called later from a scheduler thunk (parked): performs the reference's
//     on_resolve + on_progress — lowers results into guest memory NOW (via the
//     retptr captured at call time) and installs the SUBTASK pending-event
//     closure; the driver's next pred check sees it.
func (ac *AsyncCall) Resolve(results []abi.Value)

// Defer enqueues fn on the instance's deterministic FIFO run queue; it runs
// while the guest is suspended (WAIT/YIELD/wait builtin). This is how an MVP
// import defers completion without OS concurrency — including multi-hop
// chains (Defer inside Defer) to exercise multiple pump rounds.
func (ac *AsyncCall) Defer(fn func())

// AsyncHostFunc starts one async import call. Returning a non-nil error traps.
type AsyncHostFunc func(ctx context.Context, args []abi.Value, call *AsyncCall) error

// WithAsyncImport registers fn for an import that components lower with the
// async option. Binding an async-lowered import to a plain WithImport (or
// vice versa) fails loud at bind time.
func WithAsyncImport(iface, name string, fn AsyncHostFunc, params, results []binary.TypeDesc) Option
```

### 2.2 The subtask

```go
type subtaskState uint32 // wire values — packed into i32s the guest reads

const (
	subtaskStarting               subtaskState = 0
	subtaskStarted                subtaskState = 1
	subtaskReturned               subtaskState = 2
	subtaskCancelledBeforeStarted subtaskState = 3 // Phase 3
	subtaskCancelledBeforeReturn  subtaskState = 4 // Phase 3
)

type subtask struct {
	waitable
	state            subtaskState
	lenders          []lentHandle // borrow args lent for the call's duration
	resolveDelivered bool         // reference: lenders = None sentinel
	// applyResolve lowers results into guest memory (captured retptr/plans/
	// memMod — bytes fetched FRESH at resolve time) and flips state to
	// RETURNED. Set by the wrapper at call time; nil for non-host subtasks.
	applyResolve func(results []abi.Value) error
}

func (st *subtask) resolved() bool { return st.state >= subtaskReturned }

func (st *subtask) deliverResolve() { // reference deliver_resolve
	for _, l := range st.lenders {
		_ = st.unlend(l)
	}
	st.lenders = nil
	st.resolveDelivered = true
}
```

### 2.3 The async wrapper arm (buildHostWrapper)

When the consuming canon lower carries the async opt (0x06), the wrapper is
built against the async flattening (params: `MAX_FLAT_ASYNC_PARAMS = 4`, spill
→ args-pointer; results: `max_flat_results = 0`, any non-empty result goes
through a trailing retptr param) and the core signature is `(...) -> i32`:

```go
fn := api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
	memMod := mod
	if memOverride != nil { memMod = memOverride }

	// on_start: lift args (async plans), record borrow lends on the subtask
	// (NOT released at wrapper return — held until deliver_resolve, unlike the
	// sync wrapper's defer-Unlend).
	args, lent, err := liftHostArgsPlanned(asyncParamPlans, resolve, stack, memMod, resources)
	if err != nil { panic(...) }

	st := &subtask{state: subtaskStarting, lenders: lent}
	retPtr := /* trailing flat arg, if the result type is non-empty */
	st.applyResolve = func(vals []abi.Value) error {
		// Reference on_resolve: lower_flat_values(max_flat_results=0) with the
		// retptr from flat_args ⇒ results are written to guest memory AT
		// RESOLVE TIME through the pointer captured at call time. Memory may
		// have grown since: fetch bytes from memMod NOW, never capture []byte.
		if err := lowerHostResultsToMemory(ctx, resultPlan, vals, memMod, resources, retPtr, realloc); err != nil {
			return err
		}
		st.state = subtaskReturned // flat_results stays empty (spec asserts [])
		return nil
	}

	ac := &AsyncCall{in: in, st: st, inCall: true}
	st.state = subtaskStarted // on_start complete (args lifted)
	if err := hi.asyncFn(ctx, args, ac); err != nil { panic(...) }
	ac.inCall = false

	if st.resolved() {
		// Immediate fast path (reference: `if subtask.resolved(): ...
		// return [Subtask.State.RETURNED]`): no table entry, subtaski = 0.
		st.deliverResolve()
		stack[0] = uint64(subtaskReturned) // packed = 2
		return
	}
	// Park: unified table entry; index becomes the guest-visible subtask handle.
	ac.subtaski = resources.addSubtask(st)
	stack[0] = uint64(uint32(st.state) | ac.subtaski<<4) // [STARTED | i<<4]
})
```

### 2.4 Resolve — both cases

```go
func (ac *AsyncCall) Resolve(results []abi.Value) {
	if ac.resolved {
		panic("component/instance: async import: Resolve called twice")
	}
	if !ac.inCall && !ac.in.sched.pumping {
		// Structural single-threadedness guard (no goroutine-ID hacks): the
		// only legal Resolve sites are inside the import call or inside a
		// scheduler thunk — both provably on the driving goroutine.
		panic("component/instance: async import: Resolve called outside the instance scheduler; external completion requires CallAsync (not yet implemented)")
	}
	ac.resolved = true
	if err := ac.st.applyResolve(results); err != nil { panic(...) }

	if !ac.inCall {
		// Parked ⇒ reference on_progress/subtask_event: install the pending-
		// event closure. State/deliver_resolve are evaluated at DELIVERY
		// (getPendingEvent), matching the reference exactly — p2 is the state
		// when the guest receives the event, and lends are released then.
		st, si := ac.st, ac.subtaski
		st.setPendingEvent(func() eventTuple {
			if st.resolved() && !st.resolveDelivered {
				st.deliverResolve()
			}
			return eventTuple{eventSubtask, si, uint32(st.state)}
		})
	}
	// inCall: nothing more — the wrapper epilogue sees st.resolved().
}

func (ac *AsyncCall) Defer(fn func()) {
	ac.in.sched.enqueue(func() error { fn(); return nil })
}
```

### 2.5 End-to-end traces (the two acceptance cases)

**Immediate** (`fn` calls `ac.Resolve(vals)` before returning):
guest calls lowered import → wrapper lifts args → `fn` → `Resolve` writes
results through retptr into guest memory → wrapper returns packed `2`
(`RETURNED`, subtaski 0) → wit-bindgen sees RETURNED, reads its buffer, never
waits → guest computes, calls `task.return`, callback path exits with EXIT on
the first return. No table entry, no event, no pump.

**Deferred** (`fn` calls `ac.Defer(func(){ ac.Resolve(vals) })` and returns):
wrapper parks subtask at index `i`, returns `STARTED|i<<4` → guest calls
`waitable.join(i, wset)` → guest export returns packed `WAIT|wset<<4` →
`invokeAsyncCallback` releases exclusive, `drive(ws.hasPendingEvent)` → pops
the thunk → `Resolve` writes results into guest memory + installs pending
event → pred true → `getPendingEvent()` runs the closure: `deliverResolve()`
(lends released) then `(SUBTASK, i, RETURNED)` → callback invoked with
`(1, i, 2)` → guest reads buffer, `task.return`, `subtask.drop(i)` (traps if
resolve not delivered — it was), returns `EXIT` → loop breaks → task RESOLVED
→ `Call` returns the lifted result. Fully deterministic, zero goroutines.

**Truly external completion** (embedder wants to resolve after `Call` returns,
or from another goroutine): not in the MVP by construction — `Resolve` panics
outside the scheduler, and a guest WAITing on it hits the deadlock trap with a
message pointing at CallAsync. This is the honest single-threaded boundary; it
is exactly the plan §4 "failing loud if the guest blocks on a host import only
the embedder can complete".

---

## 3. Q3 — reference machinery: load-bearing vs stub (per field)

Legend: **KEEP** = implemented with real semantics in MVP; **TRAP** = the
would-block/none-such case fails loud; **OMIT** = field not added (adding later
is additive); **RESERVE** = enum value/comment only.

| Reference item | MVP disposition | Why |
|---|---|---|
| `inst.exclusive_thread` | **KEEP as `exclusiveHeld bool`** | With one task it never *schedules* anything (the WAIT predicate's `!exclusive` conjunct is vacuously true — the loop clears it first), but it is the fail-loud reentrancy tripwire (`enterTask`), and keeping the reference's set/clear sites in the loop means CallAsync lands without reshaping `invokeAsyncCallback`. |
| `enter_implicit_thread`'s cancellable backpressure wait | **TRAP** | Would-block ⇒ permanent deadlock with one task (nothing concurrent can clear backpressure/exclusive). Trapping is behavior-equivalent to the reference's hang, and honest. The cancellable-entry path only matters for CallAsync cancel (Phase 3). |
| `inst.backpressure` + `backpressure.set/inc/dec` | **KEEP the counter faithfully** | Load-bearing even solo: a guest that sets backpressure and exits leaves it set — the *next* `Call` must trap-not-hang (observable). inc's `trap_if == 2^16` / dec's `trap_if < 0` are real spec trap edges to test. |
| `inst.num_waiting_to_enter` | **OMIT** | Pure FIFO-fairness bookkeeping among multiple *pending* entrants; identically 0 with one task. Documented in `enterTask`. |
| `inst.may_enter` | **KEEP (real bool + trap)** | Load-bearing solo: a host import (or dtor) calling back into `Call`/an async export of the same instance must trap (`Store.lift`'s `trap_if(!may_enter_from)`). Cheap: one write per async call. |
| `inst.may_leave` | **KEEP (real bool + trap)** | Every async builtin and lower opens with `trap_if(!may_leave)`; sync post-return context must reject them. Two writes where relevant. |
| `Thread` objects, `inst.threads` table, `thread.index`, resume/suspend/cont | **OMIT entirely** | Callback tasks ⇒ implicit thread ≡ task (plan Phase-4 note); every suspension is a core return. The scheduler surface is `sched`. |
| `Thread.storage[2]` (context slots) | **KEEP → `task.ctxStorage`** | The one genuinely load-bearing Thread field: wit-bindgen's generated executor stashes its state pointer via `context.set` and reads it back in the callback. Without it no real rustc component runs. |
| `Task.state` INITIAL/STARTED/RESOLVED | **KEEP** | Drives the two must-have traps: `task.return` on a resolved task, and EXIT-without-`task.return` (`unregister_thread`'s `trap_if(state != RESOLVED)`). |
| `Task.state` PENDING_CANCEL / CANCEL_DELIVERED, `deliver_pending_cancel`, `request_cancellation` | **RESERVE** | Cancellation is Phase 3; no MVP caller exists. `deliver_pending_cancel` stubs to `return false`; the loop's TASK_CANCELLED arm is not emitted. Enum values reserved so numbering already matches. |
| `Task.num_borrows` | **KEEP field + its two trap sites** | Always 0 until Phase 3 borrow scopes, but `task.return`'s `trap_if(num_borrows > 0)` and the exit assert are kept now so Phase 3 is purely additive. |
| `Task.on_start` / `on_resolve` / `caller` | **KEEP as closures / OMIT caller** | onResolve is how `task.return` hands the result out (and the CallAsync/guest→guest seam). on_start collapses into "lower params before the first core call" (MVP `Call` has the args in hand). `caller` is always host/nil in MVP — omit, revisit for composition (Phase 3). |
| `Waitable.pending_event` as a **closure** | **KEEP closure form** | Semantically load-bearing: state (`p2`) and `deliver_resolve` (lend release) are evaluated at *delivery*, not at resolve. Matters for cancellation later; costs nothing now. |
| `Waitable.has_sync_waiter` | **OMIT** | Set only by `wait_for_pending_event`, reachable only from sync-lowered-to-async-callee blocking and sync `subtask.cancel` — both post-MVP. `waitable.join`'s `trap_if(has_sync_waiter)` gets a comment marking the omission. |
| `WaitableSet.num_waiting` | **KEEP (one int)** | `waitable-set.drop`'s `trap_if(num_waiting > 0)` is a spec trap edge; unreachable solo (guest code never runs while the driver is inside `drive`) but it's one increment and keeps the oracle diff clean; becomes load-bearing with multiple tasks. |
| `Subtask.on_cancel`, `cancellation_requested`, CANCELLED_* states, `BLOCKED` | **RESERVE** | `subtask.cancel` is Phase 3. States reserved; `subtask.drop`'s `trap_if(!resolve_delivered)` already covers the MVP hazard. |
| `Subtask.lenders` / `deliver_resolve` lend release | **KEEP** | Load-bearing solo: a borrow passed to a *deferred* async import must stay lent until the SUBTASK event is delivered, else the guest could drop the resource mid-flight. Finally uses the dormant `Lend`/`Unlend`. |
| `Store.waiting`, `Store.tick`, `nesting_depth` | **COLLAPSE → `sched`** | The whole point of this design. `nesting_depth` asserts become nothing (nested pumping is legal and structural). |
| Sync-caller loop `trap_if(not candidates)` | **KEEP → `errAsyncDeadlock`** | The deadlock trap, now covering both WAIT-with-empty-queue and blocked task entry. |
| `unpack_callback_result` `trap_if(code > MAX)` | **KEEP** | Real guest-reachable trap. Plus enforce table `MAX_LENGTH < 2^28` in `handleTable.add` so `si<<4` packing can't overflow. |

---

## 4. Ambiguities in the reference / deliberate deviations

1. **YIELD granularity.** `wait_until(λ: !exclusive)` under the deterministic
   profile parks unconditionally for one scheduler round; how much foreign work
   runs before resumption is scheduler freedom. wazy pins **snapshot-drain**
   (everything queued at yield time runs; newly-enqueued work waits). The
   oracle's monkeypatched profile must encode the same choice — this is a
   *pinned convention*, not derivable from the spec.
2. **Deadlock ⇒ trap.** The reference embedder would `tick()` forever on an
   unsatisfiable wait; canon_lift's sync-caller loop traps only for sync-ft
   lifts. wazy traps in *both* driver positions (WAIT drive, wait builtin,
   blocked entry). Sanctioned by plan §4 ("no-progress traps as deadlock");
   strictly more diagnostic than the reference, never less permissive for any
   program that can make progress.
3. **Backpressure would-block ⇒ trap** (vs reference park). Equivalent-to-hang
   solo; message names `backpressure` explicitly. Revisit when CallAsync makes
   parking meaningful.
4. **`Waitable.drop`'s `assert(!has_pending_event)`** is an assert, not a
   trap_if, and is unreachable through `subtask.drop` (the `resolve_delivered`
   trap fires first). Kept as a Go panic-assert; a named test documents the
   unreachability argument.
5. **Vintage drift**: the vendored definitions.py carries *both*
   `backpressure.set` and `inc`/`dec`, and models yield as `thread.yield`
   returning `[cancelled]`. Implement what the pinned wasm-tools/wit-bindgen
   emit (plan risk #3); decode-but-fail-loud anything else.
6. **Resolve-time memory writes**: async-lowered results are written through
   the call-time retptr *at resolve time* (reference `on_resolve` does
   `lower_flat_values` immediately). Two implementation hazards pinned here:
   fetch memory bytes fresh at resolve (growth between call and resolve), and
   the guest's retptr buffer must remain valid until the SUBTASK event — that
   is the ABI's contract (wit-bindgen keeps it in the future's heap state), not
   something wazy must police.
7. **Immediate-resolve packing**: fast path returns packed `2` (`RETURNED`,
   subtaski 0) and the subtask never enters the table — index 0 is not a
   handle. Free-list reuse of parked subtask indices after `subtask.drop` is
   guest-observable; the oracle asserts indices.
8. **`LiftOptions.equal` in task.return**: "same opts as the lift" — compare
   the canon's resolved memory/realloc/encoding against the lift's; wazy
   compares the *resolved identities* (bind-time where both are static), which
   is the observable meaning.

## 5. Test hooks this design gives Phase 1

- Named trap tests fall directly out of §3's KEEP rows: bad callback code,
  WAIT on non-set index, EXIT-without-task.return, task.return twice,
  task.return from sync export, waitable-set.drop non-empty, subtask.drop
  before delivery, deadlock (WAIT with no Defer), backpressure-set-then-Call,
  Resolve twice, Resolve from a raw goroutine (panic), reentrant Call from an
  import.
- `AsyncCall.Defer` chains give multi-round pump coverage without concurrency:
  `Defer(Defer(Resolve))` must take two `drive` iterations and one YIELD
  snapshot must NOT run the inner one.
- The oracle scenario scripts map 1:1: `enqueue` = "a thread became ready",
  FIFO = monkeypatched choice, table indices asserted.

# Design: thread.* execution runtime (cooperative fibers) — last 4 async .wast suites

Branch `feat/component-model-async`. Target: async conformance 26/31 → 30/31 (the 31st,
`sync-streams`, is a confirmed upstream fixture bug and stays skipped).

Status of this doc: **design only** — every claim below was verified against the actual
files/lines cited (reference `internal/component/abi/testdata/definitions.py`, the four
vendored fixtures, and wazy's instance/binary/sched/task sources) during design, not
assumed. Line numbers are as of HEAD of this branch.

---

## 0. Executive summary and the key leverage (verified)

The brief's central claim **checks out, with one correction**:

* The reference's fiber primitive (`Continuation`/`Handler`/`cont_new`/`resume`/`block`,
  definitions.py:258–310) is an OS thread + already-acquired-lock **baton**: exactly one
  thread runs at a time; every transfer of control is one lock release + one lock acquire.
  wazy's `stackfulTask` (stackful_task.go:56–127, `block` at :250–265) is the identical
  pattern with the lock pair replaced by an unbuffered channel pair (`resumeCh`/`yieldCh`),
  and `guestSegment` (guest_task.go:99–158) is a second instance of the same pattern.
  So a reference `Thread` maps 1:1 onto **a goroutine + a channel baton**, and wazy already
  has two working, `-race`-clean, reap-safe implementations of that mapping.
* The reference's scheduler (`Store.waiting` + `Store.tick`, definitions.py:571–616) maps
  onto wazy's shared-per-composition-tree `sched` (sched.go): `parked` is `Store.waiting`,
  `sched.step`'s first-ready scan (sched.go:123–130) is the deterministic-profile
  `random.choice(candidates)` (definitions.py:610–614), and `sched.drive`'s
  `errAsyncDeadlock` (sched.go:138–149) is `canon_lift`'s `trap_if(not candidates)`
  (definitions.py:2205). **The `parkedTask` interface (sched.go:52–55) already lets a new
  thread type participate with zero sched changes.**
* **Correction to the brief**: the brief says `Thread.index` is "the slot in the task's
  thread table". In the reference it is the slot in the **ComponentInstance's** thread
  table: `ComponentInstance.threads: Table[Thread]` (definitions.py:201, :213), and
  `Task.register_thread` does `thread.index = self.inst.threads.add(thread)`
  (definitions.py:509); the Task only keeps a Python *list* for cancellation candidates and
  the last-thread trap (definitions.py:467, :518–525). Also `canon_thread_resume_later` /
  `suspend_then_resume` / `yield_then_resume` all resolve the target through
  `inst.threads.get(i)` (definitions.py:2708, :2734, :2744). wazy follows the reference:
  **the thread table lives on `Instance`**; the waiting list stays where it already is
  (`sched.parked`, matching `Store.waiting` being store-global — wazy's `sched` is shared
  per composition tree, Instance.sched's doc).

**Design in one sentence**: add a `guestThread` struct — the same goroutine+baton primitive
as `stackfulTask`/`guestSegment`, minus the task-prologue baggage — registered in a small
per-Instance `threadTable`, parked in the existing `sched.parked` list, resolved as the
"current blocker" ahead of the task's implicit thread; then implement exactly five canons
(`thread.yield` 0x0c, `thread.index` 0x26, `thread.new-indirect` 0x27, `thread.suspend`
0x29, `thread.yield-then-resume` 0x2b) on top of it, in four stages that green one suite
each.

A second, load-bearing discovery from reading the fixtures (§1): **three of the four suites
need almost no new fiber machinery.** `sync-barges-in` and `cancellable` need only
`thread.yield` expressed through the *existing* `taskBlocker.block` primitive (plus one
bind-time promotion-gate widening), and `trap-if-block-and-sync` needs only binds + the
existing "cannot block a synchronous task" trap + one trap-text reword — its
`thread.new-indirect`/`thread.index`/`switch-to`/`resume-later` *call sites are commented
out in the fixture* (trap-if-block-and-sync.wast:129–147; the canons must bind, and are
never called). Only `trap-if-sync-and-waitable-set` exercises real spawned threads.

---

## 1. Exact scope: which canons each suite needs (verified against the fixtures)

| suite | canons that must **bind** | canons that must **execute** | other gaps found |
|---|---|---|---|
| sync-barges-in | thread.yield (0x0c, cancel?=false) | thread.yield from a **stackful** implicit thread ($C.yielder, async-opts stackful lift, wast:82–85,134) and from a **callback** task via packed YIELD code (yielder-cb — *existing* parkYield, no builtin) | none |
| cancellable | thread.yield (0x0c, cancel?=true) | thread.yield-cancellable from a **promoted callback** task's initial core call (wast:42–50, 76–96); delivered-cancel and pending-cancel variants | `mayBlockSync` promotion gate must cover $C (§5.4) |
| trap-if-block-and-sync | thread.yield 0x0c, thread.index 0x26, thread.new-indirect 0x27, thread.suspend 0x29 | thread.yield from a **sync task** must NOT trap (yield-is-fine, wast:118–121, 246, 308); thread.suspend from a sync task must trap "cannot block a synchronous task before returning" (wast:94–97, 296). thread.index / new-indirect are **never called** (calls commented out, wast:129–147) | callback-code trap text must contain "unsupported callback code" (wast:302–306; wazy says "invalid callback code", async_lift.go:30) |
| trap-if-sync-and-waitable-set | thread.new-indirect 0x27, thread.yield-then-resume 0x2b (cancel?=false) | full spawn + direct-switch: `$spawn-and-yield` (wast:69–72) spawns a thread that runs a **sync** future/stream copy and parks with a sync waiter, while $Main (stackful) resumes and traps | sync copies must set `syncWaiter` while blocked (§6.2); `waitable.join`'s and `subtask.cancel`'s in-set/sync-waiter trap texts must contain "waitable cannot be used synchronously while added to a waitable set" (§6.3) |

Deliberately **out of scope** (no suite reaches them): thread.resume-later 0x28,
suspend-then-resume 0x2a, suspend/yield-then-promote 0x2c/0x2d, spawn-ref/spawn-indirect/
available-parallelism 0x40–0x42 — these are not even decoded (binary/component.go:228–237),
and this design does not add them.

Decode status (verified, binary/decoder.go:940–971): all five kinds decode with their
payloads captured — `Cancellable` for 0x0c/0x29/0x2b, `TypeIdx`+`TableIdx` for 0x27
(binary/component.go:300–307). Nothing new is needed in the decoder.

---

## 2. Reference → goroutine mapping (the dictionary)

| reference (definitions.py) | wazy |
|---|---|
| `Thread` (:323) | `guestThread` (new, §3) for spawned threads; the **implicit** thread of a task stays collapsed into `stackfulTask`/`guestTask`/`syncImplicit` exactly as today (task.go:22–29's documented collapse) |
| `cont_new(f)` — spawn OS thread parked on an acquired lock (:276–288) | lazy `go th.main()` on first resume; goroutine immediately blocks on `<-th.resumeCh` |
| `resume(cont, block_result, thread)` — release callee lock, acquire own (:290–299) | `th.resumeCh <- mode; <-th.yieldCh` (`guestThread.handoff`, same shape as stackful_task.go:317–327) |
| `block(switch_to)` (:301–310) | `th.block(ready, cancellable)`: park in `sched.parked`, `yieldCh <- struct{}{}`, `<-resumeCh` (same shape as stackful_task.go:250–265) |
| `current_thread()` via thread-local handler (:312–313) | `in.activeThread *guestThread` — a plain field, set/cleared at every baton handoff; legal because the baton serializes all access (§5.2) |
| `Thread.running/suspended/waiting/ready` (:331–341) | running = holds the baton (`in.activeThread == th` or executing); suspended = parked with `parkReady == nil`; waiting = parked with `parkReady != nil`; ready = `th.ready()` (§3) |
| `Store.waiting` (:571) | `sched.parked` (shared per composition tree — matches waiting being on the Store, not the instance) |
| `Store.tick`'s `enter_from(None)`/resume/`leave_to(None)` (:606–616) | `sched.step` → `th.resumeReady()`; enter/leave brackets are `enterRun`/`suspendRun` executed *inside* the resumed goroutine (`guestThread.run`/`block`), exactly as `stackfulTask` already does |
| `Thread.resume`'s switch-to **chain** (:376–382) | `resumeInline(other)` — the switching goroutine acts as temporary driver for `other` until it parks/finishes (§4.5); single-runnable is preserved because the switcher is blocked in the handoff while `other` runs |
| `Task.threads` list + `register/unregister_thread` (:467, :505–525) | `task.liveThreads int` + registration in `in.threads` (`threadTable`) |
| `ComponentInstance.threads: Table` (:201; index 0 reserved, free-list reuse, Table :682–717) | `Instance.threads threadTable` (slice + free list, index 0 reserved) |
| `canon_thread_*` (:2677–2766) | five `hostFuncDef` constructors in a new `thread.go`, wired in `computeCanonHostFunc` exactly like the existing async builtins (graph.go:1420–1476 pattern) |

Everything else the reference threads touch — `deliver_pending_cancel` (:545),
`has_backpressure`, `exclusive_thread`, `may_enter/may_leave` — already exists in wazy
(task.go:139–145, :260–311) and is reused unchanged.

---

## 3. `guestThread`, the thread table, and where they live

New file `internal/component/instance/thread.go`.

```go
// guestThread is an explicitly-spawned thread of execution (canon
// thread.new-indirect) -- the reference's Thread (definitions.py:323) for the
// non-implicit case. Same goroutine+baton primitive as stackfulTask/
// guestSegment: at most one goroutine in the composition tree runs
// component-runtime code; every control transfer is a channel send/recv pair.
type guestThread struct {
	t   *task     // owning task (reference Thread.task)
	in  *Instance // == t.inst
	ctx context.Context

	index uint32       // slot in in.threads (reference Thread.index; :509)
	fn    api.Function // the indirect-table func, resolved AT new-indirect time (:2692)
	arg   uint64       // the `c` argument (:2696)

	// The baton (created lazily at first resume; nil while never-resumed).
	resumeCh chan resumeMode // driver -> goroutine (reuses stackful_task.go's resumeMode)
	yieldCh  chan struct{}   // goroutine -> driver
	spawned  bool

	// Park state. parkReady == nil while parked means SUSPENDED (reference
	// Thread.suspended(): not resumable by the scheduler, only by an explicit
	// switch/resume-later); parkReady != nil means WAITING (Thread.waiting()).
	parked      bool
	parkReady   func() bool
	cancellable bool
	cancelWake  bool

	done     bool
	err      error
	panicVal any
}
```

**Placement decisions and why:**

* **Thread table on `Instance`** (`threads threadTable` + a tiny free-list slice type with
  index 0 reserved, mirroring the reference `Table` (:688 `array = [None]`) so the first
  thread gets index 1): matches definitions.py:201/:509/:2708. Not on the task — the canons
  resolve indices through the *instance* — and not on `sched` — the table is a per-instance
  index space (a component must not be able to name another instance's threads).
* **Waiting list on `sched`**: spawned threads park in the existing `sched.parked` via the
  existing `parkedTask` interface (sched.go:52) — `guestThread` implements `ready()` and
  `resumeReady()`. This gives FIFO-registration-order determinism identical to the current
  deterministic-profile stance (sched.go:33–36) and makes deadlock detection, `drainReady`,
  and reap all work with **zero sched.go changes**.
* **Task keeps only `liveThreads int`** (for the reference's last-thread-unresolved trap,
  :521–523) — not a slice; requestCancellation's spawned-thread candidates are unexercised
  by any suite (§11.5).
* **The implicit thread stays collapsed** into `stackfulTask`/`guestTask`/the
  `syncImplicit` task. Refactoring stackfulTask to literally *be* "thread 0 in the table"
  would touch every one of the 26 passing suites' hot paths for zero conformance gain; the
  additive design keeps them byte-identical. The one seam this requires is current-thread
  resolution (§5.2).

**Core methods** (all bodies are near-verbatim transplants of stackful_task.go:250–327):

```go
// ready: reference Thread.ready() (:340) plus wazy's cancel-wake convention.
// NOTE deliberately scoped tighter than stackfulTask.ready's sparkBlock arm:
// t.cancelDeliverable() only wakes a CANCELLABLE park (avoids spuriously
// resuming a non-cancellable suspended/waiting thread of a PENDING_CANCEL task).
func (th *guestThread) ready() bool {
	return th.parkReady != nil &&
		(th.cancelWake || (th.cancellable && th.t.cancelDeliverable()) || th.parkReady())
}

// resumeReady: called by sched.step (driver) OR resumeInline (a switching
// thread acting as temporary driver). Spawns lazily, then hands the baton.
func (th *guestThread) resumeReady() error {
	th.in.sched.unpark(th)
	th.parked, th.parkReady = false, nil
	mode := resumeNormal
	if th.cancelWake { th.cancelWake, mode = false, resumeCancelled }
	if !th.spawned {
		th.resumeCh, th.yieldCh = make(chan resumeMode), make(chan struct{})
		th.spawned = true
		go th.main()
	}
	return th.handoff(mode)
}

// handoff / main / block: shape-identical to stackfulTask.handoff (:317),
// stackfulTask.main (:112), stackfulTask.block (:250). block additionally
// clears/restores in.activeThread (§5.2):
func (th *guestThread) block(ready func() bool, cancellable bool) (cancelled bool) {
	if th.t.deliverPendingCancel(cancellable) { return true }
	th.parked, th.parkReady, th.cancellable = true, ready, cancellable
	th.in.activeThread = nil
	th.in.suspendRun()          // same bracket as stackfulTask.block (:255)
	th.in.sched.park(th)
	th.yieldCh <- struct{}{}
	mode := <-th.resumeCh
	if mode == resumeAbort { panic(errStackfulAbort) }
	th.parkReady, th.cancellable = nil, false
	th.in.enterRun(th.t)
	th.in.activeThread = th
	return mode == resumeCancelled
}

// run: the goroutine body's payload -- reference thread_func (:2695-2697).
func (th *guestThread) run() error {
	th.in.enterRun(th.t)
	th.in.activeThread = th
	stack := []uint64{th.arg}                 // ft is (i32)->() or (i64)->(); no results
	err := th.fn.CallWithStack(th.ctx, stack) // call_and_trap_on_throw (:2696)
	th.in.activeThread = nil
	th.in.leaveRun()
	// unregister_thread (:518-525): free the table slot, last-thread trap.
	th.in.threads.remove(th.index)
	th.t.liveThreads--
	if err != nil {
		th.in.poisoned = true // guest code ran and trapped -- same rule as stackfulTask.run (:180)
		return fmt.Errorf("component/instance: thread %d: %w", th.index, err)
	}
	if th.t.liveThreads == 0 && th.t.state != taskResolved {
		return fmt.Errorf("component/instance: thread %d: last thread of the task exited before the task was resolved", th.index)
	}
	return nil
}
```

`guestThread` also satisfies `taskBlocker` (task.go:92–94) via `block` — that is the whole
integration with the blocking builtins (§5.2).

---

## 4. The five canons

All five are `hostFuncDef` constructors in `thread.go`, dispatched from new cases in
`computeCanonHostFunc`'s switch (graph.go:1244, replacing the default arm's
"kind %#x … does not produce a core func" failure at :1512 for exactly these kinds).
Each opens with `requireMayLeave(in, name)` — the reference's `trap_if(not may_leave)`
prologue shared by all thread canons (:2691, :2707, :2717, :2725, :2743). Each bind-site
also sets `in.syncTaskNeeded = true` (so sync lifts install a `syncImplicit` task,
instance.go:1640–1655 — `yield-is-fine` needs an active task to resolve against) and
`in.mayBlockSync = true` (the promotion gate, §5.4).

### 4.1 thread.yield (0x0c) — core sig `() -> i32`

Reference: `canon_thread_yield` (:2723–2727) → `Thread.yield_` (:411–412) →
`wait_until(lambda: True, cancellable)` (:402–409): deliver-pending-cancel prologue, then
enqueue-as-ready and block (deterministic profile always blocks — the `random.randint`
early-return at :406 is profile-killed).

```go
func threadYieldHostFunc(in *Instance, canon binary.Canon) hostFuncDef {
	in.syncTaskNeeded, in.mayBlockSync = true, true
	cancellable := canon.Cancellable
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "thread.yield")
		t := in.activeTask
		blk := in.currentBlocker() // §5.2: activeThread first, then t.blocker()
		switch {
		case blk != nil:
			// Stackful implicit thread, promoted callback segment, or a
			// spawned guestThread: a genuine park with an always-true
			// predicate; the driver resumes it on its next step, after any
			// already-queued thunks/ready tasks (the reference's "yielded
			// thread re-enters waiting BEHIND already-ready threads").
			if blk.block(func() bool { return true }, cancellable) {
				stack[0] = 1 // Cancelled.TRUE
				return
			}
			stack[0] = 0
		case t != nil && !t.syncImplicit:
			// Unpromoted callback task's guest code on the driving goroutine
			// (no suite reaches this arm -- cancellable's tasks are promoted
			// by the bind gate; kept for parity): one deterministic scheduler
			// round, then the pending-cancel check, mirroring wait_until's
			// order as closely as a frame-held caller allows.
			if t.deliverPendingCancel(cancellable) { stack[0] = 1; return }
			if err := in.sched.pumpSnapshot(); err != nil {
				panic(fmt.Errorf("component/instance: thread.yield: %w", err))
			}
			stack[0] = 0
		default:
			// Sync task (t == nil or t.syncImplicit): the reference ALLOWS a
			// sync task to yield (yield_ is a ready-waiting block; canon_lift's
			// sync driver loop (:2202-2206) resumes it -- unlike suspend/wait
			// there is always a ready candidate, so no trap). wazy equivalent:
			// one scheduler round on the driving goroutine, return NOT
			// cancelled. trap-if-block-and-sync's yield-is-fine asserts 42,
			// not a trap (wast:308).
			if err := in.sched.pumpSnapshot(); err != nil {
				panic(fmt.Errorf("component/instance: thread.yield: %w", err))
			}
			stack[0] = 0
		}
	})
	return hostFuncDef{fn: fn, params: nil, results: []api.ValueType{api.ValueTypeI32}}
}
```

Trap edges: only `may_leave`. Return value is the reference's `Cancelled` enum (FALSE=0,
TRUE=1, :254–256) — cancellable.wast:46/92 assert the `1`.

### 4.2 thread.suspend (0x29) — core sig `() -> i32`

Reference: `canon_thread_suspend` (:2715–2719) → `Thread.suspend` (:396–400) →
`block_internal`: park with **no ready predicate** — resumable only by an explicit
`thread.resume-later`/switch (none of which this design implements) or a cancellable
cancel delivery.

```go
requireMayLeave(in, "thread.suspend")
t, blk := blockingTask(in, "thread.suspend") // sync task -> the exact trap suite 3 asserts
if blk == nil {
	// Unpromoted callback ctx: a frame-held caller cannot suspend-forever
	// without deadlocking its own driver; fail loud (unexercised -- §11).
	panic(fmt.Errorf("component/instance: thread.suspend: not supported from an unpromoted callback task"))
}
if t.deliverPendingCancel(cancellable) { stack[0] = 1; return }
if blk.block(neverReady, cancellable) { stack[0] = 1; return } // neverReady = func() bool { return false }
stack[0] = 0
```

For the implicit-thread blockers (`stackfulTask`/`guestTask.block`) a `neverReady`
predicate parks forever; their `ready()` conjuncts (`cancelWake || t.cancelDeliverable() ||
parkReady()`, stackful_task.go:278, guest_task.go:217) mean a cancellable cancel can still
wake it — matching `Thread.resume(Cancelled.TRUE)` on a suspended thread (:372–382, legal
iff `cancellable`, :373). The **only exercised path** in the 4 suites is the
`blockingTask` sync trap: trap-if-block-and-sync's `trap-if-suspend` is a plain sync lift
(wast:242) → `syncImplicit` task → "cannot block a synchronous task before returning"
(async_builtins.go:68) — the exact substring wast:296 asserts.

### 4.3 thread.index (0x26) — core sig `() -> i32`

Reference: `canon_thread_index` (:2677–2680): return `current_thread().index`.

```go
requireMayLeave(in, "thread.index")
if th := in.activeThread; th != nil { stack[0] = uint64(th.index); return }
t := requireActiveTask(in, "thread.index")
if t.implicitThreadIdx == 0 {           // lazy register (deviation §11.4)
	t.implicitThreadIdx = in.threads.add(implicitThreadMarker{t})
}
stack[0] = uint64(t.implicitThreadIdx)
```

The implicit thread's slot is allocated **lazily on first thread.index call** and freed at
the task's completion funnels (`guestTask.finishExit`/`fail`, `stackfulTask.fail`/
`finishOK`, `invokeEntered`'s syncImplicit defer) — gated on `implicitThreadIdx != 0` so
the 26 passing suites (which can never call thread.index — they don't bind it) pay
nothing. No vendored suite calls thread.index at runtime (trap-if-block-and-sync's call is
inside commented-out `switch-to-is-fine`, wast:138–141), so this is unit-test-covered
only; the lazy scheme is chosen over reference-faithful eager registration
(`enter_implicit_thread`, :502) precisely to keep task entry untouched. Free-slot
discipline still matches the reference `Table` (:713–717).

### 4.4 thread.new-indirect (0x27) — core sig `(fi:i32, c:i32|i64) -> i32`

Reference: `canon_thread_new_indirect` (:2689–2701): resolve `ftbl.get(fi)` (traps on bad
index), `trap_if(f.t != ft)` where ft ∈ {(i32)->(), (i64)->()}, build a **suspended**
Thread whose `thread_func` calls the func then unregisters, register it, return its index.

**Bind time** (this is the one canon needing new plumbing in graph.go):

1. Thread a `coreTableTarget func(int) (api.Module, string, error)` parameter through
   `computeCanonHostFunc` (alongside the existing `coreMemTarget`/`coreFuncTarget`,
   graph.go:1242), built from the already-existing `coreTableSpace` (graph.go:181–190, the
   alias-built core-table index space that `shimSortTable` inline exports already consume
   at :1205–1214) + `instMods`. Two mechanical call sites (graph.go:490 and
   buildMergedCanonHostModule's path at :1556).
2. Resolve `canon.TableIdx` → `(ownerMod, exportName)`; cast `ownerMod` to
   `*wasm.ModuleInstance`; find the exported table via `m.Exports[exportName].Index` →
   `m.Tables[idx]` (the exact mechanism store.go:373–377 uses); precompute
   `typeID := m.GetFunctionTypeID(&wasm.FunctionType{Params: {i32|i64}})`.
3. **The `ft` payload (`canon.TypeIdx`, a core:typeidx)**: wazy does not decode the core
   type section (decoder.go:121–125 stores it raw), so `ft` is instead derived from the
   consumer's own core import signature via the already-available
   `neededTypes[groupName][entryName]` (the same source trap stubs use, graph.go:1402):
   `sig.params = [i32, i32|i64]`, `sig.results = [i32]` — the second param IS
   `flatten(ft.param)`. In any validated component these are definitionally equal; the
   call-time `LookupFunction` typeID check below then enforces the reference's
   `trap_if(f.t != ft)` exactly. Deviation logged in §11.6; bind fails loud if the
   consumer signature isn't one of the two legal shapes.

**Call time**:

```go
t := requireActiveTask(in, "thread.new-indirect") // current_task() + may_leave (:2690-2691)
fi, c := api.DecodeU32(stack[0]), stack[1]
fn := ownerWasmMod.LookupFunction(table, typeID, fi) // panics with call_indirect semantics
                                                     // on bad index / null / type mismatch
                                                     // (module_instance_lookup.go:14) == trap_if(f.t != ft)
th := &guestThread{t: t, in: in, ctx: bindCtx, fn: fn, arg: c}
th.parked, th.parkReady = true, nil // SUSPENDED (:2699 assert(new_thread.suspended()))
in.sched.park(th)
th.index = in.threads.add(th)       // register_thread (:2700, :509)
t.liveThreads++
stack[0] = uint64(th.index)
```

The goroutine is **not** spawned here (lazy spawn in `resumeReady`, §3) — a faithful
economy: the reference's freshly-`cont_new`ed OS thread exists but has provably executed
zero guest instructions until first resume; a never-resumed wazy thread costs nothing and
reaps trivially. `c` is stored as raw `uint64` bits — correct for both i32 and i64 stacks.

Trap edges: bad `fi` / null entry / wrong type (LookupFunction panic, wasm-trap text
identical to core `call_indirect`); `may_leave`; no-active-task. None are asserted by the
suites (new-indirect is only *called* by trap-if-sync-and-waitable-set on valid indices)
— unit tests carry them (§10).

### 4.5 thread.yield-then-resume (0x2b) — core sig `(i:i32) -> i32`

Reference: `canon_thread_yield_then_resume` (:2741–2747): resolve `inst.threads.get(i)`,
`trap_if(not other.suspended())`, then `Thread.yield_then_resume` (:420–425):
deliver-pending-cancel; `start_waiting_internal(lambda: True)` (self becomes
ready-waiting); `switch_to_internal(cancellable, other)` — a direct baton transfer to
`other`, with self resumed later by the scheduler.

```go
requireMayLeave(in, "thread.yield-then-resume")
i := api.DecodeU32(stack[0])
other, ok := in.threads.getThread(i) // trap: bad index / freed slot / implicit marker
if !ok { panic(fmt.Errorf("component/instance: thread.yield-then-resume: index %d is not a live thread", i)) }
if !other.suspendedState() {         // parked && parkReady == nil
	panic(fmt.Errorf("component/instance: thread.yield-then-resume: thread %d is not suspended", i))
}
t, blk := blockingTask(in, "thread.yield-then-resume") // sync task -> sync-block trap
if blk == nil {
	panic(fmt.Errorf("component/instance: thread.yield-then-resume: not supported from an unpromoted callback task"))
}
if t.deliverPendingCancel(cancellable) { stack[0] = 1; return }

// Direct switch: the CURRENT goroutine (which holds the baton) acts as the
// temporary driver for `other` -- resumes it and blocks in the handoff until
// other parks or finishes. Single-runnable holds: while other runs, we are
// blocked in <-other.yieldCh executing nothing. This is the reference resume
// chain (:376-382) with the chain link inverted into a nested handoff.
if err := other.resumeReady(); err != nil {
	// other's thread func trapped while we were suspended-in-spirit: surface
	// it on OUR task, exactly as a failing sched thunk would surface on the
	// driver (other.run already set in.poisoned for a real guest trap).
	panic(err)
}
// Now park SELF as ready-waiting (the "yield" half). The driver will resume
// us on its next step -- behind any thunks/ready tasks queued meanwhile,
// matching the reference's waiting-list re-entry.
if blk.block(func() bool { return true }, cancellable) { stack[0] = 1; return }
stack[0] = 0
```

**Ordering note (deviation §11.3)**: the reference parks self *before* switching; wazy
switches first, then parks. Proven equivalent for every observable: (a) `other` runs
before the driver does anything else in both; (b) self resumes only via the driver's next
scan in both; (c) if `other` tried to switch back to self mid-run, the reference traps
(`trap_if(not other.suspended())` — self is *waiting*, not suspended) and wazy traps the
same check (self is running-not-parked); (d) `Store.tick` cannot interleave in either
(reference: nesting_depth; wazy: the driver doesn't hold the baton).

If `other` **finishes** inside `resumeReady` (thread func returns): its `run` tail
unregisters it and returns nil; self then parks and is resumed next round — identical to
the reference chain ending with `switch_to = None` (:381 loop exit).

---

## 5. Composition with the existing task model

### 5.1 What each suite's "thread 0" is (and that it does not change)

* **cancellable**: $C's exports are callback lifts whose *initial core call* blocks. The
  task is a guest-started (`startAsyncExportTask`, async_lift.go:180–230) **promoted**
  `guestTask`; the implicit thread's live frames sit on a `guestSegment` goroutine, and
  `thread.yield` reaches it through `t.blocker()` → `gt.block` (guest_task.go:166–184).
  The stackfulTask is not involved. Nothing about guestTask changes.
* **sync-barges-in**: $C.yielder is an async-opts stackful lift → the task IS a
  `stackfulTask`; `thread.yield` reaches `st.block` (stackful_task.go:250). Unchanged.
* **trap-if-block-and-sync**: sync lifts → `syncImplicit` tasks (instance.go:1651); the
  yield/suspend builtins branch on exactly the classification `blockingTask` already
  encodes (async_builtins.go:65–71).
* **trap-if-sync-and-waitable-set**: $Main's exports are sync-opts stackful lifts
  (graph.go:1816–1822 classification) → implicit thread = `stackfulTask` goroutine;
  spawned `guestThread`s are the only new runnable kind.

So the existing `stackfulTask` **stays separate** — it is "the implicit thread of its
task", not an entry in the thread table (except the lazy thread.index slot, §4.3). This is
the deliberate deviation from full reference symmetry that buys the non-regression
guarantee (§9).

### 5.2 Current-thread resolution — the one seam

New Instance field + resolver:

```go
// activeThread is non-nil exactly while a SPAWNED guestThread's goroutine
// holds the baton (set/cleared at guestThread.run entry/exit and around
// guestThread.block's park). nil means the current thread is the implicit
// thread of in.activeTask (or no thread at all). Baton-serialized: only the
// goroutine holding the baton ever reads or writes it -- same discipline as
// activeTask itself (task.go enterRun/suspendRun).
activeThread *guestThread

func (in *Instance) currentBlocker() taskBlocker {
	if th := in.activeThread; th != nil { return th }
	if t := in.activeTask; t != nil { return t.blocker() }
	return nil
}
```

Two existing resolvers change **one line each** to prefer the spawned thread:

* `blockingTask` (async_builtins.go:65–71): keep the syncImplicit trap; its returned `blk`
  becomes `in.currentBlocker()`-style (activeThread first). This is what routes a spawned
  thread's blocking `waitable-set.wait` (if any future suite does it) to the right baton.
* `activeBlocker` (async_builtins.go:85–90): same preference. **This is the load-bearing
  one**: trap-if-sync-and-waitable-set's spawned thread performs a *sync* `future.read` /
  `stream.write`, whose blocked arm calls `activeBlocker(in)` (stream_builtins.go:493,
  :583, :720) — with the preference, it parks the **guestThread**, not the task's
  stackfulTask (which is itself parked elsewhere; parking it twice would corrupt the
  baton). Without this change, suite 4 is unimplementable; with it, the copy builtins need
  zero further changes.

For the 26 passing suites `activeThread` is permanently nil, so both resolvers evaluate
exactly as before.

`enterRun`/`leaveRun`/`suspendRun` (task.go:288–311) are reused verbatim by
`guestThread.run`/`block` — a spawned thread runs under its owning task's brackets
(mayEnter=false while its guest code is on the baton; exclusive owned by the same task the
implicit thread holds it for, so no conflict — reference: the whole task shares
`exclusive_thread` and `wait_until` never releases it, :396–409).

### 5.3 Interaction with sched: none required

`guestThread` joins `sched.parked` through the existing `parkedTask` interface. Suspended
threads (`parkReady == nil`) are never `ready()` → invisible to `step`/`drainReady`/
`pumpSnapshot`, and correctly do **not** count as progress for deadlock detection —
matching the reference, where suspended threads are absent from `Store.waiting` and
`trap_if(not candidates)` ignores them (:2204–2205). Waiting threads compete in
registration order with tasks — the same deterministic first-ready policy the whole
harness already pins.

### 5.4 The promotion gate (`mayBlockSync`) — required by cancellable

Today `mayBlockSync` is set only when an instance binds a sync lower targeting an async
lift (graph.go:1385–1387) or a sync stream/future copy site; cancellable's $C binds
neither — it has **no lowers at all** — so its guest-started callback tasks would be
unpromoted, and `waitable-set.wait`/`thread.yield` from their initial core call would take
the **nested-drive** arm (async_builtins.go:200–216). That is fatal for this suite: the
nested drive can never return STARTED to $D, so $D can never issue the `subtask.cancel`
the test is about; the drive would report a bogus deadlock.

Fix: **binding any thread.\* canon sets `in.mayBlockSync = true`** (done inside each new
`computeCanonHostFunc` case). Rationale: a component that imports thread primitives
declares itself a multi-fiber program; its callback tasks must be able to park mid-core-
call. Effect on the 26 passing suites: zero — none binds a thread canon (they would have
failed at bind, which is the very error being fixed). Within cancellable this promotes all
four $C exports (including test 1's `wait-cancel`, which needs promotion for the same
STARTED-return reason and happens to live in the same component as the thread.yield
binds).

The rest of cancellable then runs on **existing, tested Feature-1 machinery**: parkBlocked
(guest_task.go:42, :166–184), `requestCancellation`'s parkBlocked arm (task.go:224–239),
`deliverPendingCancel` at cancellable suspension prologues, `eventTaskCancelled` delivery.

---

## 6. Supporting changes outside the thread runtime

### 6.1 Trap-text: "unsupported callback code"

`unpackCallbackResult` (async_lift.go:30) says `invalid callback code %d (packed %#x)`;
trap-if-block-and-sync asserts substring `"unsupported callback code"` (wast:302–306).
Reword to `unsupported callback code %d (packed %#x)`; also `guestTask.advance`'s default
arm (guest_task.go:438) for consistency. One pinned test updates:
async_first_light_test.go:150 (`requireErrContains "invalid callback code"` →
`"unsupported callback code"`). No manifest of a passing suite asserts the old text
(verified by grep over testdata manifests).

### 6.2 `syncWaiter` on blocked sync stream/future copies

Reference `Waitable.wait_for_pending_event` sets `has_sync_waiter = True` around the block
(:775–779) and `canon_waitable_join` traps on it (:2452). wazy sets `syncWaiter` only in
`subtask.cancel`'s sync wait (async_builtins.go:416–426); the copy builtins' sync arms
don't. Suite 4's four `join-during-sync-*` tests require it: the spawned thread blocks in
a sync copy, then $Main joins that end into a set and must trap.

Change (three sites, each ±4 lines): in `streamCopyHostFunc` (stream_builtins.go:482–499),
`futureCopyHostFunc` (:577–591), and `cancelCopyHostFunc`'s sync arm (:714–724), bracket
the block/drive with `e.waitablePtr().syncWaiter = true/false` (matching the reference's
assert-guarded set/clear, :776–779). Read-side impact: only `waitable.join`'s trap and the
reference asserts — a spec-conformant fixture can never hit the new trap unless it is
*supposed* to (§9).

### 6.3 Trap-text: "waitable cannot be used synchronously while added to a waitable set"

Suite 4 asserts this exact substring for all 13 cases. Already emitted by the copy
builtins' in-set checks (stream_builtins.go:415, :523, :697 — verified green via
trap-if-transfer-in-waitable-set). Two sites must adopt the phrase:

* `waitableJoinHostFunc`'s sync-waiter trap (async_builtins.go:285–287), currently
  "handle %d has a synchronous subtask.cancel blocked on it" → append/replace with the
  spec phrase (covers `join-during-sync-*`).
* `subtaskCancelHostFuncGraph`'s in-set sync-cancel trap (async_builtins.go:369–371),
  currently "…is joined to a waitable set; a synchronous cancel is not allowed" → the spec
  phrase (covers `sync-subtask-cancel-when-in-set`).

No test or passing-suite manifest pins either old text (verified by grep).

### 6.4 Reap generalization

`reapParkedGoroutines` (stackful_task.go:342–381) gains a third case in its type switch:

```go
case *guestThread:
	in.sched.unpark(v)
	in.threads.remove(v.index)
	v.t.liveThreads--
	if !v.spawned { v.done = true; continue }   // never resumed: nothing to unwind
	v.resumeCh <- resumeAbort                    // block() panics errStackfulAbort ->
	<-v.yieldCh                                  // engine recover -> run returns -> main's final yield
```

Every existing call site (invokeStackful/invokeAsyncCallback error paths,
async_host_import.go:416, `Instance.Close` at instance.go:2005) then reaps spawned threads
with no further changes. This is the leak/hang backstop for suite 4's trap paths (§8.4)
and for any thread left suspended at Close.

---

## 7. -race, hang, and leak arguments

**Single-runnable invariant (-race)**: unchanged in kind, extended in population. The
invariant is "exactly one goroutine in a composition tree executes component-runtime code;
every transfer is an unbuffered channel send/recv pair" (stackful_task.go:21–25). Proof
obligations for the new pieces:

1. `guestThread.block`/`handoff` are the same two-channel rendezvous as
   `stackfulTask.block`/`handoff` — each send has exactly one matching receive, giving the
   same happens-before edges `-race` already accepts for stackful/segments.
2. `resumeInline` (yield-then-resume's direct switch) runs `other.resumeReady()` on the
   *current baton holder's* goroutine; that goroutine then blocks inside the handoff
   (`<-other.yieldCh`) for the entire time `other` runs. At no instant do two goroutines
   run guest or runtime code: holder → (send) → other → (send) → holder → (send to driver)
   → driver. Each arrow is a channel operation.
3. `in.activeThread`, the thread table, `t.liveThreads`, and all park fields are only
   touched by the baton holder (writes in `run`/`block`/`resumeReady`/the builtins, all of
   which execute holding the baton by construction). Same discipline — and same informal
   proof — as `activeTask`/`exclusiveHeld` today.
4. Lazy spawn: `go th.main()` happens-before the first `resumeCh` send (same goroutine),
   and `main`'s first action is `<-resumeCh` — the identical startup handshake
   stackfulTask.firstRun/main already use (:104–125).

**No hangs**: every new park is (a) ready-always (`yield`, yield-then-resume's self-park)
— the driver's very next `step` resumes it; (b) predicate-driven (a spawned thread's sync
copy) — woken by the same event plumbing that wakes every existing park; or (c)
suspended-forever (`suspend`, a never-resumed new-indirect thread) — *excluded* from
`ready()`, so `drive` correctly returns `errAsyncDeadlock` (→ the spec's deadlock trap
text, async_lift.go:154) instead of spinning, and `drainReady` terminates (no progress).
`resumeInline` cannot recurse unboundedly: a switch chain distributes across goroutines
(each link's frames live on a different parked goroutine), never stacking on one.

**No leaks**: a spawned thread ends in exactly one of: (i) thread func returns →
unregistered in `run`'s tail, goroutine exits through `main`'s deferred final yield; (ii)
thread func traps → same path with the error surfaced on whoever resumed it; (iii) parked
at invoke-error/Close → `reapParkedGoroutines` (§6.4) aborts it and *waits for the final
yield*, so reap does not return until the goroutine has fully unwound — the same
already-proven contract as stackful reap (:329–341); (iv) never-spawned → dropped with no
goroutine to unwind. A thread that outlives a *successful* invoke stays parked-suspended
(zero CPU) until Close reaps it — the same policy `drainReady`'s doc already establishes
for permanently-unready tasks (sched.go:159–161).

---

## 8. Per-suite walkthroughs (mechanized traces)

### 8.1 sync-barges-in (thread.yield only)

`yielder` (async-opts stackful): $D.run (stackfulTask goroutine) → `yielder'` async lower
→ `startStackfulExportTask` → $C st goroutine → core calls `thread.yield` → blk=$C.st →
`st.block(alwaysTrue, false)` parks → wrapper returns STARTED to $D → $D calls `poker`
(sync lift, barges in — existing invoke path, no mayEnter conflict since suspendRun set
mayEnter=true) → $D `wait-for-return` → waitable-set.wait parks $D → driver `step`:
$C.st ready (alwaysTrue) → resumes → `task.return(unblock-value)` → core returns →
asyncOpts epilogue OK (state==RESOLVED, stackful_task.go:184–195) → SUBTASK/RETURNED event
→ $D resumes → asserts 94/96/… ✓. `yielder-cb` uses the packed YIELD code — the existing
`parkYield` path, no builtin. `blocker`/`blocker-cb`/`unblocker`/dtor legs are entirely
existing machinery (waitable-set.wait blk arm, parkWait, sync barge-in, resource dtors).

### 8.2 cancellable (thread.yield cancellable + promotion gate)

Test 2 (`yield-cancel`): $D.run (stackful) → `yield-cancel'` async lower →
startAsyncExportTask → **promoted** gt (gate §5.4) → firstRun on segment →
`thread.yield-cancellable` → `gt.block(alwaysTrue, true)` parks parkBlocked → STARTED+
subtask to $D → $D `subtask.cancel` → `requestCancellation` (task.go:224–239): parkBlocked
∧ cancellable ∧ ¬heldByOther (owner is t itself) ∧ mayEnter → CANCEL_DELIVERED,
`seg.handoff(resumeCancelled)` → block returns true → core sees 1 (CANCELLED) →
`task.cancel` → resolved-cancelled → EXIT → subtask.cancel returns
CANCELLED_BEFORE_RETURNED(4) ✓. Determinism: $D's goroutine holds the baton continuously
between the STARTED return and the cancel — no scheduler step can resume the ready-waiting
yielder early.

Test 4 (`yield-cancel-pending`): wait **without** cancellable parks; cancel →
PENDING_CANCEL, BLOCKED to $D ✓; future.write completes the read; $D's own wait parks $D;
driver resumes $C (real FUTURE_READ event) → wait returns 4 ✓ → `thread.yield-cancellable`
→ `deliverPendingCancel(true)` fires **at the prologue** (never parks) → returns 1 ✓ →
task.cancel → EXIT → $D's wait sees SUBTASK/4 ✓. Tests 1/3 are the same shapes through
waitable-set.wait-cancellable / poll-cancellable — existing builtins, newly reachable via
promotion.

### 8.3 trap-if-block-and-sync (binds + existing traps + reword)

15 asserts: 12 ride existing machinery the moment bind succeeds — `trap-if-sync-call-
async1/2` (host_import.go:677), `trap-if-async-calls-sync-and-blocks` (callee is a
syncImplicit sync lift whose wait hits async_builtins.go:68), `trap-if-wait`,
`poll-is-fine` (syncImplicit active task ✓), sync stream/future/cancel traps, and
`trap-if-sync-cancel`. New: `trap-if-suspend` (§4.2 sync trap), `yield-is-fine` (§4.1 sync
arm, returns 0 → 42), `yield-is-fine-cb` (existing parkYield), `trap-if-invalid-callback-
code` ×3 (§6.1 reword), and `yield-to/switch-to/resume-later-is-fine` (return 42 without
calling anything — need thread.index/new-indirect to **bind** only).

### 8.4 trap-if-sync-and-waitable-set (the real fiber suite)

`join-during-sync-future-read`: host → invokeStackful($Main.run…) → st goroutine →
`future.new` → `thread.new-indirect(0, rx)` → guestThread th (suspended, index 1,
table-func `$thread-future-read-sync` resolved via LookupFunction) →
`thread.yield-then-resume(1)`: resolve th, suspended ✓ → `resumeInline`: spawn th's
goroutine, baton to th → th.run: enterRun + activeThread=th → core `future.read-sync(rx)`
→ copy blocks (no writer) → sync arm: **syncWaiter=true** (§6.2) → `activeBlocker` →
**th** (§5.2) → `th.block(hasPendingEvent, false)` parks th, baton back to st's goroutine
→ st parks self ready-always, baton to driver → driver `step`: th not ready (no event), st
ready → resume st → `waitable.join(rx, ws)` → `syncWaiter` trap with the spec phrase
(§6.3) → panic unwinds st's guest frames → invokeStackful error path →
`reapParkedGoroutines` aborts th (errStackfulAbort through the engine's recover, final
yield) → no goroutine leaks, `assert_trap` text matches ✓. The write/stream variants are
the same trace through the other three table slots.

`sync-*-when-in-set` (8 tests): in-set checks fire *before* any block
(stream_builtins.go:415/:523/:697) — existing, no threads involved (spawn happens only in
the join-during tests). `sync-subtask-cancel-when-in-set`: blocks-forever ($C callback
WAIT parks — existing), join subtask into set, sync `subtask.cancel` → §6.3 trap ✓; $C's
parked task reaped on the error path (existing case in reap).

---

## 9. Non-regression census (which passing suites' code paths are touched)

| change | touched shared path | passing suites on that path | why safe |
|---|---|---|---|
| new `computeCanonHostFunc` cases | bind-time switch | all | pure additions to kinds that previously **failed bind**; unreachable for components not binding them |
| `blockingTask`/`activeBlocker` prefer `activeThread` | every blocking builtin | deadlock, empty-wait, dont-block-start, partial-stream-copies, wait-during-callback, cancel-subtask, all stream suites | `activeThread` is nil unless a spawned thread holds the baton → identical evaluation; one nil-check added |
| `mayBlockSync` gate widening | promotion decision in startAsyncExportTask | none | fires only when a thread canon binds; none of the 26 binds one |
| `syncWaiter` around sync copy blocks | streamCopy/futureCopy/cancelCopy sync arms | partial-stream-copies (promoted-segment sync copies), cross-abi-calls | writes a flag read only by waitable.join's trap + subtask.cancel's assert path; a currently-green fixture triggering the new trap would have to join a sync-blocked end into a set — reference/wasmtime trap that, so a green (wasmtime-passing) fixture cannot contain it |
| join / subtask.cancel trap-text rewords | trap strings | none pin them (grepped tests + manifests) | substring assertions only exist in suite 4 |
| "unsupported callback code" reword | unpackCallbackResult | any suite with an invalid packed code — none passing | one pinned unit test updated (async_first_light_test.go:150) |
| reap `*guestThread` case | reapParkedGoroutines | all invoke error paths + Close | added case matches nothing until threads exist |
| `task.liveThreads`, `Instance.threads/activeThread`, lazy thread.index slot | struct fields | all | inert fields; the lazy slot allocates only on a thread.index call, impossible without binding 0x26 |

Specifically for the brief's named stackful suites — partial-stream-copies, deadlock,
empty-wait, dont-block-start: their drive/park/trap paths (`sched.step`, `stackfulTask.*`,
`blockingTask`'s syncImplicit trap, `errAsyncDeadlock` mapping) receive **zero semantic
edits**; the only instruction-level deltas they execute are the nil `activeThread` check
and (partial-stream-copies only) two boolean stores per blocked sync copy. Gate: run the
full async conformance + oracle + `-race` suite after every stage (§12).

---

## 10. Oracle & test coverage

**Trace oracle** (gen_async_oracle.py + async_oracle_test.go): the harness's determinism
pin #3 asserts every `random.choice` candidate set is a **singleton** (gen_async_oracle.py
`_singleton_choice`) — currently guaranteed by "every scenario has exactly ONE guest
thread". Plan:

* **Add oracle scenarios for thread.yield** (both cancellable variants + pending-cancel
  delivery + yield-under-PENDING_CANCEL-non-cancellable): op `"thread.yield"
  {cancellable}` maps 1:1 to `ref.canon_thread_yield` on the Python side and the real
  builtin on the Go side. Singleton pin survives: a yielded thread is the only ready
  candidate in these scenarios. This is cheap and covers the cancel-interplay matrix that
  cancellable asserts end-to-end.
* **thread.suspend**'s trap edge (sync ctx) and cancellable-wake: suspend never resumes
  without resume-later (out of scope), so the *Python* driver would deadlock its scenario
  thread; keep suspend out of the oracle and cover it with direct Go unit tests (sync-trap
  text, cancellable wake via requestCancellation, abort/reap).
* **thread.new-indirect / yield-then-resume**: the oracle harness has no core-table
  concept on either side (Python would need synthesized `CoreFuncRef`s and the Go replay a
  compiled table module — disproportionate). Cover via (a) the .wast suite itself (real
  bytecode, real table), (b) Go unit tests: spawn+switch happy path, bad index / freed
  slot / not-suspended traps, type-mismatch trap through LookupFunction, spawned-thread
  guest trap propagating through resumeInline, last-thread-unresolved trap, reap of
  spawned/unspawned threads at Close and at invoke-error, thread.index from a spawned
  thread and from an implicit thread (lazy slot alloc/free). Flagged as the honest
  oracle-coverage gap; the golden file needs regeneration only for the added yield
  scenarios.

Per the standing coverage gate (≥90% + every fail-loud branch tested): each fail-loud arm
in §4 ("not supported from an unpromoted callback task", table-lookup traps, bind-time
signature rejections, the last-thread trap) gets an explicit test in the stage that
introduces it.

---

## 11. Reference ambiguities & deliberate deviations (single-threaded host)

1. **Scheduling nondeterminism** (`random.choice` in `Store.tick`/canon_lift's sync loop):
   wazy pins deterministic first-ready-in-registration-order — the established equivalent
   of the deterministic profile (sched.go:33–36). Spawned threads inherit it by parking in
   the same list.
2. **`thread.yield` from a sync task**: the reference *allows* it (the sync driver loop
   :2202–2206 always finds the ready yielder; no trap) — note this contradicts a naive
   "sync tasks may never block" reading; wasmtime agrees (yield-is-fine asserts 42 while
   trap-if-suspend/wait trap). wazy realizes it as one `pumpSnapshot` round on the driving
   goroutine — behaviorally the same "everyone ready runs once, then I continue".
3. **yield-then-resume ordering**: switch-then-park-self instead of park-self-then-switch
   (§4.5) — proven observation-equivalent; required because the parker *is* the goroutine
   that must perform the switch.
4. **Implicit-thread registration**: lazy (on first thread.index) instead of eager at
   `enter_implicit_thread` (:502) — index *values* are unobservable in every vendored
   suite (new-indirect's return feeds yield-then-resume directly; nothing compares indices
   across threads), and eager registration would touch every task entry path. Revisit if a
   future suite calls resume-later/switch-to on an implicit thread's index.
5. **`request_cancellation` candidates** (:534–541): still targets only the implicit
   thread (task.go's existing arms); spawned threads are never cancellation candidates.
   Unexercised: no suite cancels a task that has spawned threads. A future resume-later
   suite would need the candidate scan; the `guestThread.cancelWake`/`ready()` plumbing is
   already shaped for it.
6. **new-indirect's `ft` payload**: validated against the consumer's core import signature
   (bind) + LookupFunction's typeID (call) instead of decoding the raw core-type section
   (decoder.go:121–125) — equal for every validated component; the call-time check is the
   reference's `trap_if(f.t != ft)` verbatim. Hardening option (decode core functypes)
   documented but not required.
7. **Implicit-thread exit while spawned threads live**: the reference defers the
   unresolved-task trap to the *last* thread's exit (:521–523); wazy's stackful/callback
   epilogues still check at implicit exit. Unexercised (every suite 4 export traps before
   returning; no other suite spawns). The spawned-thread side of the reference trap IS
   implemented (`run`'s last-thread check, §3).
8. **`thread.suspend`/`yield-then-resume` from an unpromoted callback task**: fail-loud
   panic instead of a nested-drive emulation — a frame-held caller genuinely cannot
   suspend-forever without deadlocking its own driver; no suite reaches it.

---

## 12. Landing order — staged increments (each independently green, Sonnet-sized)

Ranking easiest → hardest: **sync-barges-in → cancellable → trap-if-block-and-sync →
trap-if-sync-and-waitable-set.** Canon-dependency note: suites 1+2 need only 0x0c (with
2 adding the promotion gate); suite 3 adds 0x26/0x27/0x29 as bind-mostly; suite 4 is the
only one needing 0x27 execution + 0x2b — so it is cleanly separable as the final
increment.

**Stage A — thread.yield + un-skip sync-barges-in** (~150 LOC + tests)
1. `thread.go`: `threadYieldHostFunc` (§4.1, all three arms) — no guestThread yet.
2. graph.go: `case binary.CanonKindThreadYield` (+ `syncTaskNeeded`/`mayBlockSync` sets).
3. Remove sync-barges-in's skipReason; run suite (§8.1 predicts pass; debug residuals —
   the blocker/unblocker legs are first-time-executed compositions of existing machinery).
4. Unit tests: yield from stackful/sync/task-less; oracle scenarios for yield (§10) +
   golden regen. Full `go test ./... -race` + conformance/oracle non-regression.

**Stage B — promotion gate + un-skip cancellable** (~20 LOC + tests)
1. Verify the gate set in Stage A promotes $C; remove cancellable's skipReason.
2. Walk the 4 tests (§8.2); expected new-code surface is zero beyond Stage A — failures
   here indicate Feature-1 edges (parkBlocked cancel interplay), fix in place.
3. Unit tests: yield-cancellable delivered + pending; promoted-wait STARTED/cancel dance.

**Stage C — binds for 0x26/0x29/0x27 + reword + un-skip trap-if-block-and-sync** (~250 LOC)
1. `threadSuspendHostFunc` (§4.2), `threadIndexHostFunc` + lazy implicit slot +
   `threadTable` type (§4.3 — the table lands here, threads land in D).
2. `thread.new-indirect` **bind path**: `coreTableTarget` threading, table/typeID
   resolution, signature validation; call body = the full spawn (§4.4) *if* Stage D's
   guestThread is folded in, else a fail-loud "execution lands in the next increment"
   panic (suite 3 never calls it; the fail-loud arm gets a unit test). Recommended: land
   the fail-loud stub here, keep C small.
3. §6.1 reword + pinned-test update. Remove suite 3's skipReason; 15 asserts green.

**Stage D — guestThread runtime + un-skip trap-if-sync-and-waitable-set** (~350 LOC)
1. `guestThread` (§3) + reap case (§6.4) + `activeThread`/`currentBlocker` seam (§5.2).
2. `thread.new-indirect` execution (§4.4) + `threadYieldThenResumeHostFunc` (§4.5).
3. `syncWaiter` brackets (§6.2) + join/subtask.cancel trap texts (§6.3).
4. Remove suite 4's skipReason; 13 asserts green per §8.4.
5. Unit tests per §10 (spawn/switch/traps/reap/leak: `goleak`-style goroutine-count
   assertions after Close, mirroring existing stackful reap tests); full `-race` matrix +
   the standing cross-arch/qemu lanes.

Each stage ends with: full async+sync conformance run (expect monotone pass-count: 27, 28,
29, 30 of 31), oracle diff clean, `go test ./... -race` clean, no goroutine leaks.

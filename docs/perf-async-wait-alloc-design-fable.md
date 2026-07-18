# Design: eliminating the remaining async WAIT-path allocations

Branch `perf/component-model-async`. Companion to `docs/perf-async-wait-alloc-brief.md`.
Everything below was re-verified against the working tree on 2026-07-18 (file:line references are
exact as of this commit); the alloc counts were re-measured with `-memprofilerate=1`, which is
load-independent and therefore trustworthy on the loaded dev box. **ns/op numbers in this doc are
NOT trustworthy and are never used to justify a decision.**

---

## 0. Verified baseline: what the 7 allocs/op actually are

```
BenchmarkCallAsyncFirstLight-20    806.9 ns/op   330 B/op   1 allocs/op
BenchmarkCallAsyncAwaitImport-20  1946   ns/op   941 B/op   7 allocs/op
```

`-memprofilerate=1` profile of `BenchmarkCallAsyncAwaitImport` (5000 iterations; every row below
is 5001 ± 1 objects ⇒ exactly 1 alloc/op each, except the first which is 2):

| # | flat objs | site | what allocates |
|---|-----------|------|----------------|
| 1+2 | 10002 | `buildAsyncHostWrapper.func1` (async_host_import.go) | the `st.applyResolve = func(...)` closure (**:288**) AND `ac := &AsyncCall{...}` (**:435**) |
| 3 | 5001 | `newSubtask` (waitable.go:309, inlined) | the `subtask` struct itself — **the `lenders: []*resourceEntry{}` literal is 0 bytes (runtime zerobase), it is NOT an allocation** (measured: 1 obj/op, not 2) |
| 4 | 5001 | `waitableSetNewHostFunc.func11` (async_builtins.go:149) | `ws := &waitableSet{}` |
| 5 | 5001 | `invokeAsyncCallback` (async_lift.go:97) | `fr := &callbackFrame{}` (~290 B: task+guestTask) |
| 6 | 5001 | `BenchmarkCallAsyncAwaitImport.func1` | **benchmark host-fn code**: the `call.Defer(func(){...})` closure capturing `call` |
| 7 | 5001 | `BenchmarkCallAsyncAwaitImport.func1.1` | **benchmark host-fn code**: the `[]abi.Value{uint32(99)}` results slice (the `uint32(99)` interface box itself is free — <256 static box) |

Two consequences worth stating up front:

- **Only 5 of the 7 are runtime allocations.** Rows 6–7 live in the user's `AsyncHostFunc`; no
  runtime change removes them without an API addition (see Appendix A). The runtime floor for this
  benchmark is therefore **2 user allocs + whatever we cannot remove below**.
- **The brief's site 6 (`onCancel` closure) contributes 0 allocs to this benchmark** (the
  benchmark host fn never calls `OnCancel`, and the guest<->guest arm isn't exercised). It is
  still worth destructuring — it removes 1–2 allocs/call on the guest<->guest async-lower path
  and 1 on any host import that registers `OnCancel` — but it will not move this benchmark.

One more structural fact that constrains everything below: **the benchmark guest leaks its table
entries by design.** `testdata/async/await_import.wat`'s `run` calls `waitable-set.new` on every
invocation and its callback returns EXIT without ever calling `subtask.drop` or
`waitable-set.drop`. Both entries stay reachable from `in.resources.entries` forever. Any scheme
whose "definitely-done" point is a guest `drop` builtin therefore *cannot* reduce this benchmark's
allocs/op — the objects are genuinely live. This kills naive pooling for sites 3 and 4 as a
benchmark win and reframes them honestly (below).

---

## 1. Site-by-site design

### Site 1 — `st.applyResolve` closure (async_host_import.go:288) → **(a) destructure to fields**

**Technique: closure-destructuring, no pool.** This is the same move as the just-landed
closure-free `pendingSub` (waitable.go:56-63): split the closure's captures into a bind-time
config (allocated once per import, in `buildAsyncHostWrapper`'s bind-time body, *outside* the
`api.GoModuleFunc` per-call closure) and per-call fields on the `subtask` that already exists.

**Verified capture inventory** of the closure at :288-323:

| captured var | bind-time or per-call | goes where |
|---|---|---|
| `iface`, `funcName` | bind-time | cfg |
| `resultCount` | bind-time (buildHostResultPlan, :235) | cfg |
| `resultType` | bind-time | cfg |
| `resultUsesMem` | bind-time | cfg |
| `resolve` (`abi.Resolver`, passed to `abi.Store` :318) | bind-time | cfg |
| `resources` (`*handleTable`, for `allocHandleResult` :300) | bind-time | cfg |
| `reallocOverride` (`api.Function`, :314) | bind-time | cfg |
| `retPtr` (decoded from `stack[outPtrIdx]`, :284-287) | **per-call** | subtask field |
| `memMod` (`mod` or `memOverride`, :268-271) | **per-call** | subtask field |
| `ctx` (used by `reallocOf(ctx, memMod)` :313 / `reallocOfFunc(ctx, ...)` :315) | **per-call** | subtask field |
| `st` | per-call | the receiver itself |

**New types/fields:**

```go
// asyncResolveCfg is the bind-time half of a host-import subtask's resolve:
// one per async-lowered import binding, built once in buildAsyncHostWrapper
// (amortized), shared by every call through that binding. Immutable after bind.
type asyncResolveCfg struct {
    iface, funcName string
    resultCount     int
    resultType      binary.TypeDesc
    resultUsesMem   bool
    resolve         abi.Resolver
    resources       *handleTable
    reallocOverride api.Function
}
```

On `subtask` (waitable.go), the `applyResolve func(results []abi.Value) error` **field** is
replaced by:

```go
// resolveFn is a test-injection / future-alternate-source override for
// applyResolve; nil in production (the resolveCfg path is used). Checked first.
resolveFn  func(results []abi.Value) error
resolveCfg *asyncResolveCfg // bind-time half; set per call by buildAsyncHostWrapper
retPtr     uint32           // per-call: the trailing out-pointer, decoded at call time
memMod     api.Module       // per-call: the CALLING module (or memOverride)
resolveCtx context.Context  // per-call: for reallocOf at resolve time
```

**The method replacing the closure** (both call sites — `AsyncCall.Resolve` at :83 and the
guest<->guest `onResolve` at :374 — keep the exact spelling `st.applyResolve(vals)`; it becomes a
method call instead of a field call, a textually-invisible change at the call sites):

```go
// applyResolve replaces the per-call closure formerly assigned in
// buildAsyncHostWrapper: lower vals into the calling module's memory through
// the retptr captured at call time and flip state to RETURNED. Behavior is
// byte-identical to the old closure body; mem/realloc are still fetched fresh
// here, not at call time (memory may have grown since).
func (st *subtask) applyResolve(vals []abi.Value) error {
    if st.resolveFn != nil {
        return st.resolveFn(vals)
    }
    cfg := st.resolveCfg
    if cfg == nil {
        panic("BUG: applyResolve on a subtask with no resolve source")
    }
    // ... the exact :289-322 body, with resultCount→cfg.resultCount,
    // memMod→st.memMod, resources→cfg.resources, resultType→cfg.resultType,
    // resultUsesMem→cfg.resultUsesMem, ctx→st.resolveCtx,
    // reallocOverride→cfg.reallocOverride, retPtr→st.retPtr, resolve→cfg.resolve.
}
```

The per-call wiring in the wrapper (replacing :284-323) becomes four scalar/pointer stores, zero
allocations:

```go
st.resolveCfg, st.memMod, st.resolveCtx = cfg, memMod, ctx
if outPtrIdx >= 0 {
    st.retPtr = api.DecodeU32(stack[outPtrIdx])
}
```

**Single-assignment-site proof.** `grep -rn "applyResolve" internal/component/instance/*.go`
(non-test) shows exactly one assignment (async_host_import.go:288) and two calls (:83, :374). Both
arms of the wrapper (plain host import and guest<->guest) share that one assignment — it runs
before the arms split at :325. So a single method works; no variants exist. The three *test*
assignments (async_host_import_test.go:304, :312, :319) become `st.resolveFn = ...` mechanically.

**Correctness / -race argument.** No lifetime changes at all: the closure was reachable exactly as
long as `st`; the fields are reachable exactly as long as `st`. `memMod`/`resolveCtx` were kept
alive by the closure before and are kept alive by the fields now — identical GC behavior. All
reads/writes of the new fields happen where the closure's captures were read/written: under the
composition tree's single-runnable baton (wrapper invocation, `AsyncCall.Resolve` inside a call or
a scheduler thunk, the guest<->guest `onResolve` inside the scheduler). Nothing is freed, nothing
is reused; there is no use-after-free surface. `-race` sees strictly fewer heap objects and the
same happens-before edges.

**Fail-loud coverage** (per the project's coverage gate): the new `panic("BUG: ...")` branch and
the `resolveFn`-priority branch each need one unit test.

**Benchmark effect: 7 → 6 allocs/op** (and roughly −100 B/op — the closure carried ~11 captures).

---

### Site 2 — `ac := &AsyncCall{...}` (async_host_import.go:435) → **(c) eliminate by co-allocation into `subtask`**

**Technique: embed, don't pool.** Pooling `AsyncCall` is *impossible to prove safe*: it is handed
to arbitrary user code (`hi.asyncFn(ctx, args, ac)`, :441) which may legally retain it forever —
`Defer` (resolve later), or simply storing it. There is **no definitely-done point**: even after
`Resolve`, the host may keep the pointer (a second `Resolve` panics *by reading `ac.resolved`* —
which only works if `ac` is still that call's object; a recycled+reset `ac` would silently
corrupt an unrelated later call instead of panicking). So (b) is out. But (c) is clean: every
`AsyncCall` is created alongside exactly one `subtask` that the `AsyncCall` already points to
(`ac.st`), and the host retaining `ac` already keeps `st` reachable. Making the `AsyncCall` a
*field of the subtask* merges the two allocations without changing any object's reachable
lifetime by even one instruction:

```go
// on subtask (waitable.go):
//   ac is the AsyncCall handed to a plain host import's AsyncHostFunc,
//   co-allocated here so the (subtask, AsyncCall) pair costs one malloc.
//   Unused (zero) for a guest<->guest subtask (the tgt arm returns before
//   the AsyncCall would be built) and for test-built subtasks.
ac AsyncCall
```

and at :435:

```go
ac := &st.ac
ac.in, ac.st, ac.inCall = in, st, true
```

**Also fold out the redundant `subtaski` field.** `AsyncCall.subtaski` (:53) duplicates
`subtask.si`: the only production write (:456 `ac.subtaski = subtaski`) is immediately adjacent to
`st.setSubtaski(subtaski)` (:457) with the same value, and both start 0. Delete the field;
`installParkedPendingEventIfNeeded` (:149-155) reads `st.subtaski()` instead:

```go
func (ac *AsyncCall) installParkedPendingEventIfNeeded() {
    if ac.inCall {
        return // the wrapper's epilogue sees st.resolved() and takes the immediate path
    }
    st := ac.st
    st.setPendingSubtaskEvent(st, st.subtaski())
}
```

Semantics are identical on both paths: parked (si set at :457 before any deferred Resolve can
run — the Defer thunk runs only once the wrapper has returned and the scheduler is driven) and
immediate (si==0, but `installParkedPendingEventIfNeeded` early-returns on `inCall` anyway;
`ResolveCancelled`'s parked arm likewise only runs when parked ⇒ si != 0).

**Correctness / -race argument.** `&st.ac` escapes ⇒ `st` escapes — but `st` escaped already
(table entry, closure-free pendingSub, Defer thunk captures). The cycle `st.ac.st == st` is a
plain GC cycle, handled fine. The host holding `ac` used to keep `{ac, st}` alive; now it keeps
the single merged object alive — strictly the same reachability set. All `ac` field accesses
happen under the baton exactly as before (in-call, or inside a scheduler thunk — enforced by the
existing `pumping` guard at :75). Nothing about `-race` changes.

**Test churn (mechanical):** the 9 `&AsyncCall{...}` literals in async_host_import_test.go keep
working (the type and its remaining fields are unchanged; standalone construction is still fine —
embedding does not forbid it). Only :366's `subtaski: 5` literal becomes `st.setSubtaski(5)`.

**Benchmark effect: 6 → 5 allocs/op** (−48 B/op).

---

### Site 3 — `newSubtask()`'s `&subtask{...}` (waitable.go:309) → **(d) leave, with reasons**

**The brief's sub-question first:** the `lenders: []*resourceEntry{}` literal is **already free**
— a zero-length slice literal points at `runtime.zerobase` and mallocs nothing (confirmed by the
profile: `newSubtask` is exactly 1 object/op, and by the reference semantics: `lenders == nil` is
the load-bearing `resolveDelivered()` sentinel per definitions.py:895-899, so it *must not* start
nil; happily the empty non-nil literal costs nothing, so there is nothing to change and nothing
to gain. Do not "optimize" this to nil — it would make a fresh subtask read as delivered
(newSubtask's own doc, waitable.go:305-308).

**Why not pool the subtask itself:**

1. **No definitely-done point exists for the general case.** The candidate points all fail:
   - *`deliverResolve`*: far too early — the subtask is still in the handle table (the guest holds
     the handle and may `subtask.cancel` it (trap path reads fields), re-poll it, or `drop` it),
     and after Stage B the host's retained `AsyncCall` **is** this object.
   - *`subtask.drop` (async_builtins.go:345-360)*: the guest-controlled retirement, and the table
     side is indeed done there — but the **host** may still hold `ac` (== `&st.ac`) indefinitely;
     nothing in the API obligates the host to drop its reference when the guest drops the handle.
     A recycled subtask under a live host `ac` turns the one-shot `Resolve`-twice panic into
     silent corruption of an unrelated in-flight call — exactly the use-after-free class the
     brief warns about. `AsyncCall` would need a documented "invalid after the guest drops the
     subtask" contract, which is unverifiable and hostile.
2. **Even where it is safe, it pays nothing here**: the benchmark guest never calls
   `subtask.drop` (see §0) — the object is genuinely live, so allocs/op cannot go down.
3. After Stages 1+2 the subtask is the *one* consolidated per-import-call allocation (~200 B
   carrying what used to be three objects). One malloc per async import call, freed by GC when
   the guest's handle and the host's `ac` are both gone, is the right shape: correct by
   construction, zero reset logic, zero -race surface.

**Optional micro-follow-up (not staged):** an inline `lendersBuf [1]*resourceEntry` (the
`waitableSet.elemsBuf` pattern, waitable.go:149-156) would remove the first `addLender` append
allocation for single-borrow imports. The benchmark lends nothing; do this only if a borrow-heavy
profile ever shows it.

---

### Site 4 — `&waitableSet{}` (async_builtins.go:149) → **(d) leave now; pool design recorded for later**

**Why leave:** this allocation is guest-driven (`waitable-set.new`) and its only sound
definitely-done point is guest-driven (`waitable-set.drop`). The benchmark guest **never drops**
(§0) — it news a set per call and leaks it — so pooling cannot reduce this benchmark's allocs/op
by even one. Real wit-bindgen executors create one set and reuse it across waits, so the per-call
cost in realistic guests is already ~zero. Pooling here adds reset/race surface for a win that is
unmeasurable now on every workload we have. Leave it.

**If it is ever pooled** (recorded so the analysis isn't redone): the definitely-done point is
inside `waitableSetDropHostFunc` (async_builtins.go:291-306) *after* `ws.dropSet()` succeeds and
`in.resources.removeEntry(h)` returns. At that point, single-ownership is provable:
`dropSet` has enforced `len(elems)==0` (⇒ no `waitable.wset` back-pointer names this set — join
keeps the two sides consistent, waitable.go:123-131) and `numWaiting==0` (⇒ no frame is inside a
`sched.drive`/`blk.block` whose predicate captures this set — both wait paths bracket
`numWaiting++/--` around the suspension, async_builtins.go:231-233/244-246 and guest_task.go:443 /
resumeReady:342; a parked `gt.wset` reference likewise implies numWaiting>0 ⇒ trap, so no parked
task can name it), and `removeEntry` has retired the guest handle. Reset must set
`elemsBuf[0]=nil; elems=elemsBuf[:0]; numWaiting=0` — note `elems` must be re-pointed **into the
recycled object's own** `elemsBuf`, mirroring waitableSetNewHostFunc:150. Cross-P `sync.Pool`
migration is fine: the object carries no references out and none in at Put time.

---

### Site 5 — `&callbackFrame{}` (async_lift.go:97) → **(b) sync.Pool + Reset, host-entry frames only — with honest accounting**

This is the highest-frequency object (1/call on *every* async export call, the FirstLight
remaining 1) and the only site where a pool is genuinely the right tool. Three hard constraints
shape the design:

**(i) Only `invokeAsyncCallback`'s host-entry frames are poolable.** The frames built by
`startAsyncExportTask` (async_lift.go:214) hand out `t.requestCancellation` as the caller's
`subtask.on_cancel` (:242) and wire `t.onResolve`/`gt.onStart` into the caller's subtask — the
caller's subtask (host- or guest-held, arbitrary lifetime, see Site 3) keeps referencing the
frame's `task` after EXIT. No done-point exists there. Host-entry frames have
`onResolve == onStart == finish == nil` by construction (async_lift.go:96-102; `gt.finish` is
never assigned anywhere in production — verified) and are never promoted (`gt.promoted` set only
in startAsyncExportTask, :237) ⇒ `gt.seg == nil` always.

**(ii) The returned result aliases the frame — it must be copied out before Put.** `task.return`
writes `t.resultBuf[0] = val; result = t.resultBuf[:1]` (async_builtins.go:554-555/:572-573), and
`invokeAsyncCallback` returns `t.result` (:128) straight through `invoke` → `Call` → the user.
resultBuf's own doc (task.go:83-88) says the aliasing is safe *because a task is never reused* —
pooling breaks exactly that premise, so before Put the result must be copied to a fresh slice.
**Honest accounting:** for a non-empty result this converts the ~290 B frame alloc into a 16 B
1-element slice alloc — **allocs/op unchanged, B/op −~270**; for a void async export it is a
clean −1 alloc. This is also exactly the sync path's shape (`liftResult` returns a fresh
`[]abi.Value{val}` per call, instance.go:1884/:1916), i.e. the pool brings async result handling
to sync-path parity rather than below it. The *latency* value of trading one 290 B malloc for one
16 B malloc + a pool round-trip is precisely what cannot be measured on this box — this stage's
value (not correctness) is gated on a quiet-machine wall-clock (§3).

**(iii) The definitely-done point and its gates.** Put happens at exactly one site: the tail of
`invokeAsyncCallback`'s **full-success path** (after the `drainReady` at :124 returns nil, just
before returning), and nowhere else — every error path (`gt.start` failure, drive error,
`gt.err`, drainReady error) leaks the frame to the GC on purpose (cold; `reapParkedGoroutines`
and stranded state make unreachability unprovable there). At the success point:

- `gt.done` is true and gt is **not** in `sched.parked`: `done` is set only in `finishExit`/`fail`
  and (parkEntry-cancel) `resumeReady`, each of which runs either inline (never parked) or from
  `resumeReady` arms that `unpark` first (guest_task.go:327/348/358/366).
- `sched.runq` is empty and no parked task is ready: that is `drainReady`'s postcondition
  (`step` returned progressed=false ⇒ runq empty, sched.go:106-131) — no queued thunk can
  capture the frame. (Defer thunks capture `ac`/`st`, never the frame, but emptiness closes the
  class.)
- `in.activeTask`/`in.exclusiveOwner` were cleared by the final `leaveRun` (task.go:327-331);
  `in.syncBase` is never this task (it is never syncImplicit).
- Borrow scopes: `task.return`/`task.cancel` trap unless `numBorrows == 0` (task.go:151/186), so
  a resolved task has no live `resourceEntry.borrowScope` pointing at it.
- **Two residual-reference gates must be checked explicitly** (both cheap field reads):
  - `t.implicitThreadIdx == 0` — `thread.index` registers an `implicitThreadMarker{t: t}` in
    `in.threads` (thread.go:366-369) that is **never removed** on task resolve; a frame so
    registered must not be pooled.
  - `t.liveThreads == 0` — a spawned guestThread's `th.t` reference outlives EXIT if the thread
    is still parked (threads legitimately persist across invokes; thread.go:309-315 decrements
    only when the thread itself finishes).

**The code shape:**

```go
var callbackFramePool = sync.Pool{New: func() any { return &callbackFrame{} }}

// in invokeAsyncCallback, replacing `fr := &callbackFrame{}` (:97):
fr := callbackFramePool.Get().(*callbackFrame)
t, gt := &fr.t, &fr.gt
t.inst, t.be = in, be
gt.t, gt.in, gt.be, gt.ctx, gt.exportName = t, in, be, ctx, exportName
gt.args = args
t.gt = gt

// replacing `return t.result, nil` (:128):
res := t.result
if len(res) > 0 { // aliases fr.t.resultBuf (task.return's only write path) — detach before reuse
    out := make([]abi.Value, len(res))
    copy(out, res)
    res = out
}
if t.implicitThreadIdx == 0 && t.liveThreads == 0 {
    *fr = callbackFrame{} // Reset: one memclr wipes BOTH structs — see below
    callbackFramePool.Put(fr)
}
return res, nil
```

**What Reset must clear — everything, so clear everything.** `*fr = callbackFrame{}` memclrs the
whole ~290 B in one instruction-sequence and is the only reviewable Reset policy; enumerated, the
stale state it wipes and why each matters for the *next* call: `t.state` (a recycled frame must
start `taskInitial`, else `tryEnter`/`task.return` misbehave), `t.ctxStorage` (spec: context slots
start zeroed — leaking slot values across calls is guest-visible), `t.result`/`t.resultBuf`
(retains arbitrary lifted values — memory leak + aliasing hazard), `t.numBorrows`, `t.cancelled`,
`t.onResolve`, `t.implicitThreadIdx`, `t.liveThreads`, `t.st`; `gt.park`/`gt.wset`/`gt.cancellable`/
`gt.cancelWake` (stale park state would let `ready()` read a dead waitableSet), `gt.onStart`,
`gt.args` (retains the previous caller's arg values), `gt.done`/`gt.err`/`gt.finish`,
`gt.promoted`/`gt.seg`, `gt.ctx`/`gt.exportName`/`gt.be`/`gt.in`/`gt.t`. The Get site re-links
`t.gt = gt` and re-fills the live fields.

**-race / cross-P argument.** Every access to the frame between Get and Put happens while the
accessing goroutine holds the composition tree's baton (single-runnable invariant: the invoke
caller's goroutine through `gt.start`/`drive`/`drainReady`, or the scheduler-driven resumes on
that same goroutine). Get and Put both execute on the invoke caller's goroutine, with Put
sequenced after every frame access of that call (drive/drainReady have returned; the result was
copied out). `sync.Pool` itself is race-instrumented (its Put→Get pair establishes the
happens-before edge the recycled memory needs), and cross-P migration hands the frame to a
goroutine that, by the gates above, is the *only* holder. Two different composition trees on two
goroutines exchanging frames through the pool is therefore indistinguishable from a fresh malloc
under -race and under the memory model.

**Mandatory verification for this stage** (correctness provable now, all load-independent):
`TestAsyncWastConformance` 31/31, `TestAsyncOracle`, the full package under `-race`, **plus a new
stress test**: N goroutines × M instances each looping `Call("run-async")` on both fixtures
(await + first-light) under `-race -count=5`, with a goroutine-count check after Close (the
package's existing reap tests' pattern) to catch leaked segments; and one test each pinning the
two gates (a guest that calls `thread.index` / spawns a thread must NOT recycle — observable via
a package-internal counter or by asserting `implicitThreadIdx != 0` behavior stays correct across
two calls). **Wall-clock value check on a quiet machine required before trusting the ns/op win**
(B/op improvement is verifiable now: AwaitImport ~600→~330 B/op, FirstLight 330→~16 B/op).

---

### Site 6 — `subtask.onCancel` closures → **(a) destructure to two typed fields**

**Verified assignment sites (exactly two in production):**

1. `AsyncCall.OnCancel` (async_host_import.go:103): `ac.st.onCancel = func() error { fn(); return nil }`
   — a per-call wrapper closure whose only job is adding an `error` return to the user's `func()`.
2. The guest<->guest arm (:388): `st.onCancel = onCancel` where `onCancel` is
   `t.requestCancellation` returned by `startAsyncExportTask` (:242) / `startStackfulExportTask`
   (:277) — a **method-value allocation** on every guest<->guest async lower.

Consumer: `subtaskCancelHostFuncGraph` only (async_builtins.go:428/:448).

**Replace the `onCancel func() error` field with:**

```go
// onCancelHook: AsyncCall.OnCancel's user fn, stored directly (no error-adapting
// wrapper closure). Mutually exclusive with cancelTask.
onCancelHook func()
// cancelTask: the guest<->guest callee's task; runOnCancel calls its
// requestCancellation directly (no method-value closure).
cancelTask *task
```

with methods:

```go
// hasOnCancel replaces the `st.onCancel != nil` gate (async_builtins.go:428).
func (st *subtask) hasOnCancel() bool { return st.cancelTask != nil || st.onCancelHook != nil }

// runOnCancel is Subtask.on_cancel's invocation (canon_subtask_cancel ~2470):
// runs synchronously on the driving goroutine under the sched.pumping bracket
// its caller already holds.
func (st *subtask) runOnCancel() error {
    if st.cancelTask != nil {
        return st.cancelTask.requestCancellation()
    }
    st.onCancelHook()
    return nil
}
```

**Signature change:** `startAsyncExportTask` and `startStackfulExportTask` return
`(calleeTask *task, err error)` instead of `(onCancel func() error, err error)`; :388 becomes
`st.cancelTask = calleeTask`. Ripple is small and mechanical: graph.go:1040 and
startDelegatedFromBlocker (graph.go:1037-1040 area) already discard the value; tests
stackful_task_test.go:80/:184/:202 and async_cancel_test.go:166/:179 change `onCancel()` to
`calleeTask.requestCancellation()`; guest_guest_async_test.go:255's injected
`st.onCancel = func() error {...}` (whose body returns nil anyway) becomes
`st.onCancelHook = func() {...}`; async_host_import_test.go:327-338 asserts via
`hasOnCancel()`/`runOnCancel()`.

**Correctness argument.** The dispatch is decided by which field is set, and exactly one ever is:
`OnCancel`'s doc requires it be called before the AsyncHostFunc returns (plain-host arm only);
`cancelTask` is set only in the tgt arm — the arms are exclusive (:325 vs :422). Guard order in
`subtaskCancelHostFuncGraph` is untouched (`resolved()` checked before `hasOnCancel()`, :426-428),
so `requestCancellation`'s `panic("BUG: ... resolved task")` (task.go:289) remains unreachable
exactly as the reference's guard makes it. The `sched.pumping` bracket around the invocation
(:447-449) is unchanged — the documented synchronous-`Resolve`-from-OnCancel shape keeps working.
No lifetimes change: the closure held {fn} or {t}; the fields hold fn or t. Nothing is freed or
reused. `-race`: same accesses, same baton, fewer objects.

**Benchmark effect: none** (0 occurrences on this path — see §0). Real effect: −1 alloc per
`OnCancel`-registering host import call, −1 per guest<->guest async lower (the method value),
verifiable by allocs/op on `TestGuestGuestAsyncLower*`-shaped micro-benchmarks if we ever add one.

---

## 2. Floor analysis: where this lands

| after stage | AwaitImport allocs/op | composition |
|---|---|---|
| baseline | 7 | closure + AsyncCall + subtask + wset + frame + 2 user |
| S1 (applyResolve) | 6 | AsyncCall + subtask + wset + frame + 2 user |
| S2 (AsyncCall embed) | 5 | subtask(+ac) + wset + frame + 2 user |
| S3 (onCancel) | 5 | unchanged here (wins elsewhere) |
| S4 (frame pool) | 5 | subtask(+ac) + wset + **result-copy** + 2 user (B/op −~270; FirstLight B/op 330→~16) |

The remaining 5 are each individually accounted for: **subtask+ac** — the one legitimate
per-import-call object (host- and guest-referenced, §1.3); **waitableSet** — guest-leaked by this
fixture, ~free in real guests (§1.4); **result copy** — sync-path parity, the price of a safe
frame pool (§1.5.ii); **2 user allocs** — the benchmark host fn's own `Defer` closure and results
slice, untouchable without the Appendix A API. A void-result async export with a well-behaved
guest (drops its handles, reuses its set) lands at **1 runtime alloc/op** (the subtask).

---

## 3. Staged landing order

Safest-and-most-verifiable-now first; pooling last. Every stage independently keeps
`TestAsyncWastConformance` 31/31, `TestAsyncOracle`, and the full package `-race` green, and each
is a Sonnet-sized diff touching a small, named file set. Standard per-stage verification recipe
(all load-independent — run from repo root, never `-short`):

```
go test ./internal/component/instance/ -run 'TestAsyncWastConformance|TestAsyncOracle' -count=1
go test -race ./internal/component/instance/ -count=1
go test ./internal/component/instance/ -run '^$' -bench 'BenchmarkCallAsync' -benchtime 2000x -benchmem
go vet ./internal/component/instance/
```

**Stage 1 — applyResolve destructure (§1.1).** Files: async_host_import.go (cfg type + method
wiring), waitable.go (field swap + method), async_host_import_test.go (3 lines → `resolveFn`, +2
new fail-loud tests). Acceptance: AwaitImport **7 → 6 allocs/op** exactly; no other benchmark
moves by more than noise in B/op. Value: proven by allocs/op alone — no wall-clock needed.
Touches sched/park core: **no**.

**Stage 2 — AsyncCall co-allocation + subtaski removal (§1.2).** Files: async_host_import.go
(:435-456 rewiring, `installParkedPendingEventIfNeeded`), waitable.go (embed field),
async_host_import_test.go (:366 only). Acceptance: **6 → 5 allocs/op**. Value: allocs/op alone.
Touches sched/park core: **no**.

**Stage 3 — onCancel destructure (§1.6).** Files: waitable.go, async_host_import.go (:103, :388),
async_lift.go (two signatures), graph.go (:1040 spelling), async_builtins.go (:428/:448 →
`hasOnCancel`/`runOnCancel`), 4 test files mechanically. Acceptance: benchmark unchanged (5);
the full cancel suite (`TestAsyncOracle` cancel scenarios, async_cancel_test.go,
guest_guest_async_test.go, stackful_task_test.go, wast conformance's cancel suites) is the real
gate. Value: correctness-neutral refactor with off-benchmark alloc wins; no wall-clock needed.
Touches sched/park core: **no** (it calls `requestCancellation` exactly as the method value did).
Landed third because its blast radius is Phase-3 cancellation, the subtlest semantics in the
package — after 1-2 have proven the field-destructuring pattern on this same struct.

**Stage 4 — callbackFrame pool (§1.5).** Files: async_lift.go (Get/copy-out/gates/Put),
plus the new stress + gate tests. Acceptance: allocs/op **stays 5** (this is expected — say so in
the commit message), AwaitImport B/op drops ~270, FirstLight goes 330 B → ~16 B/op; `-race`
stress green, goroutine-leak check green. **VALUE GATE: flag the ns/op claim as unverified until
a quiet-machine wall-clock run** (load < ~2, `uptime` + `ps` checked per the benchmark-load
discipline) — if the quiet-machine run shows the pool round-trip + copy costing more than the
saved malloc, this stage is a one-file revert that loses nothing else. Touches sched/park core:
**no code changes to sched.go**, but its correctness *argument* leans on sched invariants
(done ⇒ unparked; drainReady ⇒ empty runq) — those two invariants are restated as comments at
the Put site so a future sched change trips over them deliberately.

**Not staged (documented leaves):** Site 3 (subtask — §1.3) and Site 4 (waitableSet — §1.4).
Revisit only with a workload where guests actually drop (site 4's pool design above is then
ready) or a borrow-heavy profile (site 3's lendersBuf).

---

## 4. Appendix A — the last two allocs (user code; optional future API, out of scope)

The benchmark host fn pays `call.Defer(func(){ call.Resolve(vals) })` = 1 closure + 1 slice. A
`func (ac *AsyncCall) DeferResolve(results ...abi.Value)` that enqueues a dedicated
`schedThunk` arm `{ac *AsyncCall; vals []abi.Value}` (sched.go's schedThunk already takes the
"plain fields, no wrapper closure" approach — :57-76) would remove the closure (the variadic
slice remains, at the call site). Net −1 for converted hosts. This changes public API and would
also tempt rewriting the benchmark — which must NOT be done to flatter the numbers; if added,
benchmark both spellings side by side. Not part of this design's stages.

## 5. Appendix B — invariants this design must not outlive

- **Single-runnable baton**: one goroutine executes component-runtime code per composition tree
  at any instant; every design argument above ("all accesses under the baton") cites it. If
  CallAsync's truly-external completion ever lands, Stage 4's Put-site argument must be re-proven
  (external completion could enqueue against a tree whose invoke has NOT returned — fine — but
  any future path that lets a frame's task be referenced after `drainReady` returns nil breaks
  the gate analysis).
- **task.return traps on numBorrows>0** (task.go:151): load-bearing for Stage 4's "no borrowScope
  references a resolved task".
- **`lenders == nil` is `resolveDelivered`** (waitable.go:341): load-bearing for Site 3's
  "never start lenders nil".
- **`gt.finish` is production-unassigned**: if a future feature assigns it on host-entry frames,
  Stage 4's shape gate must add `gt.finish == nil` (cheap; add it preemptively in the
  implementation).

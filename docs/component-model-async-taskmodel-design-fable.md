# Design: the last two TASK-MODEL async .wast suites (25/31 → toward 31/31)

Branch `feat/component-model-async`. Targets the two remaining task-model suites:
`big-interleaving-test` (Suite A) and `trap-on-reenter` (Suite B), currently skipped at
`internal/component/instance/wast_async_conformance_test.go:310` and `:335`.

**Headline findings, up front:**

1. **The two suites do NOT share a mechanism.** The brief hypothesized both need changes to the
   shared task/re-entrancy model. After tracing every case against the fixtures and the reference,
   Suite A is an *implicit-sync-task* change (current_task() must resolve for every call) and
   Suite B is a *static lineage guard* on delegated composition imports. They touch disjoint code,
   land independently, in either order, with no shared struct or flag.
2. **The brief's premise for Suite B is partially wrong, and this matters enormously for risk.**
   The reference's `entering_set`/ancestor-exclusion model (definitions.py:220-248) **cannot
   produce the traps `trap-on-reenter.wast` asserts** — two of its three cases contain *no async
   construct at all* (testdata/wast-async/trap-on-reenter/trap-on-reenter.wast:68-86 and :89-110
   are pure sync lifts/lowers), so no "outstanding not-yet-resolved async task" state can ever
   trap them, and the ancestor-exclusion arithmetic makes all three cases *vacuously allowed*
   (§2.1 shows the set computations). The suite pins **wasmtime's stricter "for now" behavior**
   (the .wast's own comments: "also, for now, trap on parent-to-child" / "child-to-parent"):
   *any direct static parent↔child call traps*. All three cases — including the async one — are
   direct-lineage calls, so one small call-time guard passes the whole suite. The brief's
   "instance-level outstanding-task mayEnter held across the park" design is **not needed, would
   not pass the suite by itself, and is the riskiest possible shape** (it would trap same-instance
   second entries that today correctly *park*, §2.6). This design does not build it.
3. **Risk ranking (reversed from the brief):** Suite B is now the LOWER-risk change (a trap on
   two binder arms that are provably dead in every currently-passing test); Suite A is the
   moderate-risk one (it touches `invokeEntered`, the hottest sync call path — mitigated by a
   bind-time gate that leaves sync-only components byte-identical). Recommended landing order:
   **B first, then A.**

---

## 1. Suite A — big-interleaving-test: the implicit sync task

### 1.1 Root cause and spec grounding

23/45 `assert_return`s fail with `"called outside an active async task"` (from
`requireActiveTask`, async_builtins.go:31-40). `$Driver.run`, and `$Testee`'s `poll`,
`poll-readable`, `call-import`, `mock-bp-inc/dec`, `stream-new`, `testee-write/read`, … (
big-interleaving-test.wast:405-429, :772-773) are **plain sync canon lifts** dispatching via
`invoke → invokeEntered` (instance.go:1530-1573, :1619) with `in.activeTask == nil`. Their core
code calls `waitable-set.poll`, `backpressure.inc/dec` (via `$Mock`'s sync `bp-inc`/`bp-dec`
lifts, wast:100-101/141-142), and `context.get/set` — all legal from a sync call per the spec.

Reference grounding — a Task/Thread exists for **every** call, regardless of the async option:

- `canon_lift` (definitions.py:2144-2202) constructs `task = Task(ft, opts, inst, …)` and
  `thread = Thread(task, thread_func)` unconditionally — the `if not opts.async_:` sync arm
  (:2155-2164) runs *inside* that task's implicit thread.
- `current_task()` (definitions.py:315-316) is `current_thread().task` — it always resolves.
- `Thread.__init__` (definitions.py:343-354) sets `self.storage = [0,0]` — context storage is
  **per-thread, fresh per call**. So wazy's fresh zeroed `ctxStorage [2]uint64` per sync call is
  exactly spec-correct (answering the brief's question): `canon_context_get/set`
  (definitions.py:2337-2358) read `current_thread().storage`, and a new sync call is a new Thread.

wazy only builds a `*task` in `invokeAsyncCallback` (async_lift.go:84-89), `invokeStackful`
(:140-145), and `startAsyncExportTask`/`startStackfulExportTask` (:201/:249) — never for the sync
`invokeEntered` path. That is the entire gap.

### 1.2 The design

Four pieces. All small; each has an identity argument for existing suites.

#### (a) Bind-time gate: `Instance.syncTaskNeeded`

Do **not** install a task on every sync call. `instance.go`'s pool doc (instance.go:79-86)
explicitly supports **concurrent `Call`/`CallExport` against the same Instance** on the sync path;
an unconditional `in.activeTask` write in `invokeEntered` would be a brand-new data race on a
documented-supported pattern (`-race` regression), plus one allocation per call on the hottest
path (real_* WASI components, `benchmarks/vs-wazero`).

Instead, add to the async-state block of `Instance` (instance.go, after `mayBlockSync` ~:311):

```go
// syncTaskNeeded is set at graph-bind time when this instance wires any
// canon builtin that resolves the CURRENT TASK at call time (task.return,
// task.cancel, context.get/set, backpressure.inc/dec, waitable-set.wait/poll
// -- the requireActiveTask/blockingTask family, async_builtins.go). Only
// then does invokeEntered install an implicit sync task (the reference's
// canon_lift creates a Task for EVERY call, definitions.py:2144-2202;
// current_task() always resolves, :315-316). False for every component with
// no such canon -- the sync call path stays byte-identical (no activeTask
// write, no allocation, concurrent sync Call stays race-free).
syncTaskNeeded bool

// syncBase is the innermost implicit sync task currently beneath this
// instance's call stack (nil when none). leaveRun/suspendRun restore
// activeTask to it instead of nil, so an async bracket that runs NESTED
// under a sync call (a nested sched.drive resuming one of THIS instance's
// parked tasks) cannot strand the sync caller task-less. Always nil for
// every pre-implicit-task flow, making the restore a no-op there.
syncBase *task
```

Set `in.syncTaskNeeded = true` as the first line of each of these constructors in
`async_builtins.go` (all take `in *Instance` and run once at bind time, wired only from
`computeCanonHostFunc`'s switch, graph.go:1419-1462):

- `taskCancelHostFuncGraph` (:325), `taskReturnHostFuncGraph` (:445),
- `contextGetHostFuncGraph` (:541), `contextSetHostFuncGraph` (:560),
- `backpressureIncHostFuncGraph` (:582), `backpressureDecHostFuncGraph` (:593),
- `waitableSetWaitHostFunc` (:168), `waitableSetPollHostFunc` (:227).

(8 one-line additions. Builtins that only use `requireMayLeave` — waitable-set.new/drop,
waitable.join, subtask.drop/cancel, streams/futures, error-context — do not read `activeTask`
and do not set the flag; instances wiring only those keep the byte-identical sync path.)

In big-interleaving-test: `$Testee` is flagged (wires waitable-set.poll/wait + task.return,
wast:377-380), `$Mock` is flagged (task.return/cancel, context.get/set, backpressure.inc/dec,
wast:105-110), `$Driver` is **not** flagged (its `$DM` imports only lowered funcs, no builtin
canons, wast:507-538) — which is fine because `run`'s own core never calls a builtin directly.

#### (b) The implicit task shape: `task.syncImplicit`

Add one field to `task` (task.go:30-74):

```go
// syncImplicit marks the implicit Task of a PLAIN SYNC canon lift
// (invokeEntered): the reference's Task for a not-opts.async_ lift
// (definitions.py:2155-2164). It has no gt/st (blocker() == nil), never
// enters the scheduler, and is torn down when invokeEntered returns. Its
// ONE semantic difference from a callback task between segments: it may
// NEVER block -- blockingTask traps on it (the spec's sync-task-block trap,
// "cannot block a synchronous task before returning") instead of taking
// the nested-drive arm.
syncImplicit bool
```

Constructed in `invokeEntered` as `&task{inst: in, be: be, state: taskStarted, syncImplicit: true}`:

- `be` **is set** (the sync lift's own boundExport): this makes `taskCancelHostFuncGraph`'s
  existing guard (`t.be != nil && !t.be.asyncCallback && !(stackful && asyncOpts)`,
  async_builtins.go:335) trap `task.cancel` from a sync export with the correct
  "called on a non-async task" — the reference's `trap_if(not task.opts.async_)`
  (definitions.py:2396) — with zero new code.
- `state: taskStarted`: `deliverPendingCancel` (task.go:129) then always returns false (poll's
  cancellable prologue is inert, correct — nothing can request cancellation of a sync call:
  there is no subtask handle for it), and `requestCancellation`'s `panic("resolved")` arm is
  unreachable (nothing ever calls it on an implicit task — `t.requestCancellation` is only
  handed out by `startAsyncExportTask`/`startStackfulExportTask`/`invokeAsyncCallback`'s flows,
  async_lift.go:229/:264, never by `invokeEntered`).
- `gt`/`st` nil ⇒ `blocker()` (task.go:88-96) returns nil — load-bearing, see (d).
- `ctxStorage` zeroed fresh per call — spec-correct per §1.1.
- `onResolve` nil, `numBorrows` untouched. The cross-instance borrow-scope task built by
  `delegatingHostImport` (graph.go:930-932, 953-955) is deliberately **left separate**: it is
  threaded through `repToProviderHandle` as a scope object and enforced by the delegate, not by
  the callee's own exit; merging it into the implicit task would change the borrow-enforcement
  point for every currently-passing resource-crossing suite for zero conformance gain.

#### (c) Install/teardown in `invokeEntered` + the nested-bracket restore

`instance.go:1619` — insert at the very top of `invokeEntered`, *before* `lowerParams`
(the reference runs `on_start`/lowering with the task already current, definitions.py:2148-2151;
guest `cabi_realloc` runs during lowering and may legally call `context.get`):

```go
func (in *Instance) invokeEntered(ctx context.Context, be *boundExport, exportName string, args []abi.Value) ([]abi.Value, error) {
	if in.syncTaskNeeded {
		t := &task{inst: in, be: be, state: taskStarted, syncImplicit: true}
		prevActive, prevBase := in.activeTask, in.syncBase
		in.activeTask, in.syncBase = t, t
		defer func() { in.activeTask, in.syncBase = prevActive, prevBase }()
	}
	// ... existing body unchanged (mem/realloc, lowerParams, CallWithStack,
	// liftResult, post-return; both poisoning sites unchanged) ...
```

- **Nested guest→guest sync calls**: save/restore handles same-instance re-entry
  (`invokeEntered → delegatingHostImport.fn → invoke → invokeEntered`); cross-instance nesting
  is naturally separate (each instance has its own `activeTask`/`syncBase`).
- **Trap cleanup**: the `defer` runs on every return including the two poisoning returns
  (:1668, :1695) and on panics unwinding through (host-import builtin panics degrade to
  `CallWithStack` errors inside the engine before reaching here, but the defer covers both).
- **Instantiation-time start functions stay task-less** (confirmed): start functions run inside
  `r.InstantiateModule` during `instantiateGraph`, never through `invoke`/`invokeEntered` —
  verified by call-graph: `invokeEntered`'s only callers are `invoke` (instance.go:1572) and
  nothing else; `invoke`'s callers are `Call`/`CallExport` and `delegatingHostImport.fn`
  (graph.go:945). A start fn that *delegates* to a sibling's sync export gives the **callee**
  an implicit task — which is spec-correct (the reference callee lift creates its Task even when
  the caller is a start function); the start fn's own instance stays `activeTask == nil`, so
  both `dont-block-start` traps keep firing (§1.6).

**The nested-bracket restore** — change `leaveRun`/`suspendRun` (task.go:284-301) from
`in.activeTask = nil` to `in.activeTask = in.syncBase`:

```go
func (in *Instance) leaveRun() {
	in.exclusiveHeld, in.exclusiveOwner = false, nil
	in.activeTask = in.syncBase // nil pre-implicit-task: identical to the old `= nil`
	in.mayEnter = true
}

func (in *Instance) suspendRun() {
	in.activeTask = in.syncBase // ditto
	in.mayEnter = true
}
```

Why: if a flagged instance's sync export reaches a **nested drive** (a blocking builtin or a
sync-lowered-async-callee `hi.fn` → `sched.drive`), the shared scheduler may resume one of *this
same instance's* parked async tasks; its `enterRun`/`leaveRun` bracket would otherwise null
`activeTask` and strand the still-on-stack sync export task-less, re-creating the very trap this
suite fixes on a later builtin call. In big-interleaving specifically this interleaving is
unreachable ($Testee's stream/future/subtask canons are all `async`-opt ⇒ BLOCKED-sentinel, no
nested drive under a flagged sync export), but the restore closes the hazard class for 3 lines.
`enterRun`/`tryEnter` are untouched (they overwrite `activeTask` by design; `tryEnter` setting
`activeTask` before `firstRunBody`'s `enterRun` is why a save-in-enterRun scheme would be wrong —
it would capture the task itself as "prev").

**Identity argument for the restore**: `syncBase` is written *only* by flagged instances'
`invokeEntered`. For every currently-passing suite and every async flow, `syncBase == nil`
always, so `leaveRun`/`suspendRun` compile to exactly their old behavior. This is the only
change forced on already-committed sched/task core, and it is provably inert until Suite A's
flag exists (flagging requirement from the brief: yes, this touches committed `task.go` core —
two lines, nil-identical).

#### (d) The blocking discriminator — the critical non-regression

`blockingTask` (async_builtins.go:65-71) is the **only** trap site for "a task context that may
not block" and the only place that must now distinguish three shapes:

| context | today | after |
|---|---|---|
| `t == nil` (start fn, or unflagged sync lift) | trap "cannot block a synchronous task before returning" | unchanged |
| `t != nil && t.syncImplicit` | (unreachable today) | **same trap** |
| `t != nil`, callback task between segments (`blocker()==nil`) | nested `sched.drive` | unchanged |
| `t != nil`, live blocker (stackful / promoted segment) | `blk.block` park | unchanged |

```go
func blockingTask(in *Instance, builtin string) (t *task, blk taskBlocker) {
	t = in.activeTask
	if t == nil || t.syncImplicit { // the spec's sync-task-block trap
		panic(fmt.Errorf("component/instance: %s: cannot block a synchronous task before returning", builtin))
	}
	return t, t.blocker()
}
```

**Complete enumeration of every reader of `in.activeTask` and how it behaves with an implicit
task installed** (the brief's "verify nothing else reads activeTask on a sync path"):

1. `requireActiveTask` (async_builtins.go:35) — now resolves ✓ (the point of the change).
   Users: task.return (+ new guard below), task.cancel (be-guard traps ✓), context.get/set
   (fresh storage ✓), backpressure.inc/dec (instance counter ✓), waitable-set.poll
   (non-blocking, `deliverPendingCancel` inert ✓).
2. `blockingTask` (:66) — waitable-set.wait only; traps via the new `syncImplicit` arm ✓.
3. `activeBlocker` (:86) — returns `t.blocker()` = nil for an implicit task ⇒ **identical to
   today's `t == nil` nil-return**. Users keep their existing task-less-OK nested-drive arm
   bit-for-bit: subtask.cancel's sync wait (async_builtins.go:412-416), stream/future sync
   copy waits (stream_builtins.go:493-495, :583-585, :720-722). Deliberate deviation from the
   eventual spec (a sync task's sync copy "blocks the thread"): the skipped
   `trap-if-block-and-sync` suite wants dedicated traps here; that is that suite's own future
   change, not this one — changing it now would alter `sync-streams`-adjacent behavior for a
   suite that is skipped anyway.
4. `host_import.go:660` (`in.sched.instantiating && in.activeTask == nil`) — `in` is the
   *caller's* instance; start functions never get a task (see (c)) ⇒ branch unchanged for
   `dont-block-start` case 2 ✓.
5. `host_import.go:673` (`blockingCaller = t.blocker()`) — implicit task's blocker is nil ⇒
   `blockingCaller` stays nil ⇒ `hi.fn` nested drive, same as today's `t == nil` ✓.
6. `stream_host.go:35` (`requireSchedulable`: `in.activeTask != nil && !pumping && inHostCall==0`
   ⇒ panic) — during a flagged instance's sync call, a host-import Go callback's stream ops set
   `inHostCall > 0` ✓ allowed as today. The only newly-panicking case is a *different goroutine*
   calling Stream/Future host APIs mid-sync-call on a flagged instance — already a data race on
   the handle table, now caught loudly instead of silently. No existing test does this (host
   stream ops in tests run between invokes or inside host imports).
7. `enterRun/leaveRun/suspendRun/tryEnter` (task.go) — write sites, covered by (c).

**Also add the reference's `trap_if(not task.opts.async_)` to `task.return`**
(async_builtins.go:475, inside the fn after `requireActiveTask`; canon_task_return,
definitions.py:2383):

```go
if t.syncImplicit {
	panic(fmt.Errorf("component/instance: task.return: called on a synchronous task"))
}
```

Unreachable today (requireActiveTask trapped every sync context first), so zero regression risk;
without it a sync export calling task.return would silently write a discarded `t.result`.

### 1.3 Files/functions touched (Suite A)

| file | change |
|---|---|
| `internal/component/instance/instance.go` | `Instance.syncTaskNeeded`, `Instance.syncBase` fields (~:311); `invokeEntered` prologue + defer (:1619) |
| `internal/component/instance/task.go` | `task.syncImplicit` field (~:74); `leaveRun`/`suspendRun` restore-to-syncBase (:284-301) |
| `internal/component/instance/async_builtins.go` | 8× `in.syncTaskNeeded = true` constructor one-liners; `blockingTask` discriminator (:66-69); task.return sync guard (~:476) |
| `internal/component/instance/wast_async_conformance_test.go` | replace `:310`'s skip entry with `{name: "big-interleaving-test"}` |

No changes to sched.go, guest_task.go, stackful_task.go, async_lift.go, composition/graph binder.

### 1.4 -race / hang / leak arguments

- **-race**: all new writes (`activeTask`, `syncBase`) happen on the goroutine executing
  `invokeEntered`, exactly where the handle-table writes of the very builtins that need the task
  already happen; the single-runnable invariant is untouched (an implicit task never enters
  `sched`, never parks, has no goroutine, and `leaveRun`'s restore happens inside existing
  baton-serialized brackets). Unflagged instances: zero new shared-state writes ⇒ concurrent
  sync `Call` stays exactly as race-free as today.
- **hang**: the implicit task cannot block (blockingTask traps) and never drives; every nested
  drive it can transitively reach is one that exists today with `t == nil`.
- **leak**: no goroutine is ever created for an implicit task; `reapParkedGoroutines` paths
  unchanged.

### 1.5 Non-regression, per affected currently-passing suite

- **dont-block-start** — case 1: a start fn calling waitable-set.wait; start fns never pass
  through invokeEntered ⇒ `t == nil` ⇒ same trap text. Case 2: host_import.go:660 branch keys on
  the *caller's* nil activeTask during instantiation, unchanged. ✓
- **empty-wait**, **deadlock** — their `$D.run`/`$D.f` are *async-typed* lifts (stackful path,
  `invokeStackful`), which never reaches `invokeEntered`; their waits use the stackful blocker
  arm. Additionally `deadlock`'s `$D` wires waitable-set.wait ⇒ gets flagged, but its entry is
  stackful so `invokeEntered` never runs there. ✓
- **wait-during-callback** — callback task's wait: `t != nil`, `syncImplicit` false ⇒ nested
  drive arm byte-identical. ✓
- **builtin-trap-poisons-instance** — poisoning sites in `invokeEntered` untouched; the new
  defer only restores activeTask. ✓
- **async-calls-sync / cancel-subtask / partial-stream-copies / cross-abi-calls / streams &
  futures suites** — all flow through async lifts + delegations whose activeTask discipline is
  the enterRun/leaveRun brackets; the only semantic change there is leaveRun's `= syncBase`,
  which is `nil` throughout these suites (no flagged instance ever sits in `invokeEntered`
  beneath them in these fixtures — and even where one did, restoring the caller's implicit task
  is strictly more correct). ✓
- **Sync wast/resources harness, real_resource_test, all real_* WASI tests, benchmarks** —
  none of these components declare any of the 8 flagged canon kinds ⇒ `syncTaskNeeded` false ⇒
  `invokeEntered` byte-identical, zero allocation delta, concurrent-Call race posture identical. ✓
- **Oracle (TestAsyncOracle)** — the oracle wires builtins via the same constructors (flag gets
  set on its hand-built instances) but drives callback tasks through `invokeAsyncCallback`,
  where activeTask is a real task; the flag only alters `invokeEntered`, which the oracle's
  scenarios do not route builtin calls through. **No golden regeneration.** Unit tests
  (async_builtins_test.go:111-123) construct `Instance{}` directly with nil activeTask and still
  panic as asserted. ✓

### 1.6 Verify plan (Suite A)

1. Un-skip `big-interleaving-test`; `go test ./internal/component/instance/ -run
   'TestAsyncWastConformance/big-interleaving-test' -v` — all 45 assert_returns green.
2. Full async suite: `-run TestAsyncWastConformance` — 25 pass 0 fail, with explicit eyes on
   dont-block-start, empty-wait, deadlock, wait-during-callback, builtin-trap-poisons-instance.
3. `-run TestAsyncOracle`, the sync wast/resources conformance tests, whole repo `go test ./...`,
   `-race`, the no-goroutine-leak checks, lint.
4. **Contingency**: if a subset of the 23 formerly-trapping asserts still fails, the likely
   secondary gap is *drive scheduling on a pure-sync program* (a parked `$Mock`/`$Testee` task
   whose event arrives via a later sync command with no drive point before the next expectation).
   The fix shape would be a `sched.drainReady()` after a *delegated* sync invoke of a flagged
   instance (mirroring async_lift.go:111's quiescence drain) — do NOT add it preemptively; it
   changes observable scheduling order for currently-green suites.

---

## 2. Suite B — trap-on-reenter: the lineage guard

### 2.1 Root-cause re-analysis (this replaces the brief's premise)

Both binder gaps are already fixed (Instantiate succeeds for all 3 `$Parent` variants —
skipReason at wast_async_conformance_test.go:335). The three assertions, from
`testdata/wast-async/trap-on-reenter/trap-on-reenter.wast`:

- **Case 1** (:4-65, async): host invokes `$Parent.c` (callback lift). `c` YIELDs; `c-cb` calls
  `$b` = **async canon lower, declared in $Parent, of nested child `$child`'s export `b`**
  (:56). `b` YIELDs; `b-cb` calls `$a'` = **async canon lower, declared in $Child, of `$a` — the
  outer $Parent's own local lift passed in via `(with "a" (func $a))`** (:36, :45). Expected:
  `"wasm trap: cannot enter component instance"`.
- **Case 2** (:68-86, "for now, trap on parent-to-child"): **pure sync**. `$Parent.g` (sync lift)
  calls `$f` = sync canon lower, in $Parent, of child's sync export `f` (:77). No async
  construct exists in this component at all. Same expected trap.
- **Case 3** (:89-110, "for now, trap on child-to-parent"): **pure sync**. Host invokes the
  aliased `$child.g` (sync lift); `g` calls `$f'` = sync canon lower, in $Child, of `$f` — the
  outer's own local sync lift passed via `(with "f" (func $f))` (:98, :106). Same expected trap.

Two decisive consequences:

1. **No "outstanding unresolved async task" state can pass this suite.** Cases 2 and 3 have no
   task that ever suspends — every call in them is a synchronous lift whose bracket opens and
   closes within one `CallWithStack`. Any design keyed on "mayEnter held false while a task is
   parked" leaves cases 2/3 exactly as un-trapped as today.
2. **The reference's entering_set model doesn't produce these traps either.** With
   `parent` = static nesting ($Child.parent = $Parent) per `ComponentInstance.__init__`
   (definitions.py:208-218) and `entering_set(caller) = callee.self_and_ancestors() −
   caller.self_and_ancestors()` (:236-247):
   - Case 2, Parent→Child: `{Child,Parent} − {Parent} = {Child}`, `Child.may_enter` is true ⇒
     allowed.
   - Case 3, Child→Parent: `{Parent} − {Child,Parent} = ∅` ⇒ vacuously allowed (this is exactly
     the brief's "child calling back into its static parent is ALLOWED" — and the suite says it
     must **trap**).
   - Case 1's inner call, Child→Parent: same `∅` ⇒ vacuously allowed, *regardless* of whether
     `$Parent.c`'s task is parked, because the caller-chain subtraction removes `$Parent` from
     the checked set. Holding `Parent.may_enter` false across the park changes nothing.
   The `Store.lift` bracket (:587-595) and `Store.tick` bracket (:606-616) are both transient in
   the reference too (`leave_to` runs when `canon_lift` returns at the first suspension — the
   fiber suspends *inside* `thread.resume()` at :2202, and canon_lift returns for an async ft) —
   wazy's transient `enterRun/leaveRun` is actually *closer* to the current reference than the
   brief assumed. The suite's traps come from **wasmtime's current implementation limit**, which
   the .wast pins with its own "for now" comments: *direct static parent↔child calls are refused
   outright*. All three cases are direct-lineage calls; every currently-passing composition suite
   (async-calls-sync, cancel-subtask, partial-stream-copies, deadlock, empty-wait, dont-block-
   start, big-interleaving-test) is **sibling-to-sibling only** (children of a plain root that
   has no core modules of its own).

### 2.2 The design: refuse lineage delegates at call time

wazy's binder makes this nearly free, because **the two binder arms that create lineage
delegates are exactly the two arms added (dead-before-then) by the earlier trap-on-reenter
binder fixes**, and no other arm can create one:

- **Child→parent** (cases 1 inner + 3): `outerFuncArgImport`'s ComponentFuncFromCanonLift arm —
  the outer component's *own local lift* passed as a `(with "x" (func N))` instantiate-arg to a
  child (composition.go:209-227; the delegate targets `in`, the outer = the importing child's
  static parent — lineage by construction).
- **Parent→child** (cases 1 outer + 2): `computeCanonHostFunc`'s canon-lower
  ComponentFuncFromAlias branch with `al.InstanceIdx >= numImported` — a canon lower *declared
  in the outer* targeting a locally-instantiated nested child's export (graph.go:1296-1331; the
  caller is the outer's own core modules — lineage by construction).
- Sibling delegation (all passing suites) flows through `arg.Sort == 0x05` (graph.go:827-841)
  and `outerFuncArgImport`'s sibling-alias arm (composition.go:236-253) — untouched.

**Change 1 — flag the delegate.** Add to `hostImport` (host_import.go:152-183):

```go
// lineage marks a composition delegate that crosses a DIRECT static
// parent<->child instantiation boundary: the outer's own local lift handed
// to a child as a func instantiate-arg (outerFuncArgImport's
// ComponentFuncFromCanonLift arm), or the outer's own canon lower of a
// locally-nested child's export (computeCanonHostFunc's local-sibling lower
// arm). The Component Model reference will eventually permit some of these
// (entering_set ancestor exclusion, definitions.py:220-248), but the
// conformance suite pins wasmtime's current behavior -- trap-on-reenter.wast's
// own "for now, trap on parent-to-child / child-to-parent" -- so the call-time
// wrappers refuse them with the spec's exact re-entrancy trap text.
lineage bool
```

Set sites (two one-liners):

- composition.go:227 (after `hi.asyncTarget = …` in the ComponentFuncFromCanonLift arm):
  `hi.lineage = true`
- graph.go:1326 (after `hi = delegatingHostImportDeferred(tgt, sfd, sresolve)`):
  `hi.lineage = true`

**Change 2 — trap at the call boundary.** Both wrappers close over `hi` already:

- `buildHostWrapper`'s fn closure, host_import.go:624 — first statement:

```go
if hi.lineage {
	panic(fmt.Errorf("component/instance: host import %q %q: wasm trap: cannot enter component instance", iface, funcName))
}
```

- `buildAsyncHostWrapper`'s fn closure, async_host_import.go:274 — first statement (before
  `newSubtask`; covers the `hi.asyncTarget` arm at :332, and the plain-host arm can never carry
  the flag):

```go
if hi.lineage {
	panic(fmt.Errorf("component/instance: async lower %q %q: wasm trap: cannot enter component instance", iface, funcName))
}
```

**Trap text**: trap-on-reenter.json expects the literal substring
`"wasm trap: cannot enter component instance"` (all 3 commands; harness matches via
`strings.Contains`, wast_async_conformance_test.go:485). Existing sites emitting the bare
`"cannot enter component instance"` (async_lift.go:74/:137/:199/:247, instance.go:1556) are NOT
touched — builtin-trap-poisons-instance.json pins the bare form and keeps passing. Precedent for
embedding the wasmtime-verbatim phrase at one call site: `wrapUnreachableTrap`
(instance.go:1592-1597) and invokeStackful's deadlock text (async_lift.go:154).

**Why call time, not bind time**: the suite requires `Instantiate` to *succeed* (all three are
`assert_trap` on `invoke`, and the binder fixes that made instantiation succeed are already
landed and gate-verified). A bind-time refusal would regress instantiation back to an error.

### 2.3 Trap-edge traces (all three cases)

- **Case 1**: `invoke "c"` → `invokeAsyncCallback($Parent)` → core `c` returns YIELD → parkYield
  → `sched.drive` → `resumeReady` → `runLoopBody` → `c-cb` calls `$b` → lineage-flagged
  `buildAsyncHostWrapper` panics → engine converts to `callbackFn.CallWithStack` error
  (guest_task.go:388-392) → `gt.fail` (+ `$Parent` poisoned) → drive returns the error →
  `invokeAsyncCallback` error contains the expected substring. Any parked segments are reaped
  (`reapParkedGoroutines`, async_lift.go:96) — no goroutine leak; no hang (the panic happens
  before the callee task is ever started, so nothing new can park).
- **Case 2**: `invoke "g"` → sync `invokeEntered($Parent)` → core `g` calls `$f` →
  lineage-flagged `buildHostWrapper` panics → `CallWithStack` error at instance.go:1666 →
  `$Parent` poisoned (correct; fresh component per assertion) → error contains substring. ✓
- **Case 3**: `invoke "g"` (root alias → `$child.g`, dispatched on `be.home`) →
  `invokeEntered($Child)` → core `g` calls `$f'` → lineage-flagged `buildHostWrapper` panics →
  same path. ✓

### 2.4 Files/functions touched (Suite B)

| file | change |
|---|---|
| `internal/component/instance/host_import.go` | `hostImport.lineage` field (~:182); check at top of buildHostWrapper's fn (:624) |
| `internal/component/instance/async_host_import.go` | check at top of buildAsyncHostWrapper's fn (:274) |
| `internal/component/instance/composition.go` | `hi.lineage = true` in outerFuncArgImport's local-lift arm (:227) |
| `internal/component/instance/graph.go` | `hi.lineage = true` in the local-nested-sibling lower arm (:1326) |
| `internal/component/instance/wast_async_conformance_test.go` | replace `:335`'s skip entry with `{name: "trap-on-reenter"}` |

No changes to task.go / sched.go / guest_task.go / stackful_task.go / async_lift.go. **No
ancestor pointers on `Instance` are needed** — the two arms know the relationship statically at
bind time, which is both simpler and immune to "who is the caller at a lift boundary" questions.
(If a future shape ever needs a *general* lineage test — e.g. an import forwarded two levels
down — the flag already travels with the `hostImport` object through instantiate-arg forwarding,
so transitive consumers of a flagged delegate still trap.)

### 2.5 Non-regression argument

- The two flagged arms were added by the (already-landed) binder fixes for this very suite and
  are documented as **"additive/dead-before-now"** — the exact shapes previously hard-failed
  `Instantiate` ("component instance index 0 out of range…", "func arg index 0 is not an
  instance-export alias"), so **no currently-passing suite or unit test ever instantiates,
  let alone calls through, a lineage delegate** (grep confirms: no test outside
  wast_async_conformance_test.go references these arms; the only fixtures with these shapes are
  trap-on-reenter's three components).
- Sibling arms (graph.go:827-841, composition.go:236-253) and host-import resolution
  (composition.go:255-263) are byte-identical — async-calls-sync, cancel-subtask,
  partial-stream-copies, deadlock, empty-wait, dont-block-start, big-interleaving-test all
  delegate sibling↔sibling only.
- The plain-host-import and WASI wrappers never set `lineage` ⇒ one predictable-false branch per
  host call; no behavior or measurable perf change.
- -race/hang/leak: the trap fires before any task/subtask/goroutine is created; error paths
  reuse the existing reap/poison machinery (§2.3).
- **Oracle**: no scenario builds a lineage composition; no goldens change, no regeneration.

### 2.6 What this design deliberately does NOT build (and why)

The brief's proposed mechanism — instance-level "outstanding not-yet-resolved task" state
holding `mayEnter` false across parks, plus `parent` pointers and entering_set arithmetic — is
rejected on three grounds:

1. **Insufficient**: it cannot trap cases 2/3 (no task ever parks there, §2.1), so the suite
   still fails.
2. **Unfaithful**: the current reference releases the lift bracket at first suspension
   (definitions.py:587-595 + :2200-2207) and asserts all *waiting* threads' instances are
   enterable between ticks (:608) — "may_enter false for the task's entire lifetime" is not what
   the reference does.
3. **Actively dangerous**: wazy gates second entries into a busy instance by
   backpressure/exclusive *parking* (`tryEnter` → parkEntry, task.go:263-270), and
   `invokeAsyncCallback`/`startAsyncExportTask` trap on `!mayEnter`
   (async_lift.go:73/:198/:246). Holding mayEnter false while a task is merely parked would turn
   currently-parking second entries (FIFO fairness, `numWaitingToEnter`) into traps — breaking
   precisely the composition suites the brief warns about, in the opposite direction it
   anticipated.

When the upstream suite is eventually relaxed to the entering_set semantics, the lineage flag is
the natural seam: replace the unconditional trap with the ancestor-set check at the same two
call sites, adding `Instance.parent` then (set in `instantiateNestedInstances`,
graph.go:884-890, where `in` → `sub` is established).

### 2.7 Verify plan (Suite B)

1. Un-skip; `go test ./internal/component/instance/ -run
   'TestAsyncWastConformance/trap-on-reenter' -v` — 3/3 assert_traps green.
2. Full `TestAsyncWastConformance` (especially async-calls-sync, cancel-subtask,
   partial-stream-copies, deadlock, big-interleaving if A landed first) + oracle + sync
   wast/resources + whole-repo + `-race` + leak + lint.

---

## 3. Risk ranking and landing order

| | Suite B (lineage guard) | Suite A (implicit sync task) |
|---|---|---|
| blast radius | 2 dead-until-now binder arms + 1 flag | hottest sync call path, task core (2 nil-identical lines), 1 builtin trap site |
| new state | 1 bool on hostImport | 2 fields on Instance, 1 on task |
| failure mode if wrong | a lineage call that should work traps (no such call exists in any passing test) | perf/alloc delta or race on sync path (gated off for all sync-only components), wrong trap for callback-task waits (single discriminator, table-verified) |
| spec fidelity | matches the suite (wasmtime "for now"), diverges from eventual entering_set — documented seam | matches the reference exactly (Task per call, per-thread storage) |

**Land B first** (smallest, independently verifiable), then A. Both are individually gate-able:
each un-skips exactly one suite and can be reverted independently.

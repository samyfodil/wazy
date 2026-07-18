# Design brief: the last two TASK-MODEL async .wast suites (toward 31/31)

wazy = wazero-derived Go WebAssembly runtime, single-threaded host, no reflection/WithFunc.
Component Model async runtime is complete; official async `.wast` is at **24/31, 0 fail**.
Working dir `/home/samy/Documents/wazy`, branch `feat/component-model-async`.
Reference spec impl: `internal/component/abi/testdata/definitions.py` (cite exact line numbers).

Two remaining suites both need changes to the shared **task / re-entrancy** model. Design BOTH
concretely (Go structs, signatures, control-flow, exact files/functions touched). They may share
one mechanism — say so if they do. Rank by regression risk. Must be implementable by a Sonnet
agent directly and land WITHOUT regressing any currently-passing suite.

## Shared grounding (read these first)
- `internal/component/instance/task.go`: `enterRun`/`leaveRun`/`suspendRun` (mayEnter transient,
  bracketing only each CallWithStack — task.go:272-301), `tryEnter`, `requestCancellation`,
  `hasBackpressure`. `type task` def + `blocker()`.
- `internal/component/instance/async_builtins.go:31-102`: `requireActiveTask` (traps
  "called outside an active async task" when `in.activeTask==nil`), `blockingTask`
  (traps "cannot block a synchronous task" when `t==nil`), `activeBlocker`, `requireMayLeave`.
- `internal/component/instance/instance.go:1530-1700`: `invoke` (branches asyncCallback /
  stackful / sync `invokeEntered`), `invokeEntered` (the SYNC lift body: `CallWithStack` at
  :1666, poisons on trap). `Instance.activeTask/mayEnter/exclusiveHeld/poisoned` fields ~254-322.
- `internal/component/instance/guest_task.go`, `stackful_task.go`: the async task shapes.
- Reference: `definitions.py` — `ComponentInstance.may_enter_from/enter_from/leave_to/
  entering_set/self_and_ancestors` (220-248); `current_task` (315-319); `Task.enter_implicit_thread`
  (485-503), `exit_implicit_thread` (511+), `Task.__init__` (450+); `canon_context_get/set`
  (2337/2347); `canon_lift`/`Store.lift` prologue (the enter_from/leave_to bracket).

## SUITE A — big-interleaving-test (LOWER RISK; do this design first)
Root cause (already root-caused, confirmed): 23/45 assert_returns fail with "called outside an
active async task". `$Driver.run` and several `$Testee` exports (poll, poll-readable, bp-inc, …)
are PLAIN SYNC canon lifts — they dispatch via `invoke → invokeEntered` with NO task at all. When
their core code calls a legal-in-sync async builtin (`waitable-set.poll`, `backpressure.inc/dec`,
`context.get/set`) it hits `requireActiveTask`'s trap because wazy only ever builds a `*task` for
an async-callback / stackful lift, never an IMPLICIT one for a plain sync export. Per the spec a
Task exists for EVERY call regardless of the async option (`current_task()` always resolves).

Design an **implicit sync task** for the sync `invokeEntered` path:
- Where exactly it's constructed/installed as `in.activeTask` and torn down (must survive nested
  guest→guest sync calls: `invokeEntered` can re-enter `invoke`→`invokeEntered` on the same
  Instance — save/restore previous `activeTask`, a stack via defer). Must clean up on trap too.
- What minimal `*task` shape it needs: it has no callback and no stackful goroutine; it supports
  `context.get/set` (task-local storage, fresh per call — is that correct vs the reference's
  per-Task context?), `waitable-set.poll` (non-blocking), `backpressure.inc/dec`. Give the struct.
- **The critical non-regression**: an implicit sync task must STILL trap if it tries to actually
  BLOCK (`waitable-set.wait`, a blocking stream/future copy wait) — this is the spec's
  sync-task-block trap and `dont-block-start`/`empty-wait`/`deadlock` currently PASS relying on it.
  Today that trap fires because `t==nil`; with an implicit task `t!=nil` but `t.blocker()==nil`.
  `blockingTask`/`activeBlocker` and the wait/copy-wait sites currently treat `blk==nil, t!=nil` as
  "callback task between segments → NESTED sched.drive". An implicit sync task must NOT nested-drive
  — it must trap. Design the discriminator (a `task.syncImplicit bool`? a distinct `blocker()`
  return?) and every call site that must branch on it. Enumerate them.
- Must NOT affect: instantiation-time start functions (genuinely task-less — confirm `invokeEntered`
  is never on that path); the async lift paths (they set activeTask via `enterRun`, never reach
  `invokeEntered`); `host_import.go:660`'s `in.sched.instantiating && in.activeTask==nil` branch;
  the entire sync wast/resources harness + real_resource_test (they never call async builtins, so a
  non-nil activeTask must be inert for them — verify nothing else reads activeTask on a sync path).
- Verify plan: un-skip `big-interleaving-test` in `wast_async_conformance_test.go`; all 45
  assert_returns pass; `dont-block-start`/`empty-wait`/`deadlock` still pass; oracle + resources
  .wast + whole-repo + -race + no-leak + lint all green.

## SUITE B — trap-on-reenter (HIGHER RISK)
Both binder gaps are already FIXED (Instantiate succeeds for all 3 `$Parent` variants). The
remaining gap is the re-entrancy guard itself. wazy's `mayEnter` is transient (flips true/false
around each `CallWithStack`, task.go:272-277); the reference holds `may_enter` false for a
callback async task's ENTIRE lifetime — from lift start (`enter_from`) to resolve (`leave_to`),
INCLUDING while the task is PARKED between YIELD/WAIT dispatches. So in wazy, between `$Parent`'s
`c` task yielding and `$Child`'s callback resuming and calling back into `$Parent`'s `$a`,
`mayEnter` has already flipped back to true and the call sails into `$a`'s real core code (which
unreachables — a DIFFERENT trap than the spec's re-entrancy trap the suite asserts).

Design the fix using the reference's exact model:
- The reference brackets with `enter_from(caller)` / `leave_to(caller)` over the task's whole
  lifetime, flipping `may_enter` for `entering_set(caller) = callee.self_and_ancestors() -
  caller.self_and_ancestors()` (definitions.py:226-248). The **ancestor-exclusion** is the crux: a
  nested child synchronously calling back into its own static parent is ALLOWED (parent ∈ caller's
  ancestors → excluded from the flip); only an async, scheduler-tick-mediated re-entry after a
  yield traps. Getting this wrong OVER-traps and breaks currently-passing composition suites
  (async-calls-sync, cancel-subtask, partial-stream-copies, deadlock) that legitimately chain
  calls between nested instances.
- wazy has no `parent`/ancestor pointer on `Instance` today (confirm) — design the minimal
  ancestor tracking (composition already knows the nesting: `subInstances`, `be.home`). What is
  "caller" at a lift boundary and how is it threaded in?
- Design the instance-level "has an outstanding not-yet-resolved async task" state that keeps
  `may_enter` false across the park (independent of the transient CallWithStack bracket), and how
  it composes with the existing `enterRun`/`leaveRun`/`suspendRun` (which currently toggle mayEnter
  every bracket). Does `suspendRun` stop restoring mayEnter for an outstanding-task instance?
- Trap edges + how the trace-oracle (`internal/component/abi/testdata/gen_async_oracle.py` + Go
  replay) covers the new trap; flag any oracle-golden regeneration.

## For EACH suite deliver
Concrete Go (structs/signatures/control-flow); files+functions touched; how it stays `-race`-safe
(single-runnable invariant) and can't hang (deadlock-trap) or leak goroutines; the exact non-regression
argument for each currently-passing suite the change's blast radius touches; where the reference is
genuinely ambiguous or you'd deviate for the single-threaded host. Flag any change forced on
already-committed sched/task/parkedTask core. This must be implementable by a Sonnet agent directly.

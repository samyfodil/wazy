# Design brief: thread.* execution runtime (last 4 async .wast → 30/31)

wazy = wazero-derived Go WebAssembly runtime, SINGLE-THREADED host, no reflection/WithFunc.
Official async `.wast` is at **26/31, 0 fail**. The last 4 skips all need genuine **thread.***
execution (a cooperative fiber runtime), which wazy currently only DECODES (fail-loud at bind).
Working dir `/home/samy/Documents/wazy`, branch `feat/component-model-async`. Reference spec impl:
`internal/component/abi/testdata/definitions.py` (cite exact line numbers).

Design the thread.* execution model concretely (Go structs, signatures, control-flow, files).
This is the hardest remaining feature; be rigorous. Rank the 4 suites by tractability and give a
suite-by-suite landing order. It must land WITHOUT regressing any of the 26 passing suites, stay
`-race`-safe (single-runnable invariant), never hang, never leak goroutines.

## The 4 target suites (currently skipped; decode already works, bind fails "kind 0xNN does not produce a core func")
1. **cancellable** — `$C.yield-cancel`/`yield-cancel-pending` call `thread.yield-cancellable` (0x0c, cancel? payload) from CALLBACK-lifted core, expecting suspend-then-resume with a real CANCELLED vs not return.
2. **sync-barges-in** — `$C.yielder` calls `thread.yield` (0x0c) from a STACKFUL (no-callback) lift, which already has real goroutine suspend/resume.
3. **trap-if-block-and-sync** — uses thread.index/new-indirect/suspend/yield (0x26/0x27/0x29/0x0c); every assert hits bind ("thread.yield ... does not produce a core func").
4. **trap-if-sync-and-waitable-set** — `$Main` uses thread.new-indirect (0x27) + thread.yield-then-resume (0x2b): spawns a concurrent thread to run sync waitable-set-membership probes WHILE an async blocking call is in flight.
Read each fixture under `internal/component/instance/testdata/wast-async/<name>/` AND its current
skipReason in `internal/component/instance/wast_async_conformance_test.go` (they contain the exact
bind-time error + which canon each needs). Enumerate precisely which thread.* canons each of the 4
requires — the design should scope to EXACTLY those, not the whole thread.* family.

## The reference model (this IS the blueprint — translate it 1:1 to goroutines)
- `Thread` (definitions.py:323-401): fields `cont` (a suspended fiber), `ready_func`, `task`,
  `cancellable`, `index` (slot in the task's thread table), `storage [i32,i32]`. Methods
  `running`/`suspended`/`waiting`/`ready`, `suspend`/`yield_`/`suspend_then_resume`/
  `yield_then_resume`/`suspend_then_promote`/`resume_later`, `block_internal`/`switch_to_internal`.
- `Continuation`/`Handler`/`cont_new`/`resume`/`block`/`current_thread` (definitions.py:254-319):
  a fiber built on an OS thread + already-acquired lock used as a BATON — the SAME
  single-runnable-goroutine + unbuffered-channel-baton pattern wazy ALREADY uses for stackful
  tasks (`stackful_task.go`: resumeCh/yieldCh, `sched.step`/`park`/baton handoff). This is the key
  leverage: a wazy Thread = a goroutine + a channel baton, exactly like today's stackfulTask, but
  now N-per-task instead of 1.
- `Store.waiting` + `Store.tick` (definitions.py:571-616): the scheduler picks a ready waiting
  thread and resumes it. Map to wazy's `sched` (sched.go) — which already has a parked list +
  drive/step/drainReady. Generalize "parked task" → "parked thread".
- thread.* canons: `canon_thread_index/new_indirect/resume_later/suspend/yield/
  suspend_then_resume/yield_then_resume/suspend_then_promote/yield_then_promote`
  (definitions.py:2677-2766). Each opens with `trap_if(not may_leave)`; new-indirect validates the
  indirect-table func type `(i32)->()` or `(i64)->()` and spawns a SUSPENDED thread; index returns
  the current thread's slot; suspend/yield block the current thread; the *_then_resume/*_then_promote
  variants switch directly to another thread by index.

## wazy grounding (what exists to build on / generalize)
- `internal/component/instance/stackful_task.go`: the goroutine-baton primitive (1 goroutine per
  stackful task, unbuffered-channel baton = single-runnable = -race-safe; `block`, `reapStackful`,
  the resumeCh/yieldCh handoff). THIS is the thing to generalize to N threads.
- `internal/component/instance/sched.go`: `sched` (shared *sched per composition tree), `parked`
  list, `drive`/`step`/`drainReady`/`park`. The thread scheduler layer.
- `internal/component/instance/task.go`: `task`, `enterRun`/`leaveRun`/`suspendRun`, `activeTask`.
  A task currently maps to ≤1 running thread; now a task owns a thread TABLE.
- `internal/component/instance/async_builtins.go`: builtin host-func constructors + how a canon
  kind becomes a core func (the pattern the new thread.* builtins must follow).
- `internal/component/instance/graph.go` `computeCanonHostFunc`: the bind-time switch that
  currently has NO case for the thread.* kinds (that's the "does not produce a core func" error).
  New cases needed for exactly the kinds the 4 suites use (0x0c thread.yield[-cancellable],
  0x26 thread.index, 0x27 thread.new-indirect, 0x29 thread.suspend, 0x2b thread.yield-then-resume).
- `internal/component/binary/{component.go,decoder.go}`: thread.* kinds already DECODE (commit
  6b90fec) — confirm the payloads (cancel? bool, indirect-table/type indices) are captured.
- The Instance has no `threads` table today; the reference puts `threads` on the TASK
  (`task.register_thread`/`unregister_thread`, `thread.index`) and `waiting` on the Store. Decide
  where wazy's thread table + waiting list live (task vs instance vs sched) and why.

## Deliver
For the WHOLE feature and per-suite:
- Concrete Go: the `guestThread` struct (fields), the thread table, how new-indirect spawns a
  goroutine that runs an indirect-table func and how it's scheduled, how suspend/yield/resume map to
  the baton, how `thread.index`/`current_thread` resolve, how yield-then-resume switches directly.
- How this composes with the EXISTING task model: a callback task (cancellable suite) vs a stackful
  task (sync-barges-in) as thread 0; whether the existing stackfulTask becomes "thread 0 of its
  task" or stays separate; the interaction with mayEnter/exclusiveHeld/activeTask brackets and the
  sched parked list.
- `-race`/hang/leak: the single-runnable invariant across N threads (only one goroutine holds the
  baton at a time); reaping all threads on Close/trap (generalize `reapStackful`); the deadlock-trap
  when no thread is runnable.
- The trap edges each suite asserts (trap-if-block-and-sync, trap-if-sync-and-waitable-set) and how
  the trace-oracle (`gen_async_oracle.py` + Go replay) covers them; flag any oracle-golden work.
- Which of the 26 passing suites' code paths the change touches, with a non-regression argument
  (esp. the existing stackful suites: partial-stream-copies, deadlock, empty-wait, dont-block-start).
- Where the reference is genuinely ambiguous for a single-threaded host, or where you'd deviate.
- Rank the 4 suites easiest→hardest with a landing order; note if any one needs a canon the others
  don't (so it can be a separate increment). This must be implementable by Sonnet agents in stages.

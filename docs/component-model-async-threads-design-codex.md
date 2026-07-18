# Component Model Async Threads Runtime Design

Status: design only. This document scopes the smallest thread runtime needed to
make the remaining async `.wast` suites bind and pass:

- `cancellable`
- `sync-barges-in`
- `trap-if-block-and-sync`
- `trap-if-sync-and-waitable-set`

It deliberately does not design the full WebAssembly component model thread
surface. The target is exactly the thread canons already decoded by
`internal/component/binary` and used by these fixtures:

- `thread.yield`, canon kind `0x0c`
- `thread.index`, canon kind `0x26`
- `thread.new-indirect`, canon kind `0x27`
- `thread.suspend`, canon kind `0x29`
- `thread.yield-then-resume`, canon kind `0x2b`

Out of scope for this landing:

- `thread.resume-later`
- `thread.suspend-then-resume`
- `thread.suspend-then-promote`
- `thread.yield-then-promote`
- `thread.spawn-ref`
- `thread.spawn-indirect`
- `thread.available-parallelism`

The commented calls and commented canon exports in
`trap-if-block-and-sync/component.wast` must stay irrelevant. They should not
force implementation of `yield-to`, `switch-to`, or `resume-later` aliases.

## Design Thesis

The existing `stackfulTask` is already the runtime shape required by the
reference interpreter: one goroutine owns a guest stack, an unbuffered
resume/yield channel pair acts as the baton, and `sched` keeps exactly one guest
execution active in the single-threaded host. The thread runtime should
generalize that from one goroutine per stackful task to N goroutine-backed guest
threads per task, with all runnable decisions still passing through the existing
`sched` baton.

Do not introduce OS parallel execution, locks around component state, or a
second scheduler. The new runtime is a cooperative fiber layer on top of the
same single-runnable scheduler.

## Ground Truth References

Reference interpreter:

- `internal/component/abi/testdata/definitions.py:254-319`
  defines `Continuation`, `cont_new`, `resume`, `block`, and current thread/task
  lookup.
- `internal/component/abi/testdata/definitions.py:323-425`
  defines `Thread`, its states, `resume_later`, `resume`, `block_internal`,
  `switch_to_internal`, `suspend`, `wait_until`, `yield_`,
  `suspend_then_resume`, and `yield_then_resume`.
- `internal/component/abi/testdata/definitions.py:450-568`
  defines `Task`, `implicit_thread`, `threads`, `register_thread`,
  `unregister_thread`, `request_cancellation`, and
  `deliver_pending_cancel`.
- `internal/component/abi/testdata/definitions.py:571-616`
  defines the store-level `waiting` list and `tick`.
- `internal/component/abi/testdata/definitions.py:2144-2207`
  shows `canon_lift` creating a `Task`, creating one implicit `Thread`, resuming
  it, and synchronously driving ready work until the task resolves.
- `internal/component/abi/testdata/definitions.py:2677-2766`
  defines the thread canon builtins and their signatures.

wazy implementation points:

- `internal/component/instance/stackful_task.go:56-360`
  already has the goroutine plus unbuffered-channel baton.
- `internal/component/instance/sched.go:22-220`
  already owns the shared parked-work list and the single-runnable drive loop.
- `internal/component/instance/task.go:30-330`
  currently collapses a task with its implicit thread and needs to be split.
- `internal/component/instance/async_builtins.go:31-590`
  resolves active tasks and implements the existing async canon builtins.
- `internal/component/instance/graph.go:1239-1400`
  binds canon host funcs and currently traps on thread canon kinds because they
  are decoded but not lowered to core host funcs.
- `internal/component/binary/component.go:222-308`
  documents and stores the decoded thread canon subset.
- `internal/component/binary/decoder.go:945-968`
  decodes the five target thread canons.
- `internal/component/instance/wast_async_conformance_test.go:312-334`
  skips these four suites only because thread canons bind to no host funcs.

## Reference-to-wazy Mapping

| Reference concept | Reference lines | wazy target |
| --- | --- | --- |
| `Store.waiting` | `definitions.py:571-616` | `sched.parked` |
| `Store.tick` | `definitions.py:582-616` | `sched.step`, `sched.drainReady`, `sched.drive` |
| `Continuation` with one OS thread | `definitions.py:258-319` | goroutine plus `resumeCh`/`yieldCh` |
| `Thread` | `definitions.py:323-425` | new `guestThread` |
| `Task.implicit_thread` | `definitions.py:502-516` | `task.implicitThread` |
| `Task.threads` | `definitions.py:505-523` | `task.threads` plus `Instance.threads` |
| `ComponentInstance.threads` | `definitions.py:197-206` | per-instance thread table |
| `Thread.wait_until` | `definitions.py:400-409` | `guestThread.block` with a ready predicate |
| `Thread.suspend` | `definitions.py:391-398` | `guestThread.suspend` with no ready predicate |
| `Thread.yield_` | `definitions.py:411-412` | `thread.yield` host func |
| `Thread.yield_then_resume` | `definitions.py:420-425` | `thread.yield-then-resume` host func |

The important equivalence is exact enough to implement directly:

- Reference `Continuation.block` releases the current thread and waits for a
  future resume.
- wazy `stackfulTask.block` calls `suspendRun`, parks itself, sends on
  `yieldCh`, waits on `resumeCh`, then calls `enterRun`.

The implementation should promote this baton to the thread type instead of
inventing a distinct fiber primitive.

## Data Model

### `guestThread`

Add a private type in `internal/component/instance`, preferably next to the
stackful machinery:

```go
type threadParkKind uint8

const (
	threadParkNone threadParkKind = iota
	threadParkReady
	threadParkSuspended
	threadParkDone
)

type guestThread struct {
	in *Instance
	t  *task

	index      uint32
	registered bool
	implicit   bool

	// inline means the current host call is executing synchronously on the host
	// goroutine. It has no parked guest stack and cannot support a real
	// suspend. It exists for thread.index and the sync-yield fast path.
	inline bool

	start func(context.Context) error
	ctx   context.Context

	resumeCh chan resumeMode
	yieldCh  chan struct{}

	park      threadParkKind
	readyFunc func() bool

	// cancellable is the current blocking operation's cancellation flag.
	cancellable bool

	done bool
	err  error

	// Thread-local context storage. The reference stores this on Thread, not
	// Task. Existing context.get/set should move here.
	storage [2]uint64
}
```

The existing `resumeMode` values used by `stackfulTask` are already the right
shape:

- `resumeNormal`
- `resumeCancelled`
- `resumeAbort`

If the stackful and guest-task paths cannot be unified immediately, keep
`resumeMode` shared and wrap the existing `stackfulTask` in a `guestThread`
instead of copying protocol constants.

### `task`

Extend `task` rather than replacing it at once:

```go
type task struct {
	// existing fields remain

	implicitThread *guestThread
	threads        []*guestThread
}
```

Then migrate current fields as follows:

- `ctxStorage` moves from `task` to `guestThread.storage`.
- `deliverPendingCancel` remains task-level but returns whether it delivered a
  cancel to a specific thread.
- `requestCancellation` scans `task.threads` for one cancellable blocked thread,
  matching `definitions.py:534-540`.
- `returnValues` and task resolution stay task-level.

Do not expose `guestThread` outside `internal/component/instance`.

### `Instance`

Add:

```go
type Instance struct {
	// existing fields remain

	threads      threadTable
	activeThread *guestThread

	// True if graph construction saw any of the five target thread canons.
	// Used to create an implicit thread for synchronous calls that otherwise
	// would not allocate a task.
	threadCanonsNeeded bool
}
```

The thread table should be owned by `Instance`, not by `sched`, because the
reference lookup is `current_instance().threads[t]` and the index namespace is
component-instance scoped. `task.threads` is still required for cancellation,
cleanup, and the "last thread exited before task resolved" invariant.

A compact implementation is sufficient:

```go
type threadTable struct {
	slots []*guestThread
	free  []uint32
}
```

Rules:

- `registerThread(t, th)` appends to `task.threads`, allocates an instance table
  index, stores `th.index`, and marks `registered`.
- `unregisterThread(th)` removes it from the task slice, frees the instance table
  slot, and marks it done.
- If unregistering the last thread of a task before the task has resolved, trap.
  This is the reference behavior at `definitions.py:521-522`.
- Thread index `0` is not required. Use the allocated table index. The fixtures
  treat the returned value opaquely.

### Scheduler Integration

The existing `parkedTask` interface already has the exact shape needed:

```go
type parkedTask interface {
	ready() bool
	resumeReady() error
}
```

Make `*guestThread` implement it. A suspended thread returns false from
`ready()`. A yielded thread returns its `readyFunc`. `yield_then_resume` resumes
the target directly and parks the current thread as ready.

The implementation should avoid a second run queue. `sched.park`, `sched.unpark`,
`sched.step`, and `sched.drive` remain the only mechanisms that decide what can
run next.

## Current Thread Resolution

Add a helper parallel to `requireActiveTask`:

```go
func requireCurrentThread(in *Instance, name string) (*guestThread, error)
```

Behavior:

- Trap if there is no active task or no active thread.
- Trap if the current component instance may not leave.
- Return the current `guestThread`.

Then make:

- `currentTask` derive from `activeThread.t` when a thread is active.
- `context.get` and `context.set` read and write `activeThread.storage`.
- Async builtins that block use the current thread's blocker, not only the
  task's implicit blocker.

For existing code that still calls `requireActiveTask`, keep it as a wrapper:

```go
th, err := requireCurrentThread(in, name)
return th.t, err
```

During migration, callers that are truly task-level can continue to read
`in.activeTask`, but blocking and context storage must be thread-aware.

## Enter and Leave Model

The reference has `ComponentInstance.exclusive_thread`; wazy currently has
`exclusiveOwner *task`. For the four target suites, task-level exclusivity is
enough because all additional threads are spawned inside the same task and
`yield-then-resume` switches directly to that same-task thread. No target suite
requires two different tasks in the same instance to distinguish their
individual implicit threads.

Implement the minimal extension:

```go
func (in *Instance) enterRunThread(th *guestThread) {
	in.activeTask = th.t
	in.activeThread = th
	in.enterRun(th.t)
}

func (in *Instance) leaveRunThread(th *guestThread) {
	in.suspendRun(th.t)
	in.activeThread = nil
}
```

The exact factoring can differ, but preserve these invariants:

- `activeTask` remains correct for existing resource, stream, future, and
  waitable code.
- `activeThread` is set only while that thread owns the baton.
- `mayEnter` and `mayLeave` checks still happen at the same host boundaries.
- `exclusiveOwner` remains the task owner for this landing.

Future broadening should change exclusivity from task-level to thread-level, but
that is not necessary for the current canons and would increase blast radius.

## Thread Lifecycle

### Initial implicit thread

Every async lifted call that creates a task should also create and register an
implicit thread:

```go
t := newTask(...)
th := newGuestThread(in, t, implicit=true, start=taskEntry)
t.implicitThread = th
t.registerThread(th)
```

For current stackful async exports, the entry closure should be the existing
`stackfulTask.run` body or a direct extraction of it. The first landing can keep
`stackfulTask` as the concrete goroutine owner and give it a `guestThread`
identity. The important visible behavior is that `thread.index`,
`thread.yield`, cancellation, and context storage all resolve to the implicit
thread.

For promoted callback tasks, the existing `guestTask` segment goroutine already
has baton mechanics. Either:

- wrap each blocking segment in a `guestThread`, or
- make the promoted `guestTask` segment itself satisfy the thread interface.

The first option is cleaner because `thread.yield-cancellable` needs
thread-local cancellation and storage.

### Synchronous calls

Plain synchronous component calls currently do not allocate a task unless
`syncTaskNeeded` is set. With thread canons, this is not enough because
`thread.index` and `thread.yield` can appear in a sync path.

When graph binding sees one of the target thread canons, set
`threadCanonsNeeded`. In `invokeEntered`, if either `syncTaskNeeded` or
`threadCanonsNeeded` is true, create a temporary `syncImplicit` task and an
inline implicit thread for the duration of the call.

The inline thread:

- supports `thread.index`;
- supports `thread.yield` as a bounded scheduler pump, described below;
- traps for `thread.suspend`;
- cannot be the result of `thread.new-indirect`;
- has thread-local context storage for the call duration.

This keeps synchronous call behavior narrow and avoids forcing every sync call
onto a goroutine.

## Canon Host Functions

`computeCanonHostFunc` should bind the five decoded thread canon kinds instead
of returning "kind 0xNN does not produce a core func".

### `thread.index`

Signature:

- params: none
- results: `i32`

Semantics:

- Resolve `requireCurrentThread`.
- Return `th.index`.

This is needed by `trap-if-block-and-sync` even though the active test calls
only use it indirectly or in commented sections. Binding must still succeed.

### `thread.yield`

Signature:

- params: none
- results: `i32`

Return values:

- `0` for normal resume
- `1` for cancelled resume

Semantics for a goroutine-backed thread:

1. Resolve the current thread and task.
2. If `cancellable` is true and task cancellation is already pending, consume
   and return `1`.
3. Mark the current thread as waiting with `readyFunc = func() bool { return true }`.
4. Store the blocking operation's cancellable flag on the thread.
5. Leave the instance and park the thread in `sched`.
6. Yield the baton to the scheduler.
7. On resume:
   - `resumeNormal`: enter the instance and return `0`;
   - `resumeCancelled`: enter the instance and return `1`;
   - `resumeAbort`: terminate the goroutine without re-entering guest code.

Semantics for an inline synchronous thread:

1. If `cancellable` is true and cancellation is pending, consume and return `1`.
2. Call a bounded scheduler step or `pumpSnapshot` so already-ready async work
   gets a chance to run.
3. Return `0` unless cancellation was delivered.

This sync fast path is narrower than the reference, but it is the right target
for the four suites. `trap-if-block-and-sync` expects `thread.yield` to be
"fine" in sync code while `thread.suspend` traps.

### `thread.suspend`

Signature:

- params: none
- results: `i32`

Return values are the same as `thread.yield`.

Semantics:

- For a goroutine-backed thread, park with `threadParkSuspended` and no ready
  predicate. The only way to run again is an explicit resume operation such as
  `yield-then-resume` targeting this thread.
- For an inline synchronous thread, trap immediately with the same behavioral
  class as other sync blocking operations: "cannot block a synchronous task
  before returning".
- Honor the `cancellable` flag exactly like `thread.yield`.

The target suites do not require `thread.suspend` to resume normally. They need
it to bind and to trap correctly when called from a synchronous path.

### `thread.new-indirect`

Signature as imported by the fixture:

- params: `i32` function index, then an argument value (`i32` or `i64` in the
  target suites)
- results: `i32` thread index

Bind-time work:

- Add a table resolver next to `coreMemTarget`, for example:

```go
coreTableTarget := func(idx int) (*wasm.ModuleInstance, *wasm.TableInstance, error)
```

- Follow passthrough shims the same way `coreMemTarget` follows memory shims.
  `resolveInlineExportItem` already distinguishes table aliases.
- Resolve `canon.TableIdx`.
- Resolve the expected function type from the consuming core import signature.

The decoder currently stores the core type section as raw bytes rather than a
decoded type table. For this narrow landing, do not build a full core type
decoder. Instead, infer the indirect target signature from the import type that
the canon host func is being created for:

- `thread.new-indirect` returns `i32`.
- The first param is the table function index.
- The second param is the start argument.
- The target core function type is `(arg) -> ()`.

Then get the runtime type id with:

```go
owner.GetFunctionTypeID(&wasm.FunctionType{
	Params:  []wasm.ValueType{argType},
	Results: nil,
})
```

This is an intentional narrow deviation. A full implementation should decode
and resolve `canon.TypeIdx` from the core type section.

Call-time work:

1. Resolve the current thread and task.
2. Look up the function in the resolved table using the function index and the
   expected type id. Use the same runtime lookup behavior as
   `ModuleInstance.LookupFunction` so null, bounds, and type mismatches trap
   consistently with `call_indirect`.
3. Allocate and register a non-implicit `guestThread` in the same task.
4. Spawn its goroutine immediately, but have it block on `resumeCh` before
   calling the target function. This matches `Thread(..., suspended)` in the
   reference and makes cleanup explicit.
5. Return the new thread's table index.

Thread entry:

```go
func(ctx context.Context) error {
	_, err := fn.Call(ctx, arg)
	if err != nil {
		return err
	}
	return nil
}
```

On normal return, unregister the thread. If it was the last unresolved thread of
the task, trap. On error or panic, poison the instance in the same style as
existing stackful guest failures, abort peer parked threads, and propagate the
trap to the scheduler caller.

### `thread.yield-then-resume`

Signature used by the fixture:

- params: `i32` thread index
- results: `i32`

Semantics:

1. Resolve the current thread.
2. Resolve the target thread index in `Instance.threads`.
3. Trap if the target is missing, done, from another instance, from another task,
   or not suspended.
4. If the current call is cancellable and cancellation is pending, consume and
   return `1` before switching. This matches
   `definitions.py:420-423`.
5. Mark the current thread as waiting with `readyFunc = func() bool { return true }`.
6. Park the current thread.
7. Resume the target thread directly with `resumeNormal`.
8. Current thread yields the baton.
9. When current is later resumed, return `0` or `1` based on the resume mode.

The direct handoff is important. Do not enqueue the target and then let unrelated
work run first. `yield-then-resume` is the suite's only required way to run a
`thread.new-indirect` thread.

## Blocking Builtins

The current blocking builtins generally call `activeBlocker(in)` or
`blockingTask(in, name)` and then block through the task. That path must be
thread-aware.

Required changes:

- `blockingTask` should still reject `syncImplicit` before returning, but it
  should identify the current task through `activeThread`.
- `activeBlocker` should return the current thread blocker when a thread is
  active.
- A task with multiple threads must not collapse cancellation or blocking state
  into a single `task.st` or `task.gt` slot. The current thread owns the block.
- `task.blocker()` can remain temporarily for existing call sites, but must be
  audited so it never returns the wrong thread for a multi-threaded task.

The safest migration is:

```go
type blocker interface {
	block(ctx context.Context, ready func() bool, cancellable bool) (bool, error)
}

func activeBlocker(in *Instance) (blocker, error) {
	th, err := requireCurrentThread(in, "...")
	if err != nil {
		return nil, err
	}
	return th, nil
}
```

Then let `guestThread.block` call the same scheduler baton code that
`stackfulTask.block` uses today.

## Cancellation

Reference cancellation is task-level but delivered to one cancellable thread:

- `Task.request_cancellation` chooses a cancellable thread if one exists.
- If none exists, it records `task_pending_cancel`.
- `Thread.wait_until` consumes pending cancellation before checking readiness.

Implement the same:

```go
func (t *task) requestCancellation() bool {
	for _, th := range t.threads {
		if th.cancellable && th.isBlocked() {
			th.resumeCancelled()
			return true
		}
	}
	t.flags |= taskPendingCancel
	return false
}

func (t *task) deliverPendingCancel() bool {
	if t.flags&taskPendingCancel == 0 {
		return false
	}
	t.flags &^= taskPendingCancel
	return true
}
```

`thread.yield` and `thread.suspend` check `deliverPendingCancel` before parking
when their canon is cancellable.

This is what `cancellable` requires:

- direct cancellation while a thread is blocked in `thread.yield-cancellable`
  resumes that thread with result `1`;
- cancellation that happens while no cancellable thread is blocked remains
  pending and the next cancellable `thread.yield` returns `1` without parking.

## Waitable-Set Sync Trap Edge

`trap-if-sync-and-waitable-set` has one additional requirement that is easy to
miss. The first four asserts create a thread with `thread.new-indirect`, switch
to it with `thread.yield-then-resume`, let that spawned thread enter a
synchronous stream/future read/write that blocks, and then try to join the same
waitable to a set. `waitable.join` must see that the waitable already has a
synchronous waiter and trap with:

```text
waitable cannot be used synchronously while added to a waitable set
```

Existing stream/future copy builtins already trap when a synchronous operation
starts on a waitable already in a set. They do not consistently mark the
waitable as having a sync waiter while a sync copy is blocked. Add bracketing:

1. Before a sync stream/future read/write blocks, set `waitable.syncWaiter`.
2. Clear it after normal resume, cancellation, or trap.
3. `waitable.join` keeps checking `syncWaiter` and traps with the fixture
   substring.

Also standardize `subtask.cancel` when the subtask is already joined to a set.
Its current trap text is different from the suite substring; once the suite is
unskipped, that mismatch will surface.

## Per-Suite Requirements

### `sync-barges-in`

Active canons:

- `thread.yield`, non-cancellable

Required behavior:

1. The stackful async export starts task `C.yielder`.
2. It calls `thread.yield`.
3. The implicit thread parks as immediately ready and yields the baton.
4. The scheduler permits `D`'s synchronous `poker/drop-S` work to enter while
   the task is yielded.
5. The yielded thread resumes and returns the final value observed after the
   sync work barged in.

This is the easiest suite because it only needs thread identity for the implicit
stackful task and non-cancellable yield.

### `cancellable`

Active canons:

- `thread.yield`, cancellable

Required behavior:

1. The callback-lifted core calls `thread.yield-cancellable`.
2. Cancellation of the subtask must find the blocked cancellable thread.
3. Resume mode `resumeCancelled` returns `1` from the canon.
4. A pending cancellation must be consumed by the next cancellable yield before
   parking.

This suite proves cancellation has moved from "one task has one blocker" to
"one task has a set of cancellable threads".

### `trap-if-block-and-sync`

Active canons:

- `thread.yield`
- `thread.suspend`
- `thread.index`
- `thread.new-indirect` only needs to bind in the active file because the
  direct calls to `yield-to`, `switch-to`, and `resume-later` are commented.

Required behavior:

- Existing synchronous-blocking traps still fire for sync-lowered async calls,
  waitable waits, stream/future blocking copies, and sync subtask cancel.
- `thread.suspend` in a synchronous path traps as a blocking operation before
  returning.
- `thread.yield` in a synchronous path is allowed and returns normally.
- `thread.index` returns a valid current thread index.
- `thread.new-indirect` binds even if not actively called by this fixture.

This suite is mostly a bind and trap-edge suite, not a multi-thread scheduling
suite.

### `trap-if-sync-and-waitable-set`

Active canons:

- `thread.new-indirect`
- `thread.yield-then-resume`

Required behavior:

1. `thread.new-indirect` creates a suspended same-task thread that will call an
   indirect function from the referenced core table.
2. `thread.yield-then-resume` switches directly to that suspended thread.
3. The spawned thread starts a synchronous stream/future read/write/cancel path.
4. If that waitable is in a set, or is later joined to a set while sync waiting,
   the runtime traps with the fixture substring.

This is the hardest suite because it exercises all new pieces together:
indirect table lookup, per-task thread table, direct handoff, sync waiter
marking, and cleanup after traps.

## Race, Hang, and Leak Arguments

### Race safety

The runtime remains single-runnable:

- Only the goroutine holding the baton may mutate component state.
- A blocked thread can only be resumed by sending on its unbuffered `resumeCh`.
- A yielding thread sends on `yieldCh` before waiting for its next `resumeCh`.
- The scheduler does not resume two parked threads concurrently.

Therefore the thread table, task thread list, waitable maps, resource maps, and
instance active fields do not need locks. This matches the existing
`stackfulTask` `-race` story.

### No double resume

Before resuming a parked thread:

- remove it from `sched.parked`;
- clear its parked state;
- send exactly one resume mode;
- wait for its `yieldCh` or completion.

`sched.isParked` and `sched.unpark` should continue to be used as guardrails.
For direct `yield-then-resume`, the target must be suspended and not already in
the ready parked list.

### No hangs

A parked yielded thread has a ready predicate. `thread.yield` uses a predicate
that is immediately true.

A suspended thread has no ready predicate. It can only run when another thread
explicitly resumes it. If every live thread is suspended or waiting on
unready waitables, `sched.drive` should return the existing async-deadlock error
rather than spin or block forever.

Inline synchronous `thread.suspend` traps immediately, so sync callers cannot
park the only host goroutine with no continuation.

### No goroutine leaks

The current `Close` path reaps parked stackful goroutines. Generalize that to
all goroutine-backed guest threads:

- scan `sched.parked`;
- scan `Instance.threads` as well, because `thread.new-indirect` creates a
  suspended goroutine that may not be in `sched.parked` yet;
- send `resumeAbort` to every live goroutine-backed thread;
- wait for its `yieldCh` or done signal;
- unregister and clear table slots.

Do not close channels as a control signal. Keep the current explicit
`resumeAbort` protocol.

On traps from a spawned thread, poison the instance, abort peer parked threads,
and propagate the error to the driving caller. This mirrors the existing
stackful trap propagation model.

## Oracle and Test Coverage

The existing async oracle is mostly single-threaded. Its comments describe a
world with one guest thread, so it will not independently prove
`thread.new-indirect` plus `yield-then-resume` until extended.

Add oracle coverage in this order:

1. A non-cancellable `thread.yield` case matching `sync-barges-in`.
2. A cancellable `thread.yield` case where cancellation resumes the blocked
   thread.
3. A pending-cancellation case where cancellable yield returns cancelled before
   parking.
4. A two-thread case: thread 0 creates thread 1 with `new-indirect`, then
   `yield-then-resume`s it.
5. A trap case where a spawned thread blocks synchronously on a waitable and
   another path attempts to join that waitable to a set.

If the oracle generator cannot express thread canons yet, extend the generator
first and regenerate `async_oracle_golden.json`. The `.wast` conformance suites
remain the final behavioral oracle for the feature.

## Non-Regression Argument

The 26 currently passing async suites should stay stable if the landing is kept
narrow:

- New `computeCanonHostFunc` cases only affect canon kinds that currently fail
  binding.
- Existing task, subtask, stream, future, waitable, backpressure, and
  error-context canons keep their bind paths.
- Guest tasks without thread canons should keep the existing frame-free callback
  path unless they already promote because they may block synchronously.
- Stackful suites keep the same goroutine baton, just with a thread identity.
- `sched` remains the only runnable arbiter, so deadlock and ready-order behavior
  remain deterministic.
- Context storage changes from task to thread, but single-thread tasks observe
  identical behavior.
- Waitable sync waiter marking is additive and only tightens the forbidden
  "sync wait while in a set" state that the skipped suite already expects.

Watch these existing suites specifically after each stage:

- `partial-stream-copies`: verifies copy parking and resumption still work.
- `deadlock`: verifies no-ready-work paths still trap instead of hanging.
- `empty-wait`: verifies waitable-set empty behavior is unchanged.
- `dont-block-start`: verifies task start still does not block incorrectly.
- `wait-during-callback`: verifies promoted callback blocking remains correct.

## Landing Plan, Easiest to Hardest

### Stage 1: bind scaffolding and current thread

Implement:

- `guestThread` skeleton;
- `Instance.threads`;
- `activeThread`;
- implicit thread creation for async tasks;
- inline implicit thread for sync calls when `threadCanonsNeeded`;
- `thread.index`;
- `computeCanonHostFunc` cases for all five target kinds, with unimplemented
  runtime paths returning explicit traps only where the suite will not call
  them yet.

Verification:

- The four suites should get past bind.
- `thread.index` unit coverage can prove a sync call has an active inline
  thread.

Do not unskip a suite at this point unless all active calls are implemented.

### Stage 2: non-cancellable `thread.yield`

Implement:

- `guestThread.block` for the existing stackful implicit thread;
- ready-immediate yield;
- scheduler park/unpark integration;
- sync inline yield as a bounded scheduler pump.

Expected suite:

- `sync-barges-in`

Regression focus:

- stackful task tests;
- deadlock tests;
- callback wait tests.

### Stage 3: cancellable yield

Implement:

- per-thread cancellable blocking state;
- task-level scan across `task.threads` in `requestCancellation`;
- pending cancellation delivery before parking;
- callback-promoted current thread identity.

Expected suite:

- `cancellable`

Regression focus:

- existing cancellation and waitable-set suites;
- tasks that cancel before a wait becomes ready.

### Stage 4: sync trap edges

Implement:

- `thread.suspend` for goroutine-backed threads;
- immediate sync trap for inline `thread.suspend`;
- complete `blockingTask` and `activeBlocker` audit so all sync blocking
  builtins still report "cannot block a synchronous task before returning";
- bind-only `thread.new-indirect` correctness for the active fixture imports.

Expected suite:

- `trap-if-block-and-sync`

Regression focus:

- sync-lowered async import/export tests;
- stream/future blocking trap tests;
- subtask cancel trap tests.

### Stage 5: spawned threads and direct switch

Implement:

- table resolver for `thread.new-indirect`;
- narrow indirect function signature inference from import type;
- spawned suspended goroutine-backed `guestThread`;
- `thread.yield-then-resume`;
- thread unregister and last-thread-before-resolution trap;
- reaper scan of `Instance.threads`;
- sync waiter marking while blocked on stream/future read/write;
- standardized waitable-set sync trap text.

Expected suite:

- `trap-if-sync-and-waitable-set`

Regression focus:

- waitable joins;
- stream/future copy cancellation;
- `Close` and trap cleanup with live suspended threads.

## Implementation Checklist

1. Add thread data structures and table allocation.
2. Create implicit threads at task creation and temporary inline threads for
   sync calls with thread canons.
3. Move context storage to current thread.
4. Teach blocking helpers to resolve current thread.
5. Bind `thread.index`.
6. Bind and implement `thread.yield`.
7. Widen cancellation to scan task threads.
8. Bind and implement `thread.suspend`.
9. Add table resolution support to graph binding.
10. Bind and implement `thread.new-indirect`.
11. Bind and implement `thread.yield-then-resume`.
12. Mark synchronous stream/future waiters while blocked.
13. Generalize parked goroutine reaping to all live guest threads.
14. Extend oracle scenarios and regenerate goldens.
15. Unskip suites one at a time in the stage order above.

Keep each stage small enough that failures identify one semantic boundary.

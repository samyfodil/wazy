# Component Model Async Task Model Design

This document designs the two remaining task-model conformance changes named in
`docs/component-model-async-taskmodel-design-brief.md`:

- Suite A: `big-interleaving-test`, fixed by creating an implicit sync task for
  plain sync canon lifts.
- Suite B: `trap-on-reenter`, fixed by holding an `entering_set`-based entry
  guard across callback-task lifetime, with ancestor exclusion.

The two suites share one mechanism: every canon lift needs a real task identity
and a real entry guard. The lifetimes differ. A sync implicit task exists only
for one synchronous lift invocation. A callback/stackful async task keeps its
entry guard until task completion or trap.

## Ground Truth

Current wazy state:

- `internal/component/instance/task.go:30` defines `task`; today it only models
  callback/stackful async tasks. `ctxStorage` at `task.go:56` is already the
  right per-task storage shape.
- `task.blocker()` at `task.go:88` returns a mid-call suspension capability only
  for stackful tasks or promoted callback segments. That is not the same as
  "this task may block".
- `enterRun`, `leaveRun`, and `suspendRun` at `task.go:278`,
  `task.go:284`, and `task.go:298` currently mutate `mayEnter` only while guest
  code is on the stack. This is too transient for `trap-on-reenter`.
- `requireActiveTask` at `async_builtins.go:31` traps when `activeTask == nil`.
  This is the direct `big-interleaving-test` failure for legal sync-context
  `context.get/set`, `waitable-set.poll`, and `backpressure.inc/dec`.
- `blockingTask` at `async_builtins.go:65` currently distinguishes sync context
  only by `activeTask == nil`. With an implicit sync task, that discriminator
  becomes wrong.
- `activeBlocker` at `async_builtins.go:85` returns only a blocker. It must also
  tell callers whether the active task is a sync implicit task.
- `Instance` async state is at `instance.go:254` through `instance.go:325`.
  There is no parent/ancestor link today.
- `boundExport.home` at `instance.go:410` is currently documented and assigned
  only for async re-export aliases. That is insufficient once sync exports get
  `activeTask`.
- `invoke` at `instance.go:1530` dispatches async callback, stackful, then sync
  `invokeEntered`. `invokeEntered` calls core code at `instance.go:1666`.
- `host_import.go:660` has a start-function special case for sync lower to an
  async callee during instantiation. It must remain taskless.

Reference model:

- `definitions.py:220-248` defines `may_enter_from`, `enter_from`, `leave_to`,
  `entering_set`, and `self_and_ancestors`.
- `definitions.py:315-319` says `current_task()` and `current_instance()` are
  always task-derived.
- `definitions.py:323-353` creates `Thread.storage = [0, 0]` per thread.
- `definitions.py:469-479` creates a `Task` for each lift and records
  `caller`.
- `definitions.py:485-503` enters the implicit thread, including backpressure
  and exclusive handling.
- `definitions.py:587-594` brackets `Store.lift` with
  `trap_if(not inst.may_enter_from(caller))`, `enter_from(caller)`, and
  `leave_to(caller)`.
- `definitions.py:2144-2207` creates the task/thread in `canon_lift`.
- `definitions.py:2337-2353` implements `context.get/set` from current thread
  storage.
- `definitions.py:2408-2433` implements wait vs poll: wait blocks, poll does
  not.

## Suite A: Implicit Sync Task

### Data Shape

Extend `task` in `internal/component/instance/task.go:30`:

```go
type task struct {
    inst  *Instance
    be    *boundExport
    state taskState

    gt *guestTask
    st *stackfulTask

    // syncImplicit is true for a plain sync canon lift. It has a current task
    // and context storage, but it cannot suspend, cannot nested-drive as a
    // callback task, and cannot use task.return/task.cancel.
    syncImplicit bool

    // entrySet is used by Suite B. For Suite A it is acquired and released
    // inside invokeEntered.
    entrySet []*Instance

    numBorrows int
    cancelled  bool
    ctxStorage [2]uint64
    onResolve  func(vals []abi.Value, cancelled bool)
    result     []abi.Value
}
```

Add helpers in `task.go`:

```go
func newImplicitSyncTask(in *Instance, be *boundExport) *task {
    return &task{inst: in, be: be, state: taskStarted, syncImplicit: true}
}

func (t *task) isSyncImplicit() bool {
    return t != nil && t.syncImplicit
}

func (t *task) asyncTaskBuiltinsAllowed() bool {
    return t != nil && t.be != nil &&
        (t.be.asyncCallback || (t.be.stackful && t.be.stackfulAsyncOpts))
}
```

`ctxStorage` is intentionally fresh per implicit sync call. That matches the
reference's new `Thread` with `[0, 0]` storage for each `Task`
(`definitions.py:323-353`, `definitions.py:2199-2201`).

### Sync Invoke Installation

Change `internal/component/instance/instance.go:1530` so the public `invoke`
delegates to an internal `invokeFrom`:

```go
func (in *Instance) invoke(ctx context.Context, be *boundExport, exportName string, args []abi.Value) ([]abi.Value, error) {
    return in.invokeFrom(ctx, nil, be, exportName, args)
}

func (in *Instance) invokeFrom(ctx context.Context, caller *Instance, be *boundExport, exportName string, args []abi.Value) ([]abi.Value, error)
```

Change `invokeEntered` at `instance.go:1619` to:

```go
func (in *Instance) invokeEntered(ctx context.Context, caller *Instance, be *boundExport, exportName string, args []abi.Value) ([]abi.Value, error)
```

At the top of `invokeEntered`, before `lowerParams` at `instance.go:1630`,
install the task and restore the previous task with `defer`:

```go
t := newImplicitSyncTask(in, be)
entrySet, err := in.enterFrom(caller, t) // Suite B helper; for Suite A this is one-call lifetime.
if err != nil {
    return nil, fmt.Errorf("component/instance: export %q: wasm trap: cannot enter component instance", exportName)
}
t.entrySet = entrySet

prevTask := in.activeTask
in.activeTask = t
defer func() {
    in.activeTask = prevTask
    t.closeEntrySet()
}()
```

This survives nested guest-to-guest sync calls: an inner `invokeEntered` pushes a
new task on the same `Instance`, and the defer restores the outer task even if
`CallWithStack` traps at `instance.go:1666` or post-return traps at
`instance.go:1693`.

### Correct Runtime Owner for Re-exported Sync Exports

Change `bindFuncExportGraph` at `graph.go:1728-1749`: when a bound export is a
re-export of a nested component instance, copy it and set `home = sub` for all
exports, not only async exports.

Current code only does this under:

```go
if (be.asyncCallback || be.stackful) && be.home == nil
```

Change it to:

```go
if be.home == nil {
    homeBE := *be
    homeBE.home = sub
    return &homeBE, nil
}
```

Then update the comments at `instance.go:410-427` and `instance.go:1531-1535`.
Reason: `big-interleaving-test` reaches sync exports through aliases. The core
builtin closures are bound against the nested instance, so installing
`activeTask` on the aliasing wrapper would not help.

### Blocking Classification

A sync implicit task is a task for `current_task()`, but it is still a
synchronous task and must not block.

Change `blockingTask` at `async_builtins.go:65`:

```go
func blockingTask(in *Instance, builtin string) (t *task, blk taskBlocker) {
    t = in.activeTask
    if t == nil || t.syncImplicit {
        panic(fmt.Errorf("component/instance: %s: cannot block a synchronous task before returning", builtin))
    }
    return t, t.blocker()
}
```

Change `activeBlocker` at `async_builtins.go:85` to return a sync-task flag:

```go
func activeBlocker(in *Instance) (blk taskBlocker, syncTask bool) {
    if t := in.activeTask; t != nil {
        if t.syncImplicit {
            return nil, true
        }
        return t.blocker(), false
    }
    return nil, false
}
```

Update every call site that currently treats `blk == nil` as "nested drive":

- `async_builtins.go:412-416`, `subtask.cancel` synchronous wait.
- `stream_builtins.go:493-495`, sync `stream.read/write` copy wait.
- `stream_builtins.go:583-585`, sync `future.read/write` copy wait.
- `stream_builtins.go:720-722`, sync stream/future cancel wait.
- `host_import.go:659-675`, sync canon lower to async target.

The replacement pattern is:

```go
if blk, syncTask := activeBlocker(in); syncTask {
    panic(fmt.Errorf("component/instance: %s: cannot block a synchronous task before returning", name))
} else if blk != nil {
    blk.block(pred, false)
} else if derr := in.sched.drive(pred); derr != nil {
    panic(wrapSchedErr(name, derr))
}
```

For `host_import.go:659-675`, do not route a sync implicit task to
`startDelegatedFromBlocker` and do not fall through to `hi.fn`'s nested
`sched.drive`:

```go
if t := in.activeTask; t != nil {
    if t.syncImplicit {
        panic(fmt.Errorf("component/instance: host import %q %q: cannot block a synchronous task before returning", iface, funcName))
    }
    blockingCaller = t.blocker()
}
```

Do not remove the existing `in.sched.instantiating && in.activeTask == nil`
branch at `host_import.go:660`. Start functions are still genuinely taskless
because they never enter through `invokeEntered`.

### Builtins Allowed on Sync Implicit Tasks

No change is needed for these builtins once `activeTask` exists:

- `context.get` at `async_builtins.go:548`
- `context.set` at `async_builtins.go:567`
- `waitable-set.poll` at `async_builtins.go:234`
- `backpressure.inc` at `async_builtins.go:584`
- `backpressure.dec` at `async_builtins.go:595`

Do change `task.return` and `task.cancel`:

- In `taskReturnHostFuncGraph` at `async_builtins.go:475`, reject
  `!t.asyncTaskBuiltinsAllowed()` before result-type checks.
- In `taskCancelHostFuncGraph` at `async_builtins.go:327`, replace the existing
  `t.be != nil && ...` test with the same helper.

This prevents an implicit sync task from accidentally making `task.return` legal.

## Suite B: Entry Guard Held Across Task Lifetime

### Parent and Entry State

Add to `Instance` near `instance.go:196` and `instance.go:288`:

```go
type Instance struct {
    parent *Instance

    // enterOwner is non-nil while this component instance is in an entered set.
    // It is held for one sync implicit call, or for the whole lifetime of an
    // async task. It replaces mayEnter as the source of truth.
    enterOwner *task

    // Keep mayEnter for existing tests/comments initially, but derive it from
    // enterOwner == nil. Do not let enterRun/leaveRun own it anymore.
    mayEnter bool
}
```

Set parent links:

- Add `parent *Instance` to `config` in `host_import.go` near the composition
  fields at `host_import.go:53-90`.
- In `instantiateGraph`, initialize `parent: cfg.parent` in the `Instance`
  literal at `graph.go:262`.
- In `instantiateNestedInstances`, set `parent: in` in `subCfg` at
  `graph.go:758-766`.
- The trivial no-import path at `instance.go:1256` keeps `parent == nil`.

Add methods in `task.go` or a new small `entry.go`:

```go
func (in *Instance) selfAndAncestors() []*Instance
func (in *Instance) enteringSet(caller *Instance) []*Instance
func (in *Instance) mayEnterFrom(caller *Instance) bool
func (in *Instance) enterFrom(caller *Instance, owner *task) ([]*Instance, error)
func leaveEntrySet(set []*Instance, owner *task)
func (t *task) closeEntrySet()
```

`enteringSet` must implement `definitions.py:236-240`: callee
`self_and_ancestors` minus caller `self_and_ancestors`; with `caller == nil`,
use the full callee ancestor chain.

`enterFrom` checks all entries before mutating any:

```go
func (in *Instance) enterFrom(caller *Instance, owner *task) ([]*Instance, error) {
    set := in.enteringSet(caller)
    for _, inst := range set {
        if inst.poisoned || inst.enterOwner != nil {
            return nil, errCannotEnterComponentInstance
        }
    }
    for _, inst := range set {
        inst.enterOwner = owner
        inst.mayEnter = false
    }
    return set, nil
}
```

`leaveEntrySet` must assert owner identity in debug-style code comments, clear
`enterOwner`, and set `mayEnter = true`.

Important dynamic rule: ancestor exclusion must not become "parked ancestor
re-entry is always OK". If `callee` is an ancestor of `caller`, and the callee
has an outstanding `enterOwner` whose task is not currently active on that
callee, reject entry. This is the single-threaded-host way to preserve the
brief's distinction:

- A child synchronously calling back into its static parent while the parent's
  task is still on the stack is allowed.
- A child callback resumed after a scheduler tick and then calling back into the
  parked parent traps.

Concrete helper:

```go
func (callee *Instance) excludedAncestorBlocked(caller *Instance) bool {
    for a := caller; a != nil; a = a.parent {
        if a == callee {
            return callee.enterOwner != nil && callee.activeTask != callee.enterOwner
        }
    }
    return false
}
```

Call this from `mayEnterFrom` and `enterFrom` before accepting an empty
`enteringSet`. This is the riskiest part of the design. A naive implementation
of only `callee.self_and_ancestors() - caller.self_and_ancestors()` will
over-allow the exact parked callback re-entry in `trap-on-reenter`.

The current vendored `trap-on-reenter.wast` has two extra sync assertions marked
"also, for now" at `trap-on-reenter.wast:67` and `trap-on-reenter.wast:88`.
Those are not the strict `definitions.py:236-240` ancestor-exclusion behavior:
plain parent-to-child and child-to-parent nested sync calls can be legal under
the reference rule. To unskip the suite as vendored, isolate the deviation behind
a narrow compatibility predicate rather than folding it into `enteringSet`:

```go
func (in *Instance) syncAdjacencyBlocked(caller *Instance) bool {
    if caller == nil {
        return false
    }
    // Reject only the sync "for now" same-tree cases: the callee and caller
    // share an already-entered ancestor that the strict entering_set would
    // exclude. Do not apply this to async task starts.
    callerAnc := caller.selfAndAncestorsSet()
    for a := in; a != nil; a = a.parent {
        if _, ok := callerAnc[a]; ok && a.enterOwner != nil {
            return true
        }
    }
    return false
}
```

Call this only when starting `syncImplicit` work, after the strict
`enterFrom(caller, t)` check would otherwise pass. This makes the suite's lines
`86` and `110` trap today, while keeping the reference mechanism factored so the
compatibility branch can be deleted when upstream stops asserting those sync
over-traps. Do not apply it in `startAsyncExportTask` or
`startStackfulExportTask`; that would over-trap `async-calls-sync` and the
stream/future composition suites.

### Caller Threading

Thread caller identity through lift/lower boundaries:

- Keep public `Call` and `CallExport` at `instance.go:1447` and
  `instance.go:1469` as `caller == nil`.
- Change internal sync/async dispatch to `invokeFrom(ctx, caller, ...)`.
- Change `invokeAsyncCallback` at `async_lift.go:46`,
  `invokeStackful` at `async_lift.go:125`, `startAsyncExportTask` at
  `async_lift.go:180`, and `startStackfulExportTask` at `async_lift.go:243` to
  accept `caller *Instance`.
- In `buildAsyncHostWrapper`, pass the importing instance `in` at
  `async_host_import.go:391`:

```go
onCancel, operr := tgt.sub.startAsyncExportTask(ctx, in, tgt.be, tgt.exportName, onStart, onResolve)
```

- Add an internal delegated-call hook to `hostImport` at `host_import.go:152`:

```go
call func(ctx context.Context, caller *Instance, args []abi.Value) ([]abi.Value, error)
```

Do not change the public `HostFunc` type.

- In `delegatingHostImport` at `graph.go:917`, set `call` to map resources and
  call `sub.invokeFrom(ctx, caller, be, exportName, in)`.
- In `delegatingHostImportDeferred` at `composition.go:363`, do the same.
- In `buildHostWrapper` at `host_import.go:689`, call `hi.call(ctx, in, args)`
  when present; otherwise call `hi.fn(ctx, args)`.
- Change `startDelegatedFromBlocker` at `graph.go:986` to accept
  `caller *Instance` and pass it to `sub.startAsyncExportTask`.

### Entry Lifetime

Acquire the entry guard before task start:

- `invokeAsyncCallback`, before creating/running the `guestTask` at
  `async_lift.go:84`.
- `invokeStackful`, before creating/running the `stackfulTask` at
  `async_lift.go:140`.
- `startAsyncExportTask`, before `gt.start()` at `async_lift.go:226`.
- `startStackfulExportTask`, before `st.startTask()` at `async_lift.go:261`.
- `invokeEntered`, as described for Suite A.

Release the guard exactly once:

- `guestTask.finishExit` at `guest_task.go:445`.
- `guestTask.fail` at `guest_task.go:472`.
- `guestTask.resumeReady` park-entry cancellation path at `guest_task.go:315`.
- `stackfulTask.finishOK` at `stackful_task.go:240`.
- `stackfulTask.fail` at `stackful_task.go:232`.
- `stackfulTask.resumeReady` spark-entry cancellation path at
  `stackful_task.go:293`.
- `reapParkedGoroutines` at `stackful_task.go:342`, for parked-at-entry tasks
  that never spawned a goroutine.
- `invokeEntered` via `defer`.

Do not release from `task.returnValues` at `task.go:104` or `cancelResolve` at
`task.go:139`. A callback task that has called `task.return` still must return
`EXIT`; releasing early would reopen re-entry before `exit_implicit_thread`.

### enterRun, leaveRun, suspendRun

After this change, `enterRun`, `leaveRun`, and `suspendRun` should no longer
own `mayEnter`.

Change:

- `enterRun` at `task.go:278`: set only `activeTask`, `exclusiveHeld`, and
  `exclusiveOwner`.
- `leaveRun` at `task.go:284`: clear only `exclusiveHeld`, `exclusiveOwner`,
  and `activeTask`.
- `suspendRun` at `task.go:298`: clear only `activeTask`; keep
  `exclusiveHeld/exclusiveOwner` as today.

This answers the brief's key question: yes, `suspendRun` must stop restoring
`mayEnter` for an outstanding task. The lifetime entry owner controls
`mayEnter`; running/parked status controls `activeTask` and exclusive ownership.

Update `task.requestCancellation` at `task.go:193`:

- Replace direct `t.inst.mayEnter` checks at `task.go:206`, `task.go:222`, and
  `task.go:230` with an owner-aware readiness check for resuming the same task.
- Do not call `enterFrom` as if starting a new task; this is a resume of the
  task that already owns its entry set.

### Trap Text

Use one helper:

```go
var errCannotEnterComponentInstance = errors.New("wasm trap: cannot enter component instance")
```

Wrap it at every entry rejection:

- `async_lift.go:73`
- `async_lift.go:136`
- `async_lift.go:198`
- `async_lift.go:246`
- the new sync entry check in `invokeEntered`

The `.wast` manifest for `trap-on-reenter` expects the substring
`"wasm trap: cannot enter component instance"` at
`trap-on-reenter.wast:65`, `trap-on-reenter.wast:86`, and
`trap-on-reenter.wast:110`.

## Non-regression Argument

`big-interleaving-test`: the previously failing sync exports now have a current
task, so `waitable-set.poll`, `context.get/set`, and `backpressure.inc/dec`
stop trapping at `requireActiveTask`. The task has fresh `[2]uint64` storage per
call. Actual blocking from a sync implicit task still traps because
`blockingTask`, `activeBlocker`, and the sync-lower-to-async path explicitly
recognize `syncImplicit`.

`async-calls-sync`: the promoted callback/stackful blocking path is unchanged.
Caller identity must be threaded as the importing instance, so sibling calls
exclude their shared parent exactly like `definitions.py:236-240`. Do not pass
`nil` for guest-to-guest async lowers here, or this suite can over-trap.

`cancel-subtask`: stackful/promoted callers still get a non-nil blocker and park
through `blk.block`. The new `activeBlocker` sync flag is false for stackful
tasks. Subtask cancellation still delivers through `task.requestCancellation`,
with resume treated as owner-aware rather than a new entry.

`partial-stream-copies`: the top-level async-typed no-callback export remains
stackful, not sync implicit. Its waits use the blocker arm. The stream/future
`activeBlocker` changes only add a trap for `syncImplicit`; they do not change
stackful behavior.

`deadlock`: the suite expects
`"wasm trap: deadlock detected: event loop cannot make further progress"`.
That path remains stackful and still goes through `invokeStackful`'s
`sched.drive` deadlock mapping at `async_lift.go:151-155`. Do not convert
stackful sync-opts tasks into sync implicit tasks.

`dont-block-start`: instantiation-time core start functions never reach
`invokeEntered`, so they do not get an implicit task. The existing
`in.sched.instantiating && in.activeTask == nil` branch at `host_import.go:660`
must remain and still traps sync lower to async callee before callee code runs.

`empty-wait`: the `run` export is async-typed no-callback and must stay
`be.stackful` via `graph.go:1808-1814`. It therefore uses stackful blocking and
does not become a sync implicit task.

Sync `.wast` and resources harness: sync exports that do not call async builtins
observe no semantic change beyond a transient non-nil `activeTask` during guest
execution. `stream_host.go:34` already treats any live task as "guest is
running" unless `sched.pumping` or `inHostCall` proves scheduler ownership; that
remains the correct safety guard. Resource validation failures before guest code
still must not poison the instance, preserving the rationale in
`instance.go:1604-1618`.

Oracle and resources tests: the existing oracle harness hand-builds
`&task{inst: in, gt: &guestTask{}, state: taskStarted}` at
`async_oracle_test.go:321`. The zero value `syncImplicit == false` preserves
that behavior. If adding an oracle case for re-entry, add it to
`async_scenarios.json`, regenerate `async_oracle_golden.json` with
`internal/component/abi/testdata/gen_async_oracle.py`, and expect the hash check
in `async_oracle_test.go:122-129` to force the golden update. The current
`.wast` replay is the primary coverage for `trap-on-reenter`.

## Risk Ranking

1. High: Suite B entry lifetime and caller threading. This touches all
   guest-to-guest call paths and the scheduler resume model. The failure mode is
   over-trapping legitimate nested composition calls or under-trapping parked
   re-entry.
2. Medium: making `boundExport.home` apply to sync re-export aliases. This is
   necessary for Suite A, but it changes where sync-entry state lives for nested
   aliases. It should be behaviorally inert except for async builtins because
   `be.mod`, `be.coreFn`, and ABI metadata already point at the real callee.
3. Medium: blocking-site reclassification. Missing one `activeBlocker` call site
   can silently turn a sync implicit block into a nested scheduler drive.
4. Low: per-call `ctxStorage` on implicit sync tasks. It is isolated to the task
   object and has no goroutine or table lifetime.

## Verification Plan

Implementation should unskip only the targeted suites first:

```sh
go test ./internal/component/instance -run TestAsyncWastConformance
```

Then run focused non-regression:

```sh
go test ./internal/component/instance -run 'TestAsync|TestRealResource|TestWast'
go test -race ./internal/component/instance
make test
make check
make lint
```

Do not accept a change that passes `big-interleaving-test` by allowing
sync-implicit tasks to block. That would be a real regression hidden behind a
new task pointer.

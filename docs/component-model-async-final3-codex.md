# Deferred Component Model Async Features

## Shared Runtime Rule

All three features keep the existing single-threaded runtime model: at most one runnable goroutine may own a composition tree at a time. New code must mutate `sched`, task state, waitables, resources, or handles only while running under the scheduler baton or the normal synchronous host call stack.

The only scheduler-core change required is for Feature 1: `guestTask` becomes optionally baton-threaded, like `stackfulTask`, for callback tasks that can suspend with live core frames. The FIFO `sched.runq`, `sched.parked []parkedTask`, `sched.drive`, and `errAsyncDeadlock` model stays intact.

---

## 1. Callback-Task Parking

Risk: highest.

Unblocks:

- `sync-streams`
- `zero-length`
- `async-calls-sync`, specifically `run2`

### Problem

A callback task can block before its callback ABI returns `WAIT` or `YIELD`:

- inside its core body via a sync-lowered async call
- inside a stream/future/waitable builtin that synchronously waits

Today these paths call a nested `sched.drive` while the synchronous caller is still below the Go stack. If the only code that can satisfy the rendezvous is that caller’s continuation, the nested drive cannot converge.

Reference behavior in `definitions.py` uses a real `Thread`; blocking suspends the callee’s thread and returns control to the caller’s continuation. In wazy, callback tasks need the same ability only when they may block with live frames.

### Data Model

Touch:

- `internal/component/instance/guest_task.go`
- `internal/component/instance/task.go`
- `internal/component/instance/async_builtins.go`
- `internal/component/instance/stream_builtins.go`
- `internal/component/instance/host_import.go`
- `internal/component/instance/composition.go`
- `internal/component/instance/async_lift.go`
- `internal/component/instance/stackful_task.go`
- close/reap path in `internal/component/instance/instance.go`

Add a live-frame park state to `guestTask`:

```go
type guestParkKind uint8

const (
	parkNone guestParkKind = iota
	parkEntry
	parkWait       // callback ABI returned WAIT; frame-free
	parkYield      // callback ABI returned YIELD; frame-free
	parkBlock      // live callback/core frame blocked in builtin or sync lower
)

type guestBlockReason uint8

const (
	blockBuiltin guestBlockReason = iota
	blockSyncLower
)

type guestThread struct {
	resumeCh chan resumeMode
	yieldCh  chan struct{}
	spawned  bool
}

type guestTask struct {
	// existing fields...

	park       guestParkKind
	parkReady  func() bool
	parkReason guestBlockReason

	threaded *guestThread
	panicVal any
}
```

`parkBlock` is the distinguishing state. It means the callback task has not returned a callback ABI code. It is suspended with live Go/core frames and is waiting for a predicate, usually its own sync-lowered subtask to resolve. `parkWait` remains the normal callback ABI `WAIT`, after the callback returned to the host and released exclusive access.

### Threaded Guest Task

Add methods:

```go
func (gt *guestTask) startThreaded() error
func (gt *guestTask) firstThreadedRun() error
func (gt *guestTask) threadMain()
func (gt *guestTask) handoff(mode resumeMode) error
func (gt *guestTask) block(ready func() bool, cancellable bool, reason guestBlockReason) (cancelled bool)
func (gt *guestTask) resumeFrameFree(mode resumeMode) error
```

`startThreaded` mirrors `stackfulTask.startTask`:

1. If `tryEnter` fails, set `parkEntry`, attach `threaded`, and `sched.park(gt)`. No goroutine exists yet.
2. Otherwise create `resumeCh`/`yieldCh`, spawn `threadMain`, and `handoff(resumeNormal)`.

`threadMain` owns all execution while resumed:

```go
func (gt *guestTask) threadMain() {
	defer func() {
		if v := recover(); v != nil {
			if v != errGuestTaskAbort {
				gt.panicVal = v
			}
		}
		gt.done = true
		gt.threaded.yieldCh <- struct{}{}
	}()

	mode := <-gt.threaded.resumeCh
	if mode == resumeAbort {
		return
	}

	if err := gt.firstRun(); err != nil {
		gt.fail(err)
		return
	}

	for !gt.done {
		gt.threaded.yieldCh <- struct{}{}
		mode = <-gt.threaded.resumeCh
		if mode == resumeAbort {
			return
		}
		if err := gt.resumeFrameFree(mode); err != nil {
			gt.fail(err)
			return
		}
	}
}
```

`gt.block` is called from a blocking builtin or sync-lower wait while the threaded callback goroutine is inside `CallWithStack` or callback execution:

```go
func (gt *guestTask) block(ready func() bool, cancellable bool, reason guestBlockReason) bool {
	if gt.t.deliverPendingCancel(cancellable) {
		return true
	}

	gt.park = parkBlock
	gt.parkReady = ready
	gt.parkReason = reason
	gt.cancellable = cancellable

	gt.in.suspendRun() // activeTask=nil, mayEnter=true, exclusive remains owned by gt.t
	gt.in.sched.park(gt)

	gt.threaded.yieldCh <- struct{}{}

	mode := <-gt.threaded.resumeCh
	if mode == resumeAbort {
		panic(errGuestTaskAbort)
	}

	gt.park = parkNone
	gt.parkReady = nil
	gt.cancellable = false
	gt.in.enterRun(gt.t)

	return mode == resumeCancelled
}
```

`parkBlock` intentionally keeps exclusive ownership, matching the reference: callback ABI `WAIT`/`YIELD` releases exclusive in the callback loop, but blocking while still inside the core thread does not.

### Readiness and Resume

Update `guestTask.ready`:

```go
func (gt *guestTask) ready() bool {
	heldByOther := gt.in.exclusiveHeld && gt.in.exclusiveOwner != gt.t

	switch gt.park {
	case parkEntry:
		return !gt.in.hasBackpressure()
	case parkWait:
		return !heldByOther &&
			(gt.cancelWake || gt.t.cancelDeliverable() || gt.wset.hasPendingEvent())
	case parkYield:
		return !heldByOther
	case parkBlock:
		return !heldByOther &&
			(gt.cancelWake || gt.t.cancelDeliverable() || gt.parkReady())
	default:
		return false
	}
}
```

Update `guestTask.resumeReady`:

- `parkEntry`: unpark, cancel unentered if needed, then call `firstThreadedRun` if `threaded != nil`; otherwise existing `firstRun`.
- `parkWait`/`parkYield`: unpark and either resume directly for frame-free tasks or `handoff(mode)` for threaded tasks.
- `parkBlock`: unpark and `handoff(resumeNormal)` or `handoff(resumeCancelled)`.

Cancellation must treat self-held exclusive like `stackfulTask`:

```go
heldByOther := t.inst.exclusiveHeld && t.inst.exclusiveOwner != t
if t.gt != nil && t.gt.park != parkNone && t.gt.cancellable &&
	!heldByOther && t.inst.mayEnter {
	t.gt.cancelWake = true
	return t.gt.resumeReady()
}
```

### Blocking Builtins

Add a shared blocker helper:

```go
type activeBlocker interface {
	block(ready func() bool, cancellable bool, reason guestBlockReason) bool
}

func activeBlockingTask(in *Instance) activeBlocker {
	if t := in.activeTask; t != nil {
		if t.st != nil {
			return t.st
		}
		if t.gt != nil && t.gt.threaded != nil {
			return t.gt
		}
	}
	return nil
}
```

Keep taskless sync calls trapping.

Replace callback nested-drive paths in these builtins when `activeBlockingTask` returns a threaded `guestTask`:

- `waitable-set.wait`
- sync `stream.copy`
- sync `future.copy`
- sync `stream.cancel-write` / `future.cancel-write`
- sync `subtask.cancel`

For a threaded callback:

```go
cancelled := gt.block(predicate, cancellable, blockBuiltin)
if cancelled {
	// deliver cancellation event/state exactly as the existing stackful path does
}
```

For non-threaded callback tasks, keep the current nested `sched.drive` fallback. That preserves host-entry callback behavior and avoids spawning goroutines for simple direct invocations.

### Sync-Lowered Guest Imports

Generalize `startDelegatedFromStackful` into a task-aware helper:

```go
func startDelegatedSyncLower(
	ctx context.Context,
	caller *task,
	tgt *guestAsyncTarget,
	args []abi.Value,
) ([]abi.Value, error)
```

Control flow:

1. Map importer args to provider reps with `repToProviderHandle`.
2. Start the provider task with `tgt.sub.startAsyncExportTask`.
3. `onResolve` maps provider results back with `providerHandleToRep`, stores them, and flips `resolved=true`.
4. If `resolved`, return immediately. The callee may still be parked after `task.return`; that is correct.
5. If unresolved:
   - `caller.st != nil`: `caller.st.block(func() bool { return resolved }, false)`
   - `caller.gt != nil && caller.gt.threaded != nil`: `caller.gt.block(func() bool { return resolved }, false, blockSyncLower)`
   - otherwise fall back to `tgt.sub.sched.drive(func() bool { return resolved })`

Update `buildHostWrapper`:

```go
if hi.asyncTarget != nil && targetIsAsync {
	if t := in.activeTask; t != nil && (t.st != nil || (t.gt != nil && t.gt.threaded != nil)) {
		results, err := startDelegatedSyncLower(ctx, t, hi.asyncTarget, args)
		// lower results into caller memory as today
	}
}
```

### Starting Callback Tasks

Change `startAsyncExportTask` callback branch to use `startThreaded`:

```go
gt := &guestTask{...}
t.gt = gt
if err := gt.startThreaded(); err != nil {
	return nil, err
}
```

`invokeAsyncCallback` should continue using frame-free `gt.start()` for host-entered callback exports. The official blockers are guest-to-guest starts and threaded callback callers. If a future suite proves host-entry callbacks also need live-frame parking, switch `invokeAsyncCallback` to `startThreaded` too.

### No Hang and No Leak

The non-converging nested drive disappears from the self-caller cases. A blocked callback task parks and yields the baton, allowing its synchronous caller’s continuation to run. If no queued thunk or parked task can progress, the outer `sched.drive` still returns `errAsyncDeadlock`.

Add a shared reaper:

```go
func (in *Instance) reapParkedTaskGoroutines()
```

It must abort both:

- `*stackfulTask` with spawned goroutine
- `*guestTask` with `threaded != nil && threaded.spawned`

Abort protocol is `resumeAbort`, wait for `yieldCh`, then unpark. `parkEntry` tasks without a goroutine are simply marked done/unparked. Call this from `Instance.Close` and from invoke error paths that currently call `reapStackful`.

### Race Safety

The baton protocol gives a happens-before edge:

- scheduler goroutine sends `resumeCh`
- task goroutine mutates runtime state
- task goroutine sends `yieldCh`
- scheduler resumes

No locks are added. No runtime state is accessed concurrently because the scheduler blocks inside `handoff` while the task goroutine runs.

### Trap and Trace Coverage

Trap edges:

- taskless sync blocking still traps `"cannot block a synchronous task before returning"`
- unresolved wait with empty run queue still traps deadlock via `errAsyncDeadlock`
- cancellable `parkBlock` resumes with cancellation; non-cancellable sync lowers only record pending cancellation
- abort during close must not surface as user trap
- panic from guest goroutine is propagated by `handoff`

Trace-oracle additions:

- `callback-sync-lower-block`: async callback task sync-lowers to another async callee, parks with `parkBlock/blockSyncLower`, caller continues, later callee resolves, parked callback resumes.
- `callback-stream-rendezvous`: producer returns a stream, parks on write, parent/consumer reads, producer receives stream event and exits.

Verification:

```sh
go test ./internal/integration_test/... -run 'TestComponentModelAsyncWast/(sync-streams|zero-length|async-calls-sync)'
go test -race ./internal/component/instance/... -run 'Callback.*Parking|Async.*Oracle'
```

---

## 2. Cross-Instance Realloc

Risk: lowest.

Unblocks:

- `cross-abi-calls`

### Problem

A lifted guest export can have its core function in one core instance and its canonical memory/realloc option in another. Current `graph.go` rejects that. The callee’s parameter lowering and result lifting must use the lift’s memory/realloc module, not the core-function module and not the caller’s module.

### Data Model

Touch:

- `internal/component/instance/instance.go`
- `internal/component/instance/graph.go`
- `internal/component/instance/guest_task.go`
- `internal/component/instance/stackful_task.go`

Extend `boundExport`:

```go
type boundExport struct {
	mod api.Module // core function module

	liftMemMod api.Module // canon lift memory option; nil means mod

	reallocMod      api.Module // canon lift realloc target; nil means liftMemMod/mod
	reallocFuncName string
	reallocFn       api.Function
	reallocCall     abi.Realloc

	// existing fields...
}

func (be *boundExport) memoryModule() api.Module {
	if be.liftMemMod != nil {
		return be.liftMemMod
	}
	return be.mod
}
```

### Graph Binding

Replace the cross-instance rejection in `resolveReallocFuncGraph`.

Binding should:

1. Resolve the lifted core function module as today into `be.mod`.
2. Resolve the canon lift memory option into `be.liftMemMod`.
3. Resolve the canon lift realloc function target into `be.reallocMod` and `be.reallocFuncName`.
4. Do not require `reallocMod == be.mod`.

Keep existing same-module restrictions for callback and post-return unless a separate suite requires relaxing them.

`finalizeBoundExport` resolves realloc from the explicit realloc module:

```go
func finalizeBoundExport(be *boundExport) {
	be.coreFn = be.mod.ExportedFunction(be.funcName)

	reallocMod := be.reallocMod
	if reallocMod == nil {
		reallocMod = be.liftMemMod
	}
	if reallocMod == nil {
		reallocMod = be.mod
	}

	name := be.reallocFuncName
	if name == "" {
		name = "cabi_realloc"
	}

	be.reallocFn = reallocMod.ExportedFunction(name)
	be.reallocCall = coreReallocCall(be.reallocFn)
}
```

### Call Sites

Use `be.memoryModule()` everywhere a lifted export accesses canonical ABI memory:

```go
mem, memOK := memoryBytesOf(be.memoryModule())
realloc := cachedReallocOf(ctx, be)
```

Update:

- sync `Instance.invoke`
- `guestTask.firstRun`
- `stackfulTask.run`

Do not change caller-side lower wrappers:

- `buildHostWrapper`
- `buildAsyncHostWrapper`

Those already use `canonMemoryAndRealloc` for the importing caller’s memory. The provider side is fixed by making `sub.invoke` and `startAsyncExportTask` use the provider `boundExport` memory/realloc.

### Sync and Async Delegation

Both paths work after the provider `boundExport` is corrected:

- sync delegated import: `delegatingHostImport.fn -> sub.invoke`
- async delegated import: `buildAsyncHostWrapper -> sub.startAsyncExportTask`
- sync-lowered async import after Feature 1: `startDelegatedSyncLower -> sub.startAsyncExportTask`

The caller lowers results into caller memory. The callee allocates spills/results in callee lift memory.

### Race, Hang, Leak

No goroutines or scheduler changes. All added state is immutable after instantiation.

### Trap and Trace Coverage

Trap edges:

- missing lift memory still traps through existing memory checks
- missing realloc function traps when `cachedReallocOf` is needed
- realloc trap propagates with existing call context
- post-return/callback cross-instance mismatches remain rejected

Trace-oracle addition:

- `cross-instance-realloc`: callee core function module has no memory allocation target; lift memory/realloc module records the allocation. Assert the caller receives the expected value and allocation happened in the callee lift memory.

Verification:

```sh
go test ./internal/integration_test/... -run 'TestComponentModelAsyncWast/cross-abi-calls'
go test ./internal/component/instance/... -run 'CrossInstanceRealloc|Graph'
```

---

## 3. Per-Element Resource Transfer In Streams

Risk: medium.

Unblocks:

- `passing-resources`

### Problem

`stream<own<R>>` is currently rejected. Reference semantics allow `own<R>` elements and transfer ownership on each copied element:

1. lift element from writer memory
2. remove own handle from writer table
3. lower element into reader memory
4. mint own handle in reader table

Borrow elements remain invalid for streams/futures.

### Binding Rules

Touch:

- `internal/component/instance/stream.go`
- `internal/component/instance/stream_builtins.go`
- host stream/future construction code if separate

Replace the current resource ceiling:

```go
func elemContainsBorrowHandle(t binary.TypeDesc, resolve abi.Resolver) bool
func elemContainsOwnHandle(t binary.TypeDesc, resolve abi.Resolver) bool
```

`resolveStreamOrFutureElem` should reject borrow only:

```go
if elemContainsBorrowHandle(elem, resolve) {
	return errorf("stream/future element type contains a borrow resource handle")
}
```

`own<R>` is allowed and forces the non-numeric copy path.

### Buffer Metadata

`copyElements` needs both source and destination ownership context.

Extend buffers:

```go
type copyBuffer interface {
	read(n uint32) ([]abi.Value, error)
	write(vs []abi.Value) error
	progress() uint32

	owner() *Instance
	elemDesc() binary.TypeDesc
	resolve() abi.Resolver
}

type guestBuffer struct {
	ownerInst *Instance
	elem      binary.TypeDesc
	res       abi.Resolver
	// existing fields...
}

func (b *guestBuffer) owner() *Instance          { return b.ownerInst }
func (b *guestBuffer) elemDesc() binary.TypeDesc { return b.elem }
func (b *guestBuffer) resolve() abi.Resolver     { return b.res }
```

Host buffers return `owner()==nil`. For host resource streams, host values are resource reps; guest buffers mint/take table handles at the edge.

`sharedStream` and `sharedFuture` should record the instance that created the element descriptor:

```go
type sharedStream struct {
	elemOwner *Instance
	elem      binary.TypeDesc
	elemOwns  bool
	numeric   bool
	// existing fields...
}
```

Element compatibility should compare resource identities through canonical resource tags, not raw local type indices:

```go
func sameStreamElem(aOwner *Instance, a binary.TypeDesc, bOwner *Instance, b binary.TypeDesc) bool
```

Use this in readable/writable-end validation instead of plain `reflect.DeepEqual` for resource-containing element types.

### Transfer Walker

Add:

```go
func transferStreamElement(
	srcOwner *Instance,
	srcType binary.TypeDesc,
	srcResolve abi.Resolver,
	dstOwner *Instance,
	dstType binary.TypeDesc,
	dstResolve abi.Resolver,
	v abi.Value,
) (abi.Value, error)
```

For `own<R>` leaves:

```go
func transferOwnLeaf(srcOwner, dstOwner *Instance, srcRT, dstRT uint32, v abi.Value) (abi.Value, error) {
	rep := uint32(v)

	if srcOwner != nil {
		srcTag := srcOwner.canonTag(srcRT)
		var err error
		rep, err = srcOwner.resources.TakeOwn(srcTag, rep)
		if err != nil {
			return nil, err
		}
	}

	if dstOwner != nil {
		dstTag := dstOwner.canonTag(dstRT)
		return abi.Value(dstOwner.resources.NewOwn(dstTag, rep)), nil
	}

	return abi.Value(rep), nil
}
```

The walker recurses through records, tuples, variants, options, results, lists, and flags using the same value shapes as `resolveArgHandlesDepth`. Borrow leaves return an internal error because bind-time validation should have rejected them.

Before transferring, verify `srcOwner.canonTag(srcRT) == dstOwner.canonTag(dstRT)` when both owners are non-nil. Otherwise trap with a resource type mismatch.

### Copy Loop

Change:

```go
func copyElements(numeric bool, dst, src copyBuffer, n uint32) error
```

to:

```go
func copyElements(numeric bool, elemOwns bool, dst, src copyBuffer, n uint32) error {
	if numeric {
		if d, ok := dst.(*guestBuffer); ok {
			if s, ok := src.(*guestBuffer); ok {
				return memmoveElements(d, s, n)
			}
		}
	}

	if !elemOwns {
		vs, err := src.read(n)
		if err != nil {
			return err
		}
		return dst.write(vs)
	}

	for i := uint32(0); i < n; i++ {
		vs, err := src.read(1)
		if err != nil {
			return err
		}

		v, err := transferStreamElement(
			src.owner(), src.elemDesc(), src.resolve(),
			dst.owner(), dst.elemDesc(), dst.resolve(),
			vs[0],
		)
		if err != nil {
			return err
		}

		if err := dst.write([]abi.Value{v}); err != nil {
			return err
		}
	}

	return nil
}
```

This preserves the numeric fast path. Resource elements are never numeric, so they always use the per-element path.

### Semantics

For `stream<own<R>>`:

- copied elements are removed from the writer handle table
- copied elements are minted into the reader handle table
- uncopied elements stay owned by the writer
- progress counts remain element counts, not byte counts
- same-instance non-numeric copy trap remains unchanged

For `stream<borrow<R>>` and `future<borrow<R>>`, reject at bind time.

### Race, Hang, Leak

No goroutines or scheduler changes. Handle table transfer happens inside the active scheduled task while the baton is held.

### Trap and Trace Coverage

Trap edges:

- unknown source handle
- source handle type mismatch
- source own handle currently lent
- resource type mismatch between writer and reader element descriptors
- borrow element rejected at bind time
- same-instance non-numeric copy trap remains

Trace-oracle addition:

- `stream-own-transfer-partial`: writer owns two handles, reader copies one element, assert writer no longer has the first handle, writer still owns the second, reader owns a new handle for the first rep, and progress is `1`.
- Add direct assertion matching `passing-resources`: after the copy, producer access to old handle `3` traps `"unknown handle index 3"`.

Verification:

```sh
go test ./internal/integration_test/... -run 'TestComponentModelAsyncWast/passing-resources'
go test ./internal/component/instance/... -run 'Stream.*Resource|PassingResources|Async.*Oracle'
```

---

## Risk Ranking

1. Callback-task parking: highest. It changes live-frame suspension, cancellation, sync-lowered async calls, and goroutine reaping.
2. Per-element resource transfer in streams: medium. It changes ownership side effects and resource-type equality for stream/future elements.
3. Cross-instance realloc: lowest. It is mostly immutable graph-binding metadata and call-site plumbing.

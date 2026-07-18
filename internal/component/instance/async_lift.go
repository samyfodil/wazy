package instance

import (
	"context"
	"errors"
	"fmt"

	"github.com/samyfodil/wazy/internal/component/abi"
)

// callbackCode is the reference's CallbackCode (definitions.py ~2205): the
// low nibble of a callback-lift core func's packed i32 return value.
type callbackCode uint32

const (
	callbackExit  callbackCode = 0
	callbackYield callbackCode = 1
	callbackWait  callbackCode = 2
	callbackMax   callbackCode = 2
)

// callbackFrame co-allocates a task and its guestTask in one heap object:
// every callback-lift construction site (invokeAsyncCallback,
// startAsyncExportTask) always builds exactly one task+guestTask pair, so
// giving them separate `&task{}`/`&guestTask{}` allocations doubles the
// per-call malloc count for no reason -- t and gt remain independently
// addressable (*task/*guestTask are threaded all over this package) and
// their pointers stay stable for the frame's lifetime; co-allocation only
// means one malloc backs both, freed together once both become unreachable.
type callbackFrame struct {
	t  task
	gt guestTask
}

// unpackCallbackResult is the reference's unpack_callback_result: code is
// the packed value's low nibble (trap_if(code > MAX)); the waitable-set
// index is the rest, shifted down. The reference also asserts
// Table.MAX_LENGTH < 2^28 so the shift can never be ambiguous; handleTable
// is nowhere near that size, so it is not separately re-checked here.
func unpackCallbackResult(packed uint32) (callbackCode, uint32, error) {
	code := packed & 0xf
	if code > uint32(callbackMax) {
		return 0, 0, fmt.Errorf("unsupported callback code %d (packed %#x)", code, packed)
	}
	return callbackCode(code), packed >> 4, nil
}

// invokeAsyncCallback is invoke's counterpart for an async lift with a
// callback (CanonOpt kind 0x06 async + 0x07 callback): the canon_lift
// callback-loop driver (definitions.py ~2172-2197), transliterated for wazy
// -- see docs/component-model-async-runtime-design.md §1.3 (Phase 1/2) and
// docs/component-model-async-phase3-design.md §1.4 (Phase 3: the loop body
// now lives on guestTask, guest_task.go, as a parkable state machine instead
// of a Go-frame-held loop; this func starts one and drives the instance's
// scheduler until it's done -- bit-identical host-entry behavior to Phase
// 1/2 for every export that never actually parks past its own drive, and
// for one that does, the SAME FIFO drive that used to run nested now runs
// here instead).
func (in *Instance) invokeAsyncCallback(ctx context.Context, be *boundExport, exportName string, args []abi.Value) ([]abi.Value, error) {
	fd := be.fd
	if len(args) != len(fd.Params) {
		return nil, fmt.Errorf("component/instance: export %q takes %d parameter(s), got %d", exportName, len(fd.Params), len(args))
	}
	if be.coreFn == nil {
		return nil, fmt.Errorf("component/instance: core module has no exported function %q (referenced by canon lift for export %q)", be.funcName, exportName)
	}
	if be.callbackFn == nil {
		return nil, fmt.Errorf("component/instance: core module has no exported function %q (referenced by canon lift's callback option for export %q)", be.callbackFuncName, exportName)
	}
	if be.flattenErr != nil {
		return nil, fmt.Errorf("component/instance: export %q: flatten func type: %w", exportName, be.flattenErr)
	}
	if be.coreResultCount < 1 {
		return nil, fmt.Errorf("component/instance: export %q: core func %q returns %d value(s); an async lift with a callback must return a single packed i32", exportName, be.funcName, be.coreResultCount)
	}

	// Store.lift's prologue: trap_if(!may_enter_from). Unlike Phase 1/2,
	// mayEnter is NOT held false for the whole call below -- it now brackets
	// only the guest code actually being on the stack (task.go's
	// enterRun/leaveRun), so a second entry into THIS instance while this
	// task is merely parked (not running) can succeed, exactly as the
	// reference allows. poisoned, unlike mayEnter, is sticky (see its doc on
	// Instance): once any earlier call's guestTask/stackfulTask.fail has run,
	// every later entry is refused permanently, matching the reference's
	// Store poisoning invariant.
	if in.poisoned || !in.mayEnter {
		return nil, fmt.Errorf("component/instance: export %q: cannot enter component instance", exportName)
	}

	// t.onResolve/gt.onStart are left nil: task.returnValues/cancelResolve
	// and guestTask.firstRun both fall back to writing straight into
	// t.result/t.cancelled and reading gt.args directly (see their docs) --
	// args is already a lifted component-level value list by the time this
	// host-entry call runs (no lazy-lift indirection needed), so the
	// trivial forwarding closures Phase 1/2 built here would only exist to
	// adapt a shape this call never actually needs.
	fr := &callbackFrame{}
	t, gt := &fr.t, &fr.gt
	t.inst, t.be = in, be
	gt.t, gt.in, gt.be, gt.ctx, gt.exportName = t, in, be, ctx, exportName
	gt.args = args
	t.gt = gt

	if err := gt.start(); err != nil {
		in.reapParkedGoroutines() // a trap may strand OTHER parked promoted segments (delegated callees)
		return nil, err
	}
	if err := in.sched.drive(func() bool { return gt.done }); err != nil {
		in.reapParkedGoroutines()
		if errors.Is(err, errAsyncDeadlock) {
			return nil, fmt.Errorf("component/instance: export %q: deadlock: the async task is suspended but the run queue is empty and no parked task is ready; an import resolved externally requires CallAsync (not yet implemented)", exportName)
		}
		return nil, err
	}
	if gt.err != nil {
		in.reapParkedGoroutines()
		return nil, gt.err
	}
	// Feature 1 (docs/component-model-async-final3-fable.md §1.5): drain
	// every already-ready parked task to quiescence before returning --
	// e.g. sync-streams' promoted $C.get/$C.set run to EXIT (and their
	// segment goroutines exit) here, not left dangling past this invoke's
	// return. A no-op whenever nothing is ready.
	if err := in.sched.drainReady(); err != nil {
		in.reapParkedGoroutines()
		return nil, err
	}
	return t.result, nil
}

// invokeStackful is invoke's counterpart for an async-TYPED lift with NO
// callback option (docs/component-model-async-stackful-design.md §6.1): the
// canon_lift no-callback body, started as a *stackfulTask (stackful_task.go)
// and driven to completion via the shared scheduler exactly like
// invokeAsyncCallback drives a *guestTask -- the only difference is that a
// stackful task's suspensions are a real parked goroutine rather than
// returned-and-remembered Go state.
func (in *Instance) invokeStackful(ctx context.Context, be *boundExport, exportName string, args []abi.Value) ([]abi.Value, error) {
	fd := be.fd
	if len(args) != len(fd.Params) {
		return nil, fmt.Errorf("component/instance: export %q takes %d parameter(s), got %d", exportName, len(fd.Params), len(args))
	}
	if be.coreFn == nil {
		return nil, fmt.Errorf("component/instance: core module has no exported function %q (referenced by canon lift for export %q)", be.funcName, exportName)
	}
	if be.flattenErr != nil {
		return nil, fmt.Errorf("component/instance: export %q: flatten func type: %w", exportName, be.flattenErr)
	}
	if in.poisoned || !in.mayEnter { // Store.lift's prologue: trap_if(!may_enter_from)
		return nil, fmt.Errorf("component/instance: export %q: cannot enter component instance", exportName)
	}

	t := &task{inst: in, be: be}
	st := &stackfulTask{
		t: t, in: in, be: be, ctx: ctx, exportName: exportName,
		asyncOpts: be.stackfulAsyncOpts, args: args,
	}
	t.st = st

	if err := st.startTask(); err != nil {
		in.reapParkedGoroutines() // a trap may strand OTHER parked tasks (delegated callees)
		return nil, err
	}
	if err := in.sched.drive(func() bool { return st.done }); err != nil {
		in.reapParkedGoroutines()
		if errors.Is(err, errAsyncDeadlock) {
			return nil, fmt.Errorf("component/instance: export %q: wasm trap: deadlock detected: event loop cannot make further progress", exportName)
		}
		return nil, err
	}
	if st.err != nil {
		in.reapParkedGoroutines()
		return nil, st.err
	}
	// See invokeAsyncCallback's identical drainReady call.
	if err := in.sched.drainReady(); err != nil {
		in.reapParkedGoroutines()
		return nil, err
	}
	return t.result, nil
}

// startAsyncExportTask starts be (an async callback lift of THIS instance)
// as a guestTask whose results flow to onResolve instead of a host caller
// (Phase 3 guest<->guest async lower, docs/component-model-async-phase3-
// design.md §3.2): the Store.lift func_inst + canon_lift front half
// (~587-595 + ~2144-2207) on the CALLEE instance -- the same machinery
// invokeAsyncCallback sits on, minus the drive (this instance's *sched is
// shared with the whole composition tree -- Instance.sched's doc -- so
// whoever eventually drives that shared scheduler to completion resumes
// this task; it is never driven here). Returns the started task itself
// (not a bound requestCancellation method value -- avoids a per-call alloc
// on this guest<->guest path); the caller stores it as the subtask's
// cancelTask and calls calleeTask.requestCancellation() from
// subtask.runOnCancel (reference ~2207: subtask.on_cancel = callee(...)).
func (in *Instance) startAsyncExportTask(ctx context.Context, be *boundExport, exportName string,
	onStart func(*task) ([]abi.Value, error), onResolve func([]abi.Value, bool) error,
) (calleeTask *task, err error) {
	// Dispatch on the callee's shape (docs/component-model-async-stackful-
	// design.md §6.2): a STACKFUL callee (no callback option) must be
	// driven through stackfulTask -- its inline waitable-set.wait (or any
	// other blocking builtin) can only be satisfied by genuinely parking
	// the goroutine, not by guestTask's callback-loop machinery, which
	// would misinterpret the export's real result as a packed callback
	// code the moment its core code ever returns.
	if be.stackful {
		return in.startStackfulExportTask(ctx, be, exportName, onStart, onResolve)
	}
	// trap_if(not inst.may_enter_from(caller)) (~590): mayEnter is true here
	// in every reachable interleaving (the caller's own guest code is
	// running, so by construction it cannot also be running on THIS
	// instance unless this instance IS the caller re-entering itself --
	// exactly the case mayEnter guards), kept as a real trap for parity.
	if in.poisoned || !in.mayEnter {
		return nil, fmt.Errorf("component/instance: export %q: cannot enter component instance", exportName)
	}
	fr := &callbackFrame{}
	t, gt := &fr.t, &fr.gt
	t.inst, t.be = in, be
	t.onResolve = func(vals []abi.Value, cancelled bool) {
		if e := onResolve(vals, cancelled); e != nil {
			panic(fmt.Errorf("component/instance: export %q: %w", exportName, e))
		}
	}
	gt.t, gt.in, gt.be, gt.ctx, gt.exportName = t, in, be, ctx, exportName
	gt.onStart = func() ([]abi.Value, error) { return onStart(t) }
	// Feature 1 (docs/component-model-async-final3-fable.md §1.1): a
	// GUEST-caller-started callback task (this func, never
	// invokeAsyncCallback's host-entry call) on an instance flagged
	// mayBlockSync runs its core/callback invocations on a per-segment
	// goroutine, so a mid-core-call blocking site reached from this
	// task's own guest code can park (gt.block) instead of nested-
	// driving the shared scheduler -- the fix for a callback task
	// blocking on its own sync caller's continuation (sync-streams,
	// async-calls-sync run2). Instances that never bind a sync
	// stream/future copy or a sync-lowered-to-async-lift import
	// (mayBlockSync stays false -- the overwhelmingly common case) pay
	// zero goroutines: promoted is false and runSegment/runLoop take
	// their exact pre-Feature-1 inline path.
	gt.promoted = in.mayBlockSync
	t.gt = gt
	if err := gt.start(); err != nil { // may run to EXIT, may park at entry/WAIT
		return nil, err
	}
	return t, nil
}

// startStackfulExportTask is startAsyncExportTask with guestTask swapped for
// stackfulTask (docs/component-model-async-stackful-design.md §6.2): same
// mayEnter trap, same t.onResolve panic-bridge, st.onStart adapting
// onStart(t), then st.startTask() -- may run to completion inline on the
// caller's goroutine (the immediate path async_host_import.go's
// buildAsyncHostWrapper already handles via st.resolved(), read off
// t.state == taskResolved by the caller) or park at sparkEntry/sparkBlock,
// returning t (the caller runs t.requestCancellation as onCancel). Result
// values flow through task.returnValues -> t.onResolve -> the wrapper's own
// onResolve closure -- for a sync-opts callee it's run() itself that calls
// returnValues after lifting flat results, so the wrapper needs zero
// changes.
func (in *Instance) startStackfulExportTask(ctx context.Context, be *boundExport, exportName string,
	onStart func(*task) ([]abi.Value, error), onResolve func([]abi.Value, bool) error,
) (calleeTask *task, err error) {
	if in.poisoned || !in.mayEnter {
		return nil, fmt.Errorf("component/instance: export %q: cannot enter component instance", exportName)
	}
	t := &task{inst: in, be: be}
	t.onResolve = func(vals []abi.Value, cancelled bool) {
		if e := onResolve(vals, cancelled); e != nil {
			panic(fmt.Errorf("component/instance: export %q: %w", exportName, e))
		}
	}
	st := &stackfulTask{
		t: t, in: in, be: be, ctx: ctx, exportName: exportName,
		asyncOpts: be.stackfulAsyncOpts,
		onStart:   func() ([]abi.Value, error) { return onStart(t) },
	}
	t.st = st
	if err := st.startTask(); err != nil { // may run to completion, may park at sparkEntry/sparkBlock
		return nil, err
	}
	return t, nil
}

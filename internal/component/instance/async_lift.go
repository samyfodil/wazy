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

// unpackCallbackResult is the reference's unpack_callback_result: code is
// the packed value's low nibble (trap_if(code > MAX)); the waitable-set
// index is the rest, shifted down. The reference also asserts
// Table.MAX_LENGTH < 2^28 so the shift can never be ambiguous; handleTable
// is nowhere near that size, so it is not separately re-checked here.
func unpackCallbackResult(packed uint32) (callbackCode, uint32, error) {
	code := packed & 0xf
	if code > uint32(callbackMax) {
		return 0, 0, fmt.Errorf("invalid callback code %d (packed %#x)", code, packed)
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
	// reference allows.
	if !in.mayEnter {
		return nil, fmt.Errorf("component/instance: export %q: instance is not reenterable", exportName)
	}

	// t.onResolve/gt.onStart are left nil: task.returnValues/cancelResolve
	// and guestTask.firstRun both fall back to writing straight into
	// t.result/t.cancelled and reading gt.args directly (see their docs) --
	// args is already a lifted component-level value list by the time this
	// host-entry call runs (no lazy-lift indirection needed), so the
	// trivial forwarding closures Phase 1/2 built here would only exist to
	// adapt a shape this call never actually needs.
	t := &task{inst: in, be: be}
	gt := &guestTask{
		t: t, in: in, be: be, ctx: ctx, exportName: exportName,
		args: args,
	}
	t.gt = gt

	if err := gt.start(); err != nil {
		return nil, err
	}
	if err := in.sched.drive(func() bool { return gt.done }); err != nil {
		if errors.Is(err, errAsyncDeadlock) {
			return nil, fmt.Errorf("component/instance: export %q: deadlock: the async task is suspended but the run queue is empty and no parked task is ready; an import resolved externally requires CallAsync (not yet implemented)", exportName)
		}
		return nil, err
	}
	if gt.err != nil {
		return nil, gt.err
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
	if !in.mayEnter { // Store.lift's prologue: trap_if(!may_enter_from)
		return nil, fmt.Errorf("component/instance: export %q: instance is not reenterable", exportName)
	}

	t := &task{inst: in, be: be}
	st := &stackfulTask{
		t: t, in: in, be: be, ctx: ctx, exportName: exportName,
		asyncOpts: be.stackfulAsyncOpts, args: args,
	}
	t.st = st

	if err := st.startTask(); err != nil {
		in.reapStackful() // a trap may strand OTHER parked stackful tasks (delegated callees)
		return nil, err
	}
	if err := in.sched.drive(func() bool { return st.done }); err != nil {
		in.reapStackful()
		if errors.Is(err, errAsyncDeadlock) {
			return nil, fmt.Errorf("component/instance: export %q: wasm trap: deadlock detected: event loop cannot make further progress", exportName)
		}
		return nil, err
	}
	if st.err != nil {
		in.reapStackful()
		return nil, st.err
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
// this task; it is never driven here). Returns the task's
// requestCancellation as the caller's subtask.on_cancel (reference ~2207).
func (in *Instance) startAsyncExportTask(ctx context.Context, be *boundExport, exportName string,
	onStart func(*task) ([]abi.Value, error), onResolve func([]abi.Value, bool) error,
) (onCancel func() error, err error) {
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
	if !in.mayEnter {
		return nil, fmt.Errorf("component/instance: export %q: instance is not reenterable", exportName)
	}
	t := &task{inst: in, be: be}
	t.onResolve = func(vals []abi.Value, cancelled bool) {
		if e := onResolve(vals, cancelled); e != nil {
			panic(fmt.Errorf("component/instance: export %q: %w", exportName, e))
		}
	}
	gt := &guestTask{
		t: t, in: in, be: be, ctx: ctx, exportName: exportName,
		onStart: func() ([]abi.Value, error) { return onStart(t) },
	}
	t.gt = gt
	if err := gt.start(); err != nil { // may run to EXIT, may park at entry/WAIT
		return nil, err
	}
	return t.requestCancellation, nil
}

// startStackfulExportTask is startAsyncExportTask with guestTask swapped for
// stackfulTask (docs/component-model-async-stackful-design.md §6.2): same
// mayEnter trap, same t.onResolve panic-bridge, st.onStart adapting
// onStart(t), then st.startTask() -- may run to completion inline on the
// caller's goroutine (the immediate path async_host_import.go's
// buildAsyncHostWrapper already handles via st.resolved(), read off
// t.state == taskResolved by the caller) or park at sparkEntry/sparkBlock,
// returning t.requestCancellation as onCancel. Result values flow through
// task.returnValues -> t.onResolve -> the wrapper's own onResolve closure --
// for a sync-opts callee it's run() itself that calls returnValues after
// lifting flat results, so the wrapper needs zero changes.
func (in *Instance) startStackfulExportTask(ctx context.Context, be *boundExport, exportName string,
	onStart func(*task) ([]abi.Value, error), onResolve func([]abi.Value, bool) error,
) (onCancel func() error, err error) {
	if !in.mayEnter {
		return nil, fmt.Errorf("component/instance: export %q: instance is not reenterable", exportName)
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
	return t.requestCancellation, nil
}

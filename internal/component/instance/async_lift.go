package instance

import (
	"context"
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
// callback-loop driver (definitions.py ~2172-2197), transliterated for
// wazy's single-active-task MVP -- see
// docs/component-model-async-runtime-design.md §1.3.
//
// Only the degenerate path this milestone targets is exercised end to end:
// a callback that returns EXIT on its very first call, after the core
// func's FIRST call has already invoked task.return. WAIT is decoded and
// dispatched but always fails loud -- waitable sets and the scheduler drive
// that would satisfy a WAIT land in Phase 1c.
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

	// Store.lift's prologue: trap_if(!may_enter_from) + enter_from/leave_to.
	if !in.mayEnter {
		return nil, fmt.Errorf("component/instance: export %q: instance is not reenterable", exportName)
	}
	in.mayEnter = false
	defer func() { in.mayEnter = true }()

	t := &task{inst: in, be: be}
	t.onResolve = func(vals []abi.Value) { t.result = vals }
	if err := in.enterTask(t); err != nil {
		return nil, fmt.Errorf("component/instance: export %q: %w", exportName, err)
	}
	defer in.exitTask()

	mem, memAvailable := memoryBytesOf(be.mod)
	realloc := cachedReallocOf(ctx, be)

	// on_start: lower params exactly as the sync path (invoke's identical
	// pooling discipline -- see its doc for why this is safe to pool).
	coreArgsPtr := coreValueSlicePool.Get().(*[]abi.CoreValue)
	*coreArgsPtr = (*coreArgsPtr)[:0]
	t.state = taskStarted
	coreArgs, err := in.lowerParams(be, args, mem, memAvailable, realloc, exportName, *coreArgsPtr)
	if err != nil {
		coreValueSlicePool.Put(coreArgsPtr)
		return nil, err
	}
	*coreArgsPtr = coreArgs
	if len(coreArgs) != len(be.coreParamsWant) {
		putCoreValueSlice(coreArgsPtr)
		return nil, fmt.Errorf("component/instance: export %q: parameter list flattens to %d core value(s) but the core signature expects %d; whole-parameter-list spilling to memory is not supported by this milestone", exportName, len(coreArgs), len(be.coreParamsWant))
	}

	// The first core call returns a single packed i32 (an async lift with a
	// callback always flattens its result to exactly one core value -- see
	// docs/component-model-async-runtime-design.md §1.3); size the stack to
	// whichever's larger of the lowered params or the core func's own
	// declared result count, exactly like invoke()'s stack sizing.
	numResults := be.coreResultCount
	stackLen := len(coreArgs)
	if numResults > stackLen {
		stackLen = numResults
	}
	stackPtr := getUint64Slice(stackLen)
	stack := *stackPtr
	for i, cv := range coreArgs {
		stack[i] = cv.Bits
	}
	putCoreValueSlice(coreArgsPtr)

	if err := be.coreFn.CallWithStack(ctx, stack); err != nil {
		putUint64Slice(stackPtr)
		return nil, fmt.Errorf("component/instance: export %q: call core func %q: %w", exportName, be.funcName, err)
	}
	packed := uint32(stack[0])
	putUint64Slice(stackPtr)

	// The callback loop: canon_lift lines ~2172-2197.
	var cbStack [3]uint64
	for {
		code, si, cerr := unpackCallbackResult(packed)
		if cerr != nil {
			return nil, fmt.Errorf("component/instance: export %q: %w", exportName, cerr)
		}
		if code == callbackExit {
			break
		}

		// Reference: release exclusive for the duration of the suspension,
		// re-acquire before invoking the callback. With one task this is
		// pure bookkeeping (nothing else could ever run in between), but
		// the set/clear SITES are kept verbatim so Phase 1c's CallAsync
		// drops in without reshaping this loop.
		in.exclusiveHeld = false
		switch code {
		case callbackYield:
			// wait_until(lambda: not exclusive) under the deterministic
			// profile parks for exactly one scheduler round: everything
			// already queued runs, then the yielder resumes with NONE.
			if err := in.sched.pumpSnapshot(); err != nil {
				return nil, fmt.Errorf("component/instance: export %q: %w", exportName, err)
			}
		case callbackWait:
			return nil, fmt.Errorf("component/instance: export %q: async WAIT (waitable-set %d) not yet supported (Phase 1c)", exportName, si)
		}
		in.exclusiveHeld = true

		// [packed] = callback(event_code, p1, p2). Only EventCode.NONE
		// (0,0,0) is reachable in this milestone -- YIELD is the only
		// non-EXIT code that doesn't already return above.
		cbStack[0], cbStack[1], cbStack[2] = 0, 0, 0
		if err := be.callbackFn.CallWithStack(ctx, cbStack[:]); err != nil {
			return nil, fmt.Errorf("component/instance: export %q: callback %q: %w", exportName, be.callbackFuncName, err)
		}
		packed = uint32(cbStack[0])
	}

	// exit_implicit_thread -> unregister_thread: trap_if(state != RESOLVED).
	// This is the "async export forgot to call task.return" trap.
	if t.state != taskResolved {
		return nil, fmt.Errorf("component/instance: export %q: callback returned EXIT before task.return resolved the task", exportName)
	}
	return t.result, nil
}

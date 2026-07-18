package instance

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file wires the MVP async builtins (task.return, context.get/set,
// backpressure.inc/dec) as core funcs, exactly the way graph.go's
// computeCanonHostFunc already wires a canon lower or a resource canon: each
// becomes a hostFuncDef closing over the *Instance -- fixed i32-only core
// signatures, no reflection/WithFunc. See
// docs/component-model-async-runtime-design.md §1.5 for the semantics
// transliterated here (definitions.py's canon_task_return ~2380,
// canon_context_get/set ~2337/2347, canon_backpressure_inc/dec ~2362).
//
// "Current task" for all of these is in.activeTask, set for the duration of
// invokeAsyncCallback -- these builtins are ordinary core funcs with no
// other way to reach the call in progress (mirrors the reference's
// current_task()/current_instance()).

// requireActiveTask resolves the reference's current_task() plus its
// trap_if(!task.inst.may_leave) prologue (every async builtin in §1.5 opens
// with it), shared by all five builtins below.
func requireActiveTask(in *Instance, builtin string) *task {
	if !in.mayLeave { // trap_if(!may_leave)
		panic(fmt.Errorf("component/instance: %s: instance may not be left right now", builtin))
	}
	t := in.activeTask
	if t == nil {
		panic(fmt.Errorf("component/instance: %s: called outside an active async task", builtin))
	}
	return t
}

// requireMayLeave resolves the reference's current_instance() prologue's
// trap_if(!may_leave), shared by the handle-table async builtins that operate
// on the instance's table rather than a specific task (waitable-set.*,
// waitable.join, subtask.drop -- all use current_instance(), not
// current_task()). may_leave is always true in this milestone (nothing
// toggles it false yet); the gate is wired for Phase 3.
func requireMayLeave(in *Instance, builtin string) {
	if !in.mayLeave {
		panic(fmt.Errorf("component/instance: %s: instance may not be left right now", builtin))
	}
}

// waitableSetNewHostFunc backs waitable-set.new (CanonKindWaitableSetNew,
// 0x1f) -- canon_waitable_set_new, definitions.py ~2400. Core sig () -> i32:
// allocate an empty WaitableSet in the handle table and return its index.
func waitableSetNewHostFunc(in *Instance) hostFuncDef {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "waitable-set.new")
		stack[0] = uint64(in.resources.addEntry(&waitableSet{}))
	})
	return hostFuncDef{fn: fn, params: nil, results: []api.ValueType{api.ValueTypeI32}}
}

// requireWaitableSet resolves si to a *waitableSet in in.resources -- the
// trap_if(not isinstance(wset, WaitableSet)) shared by wait/poll/drop
// (definitions.py's canon_waitable_set_wait/poll/drop, ~2410-2443).
func requireWaitableSet(in *Instance, builtin string, si uint32) *waitableSet {
	e, ok := in.resources.getEntry(si)
	ws, isWS := e.(*waitableSet)
	if !ok || !isWS {
		panic(fmt.Errorf("component/instance: %s: handle %d is not a waitable set", builtin, si))
	}
	return ws
}

// storeEvent is the reference's unpack_event (definitions.py ~2418): write
// ev's (p1,p2) at ptr/ptr+4 in mod's memory and return the event code. Shared
// by waitable-set.wait/poll.
func storeEvent(mod api.Module, builtin string, ptr uint32, ev eventTuple) eventCode {
	mem, memAvailable := memoryBytesOf(mod)
	if !memAvailable {
		panic(fmt.Errorf("component/instance: %s: calling module has no memory", builtin))
	}
	u32 := binary.PrimitiveDesc{Prim: "u32"}
	if err := abi.Store(mem, ptr, u32, ev.p1, nil, abi.Realloc{}); err != nil {
		panic(fmt.Errorf("component/instance: %s: store event p1: %w", builtin, err))
	}
	if err := abi.Store(mem, ptr+4, u32, ev.p2, nil, abi.Realloc{}); err != nil {
		panic(fmt.Errorf("component/instance: %s: store event p2: %w", builtin, err))
	}
	return ev.code
}

// waitableSetWaitHostFunc backs waitable-set.wait (CanonKindWaitableSetWait,
// 0x20) -- canon_waitable_set_wait, definitions.py ~2410. Core sig
// (si:i32, ptr:i32) -> i32: block until the named waitable set has a pending
// event (a nested drive of the instance scheduler -- sched.go -- exactly the
// mechanism invokeAsyncCallback's callbackWait arm uses; the reference's
// callback-lift implicit thread is a genuine Thread that can itself call a
// blocking wait builtin and suspend, so this is legal from callback code
// too, not just from a would-be sync/stackful task), write its (p1,p2) at
// ptr/ptr+4, and return the event code.
//
// canon carries the cancellable:bool payload finally threaded through here
// (Phase 3, docs/component-model-async-phase3-design.md §2.3): when
// cancellable and this task has a delivered-or-deliverable cancellation
// pending, the drive predicate also wakes on that, and TASK_CANCELLED is
// stored instead of a set event -- deliverPendingCancel is checked at THIS
// suspension's prologue exactly where the reference's wait_until does
// (~404). A blocking wait's cancel wake cannot resume synchronously the way
// a parked callback loop's does (this builtin has live Go frames beneath
// it -- see task.go's requestCancellation doc); it is instead a genuinely
// PENDING_CANCEL task whose own drive predicate below picks the
// cancellation up at the next scheduler check.
func waitableSetWaitHostFunc(in *Instance, canon binary.Canon) hostFuncDef {
	cancellable := canon.Cancellable
	fn := api.GoModuleFunc(func(_ context.Context, mod api.Module, stack []uint64) {
		requireMayLeave(in, "waitable-set.wait")
		si := api.DecodeU32(stack[0])
		ptr := api.DecodeU32(stack[1])
		ws := requireWaitableSet(in, "waitable-set.wait", si)
		t := requireActiveTask(in, "waitable-set.wait")

		pred := func() bool {
			return ws.hasPendingEvent() || (cancellable && t.cancelDeliverable())
		}
		ws.numWaiting++
		err := in.sched.drive(pred)
		ws.numWaiting--
		if err != nil {
			if errors.Is(err, errAsyncDeadlock) {
				panic(fmt.Errorf("component/instance: waitable-set.wait: deadlock: waitable-set %d has no pending event and the run queue is empty; an import resolved externally requires CallAsync (not yet implemented)", si))
			}
			panic(fmt.Errorf("component/instance: waitable-set.wait: %w", err))
		}
		var ev eventTuple
		if cancellable && t.deliverPendingCancel(true) {
			ev = eventTuple{code: eventTaskCancelled}
		} else {
			ev = ws.getPendingEvent()
		}
		stack[0] = uint64(storeEvent(mod, "waitable-set.wait", ptr, ev))
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}
}

// waitableSetPollHostFunc backs waitable-set.poll (CanonKindWaitableSetPoll,
// 0x21) -- canon_waitable_set_poll, definitions.py ~2427. Same as wait but
// non-blocking: returns EventCode.NONE immediately when nothing is pending
// instead of driving the scheduler. The poll(cancellable) prologue (~834):
// if cancellable and a cancellation is deliverable, TASK_CANCELLED is
// returned immediately, before even checking the set.
func waitableSetPollHostFunc(in *Instance, canon binary.Canon) hostFuncDef {
	cancellable := canon.Cancellable
	fn := api.GoModuleFunc(func(_ context.Context, mod api.Module, stack []uint64) {
		requireMayLeave(in, "waitable-set.poll")
		si := api.DecodeU32(stack[0])
		ptr := api.DecodeU32(stack[1])
		ws := requireWaitableSet(in, "waitable-set.poll", si)
		t := requireActiveTask(in, "waitable-set.poll")
		if cancellable && t.deliverPendingCancel(true) {
			stack[0] = uint64(storeEvent(mod, "waitable-set.poll", ptr, eventTuple{code: eventTaskCancelled}))
			return
		}
		stack[0] = uint64(storeEvent(mod, "waitable-set.poll", ptr, ws.poll()))
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}
}

// waitableSetDropHostFunc backs waitable-set.drop (CanonKindWaitableSetDrop,
// 0x22) -- canon_waitable_set_drop, definitions.py ~2432. Core sig (i32) -> ():
// remove the set (trap unless it names a WaitableSet) and trap if anything is
// still joined to it. Validated before removal so a kind mismatch leaves the
// table untouched (observably identical to the reference on the success path;
// the mismatch path traps either way).
func waitableSetDropHostFunc(in *Instance) hostFuncDef {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "waitable-set.drop")
		h := api.DecodeU32(stack[0])
		e, ok := in.resources.getEntry(h)
		ws, isWS := e.(*waitableSet)
		if !ok || !isWS {
			panic(fmt.Errorf("component/instance: waitable-set.drop: handle %d is not a waitable set", h))
		}
		if err := ws.dropSet(); err != nil { // trap_if(len(elems) > 0)
			panic(fmt.Errorf("component/instance: %w", err))
		}
		in.resources.removeEntry(h)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: nil}
}

// waitableJoinHostFunc backs waitable.join (CanonKindWaitableJoin, 0x23) --
// canon_waitable_join, definitions.py ~2440. Core sig (wi:i32, si:i32) -> ():
// move waitable wi into set si, or out of any set when si == 0. Phase 3
// retires the Phase-1/2 "no sync waiters exist" collapse: a synchronous
// subtask.cancel (async_builtins.go's subtaskCancelHostFuncGraph) can now
// leave a waitable with a real sync waiter across scheduler steps, so
// trap_if(has_sync_waiter) (~2452) is a real check.
func waitableJoinHostFunc(in *Instance) hostFuncDef {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "waitable.join")
		wi, si := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
		e, ok := in.resources.getEntry(wi)
		we, isW := e.(waitableEntry)
		if !ok || !isW {
			panic(fmt.Errorf("component/instance: waitable.join: handle %d is not a waitable", wi))
		}
		if we.waitablePtr().syncWaiter { // trap_if(w.has_sync_waiter)
			panic(fmt.Errorf("component/instance: waitable.join: handle %d has a synchronous subtask.cancel blocked on it", wi))
		}
		if si == 0 {
			we.waitablePtr().join(nil)
			return
		}
		se, ok := in.resources.getEntry(si)
		ws, isWS := se.(*waitableSet)
		if !ok || !isWS {
			panic(fmt.Errorf("component/instance: waitable.join: handle %d is not a waitable set", si))
		}
		we.waitablePtr().join(ws)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, results: nil}
}

// subtaskDropHostFunc backs subtask.drop (CanonKindSubtaskDrop, 0x0d) --
// canon_subtask_drop, definitions.py ~2490. Core sig (i32) -> (): remove the
// subtask (trap unless it names one) and trap unless its resolve has been
// delivered. Validated before removal (see waitable-set.drop).
func subtaskDropHostFunc(in *Instance) hostFuncDef {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "subtask.drop")
		h := api.DecodeU32(stack[0])
		e, ok := in.resources.getEntry(h)
		s, isSub := e.(*subtask)
		if !ok || !isSub {
			panic(fmt.Errorf("component/instance: subtask.drop: handle %d is not a subtask", h))
		}
		if err := s.dropSubtask(); err != nil { // trap_if(not resolve_delivered)
			panic(fmt.Errorf("component/instance: %w", err))
		}
		in.resources.removeEntry(h)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: nil}
}

// taskCancelHostFuncGraph backs task.cancel (CanonKindTaskCancel, 0x05) --
// canon_task_cancel, definitions.py ~2391. Core sig () -> (): the guest ACKs
// a cancellation the host previously delivered to this task's callback as a
// TASK_CANCELLED event (docs/component-model-async-phase3-design.md §2.1).
func taskCancelHostFuncGraph(in *Instance) hostFuncDef {
	fn := api.GoModuleFunc(func(context.Context, api.Module, []uint64) {
		t := requireActiveTask(in, "task.cancel") // may_leave + current_task (~2393-2395)
		// trap_if(not task.opts.async_) (~2396): every task this package
		// creates is an async callback lift, so the trap is structurally
		// unreachable -- asserted here for oracle parity.
		if t.be != nil && !t.be.asyncCallback {
			panic(fmt.Errorf("component/instance: task.cancel: called on a non-async task"))
		}
		if err := t.cancelResolve(); err != nil {
			panic(fmt.Errorf("component/instance: %w", err))
		}
	})
	return hostFuncDef{fn: fn, params: nil, results: nil}
}

// subtaskCancelHostFuncGraph backs subtask.cancel (CanonKindSubtaskCancel,
// 0x06) -- canon_subtask_cancel, definitions.py ~2465-2486, transliterated
// line for line. Core sig (i:i32) -> i32. Reuses blockedSentinel (stream.go)
// -- the reference's BLOCKED = 0xffff_ffff constant is shared by
// subtask.cancel and stream/future cancel alike.
func subtaskCancelHostFuncGraph(in *Instance, canon binary.Canon) hostFuncDef {
	async := canon.Async
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "subtask.cancel")
		i := api.DecodeU32(stack[0])
		e, ok := in.resources.getEntry(i)
		st, isSub := e.(*subtask)
		if !ok || !isSub { // trap_if(not isinstance(subtask, Subtask))
			panic(fmt.Errorf("component/instance: subtask.cancel: handle %d is not a subtask", i))
		}
		if st.resolveDelivered() { // trap_if(subtask.resolve_delivered())
			panic(fmt.Errorf("component/instance: subtask.cancel: handle %d has already had its resolve delivered", i))
		}
		if st.cancellationRequested { // trap_if(subtask.cancellation_requested)
			panic(fmt.Errorf("component/instance: subtask.cancel: handle %d: cancellation already requested", i))
		}
		if st.inWaitableSet() && !async { // trap_if(in_waitable_set() and not async_)
			panic(fmt.Errorf("component/instance: subtask.cancel: handle %d is joined to a waitable set; a synchronous cancel is not allowed", i))
		}

		if !st.resolved() {
			st.cancellationRequested = true
			if st.onCancel != nil { // nil only for a host import with no OnCancel hook (§2.4)
				// Bracket with sched.pumping exactly like sched.step's own
				// thunk/resumeReady dispatch (sched.go): st.onCancel runs
				// synchronously HERE, on the one driving goroutine, not from
				// inside a queued runq thunk -- but AsyncCall.OnCancel's doc
				// explicitly promises the callee may call
				// Resolve/ResolveCancelled SYNCHRONOUSLY from inside fn
				// (async_host_import.go), and those methods' single-
				// threadedness guard only recognizes "inCall" (the ORIGINAL
				// AsyncHostFunc invocation, long since returned by the time
				// a later subtask.cancel fires) or "sched.pumping" as proof
				// of running on the driving goroutine. Without this bracket
				// every synchronous OnCancel->Resolve/ResolveCancelled call
				// (a real, documented, spec-legal shape -- e.g. a host
				// import that completes cancellation immediately rather
				// than deferring it) panics "called outside the instance
				// scheduler", which is wrong: we ARE on the instance's one
				// driving goroutine, synchronously, exactly where sched.step
				// itself would be.
				in.sched.pumping = true
				cerr := st.onCancel()
				in.sched.pumping = false
				if cerr != nil {
					panic(fmt.Errorf("component/instance: subtask.cancel: %w", cerr))
				}
			}
			if !st.resolved() {
				if async {
					stack[0] = blockedSentinel
					return
				}
				// wait_for_pending_event (~776): a synchronous cancel
				// blocks the scheduler drive until the subtask resolves.
				st.syncWaiter = true
				derr := in.sched.drive(st.hasPendingEvent)
				st.syncWaiter = false
				if derr != nil {
					if errors.Is(derr, errAsyncDeadlock) {
						panic(fmt.Errorf("component/instance: subtask.cancel: deadlock: subtask %d's synchronous cancel is waiting on a subtask that nothing can resolve", i))
					}
					panic(fmt.Errorf("component/instance: subtask.cancel: %w", derr))
				}
			}
		}
		// Resolved-with-undelivered-event fast path (~2473-2474) falls
		// through to here too: a cancel of an already-resolved-but-
		// undelivered subtask just delivers, returning its state.
		ev := st.getPendingEvent() // asserts SUBTASK/i/state (~2483-2484); delivers lend release
		if ev.code != eventSubtask || ev.p1 != i {
			panic(fmt.Errorf("component/instance: subtask.cancel: BUG: delivered event (%v,%d,%d) does not name subtask %d", ev.code, ev.p1, ev.p2, i))
		}
		stack[0] = uint64(uint32(st.state))
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}
}

// taskReturnHostFuncGraph builds the core func backing a task.return canon
// (CanonKindTaskReturn, 0x09) -- canon_task_return, definitions.py ~2380.
// Its core signature follows the same param-flattening rule as a lift/
// lower's own params (MAX_FLAT_PARAMS spill threshold), computed once here
// at bind time from canon.TaskReturnResult; the task it operates on is
// resolved at CALL time (in.activeTask), since -- unlike a lift's own core
// func -- task.return is not statically tied to one export.
func taskReturnHostFuncGraph(in *Instance, canon binary.Canon) (hostFuncDef, error) {
	resolve := in.resolve
	resultRefs := funcResultTypeRefs(binary.FuncDesc{Results: canon.TaskReturnResult})
	if len(resultRefs) > 1 {
		return hostFuncDef{}, fmt.Errorf("component/instance: task.return: %d named results; multiple task.return results are not supported by this milestone", len(resultRefs))
	}

	var resultType binary.TypeDesc
	var flatKinds []string
	if len(resultRefs) == 1 {
		rt, err := resolveTypeRef(&resultRefs[0], resolve)
		if err != nil {
			return hostFuncDef{}, fmt.Errorf("component/instance: task.return: resolve result type: %w", err)
		}
		resultType = rt
		if flatKinds, err = abi.Flatten(rt, resolve); err != nil {
			return hostFuncDef{}, fmt.Errorf("component/instance: task.return: flatten result type: %w", err)
		}
	}

	spills := len(flatKinds) > abi.MaxFlatParams
	coreParamKinds := flatKinds
	if spills {
		coreParamKinds = []string{"i32"} // pointer to the result, stored via the normal (non-flat) representation
	}
	apiParams, err := toApiValueTypes(coreParamKinds)
	if err != nil {
		return hostFuncDef{}, fmt.Errorf("component/instance: task.return: %w", err)
	}

	fn := api.GoModuleFunc(func(_ context.Context, mod api.Module, stack []uint64) {
		t := requireActiveTask(in, "task.return")
		if resultType != nil && t.be != nil && !reflect.DeepEqual(resultType, t.be.resultType) {
			// trap_if(result_type != task.ft.result)
			panic(fmt.Errorf("component/instance: task.return: result type does not match the active task's declared result type"))
		}

		var result []abi.Value
		if resultType != nil {
			mem, memAvailable := memoryBytesOf(mod)
			if spills {
				if !memAvailable {
					panic(fmt.Errorf("component/instance: task.return: result flattens beyond %d core value(s) and requires linear memory, but the core module exports no memory", abi.MaxFlatParams))
				}
				ptr := api.DecodeU32(stack[0])
				val, err := abi.Load(mem, ptr, resultType, resolve)
				if err != nil {
					panic(fmt.Errorf("component/instance: task.return: load spilled result: %w", err))
				}
				result = []abi.Value{val}
			} else {
				coreVals := make([]abi.CoreValue, len(flatKinds))
				for i, k := range flatKinds {
					coreVals[i] = abi.CoreValue{Kind: k, Bits: stack[i]}
				}
				val, err := abi.LiftFlat(coreVals, resultType, resolve, mem)
				if err != nil {
					panic(fmt.Errorf("component/instance: task.return: lift result: %w", err))
				}
				result = []abi.Value{val}
			}
		}

		if err := t.returnValues(result); err != nil {
			panic(fmt.Errorf("component/instance: %w", err))
		}
	})
	return hostFuncDef{fn: fn, params: apiParams, results: nil}, nil
}

// contextValType maps canon.CoreValType (a raw core:valtype byte -- 0x7f
// i32, 0x7e i64; see Canon.CoreValType's doc) to an api.ValueType,
// mirroring the reference's assert(t == 'i32' or t == 'i64') in
// canon_context_get/set.
func contextValType(b byte) (api.ValueType, error) {
	switch b {
	case 0x7f:
		return api.ValueTypeI32, nil
	case 0x7e:
		return api.ValueTypeI64, nil
	default:
		return 0, fmt.Errorf("context.get/set: unsupported core valtype %#x (only i32/i64 are valid)", b)
	}
}

// contextGetHostFuncGraph builds the core func backing a context.get canon
// (CanonKindContextGet, 0x0a) -- canon_context_get, definitions.py ~2337.
// slot and the core valtype are static (baked into the canon at bind time,
// not runtime arguments): the core signature is `() -> t`.
func contextGetHostFuncGraph(in *Instance, canon binary.Canon) (hostFuncDef, error) {
	vt, err := contextValType(canon.CoreValType)
	if err != nil {
		return hostFuncDef{}, fmt.Errorf("component/instance: context.get: %w", err)
	}
	slot := canon.Slot
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		t := requireActiveTask(in, "context.get")
		if int(slot) >= len(t.ctxStorage) { // assert(i < len(thread.storage))
			panic(fmt.Errorf("component/instance: context.get: slot %d out of range (storage has %d slot(s))", slot, len(t.ctxStorage)))
		}
		stack[0] = t.ctxStorage[slot]
	})
	return hostFuncDef{fn: fn, params: nil, results: []api.ValueType{vt}}, nil
}

// contextSetHostFuncGraph builds the core func backing a context.set canon
// (CanonKindContextSet, 0x0b) -- canon_context_set, definitions.py ~2347.
// Core signature is `(v: t) -> ()`.
func contextSetHostFuncGraph(in *Instance, canon binary.Canon) (hostFuncDef, error) {
	vt, err := contextValType(canon.CoreValType)
	if err != nil {
		return hostFuncDef{}, fmt.Errorf("component/instance: context.set: %w", err)
	}
	slot := canon.Slot
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		t := requireActiveTask(in, "context.set")
		if int(slot) >= len(t.ctxStorage) { // assert(i < len(thread.storage))
			panic(fmt.Errorf("component/instance: context.set: slot %d out of range (storage has %d slot(s))", slot, len(t.ctxStorage)))
		}
		t.ctxStorage[slot] = stack[0]
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{vt}, results: nil}, nil
}

// backpressureIncHostFuncGraph/backpressureDecHostFuncGraph build the core
// funcs backing backpressure.inc/dec (CanonKindBackpressureInc/Dec,
// 0x24/0x25) -- canon_backpressure_inc/dec, definitions.py ~2362. Both take
// and return nothing; the counter lives on Instance (backpressure.set,
// which some vintages use instead, is not decoded -- see decodeCanonOpts
// and the design doc's §4 point 5 "vintage drift").
func backpressureIncHostFuncGraph(in *Instance) hostFuncDef {
	fn := api.GoModuleFunc(func(context.Context, api.Module, []uint64) {
		requireActiveTask(in, "backpressure.inc")
		in.backpressure++
		if in.backpressure == 1<<16 { // trap_if(inst.backpressure == 2**16)
			panic(fmt.Errorf("component/instance: backpressure.inc: counter overflow (2^16)"))
		}
	})
	return hostFuncDef{fn: fn, params: nil, results: nil}
}

func backpressureDecHostFuncGraph(in *Instance) hostFuncDef {
	fn := api.GoModuleFunc(func(context.Context, api.Module, []uint64) {
		requireActiveTask(in, "backpressure.dec")
		in.backpressure--
		if in.backpressure < 0 { // trap_if(inst.backpressure < 0)
			panic(fmt.Errorf("component/instance: backpressure.dec: counter underflow"))
		}
	})
	return hostFuncDef{fn: fn, params: nil, results: nil}
}

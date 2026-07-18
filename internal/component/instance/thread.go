package instance

import (
	"context"
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/binary"
	"github.com/samyfodil/wazy/internal/wasm"
)

// This file begins the thread.* execution runtime
// (docs/component-model-async-threads-design-fable.md, "Stage A"): a
// cooperative yield expressed entirely through the EXISTING taskBlocker
// machinery (stackfulTask.block / guestTask.block, task.go's taskBlocker
// interface) -- no new goroutine primitive yet (guestThread/the thread table
// are Stage D). Reference: canon_thread_yield (definitions.py ~2723-2727) ->
// Thread.yield_ (~411-412) -> wait_until(lambda: True, cancellable)
// (~402-409): a deliver-pending-cancel prologue, then an always-ready park.

// threadYieldHostFunc backs thread.yield (CanonKindThreadYield, 0x0c) --
// canon_thread_yield, definitions.py ~2723-2727. Core sig () -> i32
// (Cancelled: FALSE=0, TRUE=1, ~254-256).
//
// Binding any thread.* canon promotes the binding instance the same way a
// sync-lower-targeting-an-async-lift does (graph.go:1385-1387): a component
// that imports thread primitives declares itself a multi-fiber program whose
// callback tasks must be able to park mid-core-call (design §5.4) --
// cancellable's $C needs this to ever get a STARTED return to its caller.
// Also flags syncTaskNeeded so a plain sync lift that calls thread.yield
// still gets a syncImplicit task to resolve against (instance.go's
// invokeEntered), matching every other async builtin's bind-time flag
// (async_builtins.go).
func threadYieldHostFunc(in *Instance, canon binary.Canon) hostFuncDef {
	in.syncTaskNeeded, in.mayBlockSync = true, true
	cancellable := canon.Cancellable
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "thread.yield")
		t := in.activeTask
		var blk taskBlocker
		if t != nil {
			blk = t.blocker()
		}
		switch {
		case blk != nil:
			// Stackful implicit thread (sync-barges-in's $C.yielder), or a
			// promoted callback task's live segment: a genuine park with an
			// always-true ready predicate -- parks, then the driver's very
			// next step resumes it (behind any already-queued thunks/ready
			// tasks), exactly the reference's "yielded thread re-enters the
			// waiting list BEHIND already-ready threads".
			if blk.block(func() bool { return true }, cancellable) {
				stack[0] = 1 // Cancelled.TRUE
				return
			}
			stack[0] = 0

		case t != nil && !t.syncImplicit:
			// Unpromoted callback task's guest code, running on the driving
			// goroutine with no live segment to park (frame-free callback
			// loop, no suite in Stage A reaches this arm -- cancellable's
			// promotion gate (§5.4) means a bound thread.yield always gets a
			// promoted task; kept for parity with the reference, which never
			// distinguishes "callback" from "stackful" at Thread.yield_'s
			// level). One deterministic scheduler round, then the
			// pending-cancel check, mirroring wait_until's order as closely
			// as a frame-held caller allows.
			if t.deliverPendingCancel(cancellable) {
				stack[0] = 1
				return
			}
			if err := in.sched.pumpSnapshot(); err != nil {
				panic(fmt.Errorf("component/instance: thread.yield: %w", err))
			}
			stack[0] = 0

		default:
			// Sync task (t == nil, or t.syncImplicit): the reference ALLOWS
			// a sync task to yield -- yield_ is a ready-waiting block, and
			// canon_lift's sync driver loop (~2202-2206) always finds a
			// candidate (the yielder itself, once re-armed) so there is
			// never a trap here, unlike thread.suspend/waitable-set.wait's
			// "cannot block a synchronous task" trap. wazy equivalent: one
			// scheduler round on the driving goroutine, then return NOT
			// cancelled (a sync task can never have a delivered
			// cancellation to observe). trap-if-block-and-sync's
			// yield-is-fine asserts 42, not a trap.
			if err := in.sched.pumpSnapshot(); err != nil {
				panic(fmt.Errorf("component/instance: thread.yield: %w", err))
			}
			stack[0] = 0
		}
	})
	return hostFuncDef{fn: fn, params: nil, results: []api.ValueType{api.ValueTypeI32}}
}

// This section continues the thread.* runtime with "Stage C"
// (docs/component-model-async-threads-design-fable.md §4.2-§4.4, §12): binds
// for thread.suspend/thread.index/thread.new-indirect. thread.suspend gets a
// real (if narrow) body -- it is genuinely CALLED by trap-if-block-and-sync's
// trap-if-suspend. thread.index gets a real body too (its lazy implicit-slot
// allocation is cheap and self-contained), but is never called by any
// vendored suite at runtime -- only unit-tested (§4.3's doc). thread.
// new-indirect's BIND path resolves the real target table + signature (the
// work Stage D's execution will need), but its call body is a fail-loud stub:
// spawning and running the resulting guestThread is Stage D (§12); no suite
// this design lands ever calls it (trap-if-block-and-sync's only two call
// sites are commented out, wast:131/140/145).

// threadTable is the instance-scoped thread index space (design
// §3/§4.3/§12 Stage C): the reference's inst.threads (definitions.py:201,
// :509), a free-listed slice with index 0 permanently reserved so the first
// allocated thread gets index 1 (mirrors the reference Table's own `array =
// [None]` convention, :688). Stage C only ever stores implicitThreadMarker
// values (thread.index's lazy implicit-thread slot); Stage D's guestThread
// values will share the same table and index space.
type threadTable struct {
	slots []any
	free  []uint32
}

// implicitThreadMarker occupies a threadTable slot for a task's lazily
// registered "thread 0" identity (§4.3) -- a placeholder with no behavior of
// its own. Stage D's thread.yield-then-resume will need to type-switch a
// resolved slot to tell an implicit thread apart from a real *guestThread.
type implicitThreadMarker struct {
	t *task
}

// add allocates a fresh (or free-list-recycled) slot for v, returning its
// index. Index 0 is never allocated (reserved, matching the reference
// Table's convention above).
func (tt *threadTable) add(v any) uint32 {
	if len(tt.slots) == 0 {
		tt.slots = append(tt.slots, nil) // reserve index 0
	}
	if n := len(tt.free); n > 0 {
		idx := tt.free[n-1]
		tt.free = tt.free[:n-1]
		tt.slots[idx] = v
		return idx
	}
	tt.slots = append(tt.slots, v)
	return uint32(len(tt.slots) - 1)
}

// remove frees idx's slot for reuse. A no-op for index 0 or an already-free/
// out-of-range index (defensive -- Stage C never calls this; the implicit
// slot's free-at-task-completion path is deferred to Stage D, see
// threadIndexHostFunc's doc).
func (tt *threadTable) remove(idx uint32) {
	if idx == 0 || int(idx) >= len(tt.slots) || tt.slots[idx] == nil {
		return
	}
	tt.slots[idx] = nil
	tt.free = append(tt.free, idx)
}

// threadIndexHostFunc backs thread.index (CanonKindThreadIndex, 0x26) --
// canon_thread_index, definitions.py ~2677-2680: return
// current_thread().index. Core sig () -> i32.
//
// Stage C has no in.activeThread yet (Stage D's guestThread), so this only
// ever resolves the CURRENT TASK's own implicit thread, registering it
// lazily in in.threads on first call (deviation §11.4 -- the reference
// eagerly registers an implicit thread at enter_implicit_thread, :502; wazy
// defers it so the 28 other suites, which never call thread.index, pay
// nothing). The slot is never freed in Stage C (no suite exercises this at
// runtime to observe a leak -- trap-if-block-and-sync's only call sites are
// inside commented-out switch-to-is-fine/resume-later-is-fine,
// wast:138-146); Stage D's fuller guestThread lifecycle will wire the
// free-at-task-completion path this needs for real spawned threads anyway.
// Covered by thread_test.go only.
func threadIndexHostFunc(in *Instance) hostFuncDef {
	in.syncTaskNeeded, in.mayBlockSync = true, true
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "thread.index")
		t := requireActiveTask(in, "thread.index")
		if t.implicitThreadIdx == 0 {
			t.implicitThreadIdx = in.threads.add(implicitThreadMarker{t: t})
		}
		stack[0] = uint64(t.implicitThreadIdx)
	})
	return hostFuncDef{fn: fn, params: nil, results: []api.ValueType{api.ValueTypeI32}}
}

// threadSuspendHostFunc backs thread.suspend (CanonKindThreadSuspend, 0x29)
// -- canon_thread_suspend, definitions.py ~2715-2719 -> Thread.suspend
// (~396-400): a park with NO ready predicate, resumable only by an explicit
// switch/resume (Stage D; none landed here) or a cancellable cancel
// delivery. Core sig () -> i32 (Cancelled: FALSE=0, TRUE=1).
//
// The only path any vendored suite exercises (design §4.2, §8.3) is
// blockingTask's sync trap: trap-if-block-and-sync's trap-if-suspend is a
// plain sync lift (wast:242) -> syncImplicit task -> "cannot block a
// synchronous task before returning" (async_builtins.go's blockingTask) --
// the exact substring wast:296 asserts. The stackful/promoted-callback park
// arm is wired for parity with thread.yield and spec correctness but stays
// unexercised until Stage D's guestThread runtime gives a suspended thread a
// way to ever resume.
func threadSuspendHostFunc(in *Instance, canon binary.Canon) hostFuncDef {
	in.syncTaskNeeded, in.mayBlockSync = true, true
	cancellable := canon.Cancellable
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "thread.suspend")
		t, blk := blockingTask(in, "thread.suspend") // sync task -> the trap suite 3 asserts
		if blk == nil {
			// Unpromoted callback ctx: a frame-held caller cannot suspend
			// forever without deadlocking its own driver. Unexercised by any
			// suite (design §4.2, §11); fail loud rather than hang.
			panic(fmt.Errorf("component/instance: thread.suspend: not supported from an unpromoted callback task"))
		}
		if t.deliverPendingCancel(cancellable) {
			stack[0] = 1
			return
		}
		if blk.block(func() bool { return false }, cancellable) { // neverReady: suspend
			stack[0] = 1
			return
		}
		stack[0] = 0
	})
	return hostFuncDef{fn: fn, params: nil, results: []api.ValueType{api.ValueTypeI32}}
}

// threadNewIndirectHostFunc backs thread.new-indirect (CanonKindThreadNewIndirect,
// 0x27) -- canon_thread_new_indirect, definitions.py ~2689-2701. Core sig
// (fi:i32, c:i32|i64) -> i32.
//
// Stage C (design §4.4, §12) lands only the BIND path: resolve the target
// table (coreTableTarget, threaded through graph.go the same way
// coreMemTarget/coreFuncTarget are) and precompute the indirect-call-table
// typeID the reference's trap_if(f.t != ft) will need, validating that the
// CONSUMER's own declared core import signature is one of the two legal
// shapes -- (i32,i32)->i32 or (i32,i64)->i32. wazy decodes the core type
// section raw-only (binary/decoder.go), so `ft` (canon.TypeIdx, a
// core:typeidx) can't be resolved directly; it is derived instead from the
// consumer's signature (deviation logged in design §11.6 -- in any validated
// component these are definitionally equal, and a real spawn's
// LookupFunction typeID check would enforce trap_if(f.t != ft) exactly). The
// call BODY is a fail-loud stub: no suite this increment lands calls it at
// runtime (trap-if-block-and-sync's only two call sites are commented out,
// wast:131/140/145) -- spawning and running the resulting guestThread is
// Stage D.
func threadNewIndirectHostFunc(
	in *Instance, canon binary.Canon, neededTypes map[string]map[string]coreFuncSig, groupName, entryName string,
	coreTableTarget func(int) (api.Module, string, error),
) (hostFuncDef, error) {
	in.syncTaskNeeded, in.mayBlockSync = true, true

	ownerMod, tableName, err := coreTableTarget(int(canon.TableIdx))
	if err != nil {
		return hostFuncDef{}, fmt.Errorf("thread.new-indirect: %w", err)
	}
	wm, ok := ownerMod.(*wasm.ModuleInstance)
	if !ok {
		return hostFuncDef{}, fmt.Errorf("thread.new-indirect: table owner module is not a real core module instance")
	}
	exp, eok := wm.Exports[tableName]
	if !eok || exp.Type != wasm.ExternTypeTable {
		return hostFuncDef{}, fmt.Errorf("thread.new-indirect: table owner module has no exported table %q", tableName)
	}
	if int(exp.Index) >= len(wm.Tables) {
		return hostFuncDef{}, fmt.Errorf("thread.new-indirect: table export %q index %d out of range of %d table(s)", tableName, exp.Index, len(wm.Tables))
	}
	table := wm.Tables[exp.Index]

	// ft (§4.4 step 3): derived from the consumer's own declared core import
	// signature, since canon.TypeIdx can't be resolved against a raw-decoded
	// core type section. sig.params[0] is the func-table index (always i32);
	// sig.params[1] is flatten(ft.param) -- the reference's ft is always
	// exactly (i32)->() or (i64)->().
	sig, sok := neededTypes[groupName][entryName]
	if !sok {
		return hostFuncDef{}, fmt.Errorf("thread.new-indirect: cannot determine the core-level signature: no consumer declares module %q field %q", groupName, entryName)
	}
	if len(sig.params) != 2 || sig.params[0] != api.ValueTypeI32 ||
		(sig.params[1] != api.ValueTypeI32 && sig.params[1] != api.ValueTypeI64) ||
		len(sig.results) != 1 || sig.results[0] != api.ValueTypeI32 {
		return hostFuncDef{}, fmt.Errorf("thread.new-indirect: consumer signature params=%v results=%v is not one of the two legal shapes ((i32,i32)->i32 or (i32,i64)->i32)", sig.params, sig.results)
	}
	argType := wasm.ValueType(sig.params[1])
	typeID := wm.GetFunctionTypeID(&wasm.FunctionType{Params: []wasm.ValueType{argType}})

	fn := api.GoModuleFunc(func(context.Context, api.Module, []uint64) {
		panic(fmt.Errorf("component/instance: thread.new-indirect: execution lands in Stage D (guestThread runtime not yet implemented; resolved table %q (%d elements), target typeID %d)", tableName, table.Min, typeID))
	})
	return hostFuncDef{fn: fn, params: sig.params, results: sig.results}, nil
}

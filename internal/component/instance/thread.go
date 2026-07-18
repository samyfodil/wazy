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
		blk := in.currentBlocker() // design §5.2: a spawned guestThread's own baton first
		switch {
		case blk != nil:
			// Stackful implicit thread (sync-barges-in's $C.yielder), a
			// promoted callback task's live segment, or a spawned guestThread:
			// a genuine park with an always-true ready predicate -- parks,
			// then the driver's very next step resumes it (behind any
			// already-queued thunks/ready tasks), exactly the reference's
			// "yielded thread re-enters the waiting list BEHIND already-ready
			// threads".
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
// new-indirect's BIND path resolves the real target table + signature; its
// call body was originally a fail-loud stub and now (Stage D, §4.4/§12) does
// the real spawn -- see threadNewIndirectHostFunc's own doc.

// threadTable is the instance-scoped thread index space (design
// §3/§4.3/§12): the reference's inst.threads (definitions.py:201, :509), a
// free-listed slice with index 0 permanently reserved so the first allocated
// thread gets index 1 (mirrors the reference Table's own `array = [None]`
// convention, :688). Stores implicitThreadMarker values (thread.index's lazy
// implicit-thread slot) and, since Stage D, real *guestThread values
// (thread.new-indirect's spawned threads) in the same table and index space.
type threadTable struct {
	slots []any
	free  []uint32
}

// implicitThreadMarker occupies a threadTable slot for a task's lazily
// registered "thread 0" identity (§4.3) -- a placeholder with no behavior of
// its own. getThread's type-switch below tells an implicit thread apart from
// a real *guestThread (thread.yield-then-resume may only target the latter).
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
// out-of-range index (defensive -- a spawned guestThread's slot is always
// freed via guestThread.run's unregister tail or reapParkedGoroutines' reap
// arm; a task's own lazily-allocated implicit slot is never freed, see
// threadIndexHostFunc's doc).
func (tt *threadTable) remove(idx uint32) {
	if idx == 0 || int(idx) >= len(tt.slots) || tt.slots[idx] == nil {
		return
	}
	tt.slots[idx] = nil
	tt.free = append(tt.free, idx)
}

// getThread resolves idx to a live *guestThread -- ok is false for index 0,
// an out-of-range/freed slot, or a slot occupied by an implicitThreadMarker
// (design §4.5: thread.yield-then-resume may only target a REAL spawned
// thread, never a task's own implicit-thread slot). Used only by
// thread.yield-then-resume.
func (tt *threadTable) getThread(idx uint32) (*guestThread, bool) {
	if idx == 0 || int(idx) >= len(tt.slots) {
		return nil, false
	}
	th, ok := tt.slots[idx].(*guestThread)
	return th, ok
}

// guestThread is an explicitly-spawned thread of execution (canon
// thread.new-indirect) -- the reference's Thread (definitions.py:323) for
// the non-implicit case (design §3). Same goroutine+baton primitive as
// stackfulTask/guestSegment: at most one goroutine in the composition tree
// runs component-runtime code at any instant; every control transfer is a
// channel send/recv pair (proof: §7 of the design doc).
type guestThread struct {
	t   *task     // owning task (reference Thread.task) -- always the task that called thread.new-indirect
	in  *Instance // == t.inst
	ctx context.Context

	index uint32       // slot in in.threads (reference Thread.index; :509)
	fn    api.Function // the indirect-table func, resolved AT new-indirect time (:2692)
	arg   uint64       // the `c` argument (:2696), raw bits (valid for both i32 and i64)

	// The baton (created lazily at first resume; nil while never-resumed).
	resumeCh chan resumeMode // driver -> goroutine (reuses stackful_task.go's resumeMode)
	yieldCh  chan struct{}   // goroutine -> driver
	spawned  bool

	// Park state. parkReady == nil while parked means SUSPENDED (reference
	// Thread.suspended(): not resumable by the scheduler, only by an explicit
	// thread.yield-then-resume switch); parkReady != nil means WAITING
	// (Thread.waiting()).
	parked      bool
	parkReady   func() bool
	cancellable bool
	cancelWake  bool

	done     bool
	err      error
	panicVal any
}

// suspendedState reports the reference's Thread.suspended(): parked with no
// ready predicate, resumable only by an explicit thread.yield-then-resume
// switch or a cancellable cancel delivery -- never by the scheduler's own
// ready() scan (see ready's doc below).
func (th *guestThread) suspendedState() bool { return th.parked && th.parkReady == nil }

// ready mirrors the reference Thread.ready() (:340) plus wazy's cancel-wake
// convention. Deliberately scoped tighter than stackfulTask.ready's sparkBlock
// arm: a nil parkReady (SUSPENDED) is never ready via this path at all --
// only an explicit thread.yield-then-resume (or reap) can resume a suspended
// thread; cancelWake/cancelDeliverable only apply to a WAITING park, matching
// the reference's suspended-thread cancel-wake gate (:373, "cancellable").
func (th *guestThread) ready() bool {
	return th.parkReady != nil &&
		(th.cancelWake || (th.cancellable && th.t.cancelDeliverable()) || th.parkReady())
}

// resumeReady is called by sched.step (driver), pumpSnapshot, task.
// requestCancellation, OR thread.yield-then-resume's direct switch -- on
// whatever goroutine currently holds the baton. Spawns lazily on first
// resume (a never-resumed thread costs nothing until now), then hands the
// baton.
func (th *guestThread) resumeReady() error {
	th.in.sched.unpark(th)
	th.parked, th.parkReady = false, nil
	mode := resumeNormal
	if th.cancelWake {
		th.cancelWake, mode = false, resumeCancelled
	}
	if !th.spawned {
		th.resumeCh, th.yieldCh = make(chan resumeMode), make(chan struct{})
		th.spawned = true
		go th.main()
	}
	return th.handoff(mode)
}

// handoff gives the baton to the goroutine and waits for it back -- shape-
// identical to stackfulTask.handoff.
func (th *guestThread) handoff(mode resumeMode) error {
	th.resumeCh <- mode
	<-th.yieldCh
	if th.done {
		if th.panicVal != nil {
			panic(th.panicVal) // a real bug on the goroutine: surface on the driver
		}
		return th.err
	}
	return nil // parked again: block() already re-registered th in sched.parked
}

// main is the goroutine body -- shape-identical to stackfulTask.main.
func (th *guestThread) main() {
	mode := <-th.resumeCh // wait for the first baton
	defer func() {
		if r := recover(); r != nil && r != any(errStackfulAbort) {
			th.panicVal = r // driver re-panics; never swallow a real bug
		}
		th.done = true
		th.yieldCh <- struct{}{} // final handoff; driver observes done
	}()
	if mode == resumeAbort {
		return // reaped before ever running (only via reap of a just-spawned thread)
	}
	th.err = th.run()
}

// block suspends the calling spawned-thread goroutine until ready() holds
// (or a cancel wake). MUST be called with the baton held (i.e. from a
// builtin invoked by this thread's guest code, ON th's goroutine). Shape-
// identical to stackfulTask.block, plus the activeThread seam (§5.2): cleared
// before parking (so the resumed baton holder sees the implicit thread
// again), restored on our own resume.
func (th *guestThread) block(ready func() bool, cancellable bool) (cancelled bool) {
	if th.t.deliverPendingCancel(cancellable) { // wait_until's prologue (~404)
		return true
	}
	th.parked, th.parkReady, th.cancellable = true, ready, cancellable
	th.in.activeThread = nil
	th.in.suspendRun()       // activeTask=nil, mayEnter=true; exclusive KEPT
	th.in.sched.park(th)     // safe: we hold the baton
	th.yieldCh <- struct{}{} // baton -> driver (or the switcher's resumeInline caller)
	mode := <-th.resumeCh    // baton <- driver (sched.step, canceller, reaper, or a direct switch)
	if mode == resumeAbort {
		panic(errStackfulAbort)
	}
	th.parkReady, th.cancellable = nil, false
	th.in.enterRun(th.t) // re-establish running brackets
	th.in.activeThread = th
	return mode == resumeCancelled
}

// run is the goroutine body's payload -- reference thread_func
// (definitions.py:2695-2697): call the indirect-table func with `arg`, then
// unregister_thread (:518-525), including the last-thread-unresolved trap.
func (th *guestThread) run() error {
	th.in.enterRun(th.t)
	th.in.activeThread = th
	stack := []uint64{th.arg} // ft is (i32)->() or (i64)->(); no results
	err := th.fn.CallWithStack(th.ctx, stack)
	th.in.activeThread = nil
	th.in.leaveRun()
	// unregister_thread (:518-525): free the table slot, decrement the
	// owning task's live-thread count. Done unconditionally (success, guest
	// trap, or reap-abort) -- see reapParkedGoroutines' doc for why the
	// never-spawned reap arm must do this itself instead.
	th.in.threads.remove(th.index)
	th.t.liveThreads--
	if err != nil {
		th.in.poisoned = true // guest code actually ran and trapped -- same rule as stackfulTask.run
		return fmt.Errorf("component/instance: thread %d: %w", th.index, err)
	}
	if th.t.liveThreads == 0 && th.t.state != taskResolved {
		return fmt.Errorf("component/instance: thread %d: last thread of the task exited before the task was resolved", th.index)
	}
	return nil
}

// currentBlocker resolves the reference's current_thread()'s suspension
// capability (design §5.2): a SPAWNED guestThread's own baton takes
// precedence over the owning task's implicit thread, since both may be live
// (parked) at once and only the actual baton holder may legally park itself.
// activeThread is nil for every one of the 29 non-thread.new-indirect-
// execution suites, so this evaluates exactly like the old bare
// `t.blocker()` there.
func (in *Instance) currentBlocker() taskBlocker {
	if th := in.activeThread; th != nil {
		return th
	}
	if t := in.activeTask; t != nil {
		return t.blocker()
	}
	return nil
}

// threadIndexHostFunc backs thread.index (CanonKindThreadIndex, 0x26) --
// canon_thread_index, definitions.py ~2677-2680: return
// current_thread().index. Core sig () -> i32.
//
// Prefers in.activeThread (a spawned thread's own already-known index, §5.2)
// over the current TASK's implicit thread; the implicit-thread slot is
// allocated lazily in in.threads on first call (deviation §11.4 -- the
// reference eagerly registers an implicit thread at enter_implicit_thread,
// :502; wazy defers it so suites that never call thread.index pay nothing).
// The implicit slot is never freed (no vendored suite exercises this at
// runtime to observe a leak -- trap-if-block-and-sync's only call sites are
// inside commented-out switch-to-is-fine/resume-later-is-fine, wast:138-146);
// a spawned thread's OWN slot, by contrast, IS freed at guestThread.run's
// unregister tail. Covered by thread_test.go only.
func threadIndexHostFunc(in *Instance) hostFuncDef {
	in.syncTaskNeeded, in.mayBlockSync = true, true
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "thread.index")
		// Stage D (design §4.3): a SPAWNED thread calling thread.index
		// resolves to its OWN already-known index, not its owning task's
		// implicit-thread slot. Unexercised by any vendored suite (no suite
		// calls thread.index from inside a spawned thread's guest code), but
		// this is the reference's current_thread().index verbatim.
		if th := in.activeThread; th != nil {
			stack[0] = uint64(th.index)
			return
		}
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
// thread.yield-then-resume switch (real since Stage D) or a cancellable
// cancel delivery. Core sig () -> i32 (Cancelled: FALSE=0, TRUE=1).
//
// The only path any vendored suite exercises (design §4.2, §8.3) is
// blockingTask's sync trap: trap-if-block-and-sync's trap-if-suspend is a
// plain sync lift (wast:242) -> syncImplicit task -> "cannot block a
// synchronous task before returning" (async_builtins.go's blockingTask) --
// the exact substring wast:296 asserts. No vendored suite calls
// thread.suspend from a spawned guestThread either, but blockingTask's
// currentBlocker preference (§5.2) means it would correctly resolve to the
// calling guestThread and park it SUSPENDED, resumable by a later
// thread.yield-then-resume targeting its index.
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
// BIND time (design §4.4 step 1-3): resolve the target table (coreTableTarget,
// threaded through graph.go the same way coreMemTarget/coreFuncTarget are)
// and precompute the indirect-call-table typeID the reference's
// trap_if(f.t != ft) will need, validating that the CONSUMER's own declared
// core import signature is one of the two legal shapes -- (i32,i32)->i32 or
// (i32,i64)->i32. wazy decodes the core type section raw-only
// (binary/decoder.go), so `ft` (canon.TypeIdx, a core:typeidx) can't be
// resolved directly; it is derived instead from the consumer's signature
// (deviation logged in design §11.6 -- in any validated component these are
// definitionally equal, and the CALL time LookupFunction typeID check below
// enforces trap_if(f.t != ft) exactly).
//
// CALL time (§4.4 "Call time", Stage D): resolve fi through the table with
// call_indirect semantics, build a SUSPENDED guestThread (§3), register it in
// in.threads and on the owning task's liveThreads, and return its index. The
// goroutine itself is spawned lazily (guestThread.resumeReady, only once
// something actually resumes this thread -- typically
// thread.yield-then-resume, 0x2b).
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

	// Call time (§4.4 "Call time"): resolve fi through the table with
	// call_indirect semantics (LookupFunction panics -- with a wasm-trap-
	// shaped error -- on a bad index / null entry / f.t != ft, exactly the
	// reference's trap_if(f.t != ft), :2692-2694), build a SUSPENDED
	// guestThread, register it in in.threads and on the owning task, and
	// return its index. The goroutine itself is NOT spawned here (lazy spawn
	// in guestThread.resumeReady, §3) -- a never-resumed thread costs
	// nothing and reaps trivially (reapParkedGoroutines' !spawned arm).
	fn := api.GoModuleFunc(func(ctx context.Context, _ api.Module, stack []uint64) {
		t := requireActiveTask(in, "thread.new-indirect") // current_task() + may_leave (:2690-2691)
		fi := api.DecodeU32(stack[0])
		c := stack[1]
		target := wm.LookupFunction(table, typeID, fi) // call_indirect semantics == trap_if(f.t != ft)
		th := &guestThread{t: t, in: in, ctx: ctx, fn: target, arg: c}
		th.parked, th.parkReady = true, nil // SUSPENDED (:2699 assert(new_thread.suspended()))
		in.sched.park(th)
		th.index = in.threads.add(th) // register_thread (:2700, :509)
		t.liveThreads++
		stack[0] = uint64(th.index)
	})
	return hostFuncDef{fn: fn, params: sig.params, results: sig.results}, nil
}

// threadYieldThenResumeHostFunc backs thread.yield-then-resume
// (CanonKindThreadYieldThenResume, 0x2b) -- canon_thread_yield_then_resume,
// definitions.py:2741-2747: resolve inst.threads.get(i), trap_if(not other.
// suspended()), then Thread.yield_then_resume (:420-425): deliver-pending-
// cancel; a direct baton transfer to `other` (self resumed later by the
// scheduler, entering the waiting list behind already-ready work -- the same
// "yield" semantics as thread.yield). Core sig (i:i32) -> i32.
//
// Ordering (design §4.5, deviation §11.3): the reference parks self BEFORE
// switching; wazy switches first, then parks self. Proven observation-
// equivalent -- (a) other runs before the driver does anything else in both;
// (b) self resumes only via the driver's next scan in both; (c) if other
// tried to switch back to self mid-run, both trap (reference: self is
// waiting, not suspended; wazy: self is running-not-parked, so
// suspendedState() is false); (d) neither can interleave a third goroutine
// (reference: nesting_depth; wazy: the switcher's goroutine is blocked in
// the handoff for other's entire run) -- required because the parker IS the
// goroutine that must perform the switch.
func threadYieldThenResumeHostFunc(in *Instance, canon binary.Canon) hostFuncDef {
	in.syncTaskNeeded, in.mayBlockSync = true, true
	cancellable := canon.Cancellable
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "thread.yield-then-resume")
		i := api.DecodeU32(stack[0])
		other, ok := in.threads.getThread(i) // trap: bad index / freed slot / an implicit-thread marker
		if !ok {
			panic(fmt.Errorf("component/instance: thread.yield-then-resume: index %d is not a live spawned thread", i))
		}
		if !other.suspendedState() { // trap_if(not other.suspended())
			panic(fmt.Errorf("component/instance: thread.yield-then-resume: thread %d is not suspended", i))
		}
		t, blk := blockingTask(in, "thread.yield-then-resume") // sync task -> the sync-block trap
		if blk == nil {
			panic(fmt.Errorf("component/instance: thread.yield-then-resume: not supported from an unpromoted callback task"))
		}
		if t.deliverPendingCancel(cancellable) {
			stack[0] = 1
			return
		}

		// Direct switch: the CURRENT goroutine (which holds the baton) acts
		// as the temporary driver for `other` -- resumes it and blocks in the
		// handoff until other parks or finishes. Single-runnable holds: while
		// other runs, we are blocked in <-other.yieldCh executing nothing.
		if err := other.resumeReady(); err != nil {
			// other's thread func trapped while we were suspended-in-spirit:
			// surface it here, exactly as a failing sched thunk would surface
			// on the driver (other.run already set in.poisoned for a real
			// guest trap).
			panic(err)
		}
		// Now park SELF as ready-waiting (the "yield" half): the driver
		// resumes us on its next step, behind any thunks/ready tasks queued
		// meanwhile -- matching the reference's waiting-list re-entry.
		if blk.block(func() bool { return true }, cancellable) {
			stack[0] = 1
			return
		}
		stack[0] = 0
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}
}

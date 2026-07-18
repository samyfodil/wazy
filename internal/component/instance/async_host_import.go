package instance

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file wires an async-lowered host import (canon lower's async option,
// CanonOpt kind 0x06) -- the event SOURCE side of
// docs/component-model-async-runtime-design.md §2 (Q2). An ordinary
// WithImport-registered HostFunc returns its results synchronously to the
// wrapper that called it (host_import.go's buildHostWrapper, the sync arm of
// graph.go's computeCanonHostFunc); an AsyncHostFunc instead receives an
// *AsyncCall and may complete later, driven by the instance's deterministic
// FIFO scheduler (sched.go) -- see AsyncCall.Defer.

// maxFlatAsyncParams is the reference's MAX_FLAT_ASYNC_PARAMS
// (testdata/definitions.py ~1830): the flat-param spill threshold for an
// async-lowered import's core signature (vs MAX_FLAT_PARAMS=16 for a sync
// lower/lift). Async lowers also use max_flat_results=0 UNCONDITIONALLY
// (flatten_functype's async/lower case, ~1856-1861) -- unlike a sync lower's
// MaxFlatResults=1, ANY non-empty result is written through a trailing
// out-pointer parameter, never returned on the core stack. This is not
// merely a spill-threshold difference: a deferred completion writes its
// result long after the wrapper call that would otherwise have returned it
// has already returned, so there is no core return in flight to carry a
// value even when the whole result is a single i32 -- confirmed against
// wasm-tools 1.253's own component-model validator (empirically: a `(func
// async (result u32))` lower's produced core func type is `(i32) -> i32`,
// param present, not `() -> i32`; see this package's async testdata
// fixtures' build notes).
const maxFlatAsyncParams = 4

// AsyncCall is the per-invocation handle an AsyncHostFunc receives, mirroring
// the reference canon_lower's on_start/on_resolve closures
// (testdata/definitions.py ~2250-2268) collapsed into one object (on_start's
// role -- lifting args -- happens before the AsyncHostFunc is even called;
// see buildAsyncHostWrapper). Single-threaded contract: Resolve/Defer may
// only be called (a) synchronously inside the AsyncHostFunc invocation, or
// (b) from a thunk running on the instance's scheduler (i.e. from a Defer'd
// func, transitively) -- both provably on the one goroutine driving this
// Instance. Calling Resolve from anywhere else (another goroutine, or after
// the originating Call has returned) panics: external completion is
// CallAsync's job, not yet implemented (see the design doc's §2.5 "truly
// external completion").
type AsyncCall struct {
	in *Instance
	st *subtask
	// inCall is true while the AsyncHostFunc invocation is on the stack. It is
	// per-AsyncCall, so a truly-external Resolve (from a goroutine the host
	// spawned) always reads it false: this call's host invocation has returned
	// by the time that goroutine fires, whatever else the driver is doing.
	// atomic so that external read is race-free against the driver's Store.
	inCall   atomic.Bool
	resolved bool
}

// Resolve delivers the import's results (reference Subtask.on_resolve, the
// canon_lower on_resolve closure it's built from). One-shot -- a second call
// panics, matching Subtask.resolve's assert(not self.resolved()).
//
//   - Called while inCall (synchronously, before the AsyncHostFunc returns):
//     the wrapper's epilogue observes st.resolved() and takes the
//     reference's immediate fast path -- packed [RETURNED], the subtask
//     never enters the table, no event, no WAIT needed.
//   - Called later, from a scheduler thunk (the subtask already parked):
//     performs the reference's on_resolve + on_progress in one step --
//     lowers results into guest memory now (through the retptr captured at
//     call time) and installs the SUBTASK pending-event closure, so the
//     next WAIT driver's predicate check sees it.
func (ac *AsyncCall) Resolve(results []abi.Value) {
	if ac.in.asyncActive.Load() && !ac.inCall.Load() {
		// CallAsync mode, off-driver completion (hop A): hand it to the driver's
		// mailbox. A synchronous inCall resolve falls through to the inline path
		// below so it still takes the RETURNED fast path.
		ac.in.queueExternalCompletion(asyncCompletion{ac: ac, results: results})
		return
	}
	if ac.resolved {
		panic(fmt.Errorf("component/instance: async import: Resolve called twice"))
	}
	if !ac.inCall.Load() && !ac.in.sched.pumping {
		// Structural single-threadedness guard, no goroutine-ID hacks: the
		// only legal Resolve call sites are inside the import call itself,
		// or inside a scheduler thunk -- both provably on the driving
		// goroutine.
		panic(fmt.Errorf("component/instance: async import: Resolve called outside the instance scheduler; external completion requires CallAsync (not yet implemented)"))
	}
	ac.resolved = true
	if err := ac.st.applyResolve(results); err != nil {
		panic(fmt.Errorf("component/instance: async import: %w", err))
	}
	ac.installParkedPendingEventIfNeeded()
}

// OnCancel registers fn as the reference's Subtask.on_cancel for this call
// (Phase 3, docs/component-model-async-phase3-design.md §2.4): fn runs
// synchronously inside the guest's subtask.cancel. fn may Resolve/
// ResolveCancelled synchronously (the guest then sees the final state,
// never BLOCKED) or later via Defer (an async cancel then returns BLOCKED;
// a sync cancel blocks on the scheduler until fn's eventual Resolve/
// ResolveCancelled). Must be called before the AsyncHostFunc returns -- it
// sets a field on the subtask the wrapper has already constructed.
//
// An import that never calls OnCancel leaves the subtask's onCancel nil:
// subtask.cancel then just records the request and otherwise ignores it --
// spec-legal (a callee may ignore cancellation; the subtask still resolves
// whenever the import eventually completes normally via Resolve).
func (ac *AsyncCall) OnCancel(fn func()) {
	ac.st.onCancelHook = fn
}

// ResolveCancelled resolves the call as cancelled (reference on_resolve
// (None), definitions.py ~2258-2264): always CANCELLED_BEFORE_RETURNED --
// a host-import subtask is never STARTING (buildAsyncHostWrapper lifts args
// eagerly, before the AsyncHostFunc -- and therefore any OnCancel hook --
// ever runs), so it has necessarily already STARTED by the time a
// cancellation can be requested. Same one-shot + same-goroutine discipline
// as Resolve; same parked-path pending-event installation. Panics if called
// without cancellationRequested (the reference asserts a None result
// implies cancellation_requested).
func (ac *AsyncCall) ResolveCancelled() {
	if ac.in.asyncActive.Load() && !ac.inCall.Load() {
		ac.in.queueExternalCompletion(asyncCompletion{ac: ac, cancelled: true})
		return
	}
	if ac.resolved {
		panic(fmt.Errorf("component/instance: async import: Resolve/ResolveCancelled called twice"))
	}
	if !ac.inCall.Load() && !ac.in.sched.pumping {
		panic(fmt.Errorf("component/instance: async import: ResolveCancelled called outside the instance scheduler; external completion requires CallAsync (not yet implemented)"))
	}
	if !ac.st.cancellationRequested {
		panic(fmt.Errorf("component/instance: async import: ResolveCancelled called without a prior subtask.cancel request"))
	}
	ac.resolved = true
	ac.st.resolve(subtaskCancelledBeforeReturned, nil)
	ac.installParkedPendingEventIfNeeded()
}

// installSubtaskEvent installs st's SUBTASK pending-event closure once it is
// both resolved and parked at table index si -- the guest<->guest callee
// wrapper's counterpart to AsyncCall.installParkedPendingEventIfNeeded
// (async_lift.go's buildAsyncHostWrapper callee arm, §3.2's on_resolve).
// Both the delivered state (p2) and deliver_resolve (lend release) are
// evaluated at DELIVERY (Waitable.getPendingEvent), matching
// installParkedPendingEventIfNeeded and the reference exactly.
func installSubtaskEvent(st *subtask, si uint32) {
	st.setPendingSubtaskEvent(st, si) // closure-free; body lives in getPendingEvent
}

// installParkedPendingEventIfNeeded is Resolve/ResolveCancelled's shared
// epilogue: the immediate fast path (still inCall) needs nothing more --
// the wrapper's own epilogue observes st.resolved() -- but a call already
// parked in the table (reference on_progress/subtask_event) needs its
// pending-event closure installed so the next WAIT driver's predicate check
// sees it. Both the delivered state (p2) and deliver_resolve (lend release)
// are evaluated at DELIVERY (Waitable.getPendingEvent), matching the
// reference exactly, not here at resolve time.
func (ac *AsyncCall) installParkedPendingEventIfNeeded() {
	if ac.inCall.Load() {
		return // the wrapper's epilogue sees st.resolved() and takes the immediate path
	}
	st := ac.st
	st.setPendingSubtaskEvent(st, st.subtaski()) // closure-free; body lives in getPendingEvent
}

// Defer enqueues fn on the instance's deterministic FIFO run queue
// (sched.go); it runs later, while the guest is suspended on a WAIT/YIELD or
// the blocking wait builtin. This is how an MVP async import defers
// completion without OS concurrency -- including multi-hop chains (Defer
// inside Defer) to exercise multiple scheduler-drive rounds.
//
// Uses enqueueVoid, not enqueue: fn is stored directly on the runq entry
// instead of being wrapped in a func() error adapter closure, so a Deferred
// completion costs exactly fn's own allocation (if any), not two.
func (ac *AsyncCall) Defer(fn func()) {
	ac.in.sched.enqueueVoid(fn)
}

// asyncResolveCfg is the bind-time half of a host-import subtask's resolve:
// one per async-lowered import binding, built once in buildAsyncHostWrapper
// (amortized), shared by every call through that binding. Immutable after
// bind. Replaces the per-call `st.applyResolve = func(...) {...}` closure
// (which captured all of these plus the per-call retPtr/memMod/ctx) with a
// single shared allocation plus scalar/pointer stores on the subtask (see
// subtask.applyResolve, waitable.go).
type asyncResolveCfg struct {
	iface, funcName string
	resultCount     int
	resultType      binary.TypeDesc
	resultUsesMem   bool
	resolve         abi.Resolver
	resources       *handleTable
	reallocOverride api.Function
}

// applyResolve replaces the per-call closure formerly assigned in
// buildAsyncHostWrapper: lower vals into the calling module's memory through
// the retptr captured at call time and flip state to RETURNED. Behavior is
// byte-identical to the old closure body; mem/realloc are still fetched
// fresh here, not at call time (memory may have grown since). resolveFn, if
// set (test injection / future alternate source), takes priority.
func (st *subtask) applyResolve(vals []abi.Value) error {
	if st.resolveFn != nil {
		return st.resolveFn(vals)
	}
	cfg := st.resolveCfg
	if cfg == nil {
		panic("BUG: applyResolve on a subtask with no resolve source")
	}
	if len(vals) != cfg.resultCount {
		return fmt.Errorf("async import %q %q: Resolve got %d result(s), the import declares %d", cfg.iface, cfg.funcName, len(vals), cfg.resultCount)
	}
	if cfg.resultCount == 0 {
		st.resolve(subtaskReturned, nil)
		return nil
	}
	mem, memAvailable := memoryBytesOf(st.memMod)
	if cfg.resultUsesMem && !memAvailable {
		return fmt.Errorf("async import %q %q: result requires linear memory (string/list), but the calling module has none", cfg.iface, cfg.funcName)
	}
	resultVal, herr := allocHandleResult(cfg.resources, cfg.resultType, vals[0])
	if herr != nil {
		return fmt.Errorf("async import %q %q: result: %w", cfg.iface, cfg.funcName, herr)
	}
	// Resolving "cabi_realloc" (a module export lookup, allocating on both
	// the found and not-found paths -- ModuleInstance.getExport) is only
	// useful when Store might actually grow guest memory for this result (a
	// string/list); a plain-value result (e.g. u32) never calls
	// realloc.grow, so skip the lookup entirely then -- resultUsesMem is
	// precomputed at bind time, so this is a cheap branch, not a
	// re-derivation.
	var realloc abi.Realloc
	if cfg.resultUsesMem {
		realloc = reallocOf(st.resolveCtx, st.memMod)
		if cfg.reallocOverride != nil {
			realloc = reallocOfFunc(st.resolveCtx, cfg.reallocOverride)
		}
	}
	if serr := abi.Store(mem, st.retPtr, cfg.resultType, resultVal, cfg.resolve, realloc); serr != nil {
		return fmt.Errorf("async import %q %q: result: store: %w", cfg.iface, cfg.funcName, serr)
	}
	st.resolve(subtaskReturned, nil)
	return nil
}

// AsyncHostFunc starts one async import call (reference: the FuncInst callee
// canon_lower invokes with on_start/on_resolve/caller). It receives the
// lifted arguments and an *AsyncCall to complete the call, synchronously or
// later via Defer. Returning a non-nil error traps the guest call that
// invoked the import (mirroring HostFunc).
type AsyncHostFunc func(ctx context.Context, args []abi.Value, call *AsyncCall) error

// WithAsyncImport registers fn for a component import that guests lower with
// the async option (CanonOpt kind 0x06). iface/name/params/results have the
// same meaning as WithImport's. Binding an async-lowered import to a
// WithImport-registered fn (or a sync-lowered import to a
// WithAsyncImport-registered fn) fails loud at bind time -- see graph.go's
// computeCanonHostFunc.
func WithAsyncImport(iface, name string, fn AsyncHostFunc, params, results []binary.TypeDesc) Option {
	return func(c *config) {
		c.imports[mkImportKey(iface, name)] = &hostImport{asyncFn: fn, params: params, results: results}
	}
}

// buildAsyncHostWrapper is buildHostWrapper's async-lower counterpart: it
// adapts an AsyncHostFunc to the async-lowered core calling convention
// (reference canon_lower's async arm, testdata/definitions.py ~2279-2295 --
// see docs/component-model-async-runtime-design.md §2.3). Unlike
// buildHostWrapper, it never returns the callee's flattened results on the
// core stack -- the core func always returns exactly one packed i32
// (Subtask.State, or STARTED|subtaski<<4); any non-empty result is written
// through a trailing out-pointer parameter, at RESOLVE time (possibly long
// after this wrapper call returns), never at call time.
func buildAsyncHostWrapper(in *Instance, iface, funcName string, hi *hostImport, resources *handleTable, memOverride api.Module, reallocOverride api.Function) (api.GoModuleFunction, []api.ValueType, []api.ValueType, error) {
	var fd binary.FuncDesc
	var resolve abi.Resolver
	if hi.customFD != nil {
		fd, resolve = *hi.customFD, hi.customResolve
	} else {
		fd, resolve = synthFuncDesc(hi.params, hi.results)
	}

	paramPlans, err := buildHostParamPlans(fd, resolve)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("component/instance: async import %q func %q params: %w", iface, funcName, err)
	}
	flatParamWidth := 0
	for _, pp := range paramPlans {
		flatParamWidth += len(pp.flat)
	}
	// Whole-parameter-list spill (mirrors the sync LIFT side's identical
	// threshold check, instance.go's computeBoundExportABI): beyond
	// maxFlatAsyncParams, FlattenFunc's async arm collapses the core
	// signature to a single i32 pointer into the CALLING module's memory,
	// holding the whole param list stored as a tuple -- exactly what
	// cross-abi-calls.wast's guest side already emits unconditionally
	// (`(import "" "async-5-param" (func $async-5-param (param i32) (result
	// i32)))`, confirmed against the vendored fixture) regardless of whether
	// wazy understood it. paramTupleDesc's Elements reuse fd.Params' own
	// TypeRefs directly, same construction as paramTuple above.
	paramsSpill := flatParamWidth > maxFlatAsyncParams
	var paramTupleDesc binary.TupleDesc
	if paramsSpill {
		elems := make([]binary.TypeRef, len(fd.Params))
		for i, p := range fd.Params {
			elems[i] = p.Type
		}
		paramTupleDesc = binary.TupleDesc{Elements: elems}
	}

	resultCount, resultType, resultUsesMem, err := buildHostResultPlan(fd, resolve)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("component/instance: async import %q func %q results: %w", iface, funcName, err)
	}
	if resultCount > 1 {
		return nil, nil, nil, fmt.Errorf("component/instance: async import %q func %q declares %d results; multiple async-import results are not supported by this milestone", iface, funcName, resultCount)
	}

	var coreParamKinds []string
	if paramsSpill {
		coreParamKinds = make([]string, 0, 2)
		coreParamKinds = append(coreParamKinds, "i32") // pointer to the whole spilled param tuple
	} else {
		coreParamKinds = make([]string, 0, flatParamWidth+1)
		for _, pp := range paramPlans {
			coreParamKinds = append(coreParamKinds, pp.flat...)
		}
	}
	outPtrIdx := -1
	if resultCount == 1 {
		outPtrIdx = len(coreParamKinds)
		coreParamKinds = append(coreParamKinds, "i32") // trailing out-pointer (max_flat_results=0: ANY non-empty result spills)
	}
	apiParams, err := toApiValueTypes(coreParamKinds)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("component/instance: async import %q func %q params: %w", iface, funcName, err)
	}
	apiResults := []api.ValueType{api.ValueTypeI32} // always exactly the packed Subtask.State

	// cfg is the bind-time half of every call's applyResolve, built ONCE
	// here (not per call) and shared by every invocation of this binding --
	// see asyncResolveCfg's doc and subtask.applyResolve (this file).
	cfg := &asyncResolveCfg{
		iface: iface, funcName: funcName,
		resultCount: resultCount, resultType: resultType, resultUsesMem: resultUsesMem,
		resolve: resolve, resources: resources, reallocOverride: reallocOverride,
	}

	fn := api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		if hi.lineage {
			panic(fmt.Errorf("component/instance: async lower %q %q: wasm trap: cannot enter component instance", iface, funcName))
		}
		memMod := mod
		if memOverride != nil {
			memMod = memOverride
		}

		st := newSubtask()

		// Per-call wiring for st.applyResolve (the method, waitable.go/this
		// file): cfg is the bind-time half (built once above); retPtr/memMod
		// are captured now (retPtr decoded from the core stack, memMod may be
		// mem/memOverride); resolveCtx is used for reallocOf at resolve time,
		// fetched fresh there (not captured at call time) since memory may
		// have grown between the call and the eventual resolve (possibly
		// much later, after a Defer round-trip or a parked guestTask).
		st.resolveCfg, st.memMod, st.resolveCtx = cfg, memMod, ctx
		if outPtrIdx >= 0 {
			st.retPtr = api.DecodeU32(stack[outPtrIdx])
		}

		if tgt := hi.asyncTarget; tgt != nil {
			// Guest<->guest callee arm (Phase 3, docs/component-model-
			// async-phase3-design.md §3.2): the reference canon_lower
			// callee protocol (~2246-2271), transliterated. Args are
			// snapshotted from the flat core stack now but LIFTED lazily,
			// inside on_start -- the reference lifts caller args lazily
			// too (~2250-2254), observable when B's entry parks under
			// backpressure and A mutates its own arg buffer before B
			// actually starts. A spilled param list is snapshotted as just
			// its one pointer -- the memory it points into is the CALLER's,
			// read (not copied) lazily inside on_start exactly like the
			// non-spilled flat case reads its snapshotted core values lazily.
			var flat []uint64
			var argPtr uint32
			if paramsSpill {
				argPtr = api.DecodeU32(stack[0])
			} else {
				flat = append([]uint64(nil), stack[:flatParamWidth]...)
			}

			onStart := func(calleeTask *task) ([]abi.Value, error) {
				var args []abi.Value
				var aerr error
				if paramsSpill {
					args, aerr = liftAsyncHostArgsSpilled(in, paramPlans, paramTupleDesc, resolve, argPtr, memMod, resources, st)
				} else {
					args, aerr = liftAsyncHostArgsPlanned(in, paramPlans, resolve, flat, memMod, resources, st)
				}
				if aerr != nil {
					return nil, fmt.Errorf("async lower %q %q: %w", iface, funcName, aerr)
				}
				st.state = subtaskStarted // on_start complete (STARTING -> STARTED, ~2250)
				return mapArgsToProvider(tgt, args, calleeTask)
			}

			onResolve := func(vals []abi.Value, cancelled bool) error {
				if cancelled {
					// reference on_resolve(None) (~2258-2264): STARTING ->
					// CANCELLED_BEFORE_STARTED, else CANCELLED_BEFORE_RETURNED.
					if st.state == subtaskStarting {
						st.resolve(subtaskCancelledBeforeStarted, nil)
					} else {
						st.resolve(subtaskCancelledBeforeReturned, nil)
					}
				} else {
					mapped, merr := mapResultsFromProvider(tgt, vals)
					if merr != nil {
						return fmt.Errorf("async lower %q %q: %w", iface, funcName, merr)
					}
					if aerr := st.applyResolve(mapped); aerr != nil {
						return aerr
					}
				}
				if si := st.subtaski(); si != 0 { // already parked: install like AsyncCall.Resolve's parked arm
					installSubtaskEvent(st, si)
				}
				return nil
			}

			calleeTask, operr := tgt.sub.startAsyncExportTask(ctx, tgt.be, tgt.exportName, onStart, onResolve)
			if operr != nil {
				panic(fmt.Errorf("component/instance: async lower %q %q: %w", iface, funcName, operr))
			}
			st.cancelTask = calleeTask // reference: subtask.on_cancel = callee(...) (~2270)

			// If the CALLER is a plain sync lift (invokeEntered: activeTask nil
			// or a syncImplicit task) there is no scheduler driver above this
			// call to resume the callee -- unlike a callback/stackful caller,
			// whose own invoke frame runs sched.drive/drainReady when it returns
			// (async_lift.go:95/111/151/163). startAsyncExportTask deliberately
			// never drives ("whoever eventually drives that shared scheduler
			// resumes this task", async_lift.go:177), so a callee that parks on
			// an ALREADY-ready waitable-set.wait (big-interleaving-test's
			// $Testee.await, reached via this guest<->guest async-lower from
			// $Driver.run's sync lift) would otherwise sit forever and return a
			// spurious STARTED instead of RETURNED. Drain to quiescence here for
			// exactly that driverless-caller case; a real-task caller is left
			// bit-identical (its driver is above), so no currently-green
			// guest<->guest suite's scheduling order changes.
			if t := in.activeTask; t == nil || t.syncImplicit {
				if err := in.sched.drainReady(); err != nil {
					in.reapParkedGoroutines()
					panic(fmt.Errorf("component/instance: async lower %q %q: %w", iface, funcName, err))
				}
			}

			if st.resolved() { // immediate path: B ran to EXIT without parking
				st.deliverResolve()
				stack[0] = uint64(uint32(st.state)) // RETURNED (CANCELLED_* impossible: nothing could cancel before a handle exists)
				return
			}
			subtaski := resources.addEntry(st)
			st.setSubtaski(subtaski)
			stack[0] = uint64(uint32(st.state)) | uint64(subtaski)<<4 // STARTING or STARTED
			return
		}

		// Plain host-import arm (unchanged from Phase 1/2).
		var args []abi.Value
		var aerr error
		if paramsSpill {
			args, aerr = liftAsyncHostArgsSpilled(in, paramPlans, paramTupleDesc, resolve, api.DecodeU32(stack[0]), memMod, resources, st)
		} else {
			args, aerr = liftAsyncHostArgsPlanned(in, paramPlans, resolve, stack, memMod, resources, st)
		}
		if aerr != nil {
			panic(fmt.Errorf("component/instance: async import %q %q: %w", iface, funcName, aerr))
		}
		st.state = subtaskStarted // on_start complete (args lifted)

		ac := &st.ac
		ac.in, ac.st = in, st
		ac.inCall.Store(true)
		// Bracket the actual Go call -- see buildHostWrapper's identical
		// comment (host_import.go): this is what lets a Write/Read/Set/Get
		// called synchronously from inside an AsyncHostFunc invocation pass
		// requireSchedulable (stream_host.go).
		in.inHostCall++
		ferr := hi.asyncFn(ctx, args, ac)
		in.inHostCall--
		if ferr != nil {
			panic(fmt.Errorf("component/instance: async import %q %q: %w", iface, funcName, ferr))
		}
		ac.inCall.Store(false)

		if st.resolved() {
			// Immediate fast path (reference: "if subtask.resolved(): ...
			// return [Subtask.State.RETURNED]"): no table entry, subtaski 0.
			st.deliverResolve()
			stack[0] = uint64(subtaskReturned)
			return
		}
		subtaski := resources.addEntry(st)
		st.setSubtaski(subtaski)
		stack[0] = uint64(uint32(st.state)) | uint64(subtaski)<<4
	})
	return fn, apiParams, apiResults, nil
}

// mapArgsToProvider is repToProviderHandle applied across a whole (already-
// lifted, importer-side) arg list, for the guest<->guest callee arm's
// on_start (Phase 3, docs/component-model-async-phase3-design.md §3.2) --
// composition.go's delegatingHostImport does the identical mapping for the
// sync arm. calleeTask is B's real task (it already exists by the time
// on_start runs, unlike the sync delegate's throwaway scope-only task),
// so a cross-instance borrow<T> arg is minted call-scoped against it.
func mapArgsToProvider(tgt *guestAsyncTarget, args []abi.Value, calleeTask *task) ([]abi.Value, error) {
	paramDescs := tgt.be.paramTypes
	out := make([]abi.Value, len(args))
	for i, a := range args {
		if i < len(paramDescs) {
			var err error
			if a, err = repToProviderHandle(tgt.sub, paramDescs[i], a, calleeTask); err != nil {
				return nil, err
			}
		}
		out[i] = a
	}
	return out, nil
}

// mapResultsFromProvider is providerHandleToRep applied across a whole
// (provider-side) result list, for the guest<->guest callee arm's
// on_resolve -- the result-side mirror of mapArgsToProvider. The output
// feeds st.applyResolve exactly as a Go AsyncHostFunc's Resolve results do
// (a bare rep for an own/borrow result), so the two arms share the same
// lowering-into-memory code.
func mapResultsFromProvider(tgt *guestAsyncTarget, vals []abi.Value) ([]abi.Value, error) {
	resDescs := resultDescs(tgt.be)
	out := make([]abi.Value, len(vals))
	for i, v := range vals {
		if i < len(resDescs) {
			var err error
			if v, err = providerHandleToRep(tgt.sub, resDescs[i], v); err != nil {
				return nil, err
			}
		}
		out[i] = v
	}
	return out, nil
}

// liftAsyncHostArgsSpilled is liftAsyncHostArgsPlanned's counterpart for a
// param list that flattens beyond maxFlatAsyncParams (see paramsSpill's
// construction in buildAsyncHostWrapper): the core func's one real param is
// a pointer into the CALLING module's memory holding the whole param list
// stored as a tuple, mirroring the sync LIFT side's whole-parameter-list
// spill (boundExport.paramsSpill/lowerParams's abi.SpillValue) in reverse --
// this reads the tuple back out of memory instead of writing it, since this
// wrapper is the LOWER side receiving a caller's already-spilled args.
// Per-param post-processing (borrow lending, resolveHandleArg) is identical
// to liftAsyncHostArgsPlanned's, just sourced from the loaded tuple's
// elements instead of per-plan LiftFlat calls.
func liftAsyncHostArgsSpilled(in *Instance, plans []hostParamPlan, tupleDesc binary.TupleDesc, resolve abi.Resolver, ptr uint32, mod api.Module, resources *handleTable, st *subtask) ([]abi.Value, error) {
	mem, memAvailable := memoryBytesOf(mod)
	if !memAvailable {
		return nil, fmt.Errorf("parameter list spills to memory (flattens beyond the async flat limit), but the calling module has no memory")
	}
	tupleVal, err := abi.Load(mem, ptr, tupleDesc, resolve)
	if err != nil {
		return nil, fmt.Errorf("spilled parameter list: %w", err)
	}
	raw, ok := tupleVal.([]abi.Value)
	if !ok || len(raw) != len(plans) {
		return nil, fmt.Errorf("spilled parameter list: expected %d value(s), got %T", len(plans), tupleVal)
	}
	args := make([]abi.Value, len(plans))
	for i := range plans {
		pp := &plans[i]
		v := raw[i]
		if pp.isBorrow {
			if h, ok := v.(uint32); ok {
				if entry, eerr := resources.entryForLend(pp.borrowRT, h); eerr == nil {
					st.addLender(entry)
				}
			}
		}
		v, err = resolveHandleArg(in, resources, nil, pp.pt, v)
		if err != nil {
			return nil, fmt.Errorf("param %d: %w", i, err)
		}
		args[i] = v
	}
	return args, nil
}

// liftAsyncHostArgsPlanned is liftHostArgsPlanned's async-lower counterpart:
// it lifts the flat core args exactly the same way, but a borrow<T> arg's
// lend is recorded on st.lenders (subtask.addLender) instead of being
// released when THIS wrapper call returns (liftHostArgsPlanned's caller does
// that via a deferred Unlend) -- an async import's borrowed resources must
// stay lent until the subtask's SUBTASK event is delivered, which can be
// long after this call returns (see subtask.deliverResolve, called from the
// pending-event closure AsyncCall.Resolve installs).
func liftAsyncHostArgsPlanned(in *Instance, plans []hostParamPlan, resolve abi.Resolver, stack []uint64, mod api.Module, resources *handleTable, st *subtask) ([]abi.Value, error) {
	mem, memAvailable := memoryBytesOf(mod)
	args := make([]abi.Value, len(plans))
	pos := 0
	for i := range plans {
		pp := &plans[i]
		if pp.usesMem && !memAvailable {
			return nil, fmt.Errorf("param %d requires linear memory (string/list), but the calling module has none", i)
		}
		cvs := make([]abi.CoreValue, len(pp.flat))
		for k := range pp.flat {
			if pos+k >= len(stack) {
				return nil, fmt.Errorf("param %d: core stack underflow (need %d values, have %d)", i, pos+len(pp.flat), len(stack))
			}
			cvs[k] = abi.CoreValue{Kind: pp.flat[k], Bits: stack[pos+k]}
		}
		v, err := abi.LiftFlat(cvs, pp.pt, resolve, mem)
		if err != nil {
			return nil, fmt.Errorf("param %d: lift: %w", i, err)
		}
		// Lend this borrow<T> arg on the subtask, not the table's own
		// Lend/Unlend-by-call-return bookkeeping (see this func's doc): the
		// entry is resolved once via entryForLend (no lendCount change) and
		// handed to addLender, which increments and records it for
		// deliverResolve to release later.
		if pp.isBorrow {
			if h, ok := v.(uint32); ok {
				if entry, eerr := resources.entryForLend(pp.borrowRT, h); eerr == nil {
					st.addLender(entry)
				}
			}
		}
		v, err = resolveHandleArg(in, resources, nil, pp.pt, v)
		if err != nil {
			return nil, fmt.Errorf("param %d: %w", i, err)
		}
		args[i] = v
		pos += len(pp.flat)
	}
	return args, nil
}

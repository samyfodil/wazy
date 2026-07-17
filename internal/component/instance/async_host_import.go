package instance

import (
	"context"
	"fmt"

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
	in       *Instance
	st       *subtask
	subtaski uint32 // set once the subtask is parked in the table; 0 until then (0 is never a valid handle)
	inCall   bool   // true while the AsyncHostFunc invocation is on the stack
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
	if ac.resolved {
		panic(fmt.Errorf("component/instance: async import: Resolve called twice"))
	}
	if !ac.inCall && !ac.in.sched.pumping {
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
	if ac.inCall {
		return // the wrapper's epilogue sees st.resolved() and takes the immediate path
	}
	// Parked: reference on_progress/subtask_event -- install the
	// pending-event closure. Both the delivered state (p2) and
	// deliver_resolve (lend release) are evaluated at DELIVERY
	// (Waitable.getPendingEvent), matching the reference exactly, not here
	// at resolve time.
	st, si := ac.st, ac.subtaski
	st.setPendingEvent(func() eventTuple {
		if st.resolved() && !st.resolveDelivered() {
			st.deliverResolve()
		}
		return eventTuple{code: eventSubtask, p1: si, p2: uint32(st.state)}
	})
}

// Defer enqueues fn on the instance's deterministic FIFO run queue
// (sched.go); it runs later, while the guest is suspended on a WAIT/YIELD or
// the blocking wait builtin. This is how an MVP async import defers
// completion without OS concurrency -- including multi-hop chains (Defer
// inside Defer) to exercise multiple scheduler-drive rounds.
func (ac *AsyncCall) Defer(fn func()) {
	ac.in.sched.enqueue(func() error { fn(); return nil })
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
	if flatParamWidth > maxFlatAsyncParams {
		return nil, nil, nil, fmt.Errorf("component/instance: async import %q func %q has %d flat param(s), exceeding the async flat limit (%d); spilling params to memory is not supported by this milestone", iface, funcName, flatParamWidth, maxFlatAsyncParams)
	}

	resultCount, resultType, resultUsesMem, err := buildHostResultPlan(fd, resolve)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("component/instance: async import %q func %q results: %w", iface, funcName, err)
	}
	if resultCount > 1 {
		return nil, nil, nil, fmt.Errorf("component/instance: async import %q func %q declares %d results; multiple async-import results are not supported by this milestone", iface, funcName, resultCount)
	}

	coreParamKinds := make([]string, 0, flatParamWidth+1)
	for _, pp := range paramPlans {
		coreParamKinds = append(coreParamKinds, pp.flat...)
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

	fn := api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		memMod := mod
		if memOverride != nil {
			memMod = memOverride
		}

		st := newSubtask()
		args, aerr := liftAsyncHostArgsPlanned(in, paramPlans, resolve, stack, memMod, resources, st)
		if aerr != nil {
			panic(fmt.Errorf("component/instance: async import %q %q: %w", iface, funcName, aerr))
		}
		var retPtr uint32
		if outPtrIdx >= 0 {
			retPtr = api.DecodeU32(stack[outPtrIdx])
		}
		st.state = subtaskStarted // on_start complete (args lifted)

		st.applyResolve = func(vals []abi.Value) error {
			if len(vals) != resultCount {
				return fmt.Errorf("async import %q %q: Resolve got %d result(s), the import declares %d", iface, funcName, len(vals), resultCount)
			}
			if resultCount == 0 {
				st.resolve(subtaskReturned, nil)
				return nil
			}
			// Fetched fresh, not captured at call time: memory may have
			// grown between the call and this resolve (possibly much
			// later, after a Defer round-trip).
			mem, memAvailable := memoryBytesOf(memMod)
			if resultUsesMem && !memAvailable {
				return fmt.Errorf("async import %q %q: result requires linear memory (string/list), but the calling module has none", iface, funcName)
			}
			resultVal, herr := allocHandleResult(resources, resultType, vals[0])
			if herr != nil {
				return fmt.Errorf("async import %q %q: result: %w", iface, funcName, herr)
			}
			realloc := reallocOf(ctx, memMod)
			if reallocOverride != nil {
				realloc = reallocOfFunc(ctx, reallocOverride)
			}
			if serr := abi.Store(mem, retPtr, resultType, resultVal, resolve, realloc); serr != nil {
				return fmt.Errorf("async import %q %q: result: store: %w", iface, funcName, serr)
			}
			st.resolve(subtaskReturned, nil)
			return nil
		}

		ac := &AsyncCall{in: in, st: st, inCall: true}
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
		ac.inCall = false

		if st.resolved() {
			// Immediate fast path (reference: "if subtask.resolved(): ...
			// return [Subtask.State.RETURNED]"): no table entry, subtaski 0.
			st.deliverResolve()
			stack[0] = uint64(subtaskReturned)
			return
		}
		subtaski := resources.addEntry(st)
		ac.subtaski = subtaski
		stack[0] = uint64(uint32(st.state)) | uint64(subtaski)<<4
	})
	return fn, apiParams, apiResults, nil
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

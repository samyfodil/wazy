package instance

import (
	"context"
	"errors"
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file wires the stream/future canon builtins (CanonKind 0x0e-0x1b) as
// core funcs -- the same way graph.go's computeCanonHostFunc already wires a
// resource canon or the MVP async builtins (async_builtins.go): each becomes
// a hostFuncDef closing over the *Instance, fixed i32-only core signatures,
// no reflection/WithFunc. See docs/component-model-async-runtime-design.md
// (Phase 1) and docs/component-model-async-phase2-design.md §1 for the
// semantics transliterated here (testdata/definitions.py's stream_copy
// ~2526, future_copy ~2580, cancel_copy ~2632, drop ~2666,
// canon_stream_new/canon_future_new ~2500-2525).

// sideName renders an endSide for error messages.
func sideName(side endSide) string {
	if side == sideWritable {
		return "writable"
	}
	return "readable"
}

// elemContainsResourceHandle walks t for an own/borrow at any depth -- the
// Phase 2 bind-time ceiling on a stream/future ELEMENT type (design doc
// §1.4's "Bind-time fail-loud ceiling": own-in-element needs per-element
// table transfer this milestone doesn't build; borrow is a spec assert).
// Deliberately separate from typeContainsResource (instance.go), which also
// matches stream/future/error-context themselves -- a different question
// (does resolveArgHandlesDepth need to walk this param) from "does this
// ELEMENT type carry a resource handle".
func elemContainsResourceHandle(t binary.TypeDesc, resolve abi.Resolver, depth int) bool {
	if depth > maxResourceWalkDepth {
		return false
	}
	switch d := t.(type) {
	case binary.OwnDesc, binary.BorrowDesc:
		return true
	case binary.ListDesc:
		return elemRefContainsResourceHandle(&d.Element, resolve, depth)
	case binary.FixedListDesc:
		return elemRefContainsResourceHandle(&d.Element, resolve, depth)
	case binary.MapDesc:
		return elemRefContainsResourceHandle(&d.Key, resolve, depth) || elemRefContainsResourceHandle(&d.Value, resolve, depth)
	case binary.OptionDesc:
		return elemRefContainsResourceHandle(&d.Element, resolve, depth)
	case binary.RecordDesc:
		for i := range d.Fields {
			if elemRefContainsResourceHandle(&d.Fields[i].Type, resolve, depth) {
				return true
			}
		}
	case binary.TupleDesc:
		for i := range d.Elements {
			if elemRefContainsResourceHandle(&d.Elements[i], resolve, depth) {
				return true
			}
		}
	case binary.VariantDesc:
		for i := range d.Cases {
			if d.Cases[i].Type != nil && elemRefContainsResourceHandle(d.Cases[i].Type, resolve, depth) {
				return true
			}
		}
	case binary.ResultDesc:
		if d.Ok != nil && elemRefContainsResourceHandle(d.Ok, resolve, depth) {
			return true
		}
		if d.Err != nil && elemRefContainsResourceHandle(d.Err, resolve, depth) {
			return true
		}
	}
	return false
}

func elemRefContainsResourceHandle(ref *binary.TypeRef, resolve abi.Resolver, depth int) bool {
	t, err := resolveTypeRef(ref, resolve)
	if err != nil {
		return false
	}
	return elemContainsResourceHandle(t, resolve, depth+1)
}

// elemContainsBorrowHandle is elemContainsResourceHandle's borrow-only twin
// (Feature 3, docs/component-model-async-final3-fable.md §3.2): the spec
// unconditionally disallows a borrow anywhere in a stream/future element
// type (a borrow's lifetime cannot outlive the call that lent it, which a
// stream/future's async, unbounded-duration transfer can never guarantee),
// so this walk exists to reject it with a spec-grounded message, kept
// separate from own's own (allowed at the top level -- see
// resolveStreamOrFutureElem) so the two ceilings can be told apart.
func elemContainsBorrowHandle(t binary.TypeDesc, resolve abi.Resolver, depth int) bool {
	if depth > maxResourceWalkDepth {
		return false
	}
	switch d := t.(type) {
	case binary.BorrowDesc:
		return true
	case binary.OwnDesc:
		return false
	case binary.ListDesc:
		return elemRefContainsBorrowHandle(&d.Element, resolve, depth)
	case binary.FixedListDesc:
		return elemRefContainsBorrowHandle(&d.Element, resolve, depth)
	case binary.MapDesc:
		return elemRefContainsBorrowHandle(&d.Key, resolve, depth) || elemRefContainsBorrowHandle(&d.Value, resolve, depth)
	case binary.OptionDesc:
		return elemRefContainsBorrowHandle(&d.Element, resolve, depth)
	case binary.RecordDesc:
		for i := range d.Fields {
			if elemRefContainsBorrowHandle(&d.Fields[i].Type, resolve, depth) {
				return true
			}
		}
	case binary.TupleDesc:
		for i := range d.Elements {
			if elemRefContainsBorrowHandle(&d.Elements[i], resolve, depth) {
				return true
			}
		}
	case binary.VariantDesc:
		for i := range d.Cases {
			if d.Cases[i].Type != nil && elemRefContainsBorrowHandle(d.Cases[i].Type, resolve, depth) {
				return true
			}
		}
	case binary.ResultDesc:
		if d.Ok != nil && elemRefContainsBorrowHandle(d.Ok, resolve, depth) {
			return true
		}
		if d.Err != nil && elemRefContainsBorrowHandle(d.Err, resolve, depth) {
			return true
		}
	}
	return false
}

func elemRefContainsBorrowHandle(ref *binary.TypeRef, resolve abi.Resolver, depth int) bool {
	t, err := resolveTypeRef(ref, resolve)
	if err != nil {
		return false
	}
	return elemContainsBorrowHandle(t, resolve, depth+1)
}

// resolveStreamOrFutureElem resolves canon.TypeIdx (which names the
// stream<T>/future<T> TYPE ITSELF, confirmed against wasm-tools 1.253's
// StreamNew{ty:0}/FutureNew{ty:1} encoding -- see
// internal/component/binary/async_test.go) to its element type (nil for a
// bare stream/future), plus the element's size/alignment/numeric-ness,
// precomputed once at bind time.
func resolveStreamOrFutureElem(comp *binary.Component, resolve abi.Resolver, isFuture bool, typeIdx uint32) (elem binary.TypeDesc, elemSz, align uint32, numeric bool, err error) {
	td, err := comp.ResolveType(typeIdx)
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("type %d: %w", typeIdx, err)
	}
	var ref *binary.TypeRef
	if isFuture {
		fd, ok := td.(binary.FutureDesc)
		if !ok {
			return nil, 0, 0, false, fmt.Errorf("type %d is not a future type (got %T)", typeIdx, td)
		}
		ref = fd.Element
	} else {
		sd, ok := td.(binary.StreamDesc)
		if !ok {
			return nil, 0, 0, false, fmt.Errorf("type %d is not a stream type (got %T)", typeIdx, td)
		}
		ref = sd.Element
	}
	if ref == nil {
		return nil, 0, 0, true, nil
	}
	elem, err = resolveTypeRef(ref, resolve)
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("element type: %w", err)
	}
	// Feature 3 (docs/component-model-async-final3-fable.md §3.2): a
	// TOP-LEVEL own<R> element is allowed -- the per-element transfer below
	// (guestBuffer.read/write) mints/takes it in the writer's/reader's own
	// handle table at rendezvous time, exactly the reference's Buffer
	// lift/lower semantics. A borrow anywhere is still rejected (spec:
	// invalid in a stream/future element type, its scoped lifetime can't
	// survive an async, unbounded-duration transfer). own at depth > 0
	// (e.g. stream<record{own R}>) is still rejected too -- a real,
	// documented deferral: this milestone's transfer only walks the
	// TOP-LEVEL value, not arbitrary nested own leaves.
	if elemContainsBorrowHandle(elem, resolve, 0) {
		return nil, 0, 0, false, fmt.Errorf("stream/future element type contains a borrow handle, which the component-model spec disallows")
	}
	if _, isOwn := elem.(binary.OwnDesc); !isOwn && elemContainsResourceHandle(elem, resolve, 0) {
		return nil, 0, 0, false, fmt.Errorf("stream/future element type contains an own/borrow resource handle, which is not supported by this milestone")
	}
	elemSz, err = abi.Size(elem, resolve)
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("element size: %w", err)
	}
	align, err = abi.Alignment(elem, resolve)
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("element alignment: %w", err)
	}
	numeric = noneOrNumberType(elem)
	return elem, elemSz, align, numeric, nil
}

// hasAsyncOpt reports whether canon carries the async CanonOpt (kind 0x06) --
// stream.{read,write} and {stream,future}.cancel-{read,write} all gate their
// BLOCKED-vs-wait behavior on it (decoded separately as canon.Async for the
// cancel builtins; read/write instead carry it as an ordinary opt).
func hasAsyncOpt(canon binary.Canon) bool {
	for _, opt := range canon.Opts {
		if opt.Kind == 0x06 {
			return true
		}
	}
	return false
}

func isStreamCanonKind(k byte) bool {
	switch k {
	case binary.CanonKindStreamNew, binary.CanonKindStreamRead, binary.CanonKindStreamWrite,
		binary.CanonKindStreamCancelRead, binary.CanonKindStreamCancelWrite,
		binary.CanonKindStreamDropReadable, binary.CanonKindStreamDropWritable,
		binary.CanonKindStreamForward:
		return true
	}
	return false
}

func isFutureCanonKind(k byte) bool {
	switch k {
	case binary.CanonKindFutureNew, binary.CanonKindFutureRead, binary.CanonKindFutureWrite,
		binary.CanonKindFutureCancelRead, binary.CanonKindFutureCancelWrite,
		binary.CanonKindFutureDropReadable, binary.CanonKindFutureDropWritable,
		binary.CanonKindFutureForward:
		return true
	}
	return false
}

// streamFutureCanonHostFunc is graph.go's computeCanonHostFunc entry point
// for every stream/future canon (0x0e-0x1b): resolve the element once, then
// dispatch to the specific builtin builder.
func streamFutureCanonHostFunc(comp *binary.Component, in *Instance, canon binary.Canon, coreMemTarget func(int) (api.Module, error), coreFuncTarget func(int) (api.Module, string, error)) (hostFuncDef, error) {
	isFuture := isFutureCanonKind(canon.Kind)
	elemDesc, elemSz, align, numeric, err := resolveStreamOrFutureElem(comp, in.resolve, isFuture, canon.TypeIdx)
	if err != nil {
		return hostFuncDef{}, fmt.Errorf("canon %#x: %w", canon.Kind, err)
	}

	switch canon.Kind {
	case binary.CanonKindStreamNew:
		return streamNewHostFunc(in, elemDesc, elemSz, align, numeric), nil
	case binary.CanonKindFutureNew:
		return futureNewHostFunc(in, elemDesc, elemSz, align, numeric), nil

	case binary.CanonKindStreamRead, binary.CanonKindStreamWrite:
		memMod, reallocFn, merr := canonMemoryAndRealloc(canon, coreMemTarget, coreFuncTarget)
		if merr != nil {
			return hostFuncDef{}, merr
		}
		side, evCode := sideReadable, eventStreamRead
		if canon.Kind == binary.CanonKindStreamWrite {
			side, evCode = sideWritable, eventStreamWrite
		}
		async := hasAsyncOpt(canon)
		if !async {
			// Feature 1 promotion gate (docs/component-model-async-final3-
			// fable.md §1.1): a SYNC stream/write is a mid-core-call blocking
			// site (streamCopyHostFunc's wait_for_pending_event) that a
			// promoted callback task's segment can park at instead of
			// nested-driving. See guestTask.promoted.
			in.mayBlockSync = true
		}
		return streamCopyHostFunc(in, side, evCode, elemDesc, elemSz, align, numeric, async, memMod, reallocFn), nil

	case binary.CanonKindFutureRead, binary.CanonKindFutureWrite:
		memMod, reallocFn, merr := canonMemoryAndRealloc(canon, coreMemTarget, coreFuncTarget)
		if merr != nil {
			return hostFuncDef{}, merr
		}
		side, evCode := sideReadable, eventFutureRead
		if canon.Kind == binary.CanonKindFutureWrite {
			side, evCode = sideWritable, eventFutureWrite
		}
		async := hasAsyncOpt(canon)
		if !async {
			in.mayBlockSync = true // see the stream.read/write case above
		}
		return futureCopyHostFunc(in, side, evCode, elemDesc, elemSz, align, numeric, async, memMod, reallocFn), nil

	case binary.CanonKindStreamCancelRead, binary.CanonKindStreamCancelWrite:
		side, evCode := sideReadable, eventStreamRead
		if canon.Kind == binary.CanonKindStreamCancelWrite {
			side, evCode = sideWritable, eventStreamWrite
		}
		if !canon.Async {
			in.mayBlockSync = true // see the stream.read/write case above
		}
		return cancelCopyHostFunc(in, false, side, evCode, elemDesc, canon.Async), nil

	case binary.CanonKindFutureCancelRead, binary.CanonKindFutureCancelWrite:
		side, evCode := sideReadable, eventFutureRead
		if canon.Kind == binary.CanonKindFutureCancelWrite {
			side, evCode = sideWritable, eventFutureWrite
		}
		if !canon.Async {
			in.mayBlockSync = true // see the stream.read/write case above
		}
		return cancelCopyHostFunc(in, true, side, evCode, elemDesc, canon.Async), nil

	case binary.CanonKindStreamDropReadable, binary.CanonKindStreamDropWritable:
		side := sideReadable
		if canon.Kind == binary.CanonKindStreamDropWritable {
			side = sideWritable
		}
		return streamDropHostFunc(in, side, elemDesc), nil

	case binary.CanonKindFutureDropReadable, binary.CanonKindFutureDropWritable:
		side := sideReadable
		if canon.Kind == binary.CanonKindFutureDropWritable {
			side = sideWritable
		}
		return futureDropHostFunc(in, side, elemDesc), nil

	case binary.CanonKindStreamForward:
		return streamForwardHostFunc(in, elemDesc), nil
	case binary.CanonKindFutureForward:
		return futureForwardHostFunc(in, elemDesc), nil

	default:
		return hostFuncDef{}, fmt.Errorf("canon kind %#x is not a stream/future builtin", canon.Kind)
	}
}

// requireStreamEnd resolves handle i to a *streamEnd, trapping unless it
// exists, is a stream end of the expected side, and its shared element type
// matches elemDesc (trap_if(shared.t != stream_t.t)). Does NOT remove the
// entry -- callers that need to (drop) do so themselves after their own
// state checks, matching Phase 1's validate-then-remove convention.
func requireStreamEnd(in *Instance, name string, i uint32, side endSide, elemDesc binary.TypeDesc) *streamEnd {
	raw, ok := in.resources.getEntry(i)
	if !ok {
		panic(fmt.Errorf("component/instance: %s: unknown handle index %d", name, i))
	}
	se, isSE := raw.(*streamEnd)
	if !isSE {
		panic(fmt.Errorf("component/instance: %s: handle index %d used with the wrong type: not a stream end", name, i))
	}
	if se.side != side {
		panic(fmt.Errorf("component/instance: %s: handle index %d used with the wrong type: not a %s stream end", name, i, sideName(side)))
	}
	if !elemTypesCompatible(se.shared.elemIn, se.shared.elem, in, elemDesc) {
		panic(fmt.Errorf("component/instance: %s: handle index %d used with the wrong type: stream element type mismatch", name, i))
	}
	return se
}

// requireFutureEnd is requireStreamEnd's future twin.
func requireFutureEnd(in *Instance, name string, i uint32, side endSide, elemDesc binary.TypeDesc) *futureEnd {
	raw, ok := in.resources.getEntry(i)
	if !ok {
		panic(fmt.Errorf("component/instance: %s: unknown handle index %d", name, i))
	}
	fe, isFE := raw.(*futureEnd)
	if !isFE {
		panic(fmt.Errorf("component/instance: %s: handle index %d used with the wrong type: not a future end", name, i))
	}
	if fe.side != side {
		panic(fmt.Errorf("component/instance: %s: handle index %d used with the wrong type: not a %s future end", name, i, sideName(side)))
	}
	if !elemTypesCompatible(fe.shared.elemIn, fe.shared.elem, in, elemDesc) {
		panic(fmt.Errorf("component/instance: %s: handle index %d used with the wrong type: future element type mismatch", name, i))
	}
	return fe
}

// wrapSchedErr names the deadlock trap the same way async_builtins.go's
// waitable-set.wait does, for a stream/future copy/cancel's own nested
// sched.drive.
func wrapSchedErr(name string, err error) error {
	if errors.Is(err, errAsyncDeadlock) {
		return fmt.Errorf("component/instance: %s: deadlock: no pending event and the run queue is empty; an import resolved externally requires CallAsync (not yet implemented)", name)
	}
	return fmt.Errorf("component/instance: %s: %w", name, err)
}

// streamNewHostFunc backs stream.new (CanonKindStreamNew, 0x0e). Core sig
// () -> i64: pack both fresh handles. canon_stream_new adds the READABLE
// handle FIRST (deterministic index order).
func streamNewHostFunc(in *Instance, elemDesc binary.TypeDesc, elemSz, align uint32, numeric bool) hostFuncDef {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "stream.new")
		shared := &sharedStream{elem: elemDesc, elemSz: elemSz, align: align, numeric: numeric, elemIn: in}
		re := &streamEnd{side: sideReadable, state: copyIdle, shared: shared}
		we := &streamEnd{side: sideWritable, state: copyIdle, shared: shared}
		ri := in.resources.addEntry(re)
		wi := in.resources.addEntry(we)
		registerStreamEnd(re, ri)
		registerStreamEnd(we, wi)
		stack[0] = uint64(ri) | uint64(wi)<<32
	})
	return hostFuncDef{fn: fn, params: nil, results: []api.ValueType{api.ValueTypeI64}}
}

// futureNewHostFunc backs future.new (CanonKindFutureNew, 0x15) -- identical
// shape to streamNewHostFunc.
func futureNewHostFunc(in *Instance, elemDesc binary.TypeDesc, elemSz, align uint32, numeric bool) hostFuncDef {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "future.new")
		shared := &sharedFuture{elem: elemDesc, elemSz: elemSz, align: align, numeric: numeric, elemIn: in}
		re := &futureEnd{side: sideReadable, state: copyIdle, shared: shared}
		we := &futureEnd{side: sideWritable, state: copyIdle, shared: shared}
		ri := in.resources.addEntry(re)
		wi := in.resources.addEntry(we)
		registerFutureEnd(re, ri)
		registerFutureEnd(we, wi)
		stack[0] = uint64(ri) | uint64(wi)<<32
	})
	return hostFuncDef{fn: fn, params: nil, results: []api.ValueType{api.ValueTypeI64}}
}

// streamCopyHostFunc is the shared implementation of stream.read (0x0f) and
// stream.write (0x10), mirroring stream_copy. Core sig (i:i32, ptr:i32,
// n:i32) -> i32.
//
// CRITICAL: state flips COPYING->IDLE/DONE INSIDE the streamEvent thunk, at
// delivery time (getPendingEvent), never here at call time -- see
// docs/component-model-async-phase2-design.md §1.3. This is what makes
// stream.drop-readable's trap_if(copying()) fire on a completed-but-
// undelivered copy, and what makes stream.cancel-read racing a completion
// deliver COMPLETED instead of CANCELLED.
func streamCopyHostFunc(in *Instance, side endSide, evCode eventCode, elemDesc binary.TypeDesc, elemSz, align uint32, numeric, async bool, memMod api.Module, reallocFn api.Function) hostFuncDef {
	name := "stream.write"
	if side == sideReadable {
		name = "stream.read"
	}
	fn := api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		requireMayLeave(in, name)
		// The SYNC (non-async) variant needs requireNotSyncImplicit's
		// classification (its own doc), checked BEFORE resolving the handle:
		// trap-if-block-and-sync's trap-if-sync-stream-read/write
		// (wast:320/322) use deliberately INVALID handles yet still expect
		// "cannot block a synchronous task before returning" (a real
		// syncImplicit task) rather than a handle-validity trap, so the
		// ordering matters. The async variant is never genuinely blocking
		// (BLOCKED is returned immediately) and is legal from any context,
		// so it skips this entirely.
		var blk taskBlocker
		if !async {
			blk = requireNotSyncImplicit(in, name)
		}
		i, ptr, n := api.DecodeU32(stack[0]), api.DecodeU32(stack[1]), api.DecodeU32(stack[2])

		e := requireStreamEnd(in, name, i, side, elemDesc)
		if e.state != copyIdle {
			panic(fmt.Errorf("component/instance: %s: stream end %d: %s", name, i, copyNotIdleTrapText(false, side, e.state)))
		}
		if e.inWaitableSet() && !async {
			panic(fmt.Errorf("component/instance: %s: stream end %d: waitable cannot be used synchronously while added to a waitable set", name, i))
		}

		m := memMod
		if m == nil {
			m = mod
		}
		// Lazy/nil-safe: a bare stream/future (elemDesc == nil) never
		// touches memory at all (newGuestBuffer/guestBuffer.read/write
		// short-circuit before any memory access), so don't force a
		// mod.ExportedFunction lookup -- and don't panic -- when m is nil
		// (the calling module genuinely has none, or this canon carries no
		// memory opt and the runtime caller supplied none).
		var realloc abi.Realloc
		if reallocFn != nil {
			realloc = reallocOfFunc(ctx, reallocFn, m)
		} else if m != nil {
			realloc = reallocOf(ctx, m)
		}

		buf, err := newGuestBuffer(in, m, realloc, in.resolve, elemDesc, elemSz, align, numeric, ptr, n)
		if err != nil {
			panic(fmt.Errorf("component/instance: %s: %w", name, err))
		}

		// -- the three closures, verbatim from stream_copy --
		// The result is decided HERE, at delivery, from the end's state -- not
		// captured when the copy finished. StreamEnd.notify stores a closure and
		// reads self.state inside it (definitions.py ~1008-1024), which is what
		// lets a stream.cancel-write issued after a completion still report
		// CANCELLED with the progress that already happened: cancel_copy flips
		// the state to CANCELLING_COPY and then delivers the event that was
		// already pending. cancel-stream.wast's cancel-{write,read}-after-
		// completion assert exactly that packing (CANCELLED | 2<<4), and wazy
		// baked COMPLETED into the closure at copy time instead.
		//
		// dropped is the reference's `self.other is None` -- the peer end is
		// gone, which outranks a pending cancellation.
		streamEvent := func(dropped bool, reclaim func()) eventTuple {
			reclaim()
			var result copyResult
			switch {
			case dropped:
				result, e.state = copyDropped, copyDone
			case e.state == copyCancelling:
				result, e.state = copyCancelled, copyIdle
			default:
				result, e.state = copyCompleted, copyIdle
			}
			return eventTuple{code: evCode, p1: i, p2: packCopyResult(result, buf.progressed())}
		}
		onCopy := func(reclaim func()) { // partial progress: COMPLETED + live reclaim
			// LIVE (still cancellable) only when OUR buffer still has unfilled
			// capacity after this rendezvous -- an onCopy that happened to
			// fully satisfy us (buf.remain() == 0, e.g. a write sized to
			// exactly match our read) is just as FINAL as onCopyDone, even
			// though it arrived via the same pre-copy-remain()>0 branch in
			// sharedStream.write/read. See streamEnd.livePending's doc.
			e.livePending = buf.remain() > 0
			e.setPendingEvent(func() eventTuple { return streamEvent(false, reclaim) })
		}
		onCopyDone := func(result copyResult) {
			e.livePending = false // final -- see streamEnd.livePending's doc
			dropped := result == copyDropped
			e.setPendingEvent(func() eventTuple { return streamEvent(dropped, func() {}) })
		}

		e.livePending = false
		e.state = copyCopying
		var cerr error
		if side == sideReadable {
			cerr = e.shared.read(in, buf, onCopy, onCopyDone)
		} else {
			cerr = e.shared.write(in, buf, onCopy, onCopyDone)
		}
		if cerr != nil {
			panic(fmt.Errorf("component/instance: %s: %w", name, cerr))
		}

		if !e.hasPendingEvent() {
			if async {
				stack[0] = uint64(blockedSentinel) // [BLOCKED]
				return
			}
			// Sync copy: wait_for_pending_event -- split by calling context
			// (docs/component-model-async-stackful-design.md §4.2,
			// generalized by Feature 1 §1.4): a stackful caller, or a
			// promoted callback task's live segment, parks (letting the
			// peer's task, resumable only by the outer driver once our
			// frames yield the baton, make progress); everyone else keeps
			// the Phase 1-3 nested scheduler drive. THE fix for sync-streams
			// (docs/component-model-async-final3-fable.md Feature 1): a
			// promoted callback task's blk is now non-nil here, so it parks
			// instead of nested-driving and frame-holding its own caller's
			// continuation. blk was already resolved above (task-less/
			// syncImplicit already trapped there); an unpromoted callback
			// task's nil blk still falls back to the nested drive.
			// syncWaiter brackets the block (design docs/component-model-
			// async-threads-design-fable.md §6.2, reference Waitable.
			// wait_for_pending_event, definitions.py:775-779): a spawned
			// guestThread parked here across scheduler steps must be visible
			// to waitable.join's/subtask.cancel's in-set trap (§6.3).
			e.waitablePtr().syncWaiter = true
			if blk != nil {
				// A cancel-wake (subtask.cancel resuming us while parked here)
				// returns from block with no event pending: cancel the copy so it
				// reports CANCELLED, exactly as an explicit stream.cancel-read
				// would, instead of reading an event that never arrived.
				if blk.block(e.hasPendingEvent) && !e.hasPendingEvent() {
					e.sharedCancel()
				}
			} else if derr := in.sched.drive(e.hasPendingEvent); derr != nil {
				e.waitablePtr().syncWaiter = false
				panic(wrapSchedErr(name, derr))
			}
			e.waitablePtr().syncWaiter = false
		}
		ev := e.getPendingEvent() // runs streamEvent: reclaims + flips state
		stack[0] = uint64(ev.p2)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}
}

// futureCopyHostFunc is the shared implementation of future.read (0x16) and
// future.write (0x17): the same skeleton as streamCopyHostFunc minus n (core
// sig (i32, i32) -> i32; buffer length pinned to 1), minus onCopy (no partial
// progress), and with the raw result as payload (no progress shift).
func futureCopyHostFunc(in *Instance, side endSide, evCode eventCode, elemDesc binary.TypeDesc, elemSz, align uint32, numeric, async bool, memMod api.Module, reallocFn api.Function) hostFuncDef {
	name := "future.write"
	if side == sideReadable {
		name = "future.read"
	}
	fn := api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		requireMayLeave(in, name)
		// See streamCopyHostFunc's identical early requireNotSyncImplicit classification.
		var blk taskBlocker
		if !async {
			blk = requireNotSyncImplicit(in, name)
		}
		i, ptr := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])

		e := requireFutureEnd(in, name, i, side, elemDesc)
		if e.state != copyIdle {
			panic(fmt.Errorf("component/instance: %s: future end %d: %s", name, i, copyNotIdleTrapText(true, side, e.state)))
		}
		if e.inWaitableSet() && !async {
			panic(fmt.Errorf("component/instance: %s: future end %d: waitable cannot be used synchronously while added to a waitable set", name, i))
		}

		m := memMod
		if m == nil {
			m = mod
		}
		// Lazy/nil-safe: a bare stream/future (elemDesc == nil) never
		// touches memory at all (newGuestBuffer/guestBuffer.read/write
		// short-circuit before any memory access), so don't force a
		// mod.ExportedFunction lookup -- and don't panic -- when m is nil
		// (the calling module genuinely has none, or this canon carries no
		// memory opt and the runtime caller supplied none).
		var realloc abi.Realloc
		if reallocFn != nil {
			realloc = reallocOfFunc(ctx, reallocFn, m)
		} else if m != nil {
			realloc = reallocOf(ctx, m)
		}

		buf, err := newGuestBuffer(in, m, realloc, in.resolve, elemDesc, elemSz, align, numeric, ptr, 1)
		if err != nil {
			panic(fmt.Errorf("component/instance: %s: %w", name, err))
		}

		// assert((buf.remain() == 0) == (result == copyCompleted)) is a
		// reference assert (unreachable through the builtins); not
		// transliterated as a Go check.
		futureEvent := func(result copyResult) eventTuple {
			if result == copyDropped || result == copyCompleted {
				e.state = copyDone
			} else {
				e.state = copyIdle
			}
			return eventTuple{code: evCode, p1: i, p2: uint32(result)}
		}
		// assert(result != DROPPED || evCode == eventFutureWrite): only a
		// WRITER can observe DROPPED (a reader never can -- the writable end
		// can't be dropped before completing, see futureDropHostFunc).
		onCopyDone := func(result copyResult) {
			e.setPendingEvent(func() eventTuple { return futureEvent(result) })
		}

		e.state = copyCopying
		var cerr error
		if side == sideReadable {
			cerr = e.shared.read(in, buf, onCopyDone)
		} else {
			cerr = e.shared.write(in, buf, onCopyDone)
		}
		if cerr != nil {
			panic(fmt.Errorf("component/instance: %s: %w", name, cerr))
		}

		if !e.hasPendingEvent() {
			if async {
				stack[0] = uint64(blockedSentinel)
				return
			}
			// See streamCopyHostFunc's identical split (blk resolved above)
			// and identical syncWaiter bracket (§6.2).
			e.waitablePtr().syncWaiter = true
			if blk != nil {
				// A cancel-wake (subtask.cancel resuming us while parked here)
				// returns from block with no event pending: cancel the copy so it
				// reports CANCELLED, exactly as an explicit stream.cancel-read
				// would, instead of reading an event that never arrived.
				if blk.block(e.hasPendingEvent) && !e.hasPendingEvent() {
					e.sharedCancel()
				}
			} else if derr := in.sched.drive(e.hasPendingEvent); derr != nil {
				e.waitablePtr().syncWaiter = false
				panic(wrapSchedErr(name, derr))
			}
			e.waitablePtr().syncWaiter = false
		}
		ev := e.getPendingEvent()
		stack[0] = uint64(ev.p2)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}
}

// streamDropHostFunc backs stream.drop-readable (0x13) / stream.drop-writable
// (0x14), transliterating `drop` + CopyEnd.drop. Validate-then-remove: a kind
// mismatch leaves the table untouched.
func streamDropHostFunc(in *Instance, side endSide, elemDesc binary.TypeDesc) hostFuncDef {
	name := "stream.drop-writable"
	if side == sideReadable {
		name = "stream.drop-readable"
	}
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, name)
		i := api.DecodeU32(stack[0])
		e := requireStreamEnd(in, name, i, side, elemDesc)
		if e.copying() { // CopyEnd.drop: covers the undelivered-event case too
			// (state flips only at delivery -- a completed-but-undelivered
			// copy is still COPYING here). Spec text differs by side: dropping
			// the READABLE end of a busy stream is "cannot remove busy
			// stream" (drop-stream.wast's drop-while-reading); the WRITABLE
			// end is "cannot drop busy stream" (drop-while-writing).
			verb := "drop"
			if side == sideReadable {
				verb = "remove"
			}
			panic(fmt.Errorf("component/instance: %s: stream end %d: cannot %s busy stream", name, i, verb))
		}
		e.drop()
		in.resources.removeEntry(i)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: nil}
}

// drop is End.drop for an end already taken out of its table: the
// counterpart's parked copy, or the idle counterpart itself, learns of it.
func (e *streamEnd) drop() {
	peer, peerSide := peerOfStream(e)
	clearStreamEnd(e)
	e.shared.drop() // counterpart's parked copy => DROPPED event
	notifyPeerDropped(e.shared.dropDebt(), peerSide, peer)
	e.dropWaitable()
}

// drop is streamEnd.drop's future twin.
func (e *futureEnd) drop() {
	peer, peerSide := peerOfFuture(e)
	clearFutureEnd(e)
	e.shared.drop()
	notifyPeerDropped(e.shared.dropDebt(), peerSide, peer)
	e.dropWaitable()
}

// futureDropHostFunc backs future.drop-readable (0x1a) / future.drop-writable
// (0x1b). The WRITABLE side adds WritableFutureEnd.drop's EXTRA gate BEFORE
// the CopyEnd logic: trap_if(state != DONE) -- a future writer may not walk
// away without either completing its write or having observed DROPPED. This
// is what makes sharedFuture.read's "unreachable" dropped-panic and
// future_copy's reader-never-sees-DROPPED assert actually unreachable.
func futureDropHostFunc(in *Instance, side endSide, elemDesc binary.TypeDesc) hostFuncDef {
	name := "future.drop-writable"
	if side == sideReadable {
		name = "future.drop-readable"
	}
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, name)
		i := api.DecodeU32(stack[0])
		e := requireFutureEnd(in, name, i, side, elemDesc)
		if side == sideWritable && e.state != copyDone {
			panic(fmt.Errorf("component/instance: %s: future end %d: cannot drop future write end without first writing a value", name, i))
		}
		if e.copying() {
			panic(fmt.Errorf("component/instance: %s: future end %d: cannot have concurrent operations active on a future/stream", name, i))
		}
		e.drop()
		in.resources.removeEntry(i)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: nil}
}

// forwardTrapText is what canon_{stream,future}.forward traps with for an end
// that is not IDLE or is joined to a waitable set, or "" when it is neither.
func forwardTrapText(kind string, side endSide, state copyState, inSet bool) string {
	switch {
	case state == copyDone:
		peer := "writable"
		if side == sideWritable {
			peer = "readable"
		}
		return "cannot forward " + kind + " after being notified that the " + peer + " end dropped"
	case state != copyIdle:
		return "cannot forward busy " + kind
	case inSet:
		return "cannot forward " + kind + " while it's in a waitable set"
	}
	return ""
}

// streamForwardHostFunc backs stream.forward (0x2e), canon_stream_forward.
// Core sig (ri, wi: i32) -> (): this instance gives up the readable end ri and
// the writable end wi, and the writer feeding ri is joined directly to the
// reader wi feeds, so no copy passes through this instance again.
func streamForwardHostFunc(in *Instance, elemDesc binary.TypeDesc) hostFuncDef {
	const name = "stream.forward"
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, name)
		ri, wi := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
		src := requireStreamEnd(in, name, ri, sideReadable, elemDesc)
		if msg := forwardTrapText("stream", sideReadable, src.state, src.inWaitableSet()); msg != "" {
			panic(fmt.Errorf("component/instance: %s: stream end %d: %s", name, ri, msg))
		}
		if wi == ri { // the reference has already removed ri by now
			panic(fmt.Errorf("component/instance: %s: unknown handle index %d", name, wi))
		}
		dst := requireStreamEnd(in, name, wi, sideWritable, elemDesc)
		if msg := forwardTrapText("stream", sideWritable, dst.state, dst.inWaitableSet()); msg != "" {
			panic(fmt.Errorf("component/instance: %s: stream end %d: %s", name, wi, msg))
		}
		in.resources.removeEntry(ri)
		in.resources.removeEntry(wi)
		if err := forwardStream(src, dst); err != nil {
			panic(fmt.Errorf("component/instance: %s: %w", name, err))
		}
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, results: nil}
}

// futureForwardHostFunc backs future.forward (0x2f), streamForwardHostFunc's
// future twin.
func futureForwardHostFunc(in *Instance, elemDesc binary.TypeDesc) hostFuncDef {
	const name = "future.forward"
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, name)
		ri, wi := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
		src := requireFutureEnd(in, name, ri, sideReadable, elemDesc)
		if msg := forwardTrapText("future", sideReadable, src.state, src.inWaitableSet()); msg != "" {
			panic(fmt.Errorf("component/instance: %s: future end %d: %s", name, ri, msg))
		}
		if wi == ri {
			panic(fmt.Errorf("component/instance: %s: unknown handle index %d", name, wi))
		}
		dst := requireFutureEnd(in, name, wi, sideWritable, elemDesc)
		if msg := forwardTrapText("future", sideWritable, dst.state, dst.inWaitableSet()); msg != "" {
			panic(fmt.Errorf("component/instance: %s: future end %d: %s", name, wi, msg))
		}
		in.resources.removeEntry(ri)
		in.resources.removeEntry(wi)
		if err := forwardFuture(src, dst); err != nil {
			panic(fmt.Errorf("component/instance: %s: %w", name, err))
		}
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, results: nil}
}

// forwardStream is End.forward (definitions.py ~992-1008). src and dst are out
// of their table. When either has nobody on its other side, or src's other side
// is dst itself, both are simply dropped. Otherwise both are discarded without
// a drop -- the ends on their other sides must see nothing -- and dst's stream
// takes on src's writer, carrying over a copy the writer has parked.
func forwardStream(src, dst *streamEnd) error {
	from, to := src.shared, dst.shared
	if from == to || from.dropped || to.dropped {
		src.drop()
		dst.drop()
		return nil
	}
	clearStreamEnd(src)
	clearStreamEnd(dst)
	src.dropWaitable()
	dst.dropWaitable()
	return to.absorb(from)
}

// forwardFuture is forwardStream's future twin.
func forwardFuture(src, dst *futureEnd) error {
	from, to := src.shared, dst.shared
	if from == to || from.dropped || to.dropped {
		src.drop()
		dst.drop()
		return nil
	}
	clearFutureEnd(src)
	clearFutureEnd(dst)
	src.dropWaitable()
	dst.dropWaitable()
	return to.absorb(from)
}

// cancelCopyHostFunc backs stream.cancel-read/write (0x11/0x12) and
// future.cancel-read/write (0x18/0x19), transliterating cancel_copy. Core sig
// (i32) -> i32.
func cancelCopyHostFunc(in *Instance, isFuture bool, side endSide, evCode eventCode, elemDesc binary.TypeDesc, async bool) hostFuncDef {
	kind := "stream"
	if isFuture {
		kind = "future"
	}
	dir := "cancel-write"
	if side == sideReadable {
		dir = "cancel-read"
	}
	name := kind + "." + dir

	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, name)
		// See streamCopyHostFunc's identical early requireNotSyncImplicit classification.
		var blk taskBlocker
		if !async {
			blk = requireNotSyncImplicit(in, name)
		}
		i := api.DecodeU32(stack[0])

		var w *waitable
		var state *copyState
		var sharedCancel func()
		var hasPending func() bool
		var livePending func() bool
		var getPending func() eventTuple

		if isFuture {
			e := requireFutureEnd(in, name, i, side, elemDesc)
			w, state = &e.waitable, &e.state
			sharedCancel, hasPending, getPending = e.shared.cancel, e.hasPendingEvent, e.getPendingEvent
			livePending = func() bool { return false } // futures have no partial-progress notion
		} else {
			e := requireStreamEnd(in, name, i, side, elemDesc)
			w, state = &e.waitable, &e.state
			sharedCancel, hasPending, getPending = e.shared.cancel, e.hasPendingEvent, e.getPendingEvent
			livePending = func() bool { return e.livePending }
		}

		// has_sync_waiter conjunct omitted (Phase 1 convention: no sync
		// waiter can exist while guest code runs -- single-threaded).
		if *state != copyCopying {
			panic(fmt.Errorf("component/instance: %s: end %d is not COPYING", name, i))
		}
		if w.inWaitableSet() && !async {
			panic(fmt.Errorf("component/instance: %s: end %d: waitable cannot be used synchronously while added to a waitable set", name, i))
		}
		*state = copyCancelling

		// !hasPending(): no completion racing us at all. livePending():
		// something IS pending, but only as a LIVE, still-reclaimable
		// partial-progress notification (streamEnd.livePending's doc) --
		// cancel must still be allowed to supersede it with CANCELLED,
		// exactly as a genuinely uncontested cancel would. A FINAL pending
		// event (onCopyDone-installed: our own immediate completion, or a
		// buffer-exhausted rendezvous that already reset the shared object's
		// bookkeeping for us) is left alone -- sharedCancel would otherwise
		// risk hitting whichever DIFFERENT party the shared object considers
		// pending by now.
		if !hasPending() || livePending() {
			sharedCancel() // resetAndNotifyPending(CANCELLED) fires OUR
			// parked buffer's onCopyDone => installs OUR pending event
			if !hasPending() { // host end deferred its cancel ack (§3)
				if async {
					stack[0] = uint64(blockedSentinel)
					return
				}
				// See streamCopyHostFunc's identical split (blk resolved
				// above) and identical syncWaiter bracket (§6.2).
				w.syncWaiter = true
				if blk != nil {
					if blk.block(hasPending) && !hasPending() {
						sharedCancel()
					}
				} else if derr := in.sched.drive(hasPending); derr != nil {
					w.syncWaiter = false
					panic(wrapSchedErr(name, derr))
				}
				w.syncWaiter = false
			}
		}
		ev := getPending() // flips state via the event thunk
		stack[0] = uint64(ev.p2)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}
}

// registerStreamEnd records e as its side's live end on the shared object, so
// dropping the OTHER end can notify it even while it is idle (see
// notifyPeerDropped). Called wherever a streamEnd is handed to a guest: the
// stream.new pair, and every transfer that mints a fresh readable end on
// another instance.
func registerStreamEnd(e *streamEnd, idx uint32) {
	e.index = idx
	slot, _ := e.shared.ends()
	if e.side == sideWritable {
		_, slot = e.shared.ends()
	}
	*slot = e
	// A notification owed to this side from before this end existed -- the
	// producer dropped its end and the readable end was then transferred here.
	if e.shared.dropNotify[e.side] && e.state != copyDone && !e.hasPendingEvent() {
		e.endNotifyDropped()
	}
}

// registerFutureEnd is registerStreamEnd's future twin.
func registerFutureEnd(e *futureEnd, idx uint32) {
	e.index = idx
	slot, _ := e.shared.ends()
	if e.side == sideWritable {
		_, slot = e.shared.ends()
	}
	*slot = e
	if e.shared.dropNotify[e.side] && e.state != copyDone && !e.hasPendingEvent() {
		e.endNotifyDropped()
	}
}

// peerOf returns the registration of the end on the OTHER side from e's.
func peerOfStream(e *streamEnd) (endNotifier, endSide) {
	r, w := e.shared.ends()
	if e.side == sideReadable {
		return *w, sideWritable
	}
	return *r, sideReadable
}

func peerOfFuture(e *futureEnd) (endNotifier, endSide) {
	r, w := e.shared.ends()
	if e.side == sideReadable {
		return *w, sideWritable
	}
	return *r, sideReadable
}

// clearStreamEnd drops e's registration: it is gone, so a later drop of the
// other end has nobody to notify (the reference sets End.other = None on both
// sides).
func clearStreamEnd(e *streamEnd) {
	slot, _ := e.shared.ends()
	if e.side == sideWritable {
		_, slot = e.shared.ends()
	}
	*slot = nil
}

func clearFutureEnd(e *futureEnd) {
	slot, _ := e.shared.ends()
	if e.side == sideWritable {
		_, slot = e.shared.ends()
	}
	*slot = nil
}

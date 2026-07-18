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
	if elemContainsResourceHandle(elem, resolve, 0) {
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

func isFutureCanonKind(k byte) bool {
	switch k {
	case binary.CanonKindFutureNew, binary.CanonKindFutureRead, binary.CanonKindFutureWrite,
		binary.CanonKindFutureCancelRead, binary.CanonKindFutureCancelWrite,
		binary.CanonKindFutureDropReadable, binary.CanonKindFutureDropWritable:
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
		return streamCopyHostFunc(in, side, evCode, elemDesc, elemSz, align, numeric, hasAsyncOpt(canon), memMod, reallocFn), nil

	case binary.CanonKindFutureRead, binary.CanonKindFutureWrite:
		memMod, reallocFn, merr := canonMemoryAndRealloc(canon, coreMemTarget, coreFuncTarget)
		if merr != nil {
			return hostFuncDef{}, merr
		}
		side, evCode := sideReadable, eventFutureRead
		if canon.Kind == binary.CanonKindFutureWrite {
			side, evCode = sideWritable, eventFutureWrite
		}
		return futureCopyHostFunc(in, side, evCode, elemDesc, elemSz, align, numeric, hasAsyncOpt(canon), memMod, reallocFn), nil

	case binary.CanonKindStreamCancelRead, binary.CanonKindStreamCancelWrite:
		side, evCode := sideReadable, eventStreamRead
		if canon.Kind == binary.CanonKindStreamCancelWrite {
			side, evCode = sideWritable, eventStreamWrite
		}
		return cancelCopyHostFunc(in, false, side, evCode, elemDesc, canon.Async), nil

	case binary.CanonKindFutureCancelRead, binary.CanonKindFutureCancelWrite:
		side, evCode := sideReadable, eventFutureRead
		if canon.Kind == binary.CanonKindFutureCancelWrite {
			side, evCode = sideWritable, eventFutureWrite
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
	se, isSE := raw.(*streamEnd)
	if !ok || !isSE {
		panic(fmt.Errorf("component/instance: %s: handle %d is not a stream end", name, i))
	}
	if se.side != side {
		panic(fmt.Errorf("component/instance: %s: handle %d is not a %s stream end", name, i, sideName(side)))
	}
	if !reflect.DeepEqual(se.shared.elem, elemDesc) {
		panic(fmt.Errorf("component/instance: %s: handle %d: stream element type mismatch", name, i))
	}
	return se
}

// requireFutureEnd is requireStreamEnd's future twin.
func requireFutureEnd(in *Instance, name string, i uint32, side endSide, elemDesc binary.TypeDesc) *futureEnd {
	raw, ok := in.resources.getEntry(i)
	fe, isFE := raw.(*futureEnd)
	if !ok || !isFE {
		panic(fmt.Errorf("component/instance: %s: handle %d is not a future end", name, i))
	}
	if fe.side != side {
		panic(fmt.Errorf("component/instance: %s: handle %d is not a %s future end", name, i, sideName(side)))
	}
	if !reflect.DeepEqual(fe.shared.elem, elemDesc) {
		panic(fmt.Errorf("component/instance: %s: handle %d: future element type mismatch", name, i))
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
		shared := &sharedStream{elem: elemDesc, elemSz: elemSz, align: align, numeric: numeric}
		ri := in.resources.addEntry(&streamEnd{side: sideReadable, state: copyIdle, shared: shared})
		wi := in.resources.addEntry(&streamEnd{side: sideWritable, state: copyIdle, shared: shared})
		stack[0] = uint64(ri) | uint64(wi)<<32
	})
	return hostFuncDef{fn: fn, params: nil, results: []api.ValueType{api.ValueTypeI64}}
}

// futureNewHostFunc backs future.new (CanonKindFutureNew, 0x15) -- identical
// shape to streamNewHostFunc.
func futureNewHostFunc(in *Instance, elemDesc binary.TypeDesc, elemSz, align uint32, numeric bool) hostFuncDef {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "future.new")
		shared := &sharedFuture{elem: elemDesc, elemSz: elemSz, align: align, numeric: numeric}
		ri := in.resources.addEntry(&futureEnd{side: sideReadable, state: copyIdle, shared: shared})
		wi := in.resources.addEntry(&futureEnd{side: sideWritable, state: copyIdle, shared: shared})
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
		i, ptr, n := api.DecodeU32(stack[0]), api.DecodeU32(stack[1]), api.DecodeU32(stack[2])

		e := requireStreamEnd(in, name, i, side, elemDesc)
		if e.state != copyIdle {
			panic(fmt.Errorf("component/instance: %s: stream end %d is not idle", name, i))
		}
		if e.inWaitableSet() && !async {
			panic(fmt.Errorf("component/instance: %s: synchronous copy on a stream end joined to a waitable set", name))
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
			realloc = reallocOfFunc(ctx, reallocFn)
		} else if m != nil {
			realloc = reallocOf(ctx, m)
		}

		buf, err := newGuestBuffer(m, realloc, in.resolve, elemDesc, elemSz, align, numeric, ptr, n)
		if err != nil {
			panic(fmt.Errorf("component/instance: %s: %w", name, err))
		}

		// -- the three closures, verbatim from stream_copy --
		streamEvent := func(result copyResult, reclaim func()) eventTuple {
			reclaim()
			if result == copyDropped {
				e.state = copyDone
			} else {
				e.state = copyIdle
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
			e.setPendingEvent(func() eventTuple { return streamEvent(copyCompleted, reclaim) })
		}
		onCopyDone := func(result copyResult) {
			e.livePending = false // final -- see streamEnd.livePending's doc
			e.setPendingEvent(func() eventTuple { return streamEvent(result, func() {}) })
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
			// (docs/component-model-async-stackful-design.md §4.2): a
			// stackful caller parks the goroutine (letting the peer's task,
			// resumable only by the outer driver once our frames yield the
			// baton, make progress); everyone else keeps the Phase 1-3
			// nested scheduler drive.
			if st := activeStackfulTask(in); st != nil {
				st.block(e.hasPendingEvent, false)
			} else if derr := in.sched.drive(e.hasPendingEvent); derr != nil {
				panic(wrapSchedErr(name, derr))
			}
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
		i, ptr := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])

		e := requireFutureEnd(in, name, i, side, elemDesc)
		if e.state != copyIdle {
			panic(fmt.Errorf("component/instance: %s: future end %d is not idle", name, i))
		}
		if e.inWaitableSet() && !async {
			panic(fmt.Errorf("component/instance: %s: synchronous copy on a future end joined to a waitable set", name))
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
			realloc = reallocOfFunc(ctx, reallocFn)
		} else if m != nil {
			realloc = reallocOf(ctx, m)
		}

		buf, err := newGuestBuffer(m, realloc, in.resolve, elemDesc, elemSz, align, numeric, ptr, 1)
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
			// See streamCopyHostFunc's identical split.
			if st := activeStackfulTask(in); st != nil {
				st.block(e.hasPendingEvent, false)
			} else if derr := in.sched.drive(e.hasPendingEvent); derr != nil {
				panic(wrapSchedErr(name, derr))
			}
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
			// copy is still COPYING here).
			panic(fmt.Errorf("component/instance: %s: stream end %d has an in-flight or undelivered copy", name, i))
		}
		e.shared.drop() // counterpart's parked copy => DROPPED event
		e.dropWaitable()
		in.resources.removeEntry(i)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: nil}
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
			panic(fmt.Errorf("component/instance: %s: writable future end %d must complete its write (or observe DROPPED) before dropping", name, i))
		}
		if e.copying() {
			panic(fmt.Errorf("component/instance: %s: future end %d has an in-flight or undelivered copy", name, i))
		}
		e.shared.drop()
		e.dropWaitable()
		in.resources.removeEntry(i)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: nil}
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
			panic(fmt.Errorf("component/instance: %s: synchronous cancel on an end joined to a waitable set", name))
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
				// See streamCopyHostFunc's identical split.
				if st := activeStackfulTask(in); st != nil {
					st.block(hasPending, false)
				} else if derr := in.sched.drive(hasPending); derr != nil {
					panic(wrapSchedErr(name, derr))
				}
			}
		}
		ev := getPending() // flips state via the event thunk
		stack[0] = uint64(ev.p2)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}
}

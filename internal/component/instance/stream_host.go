package instance

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file is the host-facing stream/future API
// (docs/component-model-async-phase2-design.md §3): host code is simply *the
// other end* of a sharedStream/sharedFuture, using hostBuffers instead of
// guestBuffers. No new scheduler machinery -- a host op either rendezvouses
// immediately (copies against a parked guest buffer and installs the guest
// end's pending event) or parks its own host buffer (backpressure).

// CopyResult is the public mirror of copyResult.
type CopyResult uint32

const (
	CopyCompleted CopyResult = CopyResult(copyCompleted)
	CopyDropped   CopyResult = CopyResult(copyDropped)
	CopyCancelled CopyResult = CopyResult(copyCancelled)
)

// requireSchedulable panics unless the call is provably on the instance's
// driving goroutine: (a) no guest task is active (between Calls -- host code
// path), or (b) a scheduler thunk is executing (in.sched.pumping), or (c) a
// host import invocation is on the stack (in.inHostCall, bracketed by
// buildHostWrapper/buildAsyncHostWrapper). A live activeTask WITHOUT (b)/(c)
// means "called concurrently from another goroutine while the guest runs" --
// the one illegal shape (mirrors AsyncCall.Resolve's identical guard in
// async_host_import.go).
func (in *Instance) requireSchedulable(op string) {
	if in.activeTask != nil && !in.sched.pumping && in.inHostCall == 0 {
		panic(fmt.Errorf("component/instance: %s called outside the instance scheduler while a task is active; external completion requires CallAsync (not yet implemented)", op))
	}
}

// StreamWriter is the host-held writable end of a stream whose readable end
// lives (or will live) in a guest table. NOT a table entry, NOT a waitable:
// completion surfaces as Go callbacks, not events.
type StreamWriter struct {
	in     *Instance
	shared *sharedStream
	state  copyState
}

// StreamReader is the host-held readable end of a stream whose writable end
// lives in a guest table (or another host).
type StreamReader struct {
	in     *Instance
	shared *sharedStream
	state  copyState
}

// FutureWriter/FutureReader are StreamWriter/StreamReader's future twins:
// one element, one shot.
type FutureWriter struct {
	in     *Instance
	shared *sharedFuture
	state  copyState
}

type FutureReader struct {
	in     *Instance
	shared *sharedFuture
	state  copyState
}

// resolveNewElem computes elemSz/align/numeric for elem exactly as the
// stream.new/future.new bind path does (stream_builtins.go's
// resolveStreamOrFutureElem), applying the same no-resource-in-element bind
// refusal, but starting from a caller-supplied binary.TypeDesc instead of a
// canon.TypeIdx (there is no canon here -- NewStream/NewFuture are called
// directly by host Go code).
func resolveNewElem(resolve abi.Resolver, elem binary.TypeDesc) (elemSz, align uint32, numeric bool, err error) {
	if elem == nil {
		return 0, 0, true, nil
	}
	if elemContainsResourceHandle(elem, resolve, 0) {
		return 0, 0, false, fmt.Errorf("stream/future element type contains an own/borrow resource handle, which is not supported by this milestone")
	}
	elemSz, err = abi.Size(elem, resolve)
	if err != nil {
		return 0, 0, false, fmt.Errorf("element size: %w", err)
	}
	align, err = abi.Alignment(elem, resolve)
	if err != nil {
		return 0, 0, false, fmt.Errorf("element alignment: %w", err)
	}
	return elemSz, align, noneOrNumberType(elem), nil
}

// NewStream creates a stream pair for passing data INTO or OUT OF the guest:
// the host keeps one end (the returned *StreamWriter); the returned
// abi.Value is the *sharedStream identity, ready to hand to the guest as a
// Call/CallAsync arg (instance.go's resolveArgHandlesDepth mints a fresh
// READABLE end for it in the callee's table) or as an async-import Resolve
// result (host_import.go's allocHandleResult does the same). elem is nil for
// a bare stream.
func (in *Instance) NewStream(elem binary.TypeDesc) (*StreamWriter, abi.Value, error) {
	elemSz, align, numeric, err := resolveNewElem(in.resolve, elem)
	if err != nil {
		return nil, nil, err
	}
	shared := &sharedStream{elem: elem, elemSz: elemSz, align: align, numeric: numeric}
	return &StreamWriter{in: in, shared: shared, state: copyIdle}, shared, nil
}

// NewFuture is NewStream's future twin.
func (in *Instance) NewFuture(elem binary.TypeDesc) (*FutureWriter, abi.Value, error) {
	elemSz, align, numeric, err := resolveNewElem(in.resolve, elem)
	if err != nil {
		return nil, nil, err
	}
	shared := &sharedFuture{elem: elem, elemSz: elemSz, align: align, numeric: numeric}
	return &FutureWriter{in: in, shared: shared, state: copyIdle}, shared, nil
}

func (w *StreamWriter) copying() bool { return w.state == copyCopying || w.state == copyCancelling }

// Write offers vals to the stream. onDone fires EXACTLY ONCE with the result
// and the number of elements actually transferred:
//   - immediately (before Write returns), if a guest read is parked
//     (rendezvous) or the writable end's counterpart was dropped
//     (copyDropped, n=0);
//   - later, from inside a guest stream.read builtin, when the guest
//     finally reads (the host buffer was parked -- backpressure).
//
// A second Write before onDone fires panics: rendezvous streams have no
// host-side queue, by design.
func (w *StreamWriter) Write(vals []abi.Value, onDone func(res CopyResult, n uint32)) {
	w.in.requireSchedulable("StreamWriter.Write")
	if w.state != copyIdle {
		panic(fmt.Errorf("component/instance: StreamWriter.Write called while a previous Write is still pending"))
	}
	buf := newHostReadBuffer(vals)
	w.state = copyCopying
	fire := func(res copyResult) {
		w.state = copyIdle
		if res == copyDropped {
			w.state = copyDone
		}
		onDone(CopyResult(res), buf.progressed())
	}
	// onCopy fires only if THIS Write itself parked (no reader was ready
	// yet) and a LATER guest/host read drains our buffer -- reclaim
	// (un-park) INLINE and fire our own completion right away: a host end
	// has no event thunk to defer through (design doc §4.1's sanctioned
	// simplification -- reclaim-then-fire is observably the delivery step
	// for a host end, since the callback firing IS the delivery).
	onCopy := func(reclaim func()) {
		reclaim()
		fire(copyCompleted)
	}
	err := w.shared.write(nil, buf, onCopy, fire)
	if err != nil {
		panic(fmt.Errorf("component/instance: StreamWriter.Write: %w", err))
	}
}

// Close drops the writable end: a parked or future guest read completes with
// DROPPED; subsequent guest reads get DROPPED immediately. Idempotent.
func (w *StreamWriter) Close() {
	w.in.requireSchedulable("StreamWriter.Close")
	if w.copying() {
		panic(fmt.Errorf("component/instance: StreamWriter.Close called during a pending Write; Cancel it first"))
	}
	w.shared.drop()
	w.state = copyDone
}

// Cancel revokes a parked Write. onDone (from the pending Write) fires with
// CANCELLED and the partial progress. No-op if the Write already completed.
func (w *StreamWriter) Cancel() {
	w.in.requireSchedulable("StreamWriter.Cancel")
	if w.state != copyCopying {
		return
	}
	w.state = copyCancelling
	if w.shared.pendingBuffer != nil {
		w.shared.cancel()
	}
}

func (r *StreamReader) copying() bool { return r.state == copyCopying || r.state == copyCancelling }

// Read requests up to n elements. Same one-shot/park contract as Write.
func (r *StreamReader) Read(n uint32, onDone func(res CopyResult, vals []abi.Value)) {
	r.in.requireSchedulable("StreamReader.Read")
	if r.state != copyIdle {
		panic(fmt.Errorf("component/instance: StreamReader.Read called while a previous Read is still pending"))
	}
	buf := newHostWriteBuffer(n)
	r.state = copyCopying
	fire := func(res copyResult) {
		r.state = copyIdle
		if res == copyDropped {
			r.state = copyDone
		}
		onDone(CopyResult(res), buf.vals)
	}
	// See StreamWriter.Write's onCopy doc: reclaim-then-fire inline is the
	// host end's delivery step.
	onCopy := func(reclaim func()) {
		reclaim()
		fire(copyCompleted)
	}
	err := r.shared.read(nil, buf, onCopy, fire)
	if err != nil {
		panic(fmt.Errorf("component/instance: StreamReader.Read: %w", err))
	}
}

// Close drops the readable end: a parked (or future) guest write completes
// with DROPPED.
func (r *StreamReader) Close() {
	r.in.requireSchedulable("StreamReader.Close")
	if r.copying() {
		panic(fmt.Errorf("component/instance: StreamReader.Close called during a pending Read; Cancel it first"))
	}
	r.shared.drop()
	r.state = copyDone
}

// Cancel revokes a parked Read.
func (r *StreamReader) Cancel() {
	r.in.requireSchedulable("StreamReader.Cancel")
	if r.state != copyCopying {
		return
	}
	r.state = copyCancelling
	if r.shared.pendingBuffer != nil {
		r.shared.cancel()
	}
}

// Set completes immediately if a guest read is parked; otherwise parks until
// the guest reads (or reports DROPPED if the reader closed first).
func (w *FutureWriter) Set(v abi.Value, onDone func(res CopyResult)) {
	w.in.requireSchedulable("FutureWriter.Set")
	if w.state != copyIdle {
		panic(fmt.Errorf("component/instance: FutureWriter.Set called more than once"))
	}
	buf := newHostReadBuffer([]abi.Value{v})
	w.state = copyCopying
	err := w.shared.write(nil, buf, func(res copyResult) {
		w.state = copyDone // a future is one-shot: completed or dropped, both terminal
		onDone(CopyResult(res))
	})
	if err != nil {
		panic(fmt.Errorf("component/instance: FutureWriter.Set: %w", err))
	}
}

// Close is ONLY legal after Set completed or reported DROPPED
// (WritableFutureEnd.drop's trap_if(state != DONE)) -- panics otherwise.
func (w *FutureWriter) Close() {
	w.in.requireSchedulable("FutureWriter.Close")
	if w.state != copyDone {
		panic(fmt.Errorf("component/instance: FutureWriter.Close called before Set completed (or observed DROPPED)"))
	}
	w.shared.drop()
}

func (r *FutureReader) copying() bool { return r.state == copyCopying || r.state == copyCancelling }

// Get requests the future's one value.
func (r *FutureReader) Get(onDone func(res CopyResult, v abi.Value)) {
	r.in.requireSchedulable("FutureReader.Get")
	if r.state != copyIdle {
		panic(fmt.Errorf("component/instance: FutureReader.Get called more than once"))
	}
	buf := newHostWriteBuffer(1)
	r.state = copyCopying
	err := r.shared.read(nil, buf, func(res copyResult) {
		r.state = copyDone
		var v abi.Value
		if len(buf.vals) == 1 {
			v = buf.vals[0]
		}
		onDone(CopyResult(res), v)
	})
	if err != nil {
		panic(fmt.Errorf("component/instance: FutureReader.Get: %w", err))
	}
}

// Close drops the readable end: a parked (or future) guest write completes
// with DROPPED.
func (r *FutureReader) Close() {
	r.in.requireSchedulable("FutureReader.Close")
	if r.copying() {
		panic(fmt.Errorf("component/instance: FutureReader.Close called during a pending Get; Cancel it first"))
	}
	r.shared.drop()
	r.state = copyDone
}

// Cancel revokes a parked Get.
func (r *FutureReader) Cancel() {
	r.in.requireSchedulable("FutureReader.Cancel")
	if r.state != copyCopying {
		return
	}
	r.state = copyCancelling
	if r.shared.pendingBuffer != nil {
		r.shared.cancel()
	}
}

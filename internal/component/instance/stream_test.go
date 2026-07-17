package instance

import (
	"testing"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file tests the Phase 2 stream/future runtime (stream.go/
// stream_builtins.go/stream_host.go/errorcontext.go) the same way
// async_waitable_builtins_test.go tests Phase 1's builtins: calling the
// hostFuncDef closures (and the lower-level sharedStream/sharedFuture state
// machine) directly, with elemDesc == nil (a bare stream/future) wherever the
// test doesn't need real linear memory -- guestBuffer's read/write never
// touch memory for a nil element, so callBuiltin's mod=nil is exactly
// sufficient. Memory-touching tests (guestBuffer construction/bounds,
// numeric memmove fast path, error-context) use memModule(t)
// (host_import_test.go) for a real backing module.

// ---- low-level sharedStream/sharedFuture state machine ----

func u32vals(vs ...uint32) []abi.Value {
	out := make([]abi.Value, len(vs))
	for i, v := range vs {
		out[i] = v
	}
	return out
}

func TestSharedStream_WriteThenRead_ImmediateRendezvous(t *testing.T) {
	s := &sharedStream{numeric: true}
	var writeRes copyResult
	var writeN uint32
	wbuf := newHostReadBuffer(u32vals(1, 2, 3))
	// onCopy fires when a LATER read partially/fully drains this parked
	// write; chain reclaim+fire ourselves here, exactly as StreamWriter.Write
	// does (stream_host.go) -- a bare no-op onCopy (as a first draft of this
	// test had) would never surface the writer's own completion.
	onCopy := func(reclaim func()) { reclaim(); writeRes = copyCompleted; writeN = wbuf.progressed() }
	if err := s.write(nil, wbuf, onCopy, func(r copyResult) { writeRes = r; writeN = wbuf.progressed() }); err != nil {
		t.Fatal(err)
	}
	// write parked (no reader yet): writeRes not yet set.
	if writeRes != 0 || writeN != 0 {
		t.Fatalf("write should have parked, got res=%v n=%d", writeRes, writeN)
	}
	var readRes copyResult
	rbuf := newHostWriteBuffer(3)
	if err := s.read(nil, rbuf, func(func()) {}, func(r copyResult) { readRes = r }); err != nil {
		t.Fatal(err)
	}
	if readRes != copyCompleted {
		t.Fatalf("read result = %v, want COMPLETED", readRes)
	}
	if writeRes != copyCompleted || writeN != 3 {
		t.Fatalf("write not completed by rendezvous: res=%v n=%d", writeRes, writeN)
	}
	if len(rbuf.vals) != 3 {
		t.Fatalf("reader got %d values, want 3", len(rbuf.vals))
	}
}

func TestSharedStream_ZeroLengthHandshake_WriteSideDoesNotWakeReader(t *testing.T) {
	s := &sharedStream{numeric: true}
	readWoke := false
	rbuf := newHostWriteBuffer(0) // zero-length read
	if err := s.read(nil, rbuf, func(func()) {}, func(copyResult) { readWoke = true }); err != nil {
		t.Fatal(err)
	}
	if readWoke {
		t.Fatal("zero-length read should have parked, not completed yet")
	}
	var writeRes copyResult
	wbuf := newHostReadBuffer(nil) // zero-length write
	if err := s.write(nil, wbuf, func(func()) {}, func(r copyResult) { writeRes = r }); err != nil {
		t.Fatal(err)
	}
	if writeRes != copyCompleted {
		t.Fatalf("zero-length write vs parked zero-length read: got %v, want COMPLETED", writeRes)
	}
	if readWoke {
		t.Fatal("the zero-length write must NOT wake the parked zero-length read (design doc §1.1 item 3)")
	}
}

func TestSharedStream_ZeroLengthRead_NoElifAsymmetry(t *testing.T) {
	// A 0-length READ against a parked 0-length WRITE takes the remain()==0
	// else-branch: completes the parked writer, parks the reader (read has
	// no zero-length elif -- the reference asymmetry, kept verbatim).
	s := &sharedStream{numeric: true}
	var writeRes copyResult
	wbuf := newHostReadBuffer(nil)
	if err := s.write(nil, wbuf, func(func()) {}, func(r copyResult) { writeRes = r }); err != nil {
		t.Fatal(err)
	}
	if writeRes != 0 {
		t.Fatalf("zero-length write should have parked, got %v", writeRes)
	}
	readDone := false
	rbuf := newHostWriteBuffer(0)
	if err := s.read(nil, rbuf, func(func()) {}, func(copyResult) { readDone = true }); err != nil {
		t.Fatal(err)
	}
	if writeRes != copyCompleted {
		t.Fatalf("parked zero-length writer should complete: got %v", writeRes)
	}
	if readDone {
		t.Fatal("the reader must park (not complete) per the read-side asymmetry")
	}
	if s.pendingBuffer != rbuf {
		t.Fatal("reader should now be the pending side")
	}
}

func TestSharedStream_SameInstanceNonNumericTrap(t *testing.T) {
	inst := &Instance{sched: &sched{}}
	s := &sharedStream{numeric: false}
	wbuf := newHostReadBuffer(u32vals(1))
	if err := s.write(inst, wbuf, func(func()) {}, func(copyResult) {}); err != nil {
		t.Fatal(err) // parks; no trap yet
	}
	rbuf := newHostWriteBuffer(1)
	err := s.read(inst, rbuf, func(func()) {}, func(copyResult) {})
	if err == nil {
		t.Fatal("expected the same-instance non-numeric trap")
	}
}

func TestSharedStream_SameInstanceNumericAllowed(t *testing.T) {
	inst := &Instance{sched: &sched{}}
	s := &sharedStream{numeric: true}
	wbuf := newHostReadBuffer(u32vals(9))
	if err := s.write(inst, wbuf, func(func()) {}, func(copyResult) {}); err != nil {
		t.Fatal(err)
	}
	rbuf := newHostWriteBuffer(1)
	if err := s.read(inst, rbuf, func(func()) {}, func(copyResult) {}); err != nil {
		t.Fatalf("numeric same-instance copy should be allowed: %v", err)
	}
}

func TestSharedStream_DropNotifiesPending(t *testing.T) {
	s := &sharedStream{numeric: true}
	var res copyResult
	wbuf := newHostReadBuffer(u32vals(1))
	if err := s.write(nil, wbuf, func(func()) {}, func(r copyResult) { res = r }); err != nil {
		t.Fatal(err)
	}
	s.drop()
	if res != copyDropped {
		t.Fatalf("pending write should be notified DROPPED, got %v", res)
	}
	// A second drop is a no-op (idempotent).
	s.drop()
}

func TestSharedFuture_WriteThenRead(t *testing.T) {
	f := &sharedFuture{numeric: true}
	var wres copyResult
	wbuf := newHostReadBuffer(u32vals(42))
	if err := f.write(nil, wbuf, func(r copyResult) { wres = r }); err != nil {
		t.Fatal(err)
	}
	if wres != 0 {
		t.Fatalf("future write should park, got %v", wres)
	}
	var rres copyResult
	rbuf := newHostWriteBuffer(1)
	if err := f.read(nil, rbuf, func(r copyResult) { rres = r }); err != nil {
		t.Fatal(err)
	}
	if rres != copyCompleted || wres != copyCompleted {
		t.Fatalf("rendezvous incomplete: read=%v write=%v", rres, wres)
	}
	if len(rbuf.vals) != 1 || rbuf.vals[0].(uint32) != 42 {
		t.Fatalf("reader got %v, want [42]", rbuf.vals)
	}
}

func TestSharedFuture_DropReaderPanicsOnDroppedRead(t *testing.T) {
	f := &sharedFuture{numeric: true, dropped: true}
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic: sharedFuture.read is unreachable once dropped")
		}
	}()
	_ = f.read(nil, newHostWriteBuffer(1), func(copyResult) {})
}

func TestNoneOrNumberType(t *testing.T) {
	cases := []struct {
		t    binary.TypeDesc
		want bool
	}{
		{nil, true},
		{binary.PrimitiveDesc{Prim: "u8"}, true},
		{binary.PrimitiveDesc{Prim: "s64"}, true},
		{binary.PrimitiveDesc{Prim: "f32"}, true},
		{binary.PrimitiveDesc{Prim: "bool"}, false},
		{binary.PrimitiveDesc{Prim: "char"}, false},
		{binary.PrimitiveDesc{Prim: "string"}, false},
		{binary.RecordDesc{}, false},
	}
	for _, c := range cases {
		if got := noneOrNumberType(c.t); got != c.want {
			t.Errorf("noneOrNumberType(%#v) = %v, want %v", c.t, got, c.want)
		}
	}
}

func TestPackCopyResult(t *testing.T) {
	if got := packCopyResult(copyCompleted, 0); got != 0 {
		t.Errorf("packCopyResult(COMPLETED,0) = %#x, want 0", got)
	}
	if got := packCopyResult(copyCancelled, 5); got != 2|5<<4 {
		t.Errorf("packCopyResult(CANCELLED,5) = %#x, want %#x", got, 2|5<<4)
	}
}

// ---- builtin-level tests: streamNewHostFunc / streamCopyHostFunc /
// streamDropHostFunc / cancelCopyHostFunc, driven directly (elemDesc=nil so
// no real memory is needed -- callBuiltin's mod=nil is sufficient).

func newBareStreamEnds(in *Instance) (readable, writable uint32) {
	out := callBuiltin(streamNewHostFunc(in, nil, 0, 0, true), 0)
	packed := out[0]
	return uint32(packed), uint32(packed >> 32)
}

func newBareFutureEnds(in *Instance) (readable, writable uint32) {
	out := callBuiltin(futureNewHostFunc(in, nil, 0, 0, true), 0)
	packed := out[0]
	return uint32(packed), uint32(packed >> 32)
}

func TestStreamNewHostFunc_ReadableAddedFirst(t *testing.T) {
	in := newAsyncInst()
	r, w := newBareStreamEnds(in)
	if r >= w {
		t.Fatalf("readable (%d) should be allocated BEFORE writable (%d)", r, w)
	}
	re, ok := in.resources.getEntry(r)
	if !ok {
		t.Fatal("readable end missing")
	}
	se := re.(*streamEnd)
	if se.side != sideReadable || se.state != copyIdle {
		t.Fatalf("readable end: side=%v state=%v", se.side, se.state)
	}
	we, _ := in.resources.getEntry(w)
	swe := we.(*streamEnd)
	if swe.side != sideWritable || swe.shared != se.shared {
		t.Fatal("both ends must share the same *sharedStream")
	}
}

func TestStreamCopy_EndNotIdleTraps(t *testing.T) {
	in := newAsyncInst()
	r, _ := newBareStreamEnds(in)
	readFn := streamCopyHostFunc(in, sideReadable, eventStreamRead, nil, 0, 0, true, true /*async*/, nil, nil)
	// Async read on an empty stream parks (BLOCKED).
	out := callBuiltin(readFn, uint64(r), 0, 5)
	if uint32(out[0]) != blockedSentinel {
		t.Fatalf("first async read: got %#x, want BLOCKED", out[0])
	}
	// Second read while the first is still COPYING must trap "not idle".
	requirePanicContains(t, "not idle", func() { callBuiltin(readFn, uint64(r), 0, 5) })
}

func TestStreamCopy_SyncOnJoinedSetTraps(t *testing.T) {
	in := newAsyncInst()
	r, _ := newBareStreamEnds(in)
	set := uint32(callBuiltin(waitableSetNewHostFunc(in), 0)[0])
	callBuiltin(waitableJoinHostFunc(in), uint64(r), uint64(set))
	syncRead := streamCopyHostFunc(in, sideReadable, eventStreamRead, nil, 0, 0, true, false /*async*/, nil, nil)
	requirePanicContains(t, "joined to a waitable set", func() { callBuiltin(syncRead, uint64(r), 0, 1) })
}

func TestStreamCopy_DeadlockOnEmptyRunq(t *testing.T) {
	in := newAsyncInst()
	r, _ := newBareStreamEnds(in)
	syncRead := streamCopyHostFunc(in, sideReadable, eventStreamRead, nil, 0, 0, true, false, nil, nil)
	requirePanicContains(t, "deadlock", func() { callBuiltin(syncRead, uint64(r), 0, 1) })
}

func TestStreamCopy_BlockedSentinelAsync(t *testing.T) {
	in := newAsyncInst()
	r, _ := newBareStreamEnds(in)
	asyncRead := streamCopyHostFunc(in, sideReadable, eventStreamRead, nil, 0, 0, true, true, nil, nil)
	out := callBuiltin(asyncRead, uint64(r), 0, 1)
	if uint32(out[0]) != blockedSentinel {
		t.Fatalf("got %#x, want BLOCKED (%#x)", out[0], blockedSentinel)
	}
}

// TestStreamDrop_UndeliveredCompletion_TrapsCopyingNotAssert is the load-
// bearing proof of design doc §1.3: state flips COPYING->IDLE/DONE INSIDE the
// event thunk, at DELIVERY time, not at completion. A parked async read that
// the counterpart completes (via a host Write) must still be "copying" from
// the table's point of view until the guest actually retrieves the pending
// event -- so stream.drop-readable on it traps "copying", and does NOT hit
// waitable.dropWaitable's panic-assert (which would fire if the event were
// still pending when drop tried to remove it -- proving the two invariants
// (copying()==true, hasPendingEvent()==true) hold simultaneously here).
func TestStreamDrop_UndeliveredCompletion_TrapsCopyingNotAssert(t *testing.T) {
	in := newAsyncInst()
	r, w := newBareStreamEnds(in)
	asyncRead := streamCopyHostFunc(in, sideReadable, eventStreamRead, nil, 0, 0, true, true, nil, nil)
	out := callBuiltin(asyncRead, uint64(r), 0, 3)
	if uint32(out[0]) != blockedSentinel {
		t.Fatalf("read did not park: %#x", out[0])
	}

	re, _ := in.resources.getEntry(r)
	se := re.(*streamEnd)
	if se.state != copyCopying {
		t.Fatalf("parked read state = %v, want copyCopying", se.state)
	}

	// Complete it from the writer side WITHOUT the guest retrieving the
	// event yet (a host Write does exactly this -- reclaim+fire happen
	// eagerly for a HOST end, but the READER here is a Phase 2 stream end
	// living in a table, whose event thunk is what flips state; that thunk
	// has not run yet).
	shared := se.shared
	writeDone := false
	if err := shared.write(nil, newHostReadBuffer(u32vals(1, 2, 3)), func(func()) {}, func(copyResult) { writeDone = true }); err != nil {
		t.Fatal(err)
	}
	if !writeDone {
		t.Fatal("write should have rendezvoused with the parked read immediately")
	}

	// The completion happened, but state must STILL read COPYING (delivery
	// hasn't happened) -- this is the subtlety.
	if se.state != copyCopying {
		t.Fatalf("state flipped BEFORE delivery: got %v, want copyCopying (eager flip is the documented bug)", se.state)
	}
	if !se.hasPendingEvent() {
		t.Fatal("a pending event must be installed by the completion")
	}

	dropFn := streamDropHostFunc(in, sideReadable, nil)
	requirePanicContains(t, "in-flight or undelivered copy", func() { callBuiltin(dropFn, uint64(r)) })

	// NOW deliver the event (as a real wait/poll would) and confirm the
	// state flips to IDLE at that point, and drop succeeds afterward.
	ev := se.getPendingEvent()
	if ev.code != eventStreamRead || ev.p2 != packCopyResult(copyCompleted, 3) {
		t.Fatalf("delivered event = %+v, want {code=%d p2=%#x}", ev, eventStreamRead, packCopyResult(copyCompleted, 3))
	}
	if se.state != copyIdle {
		t.Fatalf("state after delivery = %v, want copyIdle", se.state)
	}
	callBuiltin(dropFn, uint64(r))
	if _, ok := in.resources.getEntry(r); ok {
		t.Fatal("stream.drop-readable should have removed the entry once idle")
	}
	_ = w
}

// TestStreamCancel_RacingCompletionDeliversCompleted proves the second half
// of §1.3: a cancel that races a completion must deliver COMPLETED (not
// CANCELLED) because the already-pending event is what gets delivered.
func TestStreamCancel_RacingCompletionDeliversCompleted(t *testing.T) {
	in := newAsyncInst()
	r, _ := newBareStreamEnds(in)
	asyncRead := streamCopyHostFunc(in, sideReadable, eventStreamRead, nil, 0, 0, true, true, nil, nil)
	callBuiltin(asyncRead, uint64(r), 0, 2)

	re, _ := in.resources.getEntry(r)
	se := re.(*streamEnd)
	shared := se.shared
	if err := shared.write(nil, newHostReadBuffer(u32vals(7, 8)), func(func()) {}, func(copyResult) {}); err != nil {
		t.Fatal(err)
	}
	// The read is now completed-but-undelivered (has a pending event).
	if !se.hasPendingEvent() {
		t.Fatal("expected a pending completion event before cancel")
	}

	cancelFn := cancelCopyHostFunc(in, false, sideReadable, eventStreamRead, nil, true)
	out := callBuiltin(cancelFn, uint64(r))
	res := copyResult(uint32(out[0]) & 0xf)
	if res != copyCompleted {
		t.Fatalf("cancel racing a completion delivered %v, want COMPLETED (the race was lost)", res)
	}
	if se.state != copyIdle {
		t.Fatalf("state after delivering the raced completion = %v, want copyIdle", se.state)
	}
}

// TestStreamCancel_ParkedNeverMatched proves the "clean" cancel path: a
// cancel of a parked, never-rendezvoused copy returns CANCELLED synchronously
// (a guest-guest/host pair's sharedStream.cancel always notifies inline, so
// the BLOCKED arm of cancel is unreachable here -- only a host end that
// defers its cancel ack could hit it, per design doc §3.4).
func TestStreamCancel_ParkedNeverMatched(t *testing.T) {
	in := newAsyncInst()
	r, _ := newBareStreamEnds(in)
	asyncRead := streamCopyHostFunc(in, sideReadable, eventStreamRead, nil, 0, 0, true, true, nil, nil)
	callBuiltin(asyncRead, uint64(r), 0, 4)

	cancelFn := cancelCopyHostFunc(in, false, sideReadable, eventStreamRead, nil, true)
	out := callBuiltin(cancelFn, uint64(r))
	res := copyResult(uint32(out[0]) & 0xf)
	if res != copyCancelled {
		t.Fatalf("cancel of an unmatched parked read: got %v, want CANCELLED", res)
	}
}

func TestStreamCancel_NotCopyingTraps(t *testing.T) {
	in := newAsyncInst()
	r, _ := newBareStreamEnds(in)
	cancelFn := cancelCopyHostFunc(in, false, sideReadable, eventStreamRead, nil, true)
	requirePanicContains(t, "not COPYING", func() { callBuiltin(cancelFn, uint64(r)) })
}

func TestStreamDrop_HandleIndexReuse(t *testing.T) {
	in := newAsyncInst()
	r1, _ := newBareStreamEnds(in)
	// Drop ONLY the readable end: it is now the sole entry on the free
	// list, so the next allocation (of any kind) must reuse exactly this
	// index -- the handleTable's free list is a stack (LIFO), so dropping
	// both ends first would make the more-recently-freed one (the writable
	// end) win the next reuse instead, which is a distinct, already-tested
	// handleTable behavior (resource_test.go), not what this test is about.
	callBuiltin(streamDropHostFunc(in, sideReadable, nil), uint64(r1))
	r2, _ := newBareStreamEnds(in)
	if r2 != r1 {
		t.Fatalf("free-list reuse: new readable index = %d, want reused %d", r2, r1)
	}
}

func TestStreamDrop_WrongSideAndKindTrap(t *testing.T) {
	in := newAsyncInst()
	r, w := newBareStreamEnds(in)
	requirePanicContains(t, "not a readable stream end", func() {
		callBuiltin(streamDropHostFunc(in, sideReadable, nil), uint64(w))
	})
	requirePanicContains(t, "not a stream end", func() {
		callBuiltin(streamDropHostFunc(in, sideReadable, nil), uint64(999))
	})
	_ = r
}

func TestFutureDrop_WritableRequiresDone(t *testing.T) {
	in := newAsyncInst()
	_, w := newBareFutureEnds(in)
	dropFn := futureDropHostFunc(in, sideWritable, nil)
	requirePanicContains(t, "must complete its write", func() { callBuiltin(dropFn, uint64(w)) })
}

func TestFutureCopy_WriteDroppedAfterReaderClose(t *testing.T) {
	in := newAsyncInst()
	r, w := newBareFutureEnds(in)
	// Drop the readable end first (simulating a reader that gave up).
	callBuiltin(futureDropHostFunc(in, sideReadable, nil), uint64(r))

	writeFn := futureCopyHostFunc(in, sideWritable, eventFutureWrite, nil, 0, 0, true, true, nil, nil)
	out := callBuiltin(writeFn, uint64(w), 0)
	res := copyResult(uint32(out[0]))
	if res != copyDropped {
		t.Fatalf("future.write after reader dropped: got %v, want DROPPED", res)
	}
	// The writable end is now DONE (having observed DROPPED), so it may drop.
	callBuiltin(futureDropHostFunc(in, sideWritable, nil), uint64(w))
}

func TestFutureCopy_ElemTypeMismatchTraps(t *testing.T) {
	in := newAsyncInst()
	r, _ := newBareFutureEnds(in) // bare (nil) element
	readFn := futureCopyHostFunc(in, sideReadable, eventFutureRead, binary.PrimitiveDesc{Prim: "u32"}, 4, 4, true, true, nil, nil)
	requirePanicContains(t, "element type mismatch", func() { callBuiltin(readFn, uint64(r), 0) })
}

// ---- lift traps (value transfer, stream.go's takeReadableStreamEnd) ----

func TestTakeReadableStreamEnd_Traps(t *testing.T) {
	in := newAsyncInst()
	r, w := newBareStreamEnds(in)

	// Transferring the WRITABLE end (not readable) traps.
	if _, err := takeReadableStreamEnd(in.resources, nil, w); err == nil {
		t.Fatal("expected a trap transferring a writable end as readable")
	}
	// Elem type mismatch traps.
	if _, err := takeReadableStreamEnd(in.resources, binary.PrimitiveDesc{Prim: "u32"}, r); err == nil {
		t.Fatal("expected an element-type-mismatch trap")
	}
	// A COPYING (in-flight) readable end cannot be transferred.
	asyncRead := streamCopyHostFunc(in, sideReadable, eventStreamRead, nil, 0, 0, true, true, nil, nil)
	callBuiltin(asyncRead, uint64(r), 0, 1)
	if _, err := takeReadableStreamEnd(in.resources, nil, r); err == nil {
		t.Fatal("expected a trap transferring a COPYING readable end")
	}

	// A joined-to-a-waitable-set idle end also traps.
	r2, _ := newBareStreamEnds(in)
	set := uint32(callBuiltin(waitableSetNewHostFunc(in), 0)[0])
	callBuiltin(waitableJoinHostFunc(in), uint64(r2), uint64(set))
	if _, err := takeReadableStreamEnd(in.resources, nil, r2); err == nil {
		t.Fatal("expected a trap transferring a waitable-set-joined readable end")
	}

	// A genuinely idle, unjoined readable end transfers cleanly and is
	// removed from the table.
	r3, _ := newBareStreamEnds(in)
	shared, err := takeReadableStreamEnd(in.resources, nil, r3)
	if err != nil {
		t.Fatalf("clean transfer failed: %v", err)
	}
	if shared == nil {
		t.Fatal("nil shared object")
	}
	if _, ok := in.resources.getEntry(r3); ok {
		t.Fatal("transferred handle should be removed from the sender's table")
	}
}

// ---- error-context ----

func TestErrorContext_NewDebugMessageDrop(t *testing.T) {
	ctx, mod := memModule(t)
	in := &Instance{sched: &sched{}, mayLeave: true, resources: newHandleTable(), resolve: func(uint32) binary.TypeDesc { return nil }}

	mem := mod.Memory()
	msg := "boom"
	if !mem.WriteString(0, msg) {
		t.Fatal("write failed")
	}

	newFn := errorContextNewHostFunc(in, nil)
	stack := []uint64{0, uint64(len(msg)), 0, 0}
	newFn.fn.Call(ctx, mod, stack)
	h := uint32(stack[0])

	raw, ok := in.resources.getEntry(h)
	if !ok {
		t.Fatal("error-context handle not in table")
	}
	ec := raw.(*errorContext)
	if ec.debugMessage != msg {
		t.Fatalf("debugMessage = %q, want %q", ec.debugMessage, msg)
	}

	// debug-message stores [ptr:4][len:4] at the given out-pointer.
	dmFn := errorContextDebugMessageHostFunc(in, nil, nil)
	outPtr := uint32(64)
	dmFn.fn.Call(ctx, mod, []uint64{uint64(h), uint64(outPtr), 0, 0})
	b, ok := mem.Read(outPtr, 8)
	if !ok {
		t.Fatal("read out-pointer failed")
	}
	strPtr := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
	strLen := uint32(b[4]) | uint32(b[5])<<8 | uint32(b[6])<<16 | uint32(b[7])<<24
	got, ok := mem.Read(strPtr, strLen)
	if !ok || string(got) != msg {
		t.Fatalf("stored message = %q (ok=%v), want %q", got, ok, msg)
	}

	dropFn := errorContextDropHostFunc(in)
	dropFn.fn.Call(ctx, mod, []uint64{uint64(h), 0, 0, 0})
	if _, ok := in.resources.getEntry(h); ok {
		t.Fatal("error-context.drop did not remove the entry")
	}

	// Copy semantics: lift is a GET; lowering that *errorContext again (the
	// Call-arg seam, resolveArgHandlesDepth's PrimitiveDesc{"error-context"}
	// case) mints a SECOND handle sharing the same underlying message,
	// independent of the first.
	stack2 := []uint64{0, uint64(len(msg)), 0, 0}
	newFn.fn.Call(ctx, mod, stack2)
	h2 := uint32(stack2[0])
	raw2, _ := in.resources.getEntry(h2)
	ec2 := raw2.(*errorContext)
	v, err := in.resolveArgHandlesDepth(ec2, binary.PrimitiveDesc{Prim: "error-context"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	h3, ok := v.(uint32)
	if !ok {
		t.Fatalf("resolveArgHandlesDepth returned %T, want uint32", v)
	}
	raw3, _ := in.resources.getEntry(h3)
	if raw3.(*errorContext) != ec2 {
		t.Fatal("the new handle should share the SAME *errorContext (copy semantics)")
	}
	if h3 == h2 {
		t.Fatal("the new handle must be DIFFERENT from the source handle (lift is a GET, not a remove)")
	}
}

// ---- Instance.NewStream / NewFuture and the host API ----

func TestNewStream_ResourceInElementRefused(t *testing.T) {
	in := &Instance{sched: &sched{}}
	_, _, err := in.NewStream(binary.OwnDesc{ResourceType: 0})
	if err == nil {
		t.Fatal("expected refusal: resource handle in stream element")
	}
}

func TestStreamWriterReader_HostHostRendezvous(t *testing.T) {
	in := &Instance{sched: &sched{}}
	sw, readableVal, err := in.NewStream(nil)
	if err != nil {
		t.Fatal(err)
	}
	shared := readableVal.(*sharedStream)
	sr := &StreamReader{in: in, shared: shared, state: copyIdle}

	var readRes CopyResult
	var readVals []abi.Value
	sr.Read(2, func(res CopyResult, vals []abi.Value) { readRes = res; readVals = vals })

	var writeRes CopyResult
	var writeN uint32
	sw.Write(u32vals(10, 20), func(res CopyResult, n uint32) { writeRes = res; writeN = n })

	if writeRes != CopyCompleted || writeN != 2 {
		t.Fatalf("write: res=%v n=%d, want COMPLETED/2", writeRes, writeN)
	}
	if readRes != CopyCompleted || len(readVals) != 2 {
		t.Fatalf("read: res=%v vals=%v, want COMPLETED/[10,20]", readRes, readVals)
	}
}

func TestStreamWriter_WriteTwicePanics(t *testing.T) {
	in := &Instance{sched: &sched{}}
	sw, _, _ := in.NewStream(nil)
	sw.Write(u32vals(1), func(CopyResult, uint32) {}) // parks
	requirePanicContains(t, "previous Write", func() { sw.Write(u32vals(2), func(CopyResult, uint32) {}) })
}

func TestFutureWriter_CloseBeforeSetPanics(t *testing.T) {
	in := &Instance{sched: &sched{}}
	fw, _, _ := in.NewFuture(nil)
	requirePanicContains(t, "before Set completed", func() { fw.Close() })
}

func TestFutureWriter_SetThenClose(t *testing.T) {
	in := &Instance{sched: &sched{}}
	fw, readableVal, _ := in.NewFuture(nil)
	shared := readableVal.(*sharedFuture)
	fr := &FutureReader{in: in, shared: shared, state: copyIdle}

	var got abi.Value
	fr.Get(func(res CopyResult, v abi.Value) {
		if res != CopyCompleted {
			t.Errorf("Get result = %v", res)
		}
		got = v
	})
	fw.Set(uint32(99), func(CopyResult) {})
	if got.(uint32) != 99 {
		t.Fatalf("got %v, want 99", got)
	}
	fw.Close() // now legal: Set completed
}

func TestRequireSchedulable_RawGoroutinePanics(t *testing.T) {
	in := &Instance{sched: &sched{}, activeTask: &task{}}
	requirePanicContains(t, "outside the instance scheduler", func() { in.requireSchedulable("test op") })
}

func TestRequireSchedulable_NoActiveTaskAllowed(t *testing.T) {
	in := &Instance{sched: &sched{}}
	in.requireSchedulable("test op") // no active task: allowed, must not panic
}

func TestRequireSchedulable_InHostCallAllowed(t *testing.T) {
	in := &Instance{sched: &sched{}, activeTask: &task{}, inHostCall: 1}
	in.requireSchedulable("test op") // bracketed by a host import: allowed
}

func TestRequireSchedulable_PumpingAllowed(t *testing.T) {
	in := &Instance{sched: &sched{}, activeTask: &task{}}
	in.sched.pumping = true
	in.requireSchedulable("test op")
}

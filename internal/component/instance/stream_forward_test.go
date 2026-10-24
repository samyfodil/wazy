package instance

import (
	"testing"
)

// addReadableStreamEnd puts a readable end of s into in's table, as lowering a
// stream value does.
func addReadableStreamEnd(in *Instance, s *sharedStream) uint32 {
	e := &streamEnd{side: sideReadable, state: copyIdle, shared: s}
	i := in.resources.addEntry(e)
	registerStreamEnd(e, i)
	return i
}

func addReadableFutureEnd(in *Instance, f *sharedFuture) uint32 {
	e := &futureEnd{side: sideReadable, state: copyIdle, shared: f}
	i := in.resources.addEntry(e)
	registerFutureEnd(e, i)
	return i
}

func pendingEventOf(t *testing.T, in *Instance, i uint32) eventTuple {
	t.Helper()
	raw, ok := in.resources.getEntry(i)
	if !ok {
		t.Fatalf("no entry %d", i)
	}
	w, ok := raw.(interface{ waitablePtr() *waitable })
	if !ok || !w.waitablePtr().hasPendingEvent() {
		t.Fatalf("entry %d has no pending event", i)
	}
	return w.waitablePtr().getPendingEvent()
}

// The host's writer, which no table holds, follows its stream into the one it
// is forwarded into.
func TestStreamForward_HostWriterFollowsTheForward(t *testing.T) {
	in := newAsyncInst()
	sw, val, err := in.NewStream(nil)
	if err != nil {
		t.Fatal(err)
	}
	rA := addReadableStreamEnd(in, val.(*sharedStream))
	rB, wB := newBareStreamEnds(in)
	callBuiltin(streamForwardHostFunc(in, nil), uint64(rA), uint64(wB))
	for _, i := range []uint32{rA, wB} {
		if _, ok := in.resources.getEntry(i); ok {
			t.Fatalf("handle %d still in the table after the forward", i)
		}
	}

	asyncRead := streamCopyHostFunc(in, sideReadable, eventStreamRead, nil, 0, 0, true, true, nil, nil)
	if out := callBuiltin(asyncRead, uint64(rB), 0, 2); uint32(out[0]) != blockedSentinel {
		t.Fatalf("read = %#x, want BLOCKED", out[0])
	}
	var res CopyResult
	var n uint32
	sw.Write(u32vals(1, 2), func(r CopyResult, k uint32) { res, n = r, k })
	if res != CopyCompleted || n != 2 {
		t.Fatalf("host write = %v, %d; want COMPLETED, 2", res, n)
	}
	if ev := pendingEventOf(t, in, rB); ev.p2 != packCopyResult(copyCompleted, 2) {
		t.Fatalf("read event = %#x, want COMPLETED|2", ev.p2)
	}

	// Closing the writer reaches the reader through the merged stream.
	sw.Close()
	if out := callBuiltin(asyncRead, uint64(rB), 0, 1); uint32(out[0]) != packCopyResult(copyDropped, 0) {
		t.Fatalf("read after close = %#x, want DROPPED", out[0])
	}
}

// A write that made partial progress before the forward is still parked, with
// its notification not yet delivered. It moves to the merged stream, and
// delivering that notification takes it off the merged stream, not the one it
// was parked on.
func TestStreamForward_CarriesALivePartialWrite(t *testing.T) {
	in := newAsyncInst()
	rA, wA := newBareStreamEnds(in)
	rB, wB := newBareStreamEnds(in)
	asyncRead := streamCopyHostFunc(in, sideReadable, eventStreamRead, nil, 0, 0, true, true, nil, nil)
	asyncWrite := streamCopyHostFunc(in, sideWritable, eventStreamWrite, nil, 0, 0, true, true, nil, nil)

	if out := callBuiltin(asyncWrite, uint64(wA), 0, 4); uint32(out[0]) != blockedSentinel {
		t.Fatalf("write = %#x, want BLOCKED", out[0])
	}
	if out := callBuiltin(asyncRead, uint64(rA), 0, 1); uint32(out[0]) != packCopyResult(copyCompleted, 1) {
		t.Fatalf("partial read = %#x, want COMPLETED|1", out[0])
	}
	callBuiltin(streamForwardHostFunc(in, nil), uint64(rA), uint64(wB))

	if ev := pendingEventOf(t, in, wA); ev.p2 != packCopyResult(copyCompleted, 1) {
		t.Fatalf("write event = %#x, want COMPLETED|1", ev.p2)
	}
	// The write is over: a read now finds nothing parked, and a new write meets it.
	if out := callBuiltin(asyncRead, uint64(rB), 0, 2); uint32(out[0]) != blockedSentinel {
		t.Fatalf("read after the write ended = %#x, want BLOCKED", out[0])
	}
	if out := callBuiltin(asyncWrite, uint64(wA), 0, 2); uint32(out[0]) != packCopyResult(copyCompleted, 2) {
		t.Fatalf("second write = %#x, want COMPLETED|2", out[0])
	}
}

func TestFutureForward_HostWriterFollowsTheForward(t *testing.T) {
	in := newAsyncInst()
	fw, val, err := in.NewFuture(nil)
	if err != nil {
		t.Fatal(err)
	}
	rA := addReadableFutureEnd(in, val.(*sharedFuture))
	rB, wB := newBareFutureEnds(in)
	callBuiltin(futureForwardHostFunc(in, nil), uint64(rA), uint64(wB))

	asyncRead := futureCopyHostFunc(in, sideReadable, eventFutureRead, nil, 0, 0, true, true, nil, nil)
	if out := callBuiltin(asyncRead, uint64(rB), 0); uint32(out[0]) != blockedSentinel {
		t.Fatalf("read = %#x, want BLOCKED", out[0])
	}
	var res CopyResult
	fw.Set(nil, func(r CopyResult) { res = r })
	if res != CopyCompleted {
		t.Fatalf("host set = %v, want COMPLETED", res)
	}
	if ev := pendingEventOf(t, in, rB); ev.p2 != uint32(copyCompleted) {
		t.Fatalf("read event = %#x, want COMPLETED", ev.p2)
	}
}

func TestForwardTrapText(t *testing.T) {
	for _, tc := range []struct {
		side  endSide
		state copyState
		inSet bool
		want  string
	}{
		{sideReadable, copyIdle, false, ""},
		{sideReadable, copyCopying, false, "cannot forward busy stream"},
		{sideWritable, copyCancelling, true, "cannot forward busy stream"},
		{sideReadable, copyIdle, true, "cannot forward stream while it's in a waitable set"},
		{sideReadable, copyDone, false, "cannot forward stream after being notified that the writable end dropped"},
		{sideWritable, copyDone, false, "cannot forward stream after being notified that the readable end dropped"},
	} {
		if got := forwardTrapText("stream", tc.side, tc.state, tc.inSet); got != tc.want {
			t.Errorf("forwardTrapText(%v, %v, %v) = %q, want %q", tc.side, tc.state, tc.inSet, got, tc.want)
		}
	}
}

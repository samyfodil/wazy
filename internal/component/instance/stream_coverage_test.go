package instance

import (
	"context"
	"errors"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file closes coverage gaps left by stream_test.go /
// stream_phase2_acceptance_test.go: guestBuffer construction/bounds, the
// numeric memmove fast path, the host API's Close/Cancel paths, the
// bind-time element resolver (resolveStreamOrFutureElem /
// streamFutureCanonHostFunc) driven through a real binary.Component, the
// element-resource-handle ceiling walk, and the composition seams
// (repToProviderHandle/providerHandleToRep) for stream/future.

// ---- guestBuffer construction ----

func TestNewGuestBuffer_Traps(t *testing.T) {
	_, mod := memModule(t)
	u32 := binary.PrimitiveDesc{Prim: "u32"}

	if _, err := newGuestBuffer(mod, abi.Realloc{}, nil, u32, 4, 4, true, 0, maxBufferLen+1); err == nil {
		t.Fatal("expected a trap: length exceeds maxBufferLen")
	}
	if _, err := newGuestBuffer(mod, abi.Realloc{}, nil, u32, 4, 4, true, 1 /* misaligned */, 2); err == nil {
		t.Fatal("expected a trap: misaligned pointer")
	}
	// Way out of bounds against the module's actual (small) memory.
	if _, err := newGuestBuffer(mod, abi.Realloc{}, nil, u32, 4, 4, true, 0, 1<<20); err == nil {
		t.Fatal("expected a trap: out of bounds")
	}
	// A bare (nil elem) buffer never touches memory, so it succeeds even
	// with an absurd length/ptr.
	buf, err := newGuestBuffer(mod, abi.Realloc{}, nil, nil, 0, 0, true, 0, 5)
	if err != nil {
		t.Fatalf("bare buffer should not trap: %v", err)
	}
	if buf.isZeroLength() {
		t.Fatal("length 5 buffer reports zero-length")
	}
	empty, err := newGuestBuffer(mod, abi.Realloc{}, nil, nil, 0, 0, true, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !empty.isZeroLength() {
		t.Fatal("length 0 buffer should report zero-length")
	}
}

// TestCopyElementsMemmove_NumericFastPath drives the numeric byte-copy fast
// path between two REAL guest buffers with actual memory content, proving it
// is observably equivalent to the general lift/lower path (design doc §4.2
// item 3).
func TestCopyElementsMemmove_NumericFastPath(t *testing.T) {
	ctx, mod := memModule(t)
	_ = ctx
	mem := mod.Memory()
	if !mem.WriteString(0, "\x11\x22\x33\x44") { // 4 bytes at ptr 0
		t.Fatal("seed write failed")
	}
	u8 := binary.PrimitiveDesc{Prim: "u8"}
	src, err := newGuestBuffer(mod, abi.Realloc{}, nil, u8, 1, 1, true, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := newGuestBuffer(mod, abi.Realloc{}, nil, u8, 1, 1, true, 100, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyElements(true, dst, src, 4); err != nil {
		t.Fatal(err)
	}
	got, ok := mem.Read(100, 4)
	if !ok {
		t.Fatal("read failed")
	}
	if string(got) != "\x11\x22\x33\x44" {
		t.Fatalf("memmove copy mismatch: got %x", got)
	}
	if src.progressed() != 4 || dst.progressed() != 4 {
		t.Fatalf("progress not advanced: src=%d dst=%d", src.progressed(), dst.progressed())
	}

	// n == 0 is a no-op (guards the early return).
	if err := copyElementsMemmove(dst, src, 0); err != nil {
		t.Fatal(err)
	}
}

func TestCopyElementsMemmove_OutOfBoundsTraps(t *testing.T) {
	_, mod := memModule(t)
	u8 := binary.PrimitiveDesc{Prim: "u8"}
	// Construct valid small buffers, then ask to copy more than exists --
	// copyElementsMemmove re-checks bounds itself (defense in depth beyond
	// the constructor's own check).
	src := &guestBuffer{memMod: mod, elem: u8, elemSz: 1, ptr: 0, length: 4}
	dst := &guestBuffer{memMod: mod, elem: u8, elemSz: 1, ptr: 0, length: 4}
	hugeMem := &guestBuffer{memMod: mod, elem: u8, elemSz: 1, ptr: 1 << 20, length: 4}
	if err := copyElementsMemmove(dst, hugeMem, 4); err == nil {
		t.Fatal("expected a source-bounds trap")
	}
	if err := copyElementsMemmove(hugeMem, src, 4); err == nil {
		t.Fatal("expected a destination-bounds trap")
	}
}

// ---- stream_host.go: Close/Cancel paths ----

func TestStreamWriter_CloseAndCancel(t *testing.T) {
	in := &Instance{}
	sw, readableVal, _ := in.NewStream(nil)
	sr := &StreamReader{in: in, shared: readableVal.(*sharedStream), state: copyIdle}

	// Cancel with nothing pending is a no-op.
	sw.Cancel()

	// Write parks; Cancel revokes it with CANCELLED.
	var res CopyResult
	sw.Write(u32vals(1, 2), func(r CopyResult, n uint32) { res = r })
	sw.Cancel()
	if res != CopyCancelled {
		t.Fatalf("cancelled write result = %v, want CANCELLED", res)
	}

	// Close during a pending Write panics.
	sw2, _, _ := in.NewStream(nil)
	sw2.Write(u32vals(1), func(CopyResult, uint32) {})
	requirePanicContains(t, "pending Write", func() { sw2.Close() })
	sw2.Cancel() // clean up the pending write so state settles

	// A clean Close drops the writable end; a subsequent host read
	// observes DROPPED.
	sw.Close()
	var readRes CopyResult
	sr.Read(1, func(r CopyResult, vals []abi.Value) { readRes = r })
	if readRes != CopyDropped {
		t.Fatalf("read after writer Close = %v, want DROPPED", readRes)
	}
}

func TestStreamReader_CloseAndCancel(t *testing.T) {
	in := &Instance{}
	sw, readableVal, _ := in.NewStream(nil)
	sr := &StreamReader{in: in, shared: readableVal.(*sharedStream), state: copyIdle}

	sr.Cancel() // no-op

	var res CopyResult
	sr.Read(2, func(r CopyResult, vals []abi.Value) { res = r })
	sr.Cancel()
	if res != CopyCancelled {
		t.Fatalf("cancelled read result = %v, want CANCELLED", res)
	}

	sr2 := &StreamReader{in: in, shared: readableVal.(*sharedStream), state: copyIdle}
	sr2.Read(1, func(CopyResult, []abi.Value) {})
	requirePanicContains(t, "pending Read", func() { sr2.Close() })
	sr2.Cancel()

	sr.Close()
	var writeRes CopyResult
	sw.Write(u32vals(1), func(r CopyResult, n uint32) { writeRes = r })
	if writeRes != CopyDropped {
		t.Fatalf("write after reader Close = %v, want DROPPED", writeRes)
	}
}

func TestFutureReader_CloseAndCancel(t *testing.T) {
	in := &Instance{}
	fw, readableVal, _ := in.NewFuture(nil)
	fr := &FutureReader{in: in, shared: readableVal.(*sharedFuture), state: copyIdle}

	fr.Cancel() // no-op

	var res CopyResult
	fr.Get(func(r CopyResult, v abi.Value) { res = r })
	fr.Cancel()
	if res != CopyCancelled {
		t.Fatalf("cancelled Get result = %v, want CANCELLED", res)
	}

	fr2 := &FutureReader{in: in, shared: readableVal.(*sharedFuture), state: copyIdle}
	fr2.Get(func(CopyResult, abi.Value) {})
	requirePanicContains(t, "pending Get", func() { fr2.Close() })

	fr3 := &FutureReader{in: in, shared: readableVal.(*sharedFuture), state: copyIdle}
	fr3.Close() // idle Close: drops the readable end
	var setRes CopyResult
	fw.Set(uint32(5), func(r CopyResult) { setRes = r })
	if setRes != CopyDropped {
		t.Fatalf("Set after reader Close = %v, want DROPPED", setRes)
	}
}

func TestFutureWriter_SetTwicePanics(t *testing.T) {
	in := &Instance{}
	fw, _, _ := in.NewFuture(nil)
	fw.Set(uint32(1), func(CopyResult) {}) // parks
	requirePanicContains(t, "more than once", func() { fw.Set(uint32(2), func(CopyResult) {}) })
}

func TestFutureReader_GetTwicePanics(t *testing.T) {
	in := &Instance{}
	_, readableVal, _ := in.NewFuture(nil)
	fr := &FutureReader{in: in, shared: readableVal.(*sharedFuture), state: copyIdle}
	fr.Get(func(CopyResult, abi.Value) {}) // parks
	requirePanicContains(t, "more than once", func() { fr.Get(func(CopyResult, abi.Value) {}) })
}

// ---- elemContainsResourceHandle: full structural coverage ----

func TestElemContainsResourceHandle_AllShapes(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc {
		switch idx {
		case 0:
			return binary.OwnDesc{ResourceType: 1}
		}
		return nil
	}
	ownRef := binary.TypeRef{TypeIndex: uintPtr(0)}
	cases := []struct {
		name string
		t    binary.TypeDesc
		want bool
	}{
		{"own", binary.OwnDesc{ResourceType: 1}, true},
		{"borrow", binary.BorrowDesc{ResourceType: 1}, true},
		{"list", binary.ListDesc{Element: ownRef}, true},
		{"option", binary.OptionDesc{Element: ownRef}, true},
		{"record", binary.RecordDesc{Fields: []binary.RecordField{{Name: "f", Type: ownRef}}}, true},
		{"tuple", binary.TupleDesc{Elements: []binary.TypeRef{ownRef}}, true},
		{"variant", binary.VariantDesc{Cases: []binary.VariantCase{{Name: "c", Type: &ownRef}}}, true},
		{"result-ok", binary.ResultDesc{Ok: &ownRef}, true},
		{"result-err", binary.ResultDesc{Err: &ownRef}, true},
		{"plain", binary.PrimitiveDesc{Prim: "u32"}, false},
		{"list-plain", binary.ListDesc{Element: binary.TypeRef{Primitive: "u32"}}, false},
	}
	for _, c := range cases {
		if got := elemContainsResourceHandle(c.t, resolve, 0); got != c.want {
			t.Errorf("%s: elemContainsResourceHandle = %v, want %v", c.name, got, c.want)
		}
	}
	// depth guard
	if elemContainsResourceHandle(binary.OwnDesc{}, resolve, maxResourceWalkDepth+1) {
		t.Error("depth guard should stop recursion and report false")
	}
}

func uintPtr(v uint32) *uint32 { return &v }

// ---- typeContainsAsyncValueNested: nested vs top-level ----

func TestTypeContainsAsyncValueNested(t *testing.T) {
	stRef := binary.TypeRef{TypeIndex: uintPtr(0)}
	resolve := func(idx uint32) binary.TypeDesc {
		if idx == 0 {
			return binary.StreamDesc{}
		}
		return nil
	}
	// Top-level stream: NOT nested (depth 0 doesn't count).
	if typeContainsAsyncValueNested(binary.StreamDesc{}, resolve, 0) {
		t.Error("a top-level stream must not trip the nested ceiling")
	}
	// Nested inside a list: trips.
	if !typeContainsAsyncValueNested(binary.ListDesc{Element: stRef}, resolve, 0) {
		t.Error("list<stream<...>> should trip the nested ceiling")
	}
	// Nested inside a record/tuple/variant/option/result.
	if !typeContainsAsyncValueNested(binary.RecordDesc{Fields: []binary.RecordField{{Name: "f", Type: stRef}}}, resolve, 0) {
		t.Error("record{f: stream<...>} should trip the ceiling")
	}
	if !typeContainsAsyncValueNested(binary.TupleDesc{Elements: []binary.TypeRef{stRef}}, resolve, 0) {
		t.Error("tuple<stream<...>> should trip the ceiling")
	}
	if !typeContainsAsyncValueNested(binary.VariantDesc{Cases: []binary.VariantCase{{Name: "c", Type: &stRef}}}, resolve, 0) {
		t.Error("variant{c(stream<...>)} should trip the ceiling")
	}
	if !typeContainsAsyncValueNested(binary.OptionDesc{Element: stRef}, resolve, 0) {
		t.Error("option<stream<...>> should trip the ceiling")
	}
	if !typeContainsAsyncValueNested(binary.ResultDesc{Ok: &stRef}, resolve, 0) {
		t.Error("result<stream<...>,_> should trip the ceiling")
	}
	if !typeContainsAsyncValueNested(binary.ResultDesc{Err: &stRef}, resolve, 0) {
		t.Error("result<_,stream<...>> should trip the ceiling")
	}
	// error-context nested.
	ecRef := binary.TypeRef{Primitive: "error-context"}
	if !typeContainsAsyncValueNested(binary.ListDesc{Element: ecRef}, resolve, 0) {
		t.Error("list<error-context> should trip the ceiling")
	}
	if typeContainsAsyncValueNested(binary.PrimitiveDesc{Prim: "u32"}, resolve, 0) {
		t.Error("plain u32 must not trip the ceiling")
	}
}

// ---- composition seams: repToProviderHandle / providerHandleToRep ----

func TestComposition_StreamFutureSeams(t *testing.T) {
	sub := &Instance{resources: newHandleTable(), resolve: func(uint32) binary.TypeDesc { return nil }}

	shared := &sharedStream{numeric: true}
	h, err := repToProviderHandle(sub, binary.StreamDesc{}, shared)
	if err != nil {
		t.Fatal(err)
	}
	hi, ok := h.(uint32)
	if !ok {
		t.Fatalf("repToProviderHandle(stream) = %T, want uint32", h)
	}
	back, err := providerHandleToRep(sub, binary.StreamDesc{}, hi)
	if err != nil {
		t.Fatal(err)
	}
	if back.(*sharedStream) != shared {
		t.Fatal("round trip did not preserve the *sharedStream identity")
	}

	sf := &sharedFuture{numeric: true}
	hf, err := repToProviderHandle(sub, binary.FutureDesc{}, sf)
	if err != nil {
		t.Fatal(err)
	}
	backF, err := providerHandleToRep(sub, binary.FutureDesc{}, hf.(uint32))
	if err != nil {
		t.Fatal(err)
	}
	if backF.(*sharedFuture) != sf {
		t.Fatal("round trip did not preserve the *sharedFuture identity")
	}

	// Type-mismatch traps.
	if _, err := repToProviderHandle(sub, binary.StreamDesc{}, "not-a-stream"); err == nil {
		t.Fatal("expected a trap: wrong Go type for a stream arg")
	}
	if _, err := repToProviderHandle(sub, binary.FutureDesc{}, "not-a-future"); err == nil {
		t.Fatal("expected a trap: wrong Go type for a future arg")
	}
	if _, err := providerHandleToRep(sub, binary.StreamDesc{}, "not-a-handle"); err == nil {
		t.Fatal("expected a trap: non-uint32 provider handle (stream)")
	}
	if _, err := providerHandleToRep(sub, binary.FutureDesc{}, "not-a-handle"); err == nil {
		t.Fatal("expected a trap: non-uint32 provider handle (future)")
	}
}

// ---- host_import.go: resolveHandleArg / allocHandleResult error paths ----

func TestResolveHandleArg_StreamFutureErrorContext_ErrorPaths(t *testing.T) {
	tbl := newHandleTable()
	if _, err := resolveHandleArg(nil, tbl, nil, binary.StreamDesc{}, "not-uint32"); err == nil {
		t.Fatal("expected a trap: non-uint32 stream arg")
	}
	if _, err := resolveHandleArg(nil, tbl, nil, binary.FutureDesc{}, "not-uint32"); err == nil {
		t.Fatal("expected a trap: non-uint32 future arg")
	}
	if _, err := resolveHandleArg(nil, tbl, nil, binary.PrimitiveDesc{Prim: "error-context"}, "not-uint32"); err == nil {
		t.Fatal("expected a trap: non-uint32 error-context arg")
	}
	// Unknown handle.
	if _, err := resolveHandleArg(nil, tbl, nil, binary.StreamDesc{}, uint32(999)); err == nil {
		t.Fatal("expected a trap: unknown stream handle")
	}
	if _, err := resolveHandleArg(nil, tbl, nil, binary.PrimitiveDesc{Prim: "error-context"}, uint32(999)); err == nil {
		t.Fatal("expected a trap: unknown error-context handle")
	}
}

func TestAllocHandleResult_StreamFutureErrorContext(t *testing.T) {
	tbl := newHandleTable()
	shared := &sharedStream{numeric: true}
	v, err := allocHandleResult(tbl, binary.StreamDesc{}, shared)
	if err != nil {
		t.Fatal(err)
	}
	h := v.(uint32)
	raw, _ := tbl.getEntry(h)
	if raw.(*streamEnd).shared != shared {
		t.Fatal("allocHandleResult(stream) did not wrap the shared object")
	}

	sf := &sharedFuture{numeric: true}
	vf, err := allocHandleResult(tbl, binary.FutureDesc{}, sf)
	if err != nil {
		t.Fatal(err)
	}
	rawF, _ := tbl.getEntry(vf.(uint32))
	if rawF.(*futureEnd).shared != sf {
		t.Fatal("allocHandleResult(future) did not wrap the shared object")
	}

	// error-context: string constructs fresh; *errorContext shares.
	vs, err := allocHandleResult(tbl, binary.PrimitiveDesc{Prim: "error-context"}, "boom")
	if err != nil {
		t.Fatal(err)
	}
	rawS, _ := tbl.getEntry(vs.(uint32))
	if rawS.(*errorContext).debugMessage != "boom" {
		t.Fatal("allocHandleResult(error-context, string) did not set the message")
	}

	// Wrong Go types trap.
	if _, err := allocHandleResult(tbl, binary.StreamDesc{}, "nope"); err == nil {
		t.Fatal("expected a trap: wrong type for stream result")
	}
	if _, err := allocHandleResult(tbl, binary.FutureDesc{}, "nope"); err == nil {
		t.Fatal("expected a trap: wrong type for future result")
	}
	if _, err := allocHandleResult(tbl, binary.PrimitiveDesc{Prim: "error-context"}, 5); err == nil {
		t.Fatal("expected a trap: wrong type for error-context result")
	}

	// *StreamReader / *FutureReader forwarding form.
	in := &Instance{}
	_, readableVal, _ := in.NewStream(nil)
	sr := &StreamReader{in: in, shared: readableVal.(*sharedStream), state: copyIdle}
	vr, err := allocHandleResult(tbl, binary.StreamDesc{}, sr)
	if err != nil {
		t.Fatal(err)
	}
	rawR, _ := tbl.getEntry(vr.(uint32))
	if rawR.(*streamEnd).shared != sr.shared {
		t.Fatal("allocHandleResult(*StreamReader) did not forward the shared object")
	}
}

// ---- bind-time element resolution + the full canon dispatch, through a
// real binary.Component (exercises resolveStreamOrFutureElem and
// streamFutureCanonHostFunc's branches directly, without a wasm fixture).

func TestStreamFutureCanonHostFunc_BindAndDispatch(t *testing.T) {
	u8 := binary.TypeRef{Primitive: "u8"}
	u32 := binary.TypeRef{Primitive: "u32"}
	comp := &binary.Component{
		Types: []binary.Type{
			{Kind: "stream", Descriptor: binary.StreamDesc{Element: &u8}},
			{Kind: "future", Descriptor: binary.FutureDesc{Element: &u32}},
		},
	}
	in := newAsyncInst()
	resolve := func(uint32) binary.TypeDesc { return nil }
	in.resolve = resolve

	kinds := []byte{
		binary.CanonKindStreamNew, binary.CanonKindStreamRead, binary.CanonKindStreamWrite,
		binary.CanonKindStreamCancelRead, binary.CanonKindStreamCancelWrite,
		binary.CanonKindStreamDropReadable, binary.CanonKindStreamDropWritable,
	}
	for _, k := range kinds {
		canon := binary.Canon{Kind: k, TypeIdx: 0}
		def, err := streamFutureCanonHostFunc(comp, in, canon, noMemTarget, noFuncTarget)
		if err != nil {
			t.Fatalf("kind %#x: %v", k, err)
		}
		if def.fn == nil {
			t.Fatalf("kind %#x: nil fn", k)
		}
	}
	fkinds := []byte{
		binary.CanonKindFutureNew, binary.CanonKindFutureRead, binary.CanonKindFutureWrite,
		binary.CanonKindFutureCancelRead, binary.CanonKindFutureCancelWrite,
		binary.CanonKindFutureDropReadable, binary.CanonKindFutureDropWritable,
	}
	for _, k := range fkinds {
		canon := binary.Canon{Kind: k, TypeIdx: 1}
		def, err := streamFutureCanonHostFunc(comp, in, canon, noMemTarget, noFuncTarget)
		if err != nil {
			t.Fatalf("kind %#x: %v", k, err)
		}
		if def.fn == nil {
			t.Fatalf("kind %#x: nil fn", k)
		}
	}

	// Unsupported kind.
	if _, err := streamFutureCanonHostFunc(comp, in, binary.Canon{Kind: 0xff, TypeIdx: 0}, noMemTarget, noFuncTarget); err == nil {
		t.Fatal("expected an error for a non-stream/future canon kind")
	}
	// Wrong type kind (TypeIdx names a future, not a stream).
	if _, err := streamFutureCanonHostFunc(comp, in, binary.Canon{Kind: binary.CanonKindStreamNew, TypeIdx: 1}, noMemTarget, noFuncTarget); err == nil {
		t.Fatal("expected an error: type 1 is a future, not a stream")
	}

	// hasAsyncOpt / wrapSchedErr direct coverage.
	if !hasAsyncOpt(binary.Canon{Opts: []binary.CanonOpt{{Kind: 0x06}}}) {
		t.Error("hasAsyncOpt should detect CanonOpt kind 0x06")
	}
	if hasAsyncOpt(binary.Canon{}) {
		t.Error("hasAsyncOpt should be false with no opts")
	}
	if wrapSchedErr("op", errAsyncDeadlock).Error() == "" {
		t.Error("wrapSchedErr should format a deadlock message")
	}
	if wrapSchedErr("op", errors.New("unrelated")).Error() == "" {
		t.Error("wrapSchedErr should format a generic error too")
	}
}

func noMemTarget(int) (api.Module, error)          { return nil, nil }
func noFuncTarget(int) (api.Module, string, error) { return nil, "", nil }

// A bare stream/future element (no Element) resolves cleanly too.
func TestResolveStreamOrFutureElem_Bare(t *testing.T) {
	comp := &binary.Component{Types: []binary.Type{
		{Kind: "stream", Descriptor: binary.StreamDesc{}},
	}}
	elem, sz, align, numeric, err := resolveStreamOrFutureElem(comp, nil, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if elem != nil || sz != 0 || align != 0 || !numeric {
		t.Fatalf("bare stream elem resolution: elem=%v sz=%d align=%d numeric=%v", elem, sz, align, numeric)
	}
}

func TestResolveStreamOrFutureElem_ResourceInElementRefused(t *testing.T) {
	ownRef := binary.TypeRef{TypeIndex: uintPtr(0)}
	comp := &binary.Component{Types: []binary.Type{
		{Kind: "stream", Descriptor: binary.StreamDesc{Element: &ownRef}},
	}}
	resolve := func(idx uint32) binary.TypeDesc {
		if idx == 0 {
			return binary.OwnDesc{ResourceType: 1}
		}
		return nil
	}
	if _, _, _, _, err := resolveStreamOrFutureElem(comp, resolve, false, 0); err == nil {
		t.Fatal("expected refusal: own<T> inside a stream element")
	}
}

// ---- ensure the entry-kind interface methods on Phase 2 table entries are
// exercised directly (some drop/composition tests already invoke them
// indirectly; these guarantee the trivial one-liners themselves run under
// coverage instrumentation).

func TestPhase2EntryKinds(t *testing.T) {
	if (&streamEnd{}).entryKind() != entryStreamEnd {
		t.Error("streamEnd.entryKind mismatch")
	}
	if (&futureEnd{}).entryKind() != entryFutureEnd {
		t.Error("futureEnd.entryKind mismatch")
	}
	if (&errorContext{}).entryKind() != entryErrorContext {
		t.Error("errorContext.entryKind mismatch")
	}
	fe := &futureEnd{}
	if fe.waitablePtr() != &fe.waitable {
		t.Error("futureEnd.waitablePtr mismatch")
	}
}

func TestFutureCancel(t *testing.T) {
	in := newAsyncInst()
	r, w := newBareFutureEnds(in)
	writeFn := futureCopyHostFunc(in, sideWritable, eventFutureWrite, nil, 0, 0, true, true, nil, nil)
	callBuiltin(writeFn, uint64(w), 0) // parks

	cancelFn := cancelCopyHostFunc(in, true, sideWritable, eventFutureWrite, nil, true)
	out := callBuiltin(cancelFn, uint64(w))
	res := copyResult(uint32(out[0]))
	if res != copyCancelled {
		t.Fatalf("future.cancel-write on a parked write: got %v, want CANCELLED", res)
	}
	_ = r
}

func TestErrorContextNew_TrapsWithoutMemory(t *testing.T) {
	in := &Instance{mayLeave: true, resources: newHandleTable()}
	newFn := errorContextNewHostFunc(in, nil)
	requirePanicContains(t, "no memory", func() {
		newFn.fn.Call(context.Background(), nil, []uint64{0, 4, 0, 0})
	})
}

func TestErrorContextDebugMessage_WrongKindTraps(t *testing.T) {
	ctx, mod := memModule(t)
	in := &Instance{mayLeave: true, resources: newHandleTable()}
	h := in.resources.addEntry(&resourceEntry{})
	dmFn := errorContextDebugMessageHostFunc(in, nil, nil)
	requirePanicContains(t, "is not an error-context", func() {
		dmFn.fn.Call(ctx, mod, []uint64{uint64(h), 0, 0, 0})
	})
}

func TestErrorContextDrop_WrongKindTraps(t *testing.T) {
	in := &Instance{mayLeave: true, resources: newHandleTable()}
	h := in.resources.addEntry(&resourceEntry{})
	dropFn := errorContextDropHostFunc(in)
	requirePanicContains(t, "is not an error-context", func() {
		dropFn.fn.Call(context.Background(), nil, []uint64{uint64(h), 0, 0, 0})
	})
}

// ---- remaining trap-edge symmetry: future twins of the stream tests above ----

func TestFutureCopy_EndNotIdleAndJoinedSetTraps(t *testing.T) {
	in := newAsyncInst()
	r, _ := newBareFutureEnds(in)
	readFn := futureCopyHostFunc(in, sideReadable, eventFutureRead, nil, 0, 0, true, true, nil, nil)
	callBuiltin(readFn, uint64(r), 0)
	requirePanicContains(t, "not idle", func() { callBuiltin(readFn, uint64(r), 0) })

	r2, _ := newBareFutureEnds(in)
	set := uint32(callBuiltin(waitableSetNewHostFunc(in), 0)[0])
	callBuiltin(waitableJoinHostFunc(in), uint64(r2), uint64(set))
	syncRead := futureCopyHostFunc(in, sideReadable, eventFutureRead, nil, 0, 0, true, false, nil, nil)
	requirePanicContains(t, "joined to a waitable set", func() { callBuiltin(syncRead, uint64(r2), 0) })
}

func TestFutureCopy_DeadlockOnEmptyRunq(t *testing.T) {
	in := newAsyncInst()
	r, _ := newBareFutureEnds(in)
	syncRead := futureCopyHostFunc(in, sideReadable, eventFutureRead, nil, 0, 0, true, false, nil, nil)
	requirePanicContains(t, "deadlock", func() { callBuiltin(syncRead, uint64(r), 0) })
}

func TestRequireFutureEnd_WrongSideAndKindTraps(t *testing.T) {
	in := newAsyncInst()
	r, w := newBareFutureEnds(in)
	requirePanicContains(t, "not a readable future end", func() {
		callBuiltin(futureDropHostFunc(in, sideReadable, nil), uint64(w))
	})
	requirePanicContains(t, "not a future end", func() {
		callBuiltin(futureDropHostFunc(in, sideReadable, nil), uint64(999))
	})
	_ = r
}

func TestCancelCopyHostFunc_WrongKindTraps(t *testing.T) {
	in := newAsyncInst()
	r, _ := newBareStreamEnds(in)
	// A future cancel builtin against a stream end's handle: wrong kind.
	cancelFn := cancelCopyHostFunc(in, true, sideReadable, eventFutureRead, nil, true)
	requirePanicContains(t, "not a future end", func() { callBuiltin(cancelFn, uint64(r)) })
}

func TestCancelCopyHostFunc_SyncDrivesScheduler(t *testing.T) {
	in := newAsyncInst()
	r, _ := newBareStreamEnds(in)
	asyncRead := streamCopyHostFunc(in, sideReadable, eventStreamRead, nil, 0, 0, true, true, nil, nil)
	callBuiltin(asyncRead, uint64(r), 0, 1) // parks

	re, _ := in.resources.getEntry(r)
	se := re.(*streamEnd)
	// Defer the counterpart's write onto the scheduler queue so a SYNC
	// cancel must drive it to get its CANCELLED delivery.
	in.sched.enqueue(func() error {
		se.shared.cancel()
		return nil
	})
	cancelFn := cancelCopyHostFunc(in, false, sideReadable, eventStreamRead, nil, false /* sync */)
	out := callBuiltin(cancelFn, uint64(r))
	res := copyResult(uint32(out[0]) & 0xf)
	if res != copyCancelled {
		t.Fatalf("sync cancel via scheduler drive: got %v, want CANCELLED", res)
	}
}

// ---- takeReadableFutureEnd: full trap symmetry with takeReadableStreamEnd ----

func TestTakeReadableFutureEnd_Traps(t *testing.T) {
	in := newAsyncInst()
	r, w := newBareFutureEnds(in)

	if _, err := takeReadableFutureEnd(in.resources, nil, w); err == nil {
		t.Fatal("expected a trap transferring a writable future end as readable")
	}
	if _, err := takeReadableFutureEnd(in.resources, binary.PrimitiveDesc{Prim: "u32"}, r); err == nil {
		t.Fatal("expected an element-type-mismatch trap")
	}

	readFn := futureCopyHostFunc(in, sideReadable, eventFutureRead, nil, 0, 0, true, true, nil, nil)
	callBuiltin(readFn, uint64(r), 0)
	if _, err := takeReadableFutureEnd(in.resources, nil, r); err == nil {
		t.Fatal("expected a trap transferring a COPYING readable future end")
	}

	r2, _ := newBareFutureEnds(in)
	set := uint32(callBuiltin(waitableSetNewHostFunc(in), 0)[0])
	callBuiltin(waitableJoinHostFunc(in), uint64(r2), uint64(set))
	if _, err := takeReadableFutureEnd(in.resources, nil, r2); err == nil {
		t.Fatal("expected a trap transferring a waitable-set-joined readable future end")
	}

	r3, _ := newBareFutureEnds(in)
	shared, err := takeReadableFutureEnd(in.resources, nil, r3)
	if err != nil {
		t.Fatalf("clean transfer failed: %v", err)
	}
	if shared == nil {
		t.Fatal("nil shared object")
	}
	if _, ok := in.resources.getEntry(r3); ok {
		t.Fatal("transferred handle should be removed from the sender's table")
	}
	// Unknown handle.
	if _, err := takeReadableFutureEnd(in.resources, nil, 999999); err == nil {
		t.Fatal("expected a trap: unknown future handle")
	}
}

func TestTakeReadableStreamEnd_UnknownHandle(t *testing.T) {
	in := newAsyncInst()
	if _, err := takeReadableStreamEnd(in.resources, nil, 999999); err == nil {
		t.Fatal("expected a trap: unknown stream handle")
	}
}

func TestSideName(t *testing.T) {
	if sideName(sideReadable) != "readable" {
		t.Errorf("sideName(readable) = %q", sideName(sideReadable))
	}
	if sideName(sideWritable) != "writable" {
		t.Errorf("sideName(writable) = %q", sideName(sideWritable))
	}
}

func TestNewStream_WithConcreteElement(t *testing.T) {
	in := &Instance{}
	sw, readableVal, err := in.NewStream(binary.PrimitiveDesc{Prim: "u8"})
	if err != nil {
		t.Fatal(err)
	}
	shared := readableVal.(*sharedStream)
	if shared.elemSz != 1 || shared.align != 1 || !shared.numeric {
		t.Fatalf("u8 element facts: sz=%d align=%d numeric=%v", shared.elemSz, shared.align, shared.numeric)
	}
	_ = sw
}

func TestElemRefContainsResourceHandle_ResolveError(t *testing.T) {
	badRef := binary.TypeRef{} // neither Primitive nor TypeIndex set: resolveTypeRef errors
	if elemRefContainsResourceHandle(&badRef, nil, 0) {
		t.Fatal("an unresolvable ref must report false (fail-open at this leaf), not panic")
	}
}

// TestSharedStream_WriteThenSecondWrite_PendingAlreadyDrained exercises the
// THIRD branch of sharedStream.write: a pending reader whose buffer already
// reached remain()==0 (drained by an earlier partial rendezvous) is
// completed, and the new arrival becomes the pending side in its place.
func TestSharedStream_WriteThenSecondWrite_PendingAlreadyDrained(t *testing.T) {
	s := &sharedStream{numeric: true}
	rbuf := newHostWriteBuffer(2)
	readDone := false
	if err := s.read(nil, rbuf, func(func()) {}, func(copyResult) { readDone = true }); err != nil {
		t.Fatal(err)
	}
	// First write exactly fills the reader (remain drops to 0) without
	// completing it (onCopy just reclaims -- read stays "pending" from the
	// shared object's point of view only until reclaim runs; here we
	// deliberately DON'T reclaim, to land in the exact drained-but-still-
	// pending state the third branch requires).
	firstWriteRes := copyResult(99)
	if err := s.write(nil, newHostReadBuffer(u32vals(1, 2)), func(func()) {}, func(r copyResult) { firstWriteRes = r }); err != nil {
		t.Fatal(err)
	}
	if firstWriteRes != copyCompleted {
		t.Fatalf("first write result = %v, want COMPLETED", firstWriteRes)
	}
	if readDone {
		t.Fatal("the reader's own onCopyDone must not fire from onCopy (only reclaim, which this test withholds)")
	}
	if s.pendingBuffer != rbuf || rbuf.remain() != 0 {
		t.Fatalf("reader should still be pending with remain()==0, got pending=%v remain=%d", s.pendingBuffer == rbuf, rbuf.remain())
	}

	// A second write now arrives: pendingBuffer(reader).remain()==0 and it's
	// not a zero-length handshake (src is non-zero-length) -- the THIRD
	// branch: complete the drained reader, then park the new write.
	secondWriteRes := copyResult(99)
	wbuf2 := newHostReadBuffer(u32vals(3))
	if err := s.write(nil, wbuf2, func(func()) {}, func(r copyResult) { secondWriteRes = r }); err != nil {
		t.Fatal(err)
	}
	if !readDone {
		t.Fatal("the drained reader must be completed by the second write")
	}
	if secondWriteRes != 99 {
		t.Fatal("the second write should PARK (become the new pending side), not complete yet")
	}
	if s.pendingBuffer != wbuf2 {
		t.Fatal("the second write should now be the pending side")
	}
}

// TestSharedStream_ReadThenSecondRead_PendingAlreadyDrained is the read-side
// mirror (a parked WRITER drained to remain()==0 by an earlier partial
// rendezvous, then a second read arrives).
func TestSharedStream_ReadThenSecondRead_PendingAlreadyDrained(t *testing.T) {
	s := &sharedStream{numeric: true}
	wbuf := newHostReadBuffer(u32vals(1, 2))
	writeDone := false
	if err := s.write(nil, wbuf, func(func()) {}, func(copyResult) { writeDone = true }); err != nil {
		t.Fatal(err)
	}
	firstReadRes := copyResult(99)
	if err := s.read(nil, newHostWriteBuffer(2), func(func()) {}, func(r copyResult) { firstReadRes = r }); err != nil {
		t.Fatal(err)
	}
	if firstReadRes != copyCompleted {
		t.Fatalf("first read result = %v, want COMPLETED", firstReadRes)
	}
	if writeDone {
		t.Fatal("the writer's own onCopyDone must not fire from onCopy alone")
	}

	secondReadRes := copyResult(99)
	rbuf2 := newHostWriteBuffer(1)
	if err := s.read(nil, rbuf2, func(func()) {}, func(r copyResult) { secondReadRes = r }); err != nil {
		t.Fatal(err)
	}
	if !writeDone {
		t.Fatal("the drained writer must be completed by the second read")
	}
	if secondReadRes != 99 {
		t.Fatal("the second read should PARK, not complete yet")
	}
	if s.pendingBuffer != rbuf2 {
		t.Fatal("the second read should now be the pending side")
	}
}

func TestResolveNewElem_SizeAndAlignmentErrors(t *testing.T) {
	// A record field whose type index the resolver can't resolve makes
	// abi.Size/Alignment fail while walking the record's fields.
	unresolvedIdx := uint32(7)
	bad := binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "f", Type: binary.TypeRef{TypeIndex: &unresolvedIdx}},
	}}
	resolve := func(uint32) binary.TypeDesc { return nil }
	if _, _, _, err := resolveNewElem(resolve, bad); err == nil {
		t.Fatal("expected an error resolving size/alignment for an unresolvable field type")
	}
}

func TestErrorContextDebugMessage_TrapsWithoutMemory(t *testing.T) {
	in := &Instance{mayLeave: true, resources: newHandleTable()}
	h := in.resources.addEntry(&errorContext{debugMessage: "x"})
	dmFn := errorContextDebugMessageHostFunc(in, nil, nil)
	requirePanicContains(t, "no memory", func() {
		dmFn.fn.Call(context.Background(), nil, []uint64{uint64(h), 0, 0, 0})
	})
}

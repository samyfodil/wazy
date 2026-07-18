package binary

import (
	"bytes"
	"testing"
)

// Phase 0 decoder round-trip tests for the async canonical ABI: canon kinds
// 0x05+ (task/subtask/context/stream/future/error-context/waitable-set/
// backpressure builtins) and the stream/future defvaltype tags (0x65/0x66).
// Ground truth for every assertion below is `wasm-tools dump` (wasm-tools
// 1.253) on the corresponding testdata/async/*.wasm fixture -- see the .wat
// sources next to each .wasm for the human-readable source.

// loadAsyncFixture decodes a fixture from testdata/async/ via the embedded
// fixtureFS (testdata_embed_test.go), matching loadFixture's pattern for the
// top-level testdata fixtures.
func loadAsyncFixture(t *testing.T, name string) *Component {
	t.Helper()
	data, err := fixtureFS.ReadFile("testdata/async/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	c, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode %s: %v", name, err)
	}
	return c
}

// primRef builds a TypeRef{Primitive: prim} for comparison convenience.
func primRef(prim string) TypeRef { return TypeRef{Primitive: prim} }

// TestDecodeAsyncBuiltins_Types decodes builtins.wasm's two component types
// -- `(type $st (stream u8))` and `(type $ft (future u32))` -- and checks
// their descriptors against `wasm-tools dump`:
//
//	0x3f | 66 01 7d | [type 0] Defined(Stream(Some(Primitive(U8))))
//	0x42 | 65 01 79 | [type 1] Defined(Future(Some(Primitive(U32))))
func TestDecodeAsyncBuiltins_Types(t *testing.T) {
	c := loadAsyncFixture(t, "builtins.wasm")

	if len(c.Types) != 2 {
		t.Fatalf("types: got %d, want 2 (%+v)", len(c.Types), c.Types)
	}

	st, ok := c.Types[0].Descriptor.(StreamDesc)
	if !ok {
		t.Fatalf("type[0] descriptor: got %T, want StreamDesc", c.Types[0].Descriptor)
	}
	if st.Element == nil || *st.Element != primRef("u8") {
		t.Errorf("type[0] stream element: got %+v, want u8", st.Element)
	}
	if c.Types[0].Kind != "stream" {
		t.Errorf("type[0] kind: got %q, want stream", c.Types[0].Kind)
	}

	ft, ok := c.Types[1].Descriptor.(FutureDesc)
	if !ok {
		t.Fatalf("type[1] descriptor: got %T, want FutureDesc", c.Types[1].Descriptor)
	}
	if ft.Element == nil || *ft.Element != primRef("u32") {
		t.Errorf("type[1] future element: got %+v, want u32", ft.Element)
	}
	if c.Types[1].Kind != "future" {
		t.Errorf("type[1] kind: got %q, want future", c.Types[1].Kind)
	}
}

// TestDecodeAsyncBuiltins_Canons decodes builtins.wasm's 24 canon entries and
// checks each Kind + payload against `wasm-tools dump`'s per-entry listing
// (reproduced in the table comment inline with each assertion below).
func TestDecodeAsyncBuiltins_Canons(t *testing.T) {
	c := loadAsyncFixture(t, "builtins.wasm")

	if len(c.Canons) != 24 {
		t.Fatalf("canons: got %d, want 24", len(c.Canons))
	}
	cn := c.Canons

	// [core func 0] BackpressureInc
	if cn[0].Kind != CanonKindBackpressureInc {
		t.Errorf("canon[0] kind: got %#x, want BackpressureInc", cn[0].Kind)
	}
	// [core func 1] BackpressureDec
	if cn[1].Kind != CanonKindBackpressureDec {
		t.Errorf("canon[1] kind: got %#x, want BackpressureDec", cn[1].Kind)
	}
	// [core func 2] TaskReturn { result: Some(Primitive(U32)), options: [] }
	if cn[2].Kind != CanonKindTaskReturn {
		t.Errorf("canon[2] kind: got %#x, want TaskReturn", cn[2].Kind)
	}
	if cn[2].TaskReturnResult.Unnamed == nil || *cn[2].TaskReturnResult.Unnamed != primRef("u32") {
		t.Errorf("canon[2] task.return result: got %+v, want Some(u32)", cn[2].TaskReturnResult)
	}
	if len(cn[2].Opts) != 0 {
		t.Errorf("canon[2] opts: got %+v, want none", cn[2].Opts)
	}
	// [core func 3] TaskCancel
	if cn[3].Kind != CanonKindTaskCancel {
		t.Errorf("canon[3] kind: got %#x, want TaskCancel", cn[3].Kind)
	}
	// [core func 4] SubtaskDrop
	if cn[4].Kind != CanonKindSubtaskDrop {
		t.Errorf("canon[4] kind: got %#x, want SubtaskDrop", cn[4].Kind)
	}
	// [core func 5] SubtaskCancel { async_: false }
	if cn[5].Kind != CanonKindSubtaskCancel {
		t.Errorf("canon[5] kind: got %#x, want SubtaskCancel", cn[5].Kind)
	}
	if cn[5].Async {
		t.Errorf("canon[5] async_: got true, want false")
	}
	// [core func 6] WaitableSetNew
	if cn[6].Kind != CanonKindWaitableSetNew {
		t.Errorf("canon[6] kind: got %#x, want WaitableSetNew", cn[6].Kind)
	}
	// [core func 7] WaitableSetWait { cancellable: false, memory: 0 }
	if cn[7].Kind != CanonKindWaitableSetWait {
		t.Errorf("canon[7] kind: got %#x, want WaitableSetWait", cn[7].Kind)
	}
	if cn[7].Cancellable || cn[7].MemIdx != 0 {
		t.Errorf("canon[7] wait payload: got cancellable=%v memIdx=%d, want false,0", cn[7].Cancellable, cn[7].MemIdx)
	}
	// [core func 8] WaitableSetPoll { cancellable: false, memory: 0 }
	if cn[8].Kind != CanonKindWaitableSetPoll {
		t.Errorf("canon[8] kind: got %#x, want WaitableSetPoll", cn[8].Kind)
	}
	if cn[8].Cancellable || cn[8].MemIdx != 0 {
		t.Errorf("canon[8] poll payload: got cancellable=%v memIdx=%d, want false,0", cn[8].Cancellable, cn[8].MemIdx)
	}
	// [core func 9] WaitableSetDrop
	if cn[9].Kind != CanonKindWaitableSetDrop {
		t.Errorf("canon[9] kind: got %#x, want WaitableSetDrop", cn[9].Kind)
	}
	// [core func 10] WaitableJoin
	if cn[10].Kind != CanonKindWaitableJoin {
		t.Errorf("canon[10] kind: got %#x, want WaitableJoin", cn[10].Kind)
	}
	// [core func 11] StreamNew { ty: 0 }
	if cn[11].Kind != CanonKindStreamNew || cn[11].TypeIdx != 0 {
		t.Errorf("canon[11]: got kind=%#x ty=%d, want StreamNew ty=0", cn[11].Kind, cn[11].TypeIdx)
	}
	// [core func 12] StreamRead { ty: 0, options: [] }
	if cn[12].Kind != CanonKindStreamRead || cn[12].TypeIdx != 0 || len(cn[12].Opts) != 0 {
		t.Errorf("canon[12]: got %+v, want StreamRead ty=0 opts=[]", cn[12])
	}
	// [core func 13] StreamWrite { ty: 0, options: [] }
	if cn[13].Kind != CanonKindStreamWrite || cn[13].TypeIdx != 0 || len(cn[13].Opts) != 0 {
		t.Errorf("canon[13]: got %+v, want StreamWrite ty=0 opts=[]", cn[13])
	}
	// [core func 14] StreamDropReadable { ty: 0 }
	if cn[14].Kind != CanonKindStreamDropReadable || cn[14].TypeIdx != 0 {
		t.Errorf("canon[14]: got %+v, want StreamDropReadable ty=0", cn[14])
	}
	// [core func 15] StreamDropWritable { ty: 0 }
	if cn[15].Kind != CanonKindStreamDropWritable || cn[15].TypeIdx != 0 {
		t.Errorf("canon[15]: got %+v, want StreamDropWritable ty=0", cn[15])
	}
	// [core func 16] FutureNew { ty: 1 }
	if cn[16].Kind != CanonKindFutureNew || cn[16].TypeIdx != 1 {
		t.Errorf("canon[16]: got %+v, want FutureNew ty=1", cn[16])
	}
	// [core func 17] FutureRead { ty: 1, options: [] }
	if cn[17].Kind != CanonKindFutureRead || cn[17].TypeIdx != 1 || len(cn[17].Opts) != 0 {
		t.Errorf("canon[17]: got %+v, want FutureRead ty=1 opts=[]", cn[17])
	}
	// [core func 18] FutureWrite { ty: 1, options: [] }
	if cn[18].Kind != CanonKindFutureWrite || cn[18].TypeIdx != 1 || len(cn[18].Opts) != 0 {
		t.Errorf("canon[18]: got %+v, want FutureWrite ty=1 opts=[]", cn[18])
	}
	// [core func 19] ErrorContextNew { options: [Memory(0)] }
	if cn[19].Kind != CanonKindErrorContextNew {
		t.Errorf("canon[19] kind: got %#x, want ErrorContextNew", cn[19].Kind)
	}
	if len(cn[19].Opts) != 1 || cn[19].Opts[0].Kind != 0x03 || cn[19].Opts[0].Idx != 0 {
		t.Errorf("canon[19] opts: got %+v, want [Memory(0)]", cn[19].Opts)
	}
	// [core func 20] ErrorContextDebugMessage { options: [Memory(0)] }
	if cn[20].Kind != CanonKindErrorContextDebugMessage {
		t.Errorf("canon[20] kind: got %#x, want ErrorContextDebugMessage", cn[20].Kind)
	}
	if len(cn[20].Opts) != 1 || cn[20].Opts[0].Kind != 0x03 || cn[20].Opts[0].Idx != 0 {
		t.Errorf("canon[20] opts: got %+v, want [Memory(0)]", cn[20].Opts)
	}
	// [core func 21] ErrorContextDrop
	if cn[21].Kind != CanonKindErrorContextDrop {
		t.Errorf("canon[21] kind: got %#x, want ErrorContextDrop", cn[21].Kind)
	}
	// [core func 22] ContextGet { ty: I32, slot: 0 }
	if cn[22].Kind != CanonKindContextGet || cn[22].CoreValType != 0x7f || cn[22].Slot != 0 {
		t.Errorf("canon[22]: got %+v, want ContextGet ty=i32 slot=0", cn[22])
	}
	// [core func 23] ContextSet { ty: I32, slot: 0 }
	if cn[23].Kind != CanonKindContextSet || cn[23].CoreValType != 0x7f || cn[23].Slot != 0 {
		t.Errorf("canon[23]: got %+v, want ContextSet ty=i32 slot=0", cn[23])
	}
}

// TestDecodeAsyncBuiltins_CoreFuncSpace checks that every one of the 24 async
// canons occupies the component's core func index space (none of them are
// canon lift, so none should land in ComponentFuncSpace) -- this is the
// structural invariant corefuncspace.go documents and the async decode must
// preserve for any subsequent core-func alias to resolve to the right index.
func TestDecodeAsyncBuiltins_CoreFuncSpace(t *testing.T) {
	c := loadAsyncFixture(t, "builtins.wasm")

	if len(c.CoreFuncSpace) != 24 {
		t.Fatalf("CoreFuncSpace: got %d entries, want 24", len(c.CoreFuncSpace))
	}
	for i, e := range c.CoreFuncSpace {
		if e.Kind != CoreFuncFromCanon {
			t.Errorf("CoreFuncSpace[%d].Kind: got %v, want CoreFuncFromCanon", i, e.Kind)
		}
		if e.Canon != uint32(i) {
			t.Errorf("CoreFuncSpace[%d].Canon: got %d, want %d", i, e.Canon, i)
		}
	}
	if len(c.ComponentFuncSpace) != 0 {
		t.Errorf("ComponentFuncSpace: got %d entries, want 0 (no canon lift in this fixture)", len(c.ComponentFuncSpace))
	}
}

// TestDecodeLiftAsyncCallback decodes lift_async_callback.wasm's single
// `canon lift` with async+callback options. Ground truth:
//
//	[func 0] Lift { core_func_index: 1, type_index: 0,
//	                options: [Memory(0), Realloc(2), Async, Callback(0)] }
//
// This exercises the EXISTING lift decode path (kind 0x00 was already
// implemented, and decodeCanonOpts already accepted 0x06/0x07) -- Phase 0
// changes nothing here, but it's the one fixture proving an async+callback
// lift decodes end-to-end without regression.
func TestDecodeLiftAsyncCallback(t *testing.T) {
	c := loadAsyncFixture(t, "lift_async_callback.wasm")

	if len(c.Canons) != 1 {
		t.Fatalf("canons: got %d, want 1", len(c.Canons))
	}
	cn := c.Canons[0]
	if cn.Kind != CanonKindLift {
		t.Fatalf("canon[0] kind: got %#x, want lift", cn.Kind)
	}
	if cn.CoreFuncIdx != 1 {
		t.Errorf("canon[0] core func index: got %d, want 1", cn.CoreFuncIdx)
	}
	if cn.TypeIdx != 0 {
		t.Errorf("canon[0] type index: got %d, want 0", cn.TypeIdx)
	}
	if len(cn.Opts) != 4 {
		t.Fatalf("canon[0] opts: got %d, want 4 (%+v)", len(cn.Opts), cn.Opts)
	}
	wantOpts := []CanonOpt{
		{Kind: 0x03, Idx: 0}, // memory 0
		{Kind: 0x04, Idx: 2}, // realloc 2
		{Kind: 0x06},         // async
		{Kind: 0x07, Idx: 0}, // callback 0
	}
	for i, want := range wantOpts {
		if cn.Opts[i] != want {
			t.Errorf("canon[0] opts[%d]: got %+v, want %+v", i, cn.Opts[i], want)
		}
	}

	// The lifted export ("lifted") should still resolve through
	// ComponentFuncSpace, unaffected by the async options.
	if len(c.Exports) != 1 || c.Exports[0].Name != "lifted" {
		t.Errorf("exports: got %+v, want one \"lifted\"", c.Exports)
	}
}

// TestDecodeStreamFutureCancelDrop decodes stream_future_cancel_drop.wasm's
// six canons (cancel-read/write for both stream and future, plus future's
// drop-readable/drop-writable). Ground truth:
//
//	[core func 0] StreamCancelRead { ty: 0, async_: false }
//	[core func 1] StreamCancelWrite { ty: 0, async_: false }
//	[core func 2] FutureCancelRead { ty: 1, async_: false }
//	[core func 3] FutureCancelWrite { ty: 1, async_: false }
//	[core func 4] FutureDropReadable { ty: 1 }
//	[core func 5] FutureDropWritable { ty: 1 }
func TestDecodeStreamFutureCancelDrop(t *testing.T) {
	c := loadAsyncFixture(t, "stream_future_cancel_drop.wasm")

	if len(c.Canons) != 6 {
		t.Fatalf("canons: got %d, want 6", len(c.Canons))
	}
	cn := c.Canons

	if cn[0].Kind != CanonKindStreamCancelRead || cn[0].TypeIdx != 0 || cn[0].Async {
		t.Errorf("canon[0]: got %+v, want StreamCancelRead ty=0 async_=false", cn[0])
	}
	if cn[1].Kind != CanonKindStreamCancelWrite || cn[1].TypeIdx != 0 || cn[1].Async {
		t.Errorf("canon[1]: got %+v, want StreamCancelWrite ty=0 async_=false", cn[1])
	}
	if cn[2].Kind != CanonKindFutureCancelRead || cn[2].TypeIdx != 1 || cn[2].Async {
		t.Errorf("canon[2]: got %+v, want FutureCancelRead ty=1 async_=false", cn[2])
	}
	if cn[3].Kind != CanonKindFutureCancelWrite || cn[3].TypeIdx != 1 || cn[3].Async {
		t.Errorf("canon[3]: got %+v, want FutureCancelWrite ty=1 async_=false", cn[3])
	}
	if cn[4].Kind != CanonKindFutureDropReadable || cn[4].TypeIdx != 1 {
		t.Errorf("canon[4]: got %+v, want FutureDropReadable ty=1", cn[4])
	}
	if cn[5].Kind != CanonKindFutureDropWritable || cn[5].TypeIdx != 1 {
		t.Errorf("canon[5]: got %+v, want FutureDropWritable ty=1", cn[5])
	}

	// All six occupy the core func index space (none is canon lift).
	if len(c.CoreFuncSpace) != 6 {
		t.Fatalf("CoreFuncSpace: got %d entries, want 6", len(c.CoreFuncSpace))
	}
}

// TestDecodeTaskReturnNoResult probes the OTHER half of the brief's flagged
// task.return encoding ambiguity: a task.return with NO result. Confirmed by
// hand-building `(canon task.return (core func $tr))` with wasm-tools parse
// and dumping it: bytes `09 01 00 00` decode as kind=0x09, tag=0x01 (named
// result list) count=0 (empty -- "no results", the same shape a func with no
// return value would use), then an empty opts vec. This is NOT tag=0x00
// (which always requires a following valtype) -- see
// TestDecodeAsyncBuiltins_Canons's canon[2] for the tag=0x00 case.
func TestDecodeTaskReturnNoResult(t *testing.T) {
	buf := preamble()
	canonBody := []byte{
		0x01,       // count = 1
		0x09,       // kind = task.return
		0x01, 0x00, // result: tag=0x01 (named), count=0 (no results)
		0x00, // opts: count=0
	}
	buf = append(buf, 8, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Canons) != 1 {
		t.Fatalf("canons: got %d, want 1", len(c.Canons))
	}
	cn := c.Canons[0]
	if cn.Kind != CanonKindTaskReturn {
		t.Fatalf("kind: got %#x, want TaskReturn", cn.Kind)
	}
	if cn.TaskReturnResult.Unnamed != nil {
		t.Errorf("result.Unnamed: got %+v, want nil", cn.TaskReturnResult.Unnamed)
	}
	if len(cn.TaskReturnResult.Named) != 0 {
		t.Errorf("result.Named: got %+v, want empty", cn.TaskReturnResult.Named)
	}
	if len(cn.Opts) != 0 {
		t.Errorf("opts: got %+v, want empty", cn.Opts)
	}
}

// TestDecodeErrorContextInSignature probes the brief's flagged-for-"verify"
// encoding: error-context (tag 0x64) is a TERMINAL PRIMITIVE valtype, not a
// defined type with a payload -- confirmed via `wasm-tools dump` on
// `(func (result error-context))`, which encodes as `40 00 00 64` and is
// printed as `Func(ComponentFuncType { ..., result: Some(Primitive(ErrorContext)) })`.
// This constructs that exact byte sequence by hand (no fixture file needed --
// the brief's suggested fixture is exactly this shape) and decodes it as a
// standalone component type section.
func TestDecodeErrorContextInSignature(t *testing.T) {
	buf := preamble()
	// Component type section (id 7): 1 count, func type with no params and
	// an error-context result: 40 (func) 00 (0 params) 00 (unnamed result
	// tag) 64 (error-context primitive).
	typeBody := []byte{0x01, 0x40, 0x00, 0x00, 0x64}
	buf = append(buf, 7, byte(len(typeBody)))
	buf = append(buf, typeBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Types) != 1 {
		t.Fatalf("types: got %d, want 1", len(c.Types))
	}
	fn, ok := c.Types[0].Descriptor.(FuncDesc)
	if !ok {
		t.Fatalf("type[0] descriptor: got %T, want FuncDesc", c.Types[0].Descriptor)
	}
	if fn.Results.Unnamed == nil || fn.Results.Unnamed.Primitive != "error-context" {
		t.Errorf("func result: got %+v, want Some(error-context)", fn.Results.Unnamed)
	}
}

package instance

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// testdata/async/return_then_write.wat: exports returning a stream they
// write only after they have returned.
//
//go:embed testdata/async/return_then_write.wasm
var returnThenWriteWasm []byte

func instantiateReturnThenWrite(t *testing.T) *Instance {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	in, err := Instantiate(ctx, r, returnThenWriteWasm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(ctx) })
	return in
}

// readOnce reads what the stream has, failing t unless the read completes
// before Read returns.
func readOnce(t *testing.T, r *StreamReader) (CopyResult, []abi.Value) {
	t.Helper()
	var res CopyResult
	var vals []abi.Value
	done := false
	r.Read(4, func(rs CopyResult, vs []abi.Value) { res, vals, done = rs, vs, true })
	if !done {
		t.Fatal("the read did not complete")
	}
	return res, vals
}

// A call returns at task.return, though the task is parked writing the
// stream it returned; the host's read lets it finish, with no further call.
func TestCallReturnsBeforeTaskExits(t *testing.T) {
	ctx := context.Background()
	for _, export := range []string{"produce", "produce-stackful"} {
		t.Run(export, func(t *testing.T) {
			in := instantiateReturnThenWrite(t)
			for i := 0; i < 2; i++ {
				res, err := in.Call(ctx, export, false)
				if err != nil {
					t.Fatal(err)
				}
				r := res[0].(*StreamReader)
				if rs, vals := readOnce(t, r); rs != CopyCompleted || len(vals) != 1 || vals[0] != uint32(7) {
					t.Fatalf("read = %v %v, want completed [7]", rs, vals)
				}
				if rs, _ := readOnce(t, r); rs != CopyDropped {
					t.Fatalf("second read = %v, want dropped: the task should have finished", rs)
				}
			}
		})
	}

	in := instantiateReturnThenWrite(t)
	p, err := in.CallAsync(ctx, "produce", false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Await(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rs, vals := readOnce(t, res[0].(*StreamReader)); rs != CopyCompleted || len(vals) != 1 {
		t.Fatalf("read = %v %v, want completed", rs, vals)
	}
}

// A task that traps after its call returned, resumed by a host read, poisons
// the instance, and the next call says why.
func TestTrapAfterReturnPoisons(t *testing.T) {
	ctx := context.Background()
	for _, export := range []string{"produce", "produce-stackful"} {
		t.Run(export, func(t *testing.T) {
			in := instantiateReturnThenWrite(t)
			res, err := in.Call(ctx, export, true)
			if err != nil {
				t.Fatal(err)
			}
			readOnce(t, res[0].(*StreamReader))
			_, err = in.Call(ctx, export, false)
			if err == nil || !strings.Contains(err.Error(), "cannot enter component instance: ") || !strings.Contains(err.Error(), "unreachable") {
				t.Fatalf("call after the trap: %v", err)
			}
			if export == "produce" { // CallAsync takes only a callback lift
				if _, err := in.CallAsync(ctx, export, false); err == nil || !strings.Contains(err.Error(), "unreachable") {
					t.Fatalf("CallAsync after the trap: %v", err)
				}
			}
		})
	}
}

// An export's stream, future or error-context result leaves the guest's
// table as the value itself; a handle that is not one fails the call.
func TestTakeAsyncResult(t *testing.T) {
	in := &Instance{sched: &sched{}, resources: newHandleTable()}
	shared, fs := &sharedStream{}, &sharedFuture{}
	ec := NewErrorContext("m")
	w := in.resources.addEntry(&streamEnd{side: sideWritable, shared: shared})
	busy := in.resources.addEntry(&streamEnd{side: sideReadable, state: copyCopying, shared: shared})
	ok := in.resources.addEntry(&streamEnd{side: sideReadable, shared: shared})
	fw := in.resources.addEntry(&futureEnd{side: sideWritable, shared: fs})
	fok := in.resources.addEntry(&futureEnd{side: sideReadable, shared: fs})
	eh := in.resources.addEntry(ec)
	idx := uint32(5)
	unresolved := &binary.TypeRef{TypeIndex: &idx}
	in.resolve = func(uint32) binary.TypeDesc { return nil }
	ecDesc := binary.PrimitiveDesc{Prim: "error-context"}

	for _, c := range []struct {
		name string
		rt   binary.TypeDesc
		v    abi.Value
		want string
	}{
		{"not a handle", binary.StreamDesc{}, "x", "expected a uint32 handle, got string"},
		{"writable", binary.StreamDesc{}, w, "is not a READABLE stream end"},
		{"busy", binary.StreamDesc{}, busy, "handle"},
		{"future writable", binary.FutureDesc{}, fw, "is not a READABLE future end"},
		{"element", binary.StreamDesc{Element: unresolved}, ok, "element type"},
		{"not an error-context", ecDesc, w, "is not an error-context"},
	} {
		if _, err := in.takeAsyncResult(c.rt, c.v, "f"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if v, err := in.takeAsyncResult(binary.StreamDesc{}, ok, "f"); err != nil || v.(*StreamReader).shared != shared {
		t.Fatalf("stream: %v", err)
	}
	if _, found := in.resources.getEntry(ok); found {
		t.Fatal("the readable end left the guest's table")
	}
	if v, err := in.takeAsyncResult(binary.FutureDesc{}, fok, "f"); err != nil || v.(*FutureReader).shared != fs {
		t.Fatalf("future: %v", err)
	}
	if v, err := in.takeAsyncResult(ecDesc, eh, "f"); err != nil || v != ec {
		t.Fatalf("error-context: %v", err)
	}
	if _, found := in.resources.getEntry(eh); !found {
		t.Fatal("the guest keeps its error-context handle")
	}
	if v, err := in.takeAsyncResult(binary.PrimitiveDesc{Prim: "u32"}, uint32(3), "f"); err != nil || v != uint32(3) {
		t.Fatalf("u32: %v %v", v, err)
	}
	if ec.DebugMessage() != "m" {
		t.Fatal(ec.DebugMessage())
	}
}

// NewStreamOf and NewFutureOf resolve the element in the host's own space,
// refusing one it cannot resolve or that holds a resource; the reader they
// return goes to a guest as an argument like the identity NewStream does.
func TestNewStreamOf(t *testing.T) {
	in := &Instance{sched: &sched{}, resources: newHandleTable()}
	host := []binary.TypeDesc{binary.ListDesc{Element: binary.TypeRef{Primitive: "string"}}, binary.OwnDesc{ResourceType: 1}}
	resolve := func(i uint32) binary.TypeDesc {
		if int(i) < len(host) {
			return host[i]
		}
		return nil
	}
	idx := func(i uint32) *binary.TypeRef { return &binary.TypeRef{TypeIndex: &i} }

	w, r, err := in.NewStreamOf(idx(0), resolve)
	if err != nil || w.shared != r.shared || r.shared.elemResolve == nil {
		t.Fatalf("NewStreamOf: %v", err)
	}
	h, err := in.resolveArgHandlesDepth(r, binary.StreamDesc{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := in.resources.getEntry(h.(uint32)); e.(*streamEnd).shared != r.shared {
		t.Fatal("a *StreamReader argument mints a readable end for its stream")
	}
	fw, fr, err := in.NewFutureOf(nil, nil)
	if err != nil || fw.shared != fr.shared {
		t.Fatalf("NewFutureOf: %v", err)
	}
	h, err = in.resolveArgHandlesDepth(fr, binary.FutureDesc{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := in.resources.getEntry(h.(uint32)); e.(*futureEnd).shared != fr.shared {
		t.Fatal("a *FutureReader argument mints a readable end for its future")
	}

	for name, err := range map[string]error{
		"stream unresolved": second(in.NewStreamOf(idx(9), resolve)),
		"future unresolved": second(in.NewFutureOf(idx(9), resolve)),
		"stream resource":   second(in.NewStreamOf(idx(1), resolve)),
		"future resource":   second(in.NewFutureOf(idx(1), resolve)),
		"no resolver":       second(in.NewStreamOf(idx(0), nil)),
	} {
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func second[A, B any](_ A, _ B, err error) error { return err }

// Element types compare by structure, each side through its own resolver:
// the same type has different indices in a host's table and a component's.
func TestSameElemType(t *testing.T) {
	ref := func(i uint32) binary.TypeRef { return binary.TypeRef{TypeIndex: &i} }
	prim := func(p string) binary.TypeRef { return binary.TypeRef{Primitive: p} }
	ptr := func(r binary.TypeRef) *binary.TypeRef { return &r }
	space := func(ts ...binary.TypeDesc) abi.Resolver {
		return func(i uint32) binary.TypeDesc {
			if int(i) < len(ts) {
				return ts[i]
			}
			return nil
		}
	}
	// a's list<string> is its index 0, b's is its index 3.
	a := space(binary.ListDesc{Element: prim("string")})
	b := space(nil, nil, nil, binary.ListDesc{Element: prim("string")})
	la, lb := ref(0), ref(3)
	for _, c := range []struct {
		name string
		x, y binary.TypeDesc
		want bool
	}{
		{"nil", nil, nil, true},
		{"nil and prim", nil, binary.PrimitiveDesc{Prim: "u8"}, false},
		{"prim", binary.PrimitiveDesc{Prim: "u8"}, binary.PrimitiveDesc{Prim: "u8"}, true},
		{"prim differs", binary.PrimitiveDesc{Prim: "u8"}, binary.PrimitiveDesc{Prim: "u16"}, false},
		{"list", binary.ListDesc{Element: la}, binary.ListDesc{Element: lb}, true},
		{"list differs", binary.ListDesc{Element: la}, binary.ListDesc{Element: prim("u8")}, false},
		{"fixed", binary.FixedListDesc{Element: la, Length: 2}, binary.FixedListDesc{Element: lb, Length: 2}, true},
		{"fixed length", binary.FixedListDesc{Element: la, Length: 2}, binary.FixedListDesc{Element: lb, Length: 3}, false},
		{"option", binary.OptionDesc{Element: la}, binary.OptionDesc{Element: lb}, true},
		{"map", binary.MapDesc{Key: prim("string"), Value: la}, binary.MapDesc{Key: prim("string"), Value: lb}, true},
		{"map key", binary.MapDesc{Key: prim("u8"), Value: la}, binary.MapDesc{Key: prim("string"), Value: lb}, false},
		{"result", binary.ResultDesc{Ok: ptr(la)}, binary.ResultDesc{Ok: ptr(lb)}, true},
		{"result arm", binary.ResultDesc{Ok: ptr(la)}, binary.ResultDesc{Err: ptr(lb)}, false},
		{"stream", binary.StreamDesc{Element: ptr(la)}, binary.StreamDesc{Element: ptr(lb)}, true},
		{"future", binary.FutureDesc{}, binary.FutureDesc{}, true},
		{"future differs", binary.FutureDesc{}, binary.FutureDesc{Element: ptr(la)}, false},
		{"enum", binary.EnumDesc{Cases: []string{"a"}}, binary.EnumDesc{Cases: []string{"a"}}, true},
		{"enum differs", binary.EnumDesc{Cases: []string{"a"}}, binary.EnumDesc{Cases: []string{"b"}}, false},
		{"flags", binary.FlagsDesc{Names: []string{"a"}}, binary.FlagsDesc{Names: []string{"a"}}, true},
		{"tuple", binary.TupleDesc{Elements: []binary.TypeRef{la}}, binary.TupleDesc{Elements: []binary.TypeRef{lb}}, true},
		{"tuple length", binary.TupleDesc{Elements: []binary.TypeRef{la}}, binary.TupleDesc{}, false},
		{"tuple element", binary.TupleDesc{Elements: []binary.TypeRef{la}}, binary.TupleDesc{Elements: []binary.TypeRef{prim("u8")}}, false},
		{
			"record",
			binary.RecordDesc{Fields: []binary.RecordField{{Name: "x", Type: la}}},
			binary.RecordDesc{Fields: []binary.RecordField{{Name: "x", Type: lb}}},
			true,
		},
		{
			"record name",
			binary.RecordDesc{Fields: []binary.RecordField{{Name: "x", Type: la}}},
			binary.RecordDesc{Fields: []binary.RecordField{{Name: "y", Type: lb}}},
			false,
		},
		{"record length", binary.RecordDesc{Fields: []binary.RecordField{{Name: "x", Type: la}}}, binary.RecordDesc{}, false},
		{
			"variant",
			binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a", Type: ptr(la)}, {Name: "b"}}},
			binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a", Type: ptr(lb)}, {Name: "b"}}},
			true,
		},
		{
			"variant payload",
			binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a", Type: ptr(la)}}},
			binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a"}}},
			false,
		},
		{"variant length", binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a"}}}, binary.VariantDesc{}, false},
		{"own", binary.OwnDesc{ResourceType: 1}, binary.OwnDesc{ResourceType: 1}, true},
		{"own differs", binary.OwnDesc{ResourceType: 1}, binary.OwnDesc{ResourceType: 2}, false},
		{"borrow", binary.BorrowDesc{ResourceType: 1}, binary.BorrowDesc{ResourceType: 1}, true},
		{"kinds differ", binary.ListDesc{Element: la}, binary.OptionDesc{Element: lb}, false},
		{"unresolvable both, same index", binary.ListDesc{Element: ref(7)}, binary.ListDesc{Element: ref(7)}, true},
		{"unresolvable both, other index", binary.ListDesc{Element: ref(7)}, binary.ListDesc{Element: ref(8)}, false},
		{"unresolvable one", binary.ListDesc{Element: ref(7)}, binary.ListDesc{Element: lb}, false},
	} {
		if got := elemTypesCompatible(nil, c.x, a, nil, c.y, b); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	// Indices with no resolver compare as themselves, as a test's bare
	// stream did before element types had resolvers.
	if !elemTypesCompatible(nil, binary.ListDesc{Element: la}, nil, nil, binary.ListDesc{Element: ref(0)}, nil) {
		t.Error("equal unresolved indices")
	}
	// A type that refers to itself ends at the walk's depth limit.
	loop := space(binary.ListDesc{Element: ref(0)})
	if elemTypesCompatible(nil, binary.ListDesc{Element: ref(0)}, loop, nil, binary.ListDesc{Element: ref(0)}, loop) {
		t.Error("a cyclic type compares unequal at the depth limit")
	}
}

// A host end's completion callback runs inside the guest's stream builtin, on
// the driving goroutine, so it may use the stream: here, close it.
func TestCallbackMayUseStream(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	closed := false
	sinkParams := []binary.TypeDesc{binary.StreamDesc{Element: &binary.TypeRef{Primitive: "u8"}}}
	opt := WithImport("sink", "", func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		reader := args[0].(*StreamReader)
		reader.Read(4, func(CopyResult, []abi.Value) {
			reader.Close()
			closed = true
		})
		return nil, nil
	}, sinkParams, nil)
	in, err := Instantiate(ctx, r, streamWriteHostReadWasm, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close(ctx)
	if _, err := in.Call(ctx, "run"); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("the read did not complete")
	}
}

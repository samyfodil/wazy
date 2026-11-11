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

// Streams, futures and error contexts nested in each kind of composite lower
// into a guest's table as handles and lift back out as the same values.
func TestAsyncNest(t *testing.T) {
	in := &Instance{sched: &sched{}, resources: newHandleTable()}
	idx := func(i uint32) binary.TypeRef { return binary.TypeRef{TypeIndex: &i} }
	ptr := func(r binary.TypeRef) *binary.TypeRef { return &r }
	u8, u32, str, ec := binary.TypeRef{Primitive: "u8"}, binary.TypeRef{Primitive: "u32"}, binary.TypeRef{Primitive: "string"}, binary.TypeRef{Primitive: "error-context"}
	types := []binary.TypeDesc{
		binary.StreamDesc{Element: &u8},
		binary.FutureDesc{Element: &u32},
		binary.RecordDesc{Fields: []binary.RecordField{{Name: "s", Type: idx(0)}, {Name: "f", Type: idx(1)}, {Name: "n", Type: u32}}},
		binary.ListDesc{Element: ec},
		binary.OptionDesc{Element: idx(0)},
		binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a", Type: ptr(idx(0))}, {Name: "b"}}},
		binary.ResultDesc{Ok: ptr(idx(3)), Err: ptr(idx(1))},
		binary.MapDesc{Key: str, Value: idx(0)},
		binary.FixedListDesc{Element: idx(1), Length: 2},
		binary.TupleDesc{Elements: []binary.TypeRef{idx(0), u32}},
		binary.OptionDesc{Element: idx(4)},
	}
	resolve := func(i uint32) binary.TypeDesc {
		if int(i) < len(types) {
			return types[i]
		}
		return nil
	}
	in.resolve = resolve
	stream := func() *StreamReader {
		_, r, err := in.NewStreamOf(&u8, resolve)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	future := func() *FutureReader {
		_, r, err := in.NewFutureOf(&u32, resolve)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	s, f, e := stream(), future(), NewErrorContext("e")
	for _, c := range []struct {
		name string
		t    uint32
		v    abi.Value
	}{
		{"record", 2, []abi.Value{s, f, uint32(7)}},
		{"list", 3, []abi.Value{e, e}},
		{"option", 4, s},
		{"none", 4, nil},
		{"variant", 5, abi.VariantValue{Disc: 0, Payload: s}},
		{"variant without payload", 5, abi.VariantValue{Disc: 1}},
		{"result ok", 6, abi.ResultValue{Payload: []abi.Value{e}}},
		{"result err", 6, abi.ResultValue{IsErr: true, Payload: f}},
		{"map", 7, []abi.Value{[]abi.Value{"k", s}}},
		{"fixed list", 8, []abi.Value{f, f}},
		{"tuple", 9, []abi.Value{s, uint32(1)}},
		{"some(some)", 10, abi.Some{Value: s}},
	} {
		n := newAsyncNest(types[c.t], resolve)
		if n == nil {
			t.Fatalf("%s: no nest", c.name)
		}
		lowered, err := n.lower(in.resources, c.v)
		if err != nil {
			t.Fatalf("%s: lower: %v", c.name, err)
		}
		lifted, err := n.lift(in, in.resources, lowered)
		if err != nil {
			t.Fatalf("%s: lift: %v", c.name, err)
		}
		if !sameAsync(lifted, c.v) {
			t.Errorf("%s: %#v round-trips to %#v", c.name, c.v, lifted)
		}
	}
	if newAsyncNest(binary.ListDesc{Element: u32}, resolve) != nil || newAsyncNest(types[0], resolve) != nil || newAsyncNest(nil, resolve) != nil {
		t.Error("a type with nothing nested has no nest")
	}

	for _, c := range []struct {
		name string
		td   binary.TypeDesc
		v    abi.Value
		want string
	}{
		{"stream", binary.StreamDesc{}, "x", "stream: expected a *StreamReader, got string"},
		{"future", binary.FutureDesc{}, "x", "future: expected a *FutureReader, got string"},
		{"error-context", binary.PrimitiveDesc{Prim: "error-context"}, 1, "error-context: expected a *ErrorContext or string, got int"},
	} {
		if _, err := lowerAsyncValue(in.resources, c.td, c.v); err == nil || err.Error() != c.want {
			t.Errorf("lower %s: %v", c.name, err)
		}
	}
	h, err := lowerAsyncValue(in.resources, binary.PrimitiveDesc{Prim: "error-context"}, "made")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := liftAsyncValue(in, in.resources, resolve, binary.PrimitiveDesc{Prim: "error-context"}, h); err != nil || v.(*ErrorContext).DebugMessage() != "made" {
		t.Fatalf("a string lowers to a new error context: %v", err)
	}
	sh, _ := lowerAsyncValue(in.resources, binary.StreamDesc{Element: &u8}, s.shared)
	fh, _ := lowerAsyncValue(in.resources, binary.FutureDesc{Element: &u32}, f.shared)
	bad := idx(99)
	for _, c := range []struct {
		name string
		td   binary.TypeDesc
		v    abi.Value
		want string
	}{
		{"not a handle", binary.StreamDesc{}, "x", "stream: expected a uint32 handle, got string"},
		{"stream element", binary.StreamDesc{Element: &bad}, sh, "element type"},
		{"future element", binary.FutureDesc{Element: &bad}, fh, "element type"},
		{"stream mismatch", binary.StreamDesc{Element: &u32}, sh, "stream element type mismatch"},
		{"future mismatch", binary.FutureDesc{Element: &u8}, fh, "future element type mismatch"},
		{"not an error-context", binary.PrimitiveDesc{Prim: "error-context"}, uint32(999), "handle 999 is not an error-context"},
	} {
		if _, err := liftAsyncValue(in, in.resources, resolve, c.td, c.v); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("lift %s: %v", c.name, err)
		}
	}
}

// sameAsync compares lifted values, a reader by the stream or future it
// reads.
func sameAsync(a, b abi.Value) bool {
	switch x := a.(type) {
	case *StreamReader:
		y, ok := b.(*StreamReader)
		return ok && x.shared == y.shared
	case *FutureReader:
		y, ok := b.(*FutureReader)
		return ok && x.shared == y.shared
	case []abi.Value:
		y, ok := b.([]abi.Value)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameAsync(x[i], y[i]) {
				return false
			}
		}
		return true
	case abi.VariantValue:
		y, ok := b.(abi.VariantValue)
		return ok && x.Disc == y.Disc && sameAsync(x.Payload, y.Payload)
	case abi.ResultValue:
		y, ok := b.(abi.ResultValue)
		return ok && x.IsErr == y.IsErr && sameAsync(x.Payload, y.Payload)
	case abi.Some:
		y, ok := b.(abi.Some)
		return ok && sameAsync(x.Value, y.Value)
	}
	return a == b
}

//go:embed testdata/async/nested_result.wasm
var nestedResultWasm []byte

// A sync export's result holding a stream and an error context below its top
// level gives the host both, taken out of the guest's table as it returns.
func TestNestedExportResult(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	in, err := Instantiate(ctx, r, nestedResultWasm)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close(ctx)
	res, err := in.Call(ctx, "pair")
	if err != nil {
		t.Fatal(err)
	}
	pair := res[0].([]abi.Value)
	if got := pair[1].(*ErrorContext).DebugMessage(); got != "nested" {
		t.Fatalf("error context %q", got)
	}
	sr := pair[0].(*StreamReader)
	if rs, _ := readOnce(t, sr); rs != CopyDropped {
		t.Fatalf("read = %v, want dropped: the guest closed the stream", rs)
	}
	if _, found := in.resources.getEntry(1); found {
		t.Fatal("the readable end left the guest's table")
	}
}

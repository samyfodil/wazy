package instance

import (
	"reflect"
	"testing"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// A nested component's host import moves the handles below the top level of
// its args into the embedder's table and those of its results back out
// (hostHandleTransfer). These cases walk every composite a handle can sit
// in, each way, and every way a value can fail to be what its type says.

// handleTypes is a resolver over a fixed type table; index 0 is the
// resource's own<R>, 1 its borrow<R>, and 99 resolves to nothing.
func handleTypes(extra ...binary.TypeDesc) (abi.Resolver, func(binary.TypeDesc) binary.TypeRef) {
	table := []binary.TypeDesc{binary.OwnDesc{ResourceType: resTag}, binary.BorrowDesc{ResourceType: resTag}}
	table = append(table, extra...)
	resolve := func(idx uint32) binary.TypeDesc {
		if int(idx) < len(table) {
			return table[idx]
		}
		return nil
	}
	ref := func(td binary.TypeDesc) binary.TypeRef {
		idx := uint32(len(table))
		table = append(table, td)
		return binary.TypeRef{TypeIndex: &idx}
	}
	return resolve, ref
}

const resTag = 5

func idxRef(i uint32) binary.TypeRef { return binary.TypeRef{TypeIndex: &i} }

var (
	ownRef    = idxRef(0)
	borrowRef = idxRef(1)
	badRef    = idxRef(99)
	u32Ref    = binary.TypeRef{Primitive: "u32"}
)

func TestHostHandleTransfer_Composites(t *testing.T) {
	resolve, ref := handleTypes()
	rec := binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: ownRef}, {Name: "b", Type: u32Ref}}}
	variant := binary.VariantDesc{Cases: []binary.VariantCase{{Name: "none"}, {Name: "some", Type: &ownRef}}}
	result := binary.ResultDesc{Ok: &ownRef}
	errResult := binary.ResultDesc{Err: &ownRef}
	nestedList := binary.ListDesc{Element: ref(binary.TupleDesc{Elements: []binary.TypeRef{ownRef}})}
	for _, tc := range []struct {
		name string
		t    binary.TypeDesc
		v    func(h uint32) abi.Value
		want func(h uint32) abi.Value
	}{
		{"record", rec, func(h uint32) abi.Value { return []abi.Value{h, uint32(7)} }, nil},
		{"tuple", binary.TupleDesc{Elements: []binary.TypeRef{ownRef}}, func(h uint32) abi.Value { return []abi.Value{h} }, nil},
		{"list", binary.ListDesc{Element: ownRef}, func(h uint32) abi.Value { return []abi.Value{h} }, nil},
		{"list of tuples", nestedList, func(h uint32) abi.Value { return []abi.Value{[]abi.Value{h}} }, nil},
		{"fixed list", binary.FixedListDesc{Element: ownRef, Length: 1}, func(h uint32) abi.Value { return []abi.Value{h} }, nil},
		{"map", binary.MapDesc{Key: u32Ref, Value: ownRef}, func(h uint32) abi.Value { return []abi.Value{[]abi.Value{uint32(1), h}} }, nil},
		{"variant case", variant, func(h uint32) abi.Value { return abi.VariantValue{Disc: 1, Payload: h} }, nil},
		{"variant case without payload", variant, func(uint32) abi.Value { return abi.VariantValue{Disc: 0} }, func(uint32) abi.Value { return abi.VariantValue{Disc: 0} }},
		{"option some", binary.OptionDesc{Element: ownRef}, func(h uint32) abi.Value { return h }, nil},
		{"option none", binary.OptionDesc{Element: ownRef}, func(uint32) abi.Value { return nil }, func(uint32) abi.Value { return nil }},
		{"result ok", result, func(h uint32) abi.Value { return abi.ResultValue{Payload: h} }, nil},
		{"result err without payload", result, func(uint32) abi.Value { return abi.ResultValue{IsErr: true} }, func(uint32) abi.Value { return abi.ResultValue{IsErr: true} }},
		{"result err", errResult, func(h uint32) abi.Value { return abi.ResultValue{IsErr: true, Payload: h} }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !nestedHandleIn(tc.t, resolve, 0) && tc.want == nil {
				t.Fatal("nestedHandleIn missed the handle")
			}
			guest, host := newHandleTable(), newHandleTable()
			x := &hostHandleTransfer{resolve: resolve, guest: guest, host: host, params: []binary.TypeDesc{tc.t}, results: []binary.TypeDesc{tc.t}}
			gh := guest.NewOwn(resTag, 42)
			args := []abi.Value{tc.v(gh)}
			borrows, err := x.moveArgs(args)
			if err != nil || len(borrows) != 0 {
				t.Fatalf("moveArgs: %v, %v", borrows, err)
			}
			if tc.want != nil { // no handle in this value: it passes untouched
				if !reflect.DeepEqual(args[0], tc.want(0)) {
					t.Fatalf("got %#v, want %#v", args[0], tc.want(0))
				}
				return
			}
			if _, err := guest.Rep(resTag, gh); err == nil {
				t.Fatal("the guest still holds the handle it gave up")
			}
			hh := uint32(1) // the host table's first handle
			if rep, err := host.Rep(resTag, hh); err != nil || rep != 42 {
				t.Fatalf("host table: rep %d, %v", rep, err)
			}
			if !reflect.DeepEqual(args[0], tc.v(hh)) {
				t.Fatalf("args = %#v, want %#v", args[0], tc.v(hh))
			}
			// And back out, as a result: the host's handle goes, a fresh
			// guest one arrives, and the host's value is not rewritten.
			hostVal := tc.v(hh)
			results := []abi.Value{hostVal}
			if err := x.moveResults(results); err != nil {
				t.Fatalf("moveResults: %v", err)
			}
			if _, err := host.Rep(resTag, hh); err == nil {
				t.Fatal("the host table still holds the handle it returned")
			}
			if !reflect.DeepEqual(hostVal, tc.v(hh)) {
				t.Fatalf("the host's own value was rewritten: %#v", hostVal)
			}
			// The guest's table reuses the slot it gave up.
			if rep, err := guest.Rep(resTag, gh); err != nil || rep != 42 || !reflect.DeepEqual(results[0], tc.v(gh)) {
				t.Fatalf("result = %#v (rep %d, %v), want %#v", results[0], rep, err, tc.v(gh))
			}
		})
	}
}

func TestHostHandleTransfer_BorrowLastsTheCall(t *testing.T) {
	resolve, _ := handleTypes()
	guest, host := newHandleTable(), newHandleTable()
	list := binary.ListDesc{Element: borrowRef}
	x := &hostHandleTransfer{resolve: resolve, guest: guest, host: host, params: []binary.TypeDesc{nil, list}, results: []binary.TypeDesc{nil}}
	gh := guest.NewOwn(resTag, 9)
	args := []abi.Value{uint32(77), []abi.Value{gh}}
	borrows, err := x.moveArgs(args)
	if err != nil || len(borrows) != 1 {
		t.Fatalf("moveArgs: %v, %v", borrows, err)
	}
	if args[0] != uint32(77) {
		t.Fatalf("a param without a nested handle changed: %v", args[0])
	}
	hb := args[1].([]abi.Value)[0].(uint32)
	if rep, err := host.Rep(resTag, hb); err != nil || rep != 9 {
		t.Fatalf("host borrow: rep %d, %v", rep, err)
	}
	if rep, err := guest.Rep(resTag, gh); err != nil || rep != 9 {
		t.Fatalf("a borrow took the guest's handle: %d, %v", rep, err)
	}
	x.releaseBorrows(borrows)
	if _, err := host.Rep(resTag, hb); err == nil {
		t.Fatal("the borrow outlived the call")
	}
	if err := x.moveResults([]abi.Value{uint32(77)}); err != nil {
		t.Fatalf("a result without a nested handle: %v", err)
	}
}

func TestHostHandleTransfer_Refusals(t *testing.T) {
	resolve, _ := handleTypes()
	rec := binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: ownRef}}}
	brec := binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: borrowRef}}}
	for _, tc := range []struct {
		name    string
		t       binary.TypeDesc
		v       abi.Value
		results bool
		want    string
	}{
		{"own not a handle", rec, []abi.Value{"x"}, false, "own<resource type 5>: expected a uint32 handle, got string"},
		{"own not live", rec, []abi.Value{uint32(3)}, false, "param 0: "},
		{"borrow not a handle", brec, []abi.Value{"x"}, false, "borrow<resource type 5>: expected a uint32 handle, got string"},
		{"borrow not live", brec, []abi.Value{uint32(3)}, false, "param 0: "},
		{"borrow in a result", brec, []abi.Value{uint32(1)}, true, "result 0: borrow<resource type 5> in a result"},
		{"record not a list", rec, uint32(1), false, "binary.RecordDesc: expected []Value, got uint32"},
		{"variant not a variant", binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a", Type: &ownRef}}}, uint32(1), false, "variant: expected VariantValue, got uint32"},
		{"result not a result", binary.ResultDesc{Ok: &ownRef}, uint32(1), false, "result: expected ResultValue, got uint32"},
		{"variant payload not live", binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a", Type: &ownRef}}}, abi.VariantValue{Payload: uint32(3)}, false, "param 0: "},
		{"result payload not live", binary.ResultDesc{Ok: &ownRef}, abi.ResultValue{Payload: uint32(3)}, false, "param 0: "},
		{"map entry not live", binary.MapDesc{Key: u32Ref, Value: ownRef}, []abi.Value{[]abi.Value{uint32(1), uint32(3)}}, false, "param 0: "},
		{"element unresolvable", binary.TupleDesc{Elements: []binary.TypeRef{badRef}}, []abi.Value{uint32(1)}, false, "param 0: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guest, host := newHandleTable(), newHandleTable()
			guest.NewOwn(resTag, 1)
			host.NewOwn(resTag, 1)
			x := &hostHandleTransfer{resolve: resolve, guest: guest, host: host, params: []binary.TypeDesc{tc.t}, results: []binary.TypeDesc{tc.t}}
			var err error
			if tc.results {
				err = x.moveResults([]abi.Value{tc.v})
			} else {
				_, err = x.moveArgs([]abi.Value{tc.v})
			}
			requireErrContains(t, err, tc.want)
		})
	}
}

func TestNewHostHandleTransfer(t *testing.T) {
	resolve, ref := handleTypes()
	guest, host := newHandleTable(), newHandleTable()
	nested := ref(binary.ListDesc{Element: ownRef})
	// Top-level handles only: the lift already turns them into reps.
	if x := newHostHandleTransfer(binary.FuncDesc{Params: []binary.FuncParam{{Type: ownRef}}, Results: binary.FuncResults{Unnamed: &borrowRef}}, resolve, guest, host); x != nil {
		t.Fatalf("top-level handles: %+v; want no transfer", x)
	}
	x := newHostHandleTransfer(binary.FuncDesc{Params: []binary.FuncParam{{Type: u32Ref}, {Type: nested}, {Type: badRef}}, Results: binary.FuncResults{Unnamed: &nested}}, resolve, guest, host)
	if x == nil || x.params[0] != nil || x.params[1] == nil || x.params[2] != nil || x.results[0] == nil {
		t.Fatalf("nested handles: %+v", x)
	}
	// Where a type does not resolve, it holds no handle.
	if nestedHandleIn(binary.OptionDesc{Element: badRef}, resolve, 0) || nestedHandleIn(binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a"}}}, resolve, 0) {
		t.Fatal("found a handle in a type without one")
	}
	if !nestedHandleIn(binary.RecordDesc{Fields: []binary.RecordField{{Type: u32Ref}, {Type: ownRef}}}, resolve, 0) ||
		!nestedHandleIn(binary.TupleDesc{Elements: []binary.TypeRef{u32Ref, borrowRef}}, resolve, 0) ||
		!nestedHandleIn(binary.ResultDesc{Err: &ownRef}, resolve, 0) ||
		!nestedHandleIn(binary.MapDesc{Key: u32Ref, Value: ownRef}, resolve, 0) {
		t.Fatal("missed a nested handle")
	}
}

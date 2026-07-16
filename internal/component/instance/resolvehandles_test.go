package instance

import (
	"testing"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// TestResolveArgHandles covers resolveArgHandles/typeContainsResource across
// every composite nesting -- a guest-owned borrow<counter> handle buried in a
// record/tuple/list/variant/option/result must be converted to the guest's rep
// at any depth, while a host-owned resource and resource-free subtrees pass
// through unchanged. Driven directly (no guest) so all branches are exercised.
func TestResolveArgHandles(t *testing.T) {
	const guestRes, hostRes = uint32(99), uint32(7)
	tbl := newHandleTable()
	rep := uint32(4242)
	borrowH := tbl.NewBorrow(guestRes, rep) // guest-owned borrow handle -> rep
	hostH := tbl.NewBorrow(hostRes, 5)      // host-owned -> stays a handle

	// A tiny type-index space the resolver serves. 0: borrow<guest>, 1: u32,
	// 2: borrow<host>.
	descs := []binary.TypeDesc{
		binary.BorrowDesc{ResourceType: guestRes},
		binary.PrimitiveDesc{Prim: "u32"},
		binary.BorrowDesc{ResourceType: hostRes},
	}
	resolve := func(i uint32) binary.TypeDesc { return descs[i] }
	ref := func(i uint32) binary.TypeRef { return binary.TypeRef{TypeIndex: &[]uint32{i}[0]} }

	in := &Instance{
		resolve:         resolve,
		resources:       tbl,
		isGuestResource: func(r uint32) bool { return r == guestRes },
	}

	borrowRef, u32Ref := ref(0), ref(1)

	cases := []struct {
		name string
		t    binary.TypeDesc
		in   abi.Value
		want abi.Value
	}{
		{"top-level borrow", descs[0], borrowH, rep},
		{"host-owned borrow untouched", descs[2], hostH, hostH},
		{"list", binary.ListDesc{Element: borrowRef}, []abi.Value{borrowH, borrowH}, []abi.Value{rep, rep}},
		{"record", binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: u32Ref}, {Name: "c", Type: borrowRef}}}, []abi.Value{uint32(1), borrowH}, []abi.Value{uint32(1), rep}},
		{"tuple", binary.TupleDesc{Elements: []binary.TypeRef{borrowRef, u32Ref}}, []abi.Value{borrowH, uint32(9)}, []abi.Value{rep, uint32(9)}},
		{"option some", binary.OptionDesc{Element: borrowRef}, borrowH, rep},
		{"option none", binary.OptionDesc{Element: borrowRef}, nil, nil},
		{"variant payload", binary.VariantDesc{Cases: []binary.VariantCase{{Name: "x", Type: &borrowRef}}}, abi.VariantValue{Disc: 0, Payload: borrowH}, abi.VariantValue{Disc: 0, Payload: rep}},
		{"result ok", binary.ResultDesc{Ok: &borrowRef}, abi.ResultValue{Payload: borrowH}, abi.ResultValue{Payload: rep}},
		{"result err", binary.ResultDesc{Err: &borrowRef}, abi.ResultValue{IsErr: true, Payload: borrowH}, abi.ResultValue{IsErr: true, Payload: rep}},
		{"no resource passes through", binary.PrimitiveDesc{Prim: "u32"}, uint32(3), uint32(3)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// typeContainsResource must agree with whether a resolve would matter.
			got, err := in.resolveArgHandles(tc.in, tc.t)
			if err != nil {
				t.Fatalf("resolveArgHandles: %v", err)
			}
			if !valuesEqual(got, tc.want) {
				t.Fatalf("resolveArgHandles = %#v, want %#v", got, tc.want)
			}
		})
	}

	// typeContainsResource across every composite branch.
	yes := []binary.TypeDesc{
		binary.ListDesc{Element: borrowRef},
		binary.OptionDesc{Element: borrowRef},
		binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: u32Ref}, {Name: "b", Type: borrowRef}}},
		binary.TupleDesc{Elements: []binary.TypeRef{u32Ref, borrowRef}},
		binary.VariantDesc{Cases: []binary.VariantCase{{Name: "n"}, {Name: "x", Type: &borrowRef}}},
		binary.ResultDesc{Ok: &u32Ref, Err: &borrowRef},
		binary.BorrowDesc{ResourceType: guestRes},
	}
	for i, d := range yes {
		if !typeContainsResource(d, resolve, 0) {
			t.Errorf("yes[%d] %T should contain a resource", i, d)
		}
	}
	no := []binary.TypeDesc{
		binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: u32Ref}}},
		binary.TupleDesc{Elements: []binary.TypeRef{u32Ref, u32Ref}},
		binary.PrimitiveDesc{Prim: "string"},
		binary.ResultDesc{Ok: &u32Ref},
	}
	for i, d := range no {
		if typeContainsResource(d, resolve, 0) {
			t.Errorf("no[%d] %T should not contain a resource", i, d)
		}
	}
}

// TestDropResourceErrors covers DropResource/DropOwned's fail-loud branches:
// an unknown handle, and a borrow (non-own) handle. Both fail before any
// destructor lookup, so a minimal Instance (no closers) suffices.
func TestDropResourceErrors(t *testing.T) {
	tbl := newHandleTable()
	borrowH := tbl.NewBorrow(1, 100)
	ownH := tbl.NewOwn(1, 200)
	in := &Instance{resources: tbl}
	ctx := t.Context()

	if err := in.DropResource(ctx, "i", "r", 99999); err == nil {
		t.Error("dropping an unknown handle should fail")
	}
	if err := in.DropResource(ctx, "i", "r", borrowH); err == nil {
		t.Error("dropping a borrow handle should fail")
	}
	// An own handle with no destructor export just removes the handle.
	if err := in.DropResource(ctx, "i", "r", ownH); err != nil {
		t.Fatalf("dropping an own handle (no dtor) should succeed: %v", err)
	}
	if err := in.DropResource(ctx, "i", "r", ownH); err == nil {
		t.Error("double drop should fail")
	}
}

func valuesEqual(a, b abi.Value) bool {
	switch av := a.(type) {
	case []abi.Value:
		bv, ok := b.([]abi.Value)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !valuesEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case abi.VariantValue:
		bv, ok := b.(abi.VariantValue)
		return ok && av.Disc == bv.Disc && valuesEqual(av.Payload, bv.Payload)
	case abi.ResultValue:
		bv, ok := b.(abi.ResultValue)
		return ok && av.IsErr == bv.IsErr && valuesEqual(av.Payload, bv.Payload)
	default:
		return a == b
	}
}

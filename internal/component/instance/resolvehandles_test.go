package instance

import (
	"testing"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// A borrow of a host resource is lent to the call: the callee gets a borrow
// scoped to it, which it must drop, and never the host's own handle, whose
// drop would destroy the resource. A borrow already scoped to the call -- a
// composition's delegated one -- passes through as it is.
func TestResolveArgHandlesLendsHostBorrows(t *testing.T) {
	const hostRes = uint32(7)
	tbl := newHandleTable()
	in := &Instance{sched: &sched{}, resources: tbl, isGuestResource: func(uint32) bool { return false }}
	borrow := binary.BorrowDesc{ResourceType: 30}

	for name, h := range map[string]uint32{
		"own":             tbl.NewOwn(hostRes, 5),
		"unscoped borrow": tbl.NewBorrow(hostRes, 5),
	} {
		var scope *task
		got, err := in.resolveArgHandles(h, borrow, &scope)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		lent := got.(uint32)
		if lent == h || scope == nil || scope.numBorrows != 1 {
			t.Fatalf("%s: got handle %d (host's %d), scope %+v; want a fresh scoped borrow", name, lent, h, scope)
		}
		if rep, err := tbl.Rep(hostRes, lent); err != nil || rep != 5 {
			t.Fatalf("%s: lent handle rep = %d, %v; want 5 under the host's tag", name, rep, err)
		}
		if err := tbl.Drop(hostRes, lent); err != nil || scope.numBorrows != 0 {
			t.Fatalf("%s: the callee's drop: %v, %d borrows left", name, err, scope.numBorrows)
		}
		if rep, err := tbl.Rep(hostRes, h); err != nil || rep != 5 {
			t.Fatalf("%s: the host's handle after the call: %d, %v", name, rep, err)
		}
	}

	outer := &task{inst: in}
	scoped := tbl.NewBorrowScoped(hostRes, 5, outer)
	var scope *task
	got, err := in.resolveArgHandles(scoped, borrow, &scope)
	if err != nil || got != scoped || scope != nil {
		t.Fatalf("a scoped borrow: got %v, %v, scope %v; want it passed through", got, err, scope)
	}

	if _, err := in.resolveArgHandles("x", borrow, &scope); err == nil {
		t.Fatal("a non-handle borrow arg was accepted")
	}
	if _, err := in.resolveArgHandles(uint32(999), borrow, &scope); err == nil {
		t.Fatal("an unknown handle was lent")
	}
}

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
		sched:           &sched{},
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
		{"list", binary.ListDesc{Element: borrowRef}, []abi.Value{borrowH, borrowH}, []abi.Value{rep, rep}},
		{"fixed-length list", binary.FixedListDesc{Element: borrowRef, Length: 2}, []abi.Value{borrowH, borrowH}, []abi.Value{rep, rep}},
		{"map", binary.MapDesc{Key: u32Ref, Value: borrowRef}, []abi.Value{[]abi.Value{uint32(1), borrowH}}, []abi.Value{[]abi.Value{uint32(1), rep}}},
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
			var scope *task
			got, err := in.resolveArgHandles(tc.in, tc.t, &scope)
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
		binary.FixedListDesc{Element: borrowRef, Length: 3},
		binary.MapDesc{Key: u32Ref, Value: borrowRef},
		binary.MapDesc{Key: borrowRef, Value: u32Ref},
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
		binary.MapDesc{Key: u32Ref, Value: u32Ref},
		binary.FixedListDesc{Element: u32Ref, Length: 3},
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
	in := &Instance{sched: &sched{}, resources: tbl}
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

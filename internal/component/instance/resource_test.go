package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

//go:embed testdata/resource_roundtrip.wasm
var resourceRoundtripWasm []byte

// ------- handleTable unit tests -------

func TestHandleTable_NewOwnRepDrop(t *testing.T) {
	tbl := newHandleTable()
	h := tbl.NewOwn(5, 42)

	rep, err := tbl.Rep(5, h)
	if err != nil {
		t.Fatalf("Rep: %v", err)
	}
	if rep != 42 {
		t.Fatalf("Rep = %d, want 42", rep)
	}

	if err := tbl.Drop(5, h); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	if _, err := tbl.Rep(5, h); err == nil {
		t.Fatal("expected an error reading the rep of a dropped handle")
	}
}

func TestHandleTable_HandleNumberingStartsAtOne(t *testing.T) {
	tbl := newHandleTable()
	if h := tbl.NewOwn(0, 1); h != 1 {
		t.Fatalf("first handle = %d, want 1", h)
	}
	if h := tbl.NewOwn(0, 2); h != 2 {
		t.Fatalf("second handle = %d, want 2", h)
	}
}

// TestHandleTable_FreeListReuse: a dropped handle's index is reclaimed by the
// next allocation before the counter grows, matching the reference Table's free
// list. A guest may rely on this dense numbering (e.g. component-model
// wasmtime/resources.wast asserts resource.new returns 1 again after a drop).
func TestHandleTable_FreeListReuse(t *testing.T) {
	tbl := newHandleTable()
	h1 := tbl.NewOwn(0, 10) // 1
	h2 := tbl.NewOwn(0, 20) // 2
	if h1 != 1 || h2 != 2 {
		t.Fatalf("handles = %d,%d want 1,2", h1, h2)
	}
	if err := tbl.Drop(0, h1); err != nil { // free 1
		t.Fatal(err)
	}
	if h := tbl.NewOwn(0, 30); h != 1 { // reuse 1
		t.Fatalf("after drop, next handle = %d, want reused 1", h)
	}
	if h := tbl.NewOwn(0, 40); h != 3 { // free list empty -> grow
		t.Fatalf("next fresh handle = %d, want 3", h)
	}
}

func TestHandleTable_DropAfterDropFails(t *testing.T) {
	tbl := newHandleTable()
	h := tbl.NewOwn(1, 1)
	if err := tbl.Drop(1, h); err != nil {
		t.Fatalf("first Drop: %v", err)
	}
	err := tbl.Drop(1, h)
	requireErrContains(t, err, "unknown handle")
}

func TestHandleTable_RepOfDroppedHandleFails(t *testing.T) {
	tbl := newHandleTable()
	h := tbl.NewOwn(1, 1)
	if err := tbl.Drop(1, h); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	_, err := tbl.Rep(1, h)
	requireErrContains(t, err, "unknown handle")
}

func TestHandleTable_UnknownHandle(t *testing.T) {
	tbl := newHandleTable()
	if _, err := tbl.Rep(1, 999); err == nil {
		t.Fatal("expected an error for an unknown handle")
	}
	requireErrContains(t, tbl.Drop(1, 999), "unknown handle")
	if _, err := tbl.TakeOwn(1, 999); err == nil {
		t.Fatal("expected an error for an unknown handle")
	}
}

func TestHandleTable_WrongResourceType(t *testing.T) {
	tbl := newHandleTable()
	h := tbl.NewOwn(5, 1)

	if _, err := tbl.Rep(6, h); err == nil {
		t.Fatal("expected an error reading a handle under the wrong resource type")
	} else {
		requireErrContains(t, err, "resource type")
	}
	if err := tbl.Drop(6, h); err == nil {
		t.Fatal("expected an error dropping a handle under the wrong resource type")
	} else {
		requireErrContains(t, err, "resource type")
	}

	// The handle is still valid under its real type: the failed operations
	// above must not have consumed or corrupted it.
	if rep, err := tbl.Rep(5, h); err != nil || rep != 1 {
		t.Fatalf("Rep(5, h) = (%d, %v), want (1, nil)", rep, err)
	}
}

func TestHandleTable_BorrowCannotBeDropped(t *testing.T) {
	tbl := newHandleTable()
	h := tbl.NewBorrow(1, 7)

	err := tbl.Drop(1, h)
	if err == nil {
		t.Fatal("expected an error dropping a borrow handle")
	}
	requireErrContains(t, err, "borrow")

	// The handle must still be readable: the rejected Drop must not have
	// removed it.
	rep, err := tbl.Rep(1, h)
	if err != nil || rep != 7 {
		t.Fatalf("Rep(1, h) after rejected Drop = (%d, %v), want (7, nil)", rep, err)
	}
}

func TestHandleTable_TakeOwn(t *testing.T) {
	tbl := newHandleTable()
	h := tbl.NewOwn(2, 9)

	rep, err := tbl.TakeOwn(2, h)
	if err != nil {
		t.Fatalf("TakeOwn: %v", err)
	}
	if rep != 9 {
		t.Fatalf("TakeOwn rep = %d, want 9", rep)
	}

	// TakeOwn consumes the handle, same as Drop.
	if _, err := tbl.Rep(2, h); err == nil {
		t.Fatal("expected an error reading a taken handle")
	}
}

func TestHandleTable_TakeOwnOfBorrowFails(t *testing.T) {
	tbl := newHandleTable()
	h := tbl.NewBorrow(2, 9)
	_, err := tbl.TakeOwn(2, h)
	if err == nil {
		t.Fatal("expected an error taking ownership of a borrow handle")
	}
	requireErrContains(t, err, "borrow")
}

func TestHandleTable_LendBlocksTakeOwnAndDrop(t *testing.T) {
	tbl := newHandleTable()
	h := tbl.NewOwn(3, 4)

	if err := tbl.Lend(3, h); err != nil {
		t.Fatalf("Lend: %v", err)
	}

	if _, err := tbl.TakeOwn(3, h); err == nil {
		t.Fatal("expected TakeOwn to fail while a lend is outstanding")
	} else {
		requireErrContains(t, err, "outstanding")
	}
	if err := tbl.Drop(3, h); err == nil {
		t.Fatal("expected Drop to fail while a lend is outstanding")
	} else {
		requireErrContains(t, err, "outstanding")
	}

	if err := tbl.Unlend(3, h); err != nil {
		t.Fatalf("Unlend: %v", err)
	}
	if err := tbl.Drop(3, h); err != nil {
		t.Fatalf("Drop after Unlend: %v", err)
	}
}

func TestHandleTable_UnlendWithoutLendFails(t *testing.T) {
	tbl := newHandleTable()
	h := tbl.NewOwn(1, 1)
	err := tbl.Unlend(1, h)
	if err == nil {
		t.Fatal("expected an error releasing a lend that was never taken")
	}
	requireErrContains(t, err, "no outstanding lends")
}

func TestHandleTable_LendUnlendUnknownHandle(t *testing.T) {
	tbl := newHandleTable()
	if err := tbl.Lend(1, 999); err == nil {
		t.Fatal("expected an error lending an unknown handle")
	}
	if err := tbl.Unlend(1, 999); err == nil {
		t.Fatal("expected an error unlending an unknown handle")
	}
}

// ------- own/borrow <-> rep translation unit tests -------

func TestResolveHandleArg_Own(t *testing.T) {
	tbl := newHandleTable()
	h := tbl.NewOwn(3, 77)

	v, err := resolveHandleArg(tbl, nil, binary.OwnDesc{ResourceType: 3}, h)
	if err != nil {
		t.Fatalf("resolveHandleArg: %v", err)
	}
	if v.(uint32) != 77 {
		t.Fatalf("resolved rep = %v, want 77", v)
	}
	// own consumes the handle.
	if _, err := tbl.Rep(3, h); err == nil {
		t.Fatal("expected the own handle to be consumed")
	}
}

func TestResolveHandleArg_Borrow(t *testing.T) {
	tbl := newHandleTable()
	h := tbl.NewOwn(3, 77)

	v, err := resolveHandleArg(tbl, nil, binary.BorrowDesc{ResourceType: 3}, h)
	if err != nil {
		t.Fatalf("resolveHandleArg: %v", err)
	}
	if v.(uint32) != 77 {
		t.Fatalf("resolved rep = %v, want 77", v)
	}
	// borrow does not consume the handle.
	if rep, err := tbl.Rep(3, h); err != nil || rep != 77 {
		t.Fatalf("Rep after borrow = (%d, %v), want (77, nil)", rep, err)
	}
}

func TestResolveHandleArg_WrongGoType(t *testing.T) {
	tbl := newHandleTable()
	if _, err := resolveHandleArg(tbl, nil, binary.OwnDesc{ResourceType: 1}, "not-a-handle"); err == nil {
		t.Fatal("expected an error for a non-uint32 own arg")
	}
	if _, err := resolveHandleArg(tbl, nil, binary.BorrowDesc{ResourceType: 1}, "not-a-handle"); err == nil {
		t.Fatal("expected an error for a non-uint32 borrow arg")
	}
}

func TestResolveHandleArg_PassThrough(t *testing.T) {
	tbl := newHandleTable()
	v, err := resolveHandleArg(tbl, nil, binary.PrimitiveDesc{Prim: "u32"}, uint32(5))
	if err != nil {
		t.Fatalf("resolveHandleArg: %v", err)
	}
	if v.(uint32) != 5 {
		t.Fatalf("v = %v, want 5 (pass-through)", v)
	}
}

func TestAllocHandleResult_Own(t *testing.T) {
	tbl := newHandleTable()
	v, err := allocHandleResult(tbl, binary.OwnDesc{ResourceType: 9}, uint32(123))
	if err != nil {
		t.Fatalf("allocHandleResult: %v", err)
	}
	h := v.(uint32)
	rep, err := tbl.Rep(9, h)
	if err != nil || rep != 123 {
		t.Fatalf("Rep(9, h) = (%d, %v), want (123, nil)", rep, err)
	}
}

func TestAllocHandleResult_Borrow(t *testing.T) {
	tbl := newHandleTable()
	v, err := allocHandleResult(tbl, binary.BorrowDesc{ResourceType: 9}, uint32(321))
	if err != nil {
		t.Fatalf("allocHandleResult: %v", err)
	}
	h := v.(uint32)
	rep, err := tbl.Rep(9, h)
	if err != nil || rep != 321 {
		t.Fatalf("Rep(9, h) = (%d, %v), want (321, nil)", rep, err)
	}
	// Handles minted this way are borrows: they cannot be dropped.
	requireErrContains(t, tbl.Drop(9, h), "borrow")
}

func TestAllocHandleResult_WrongGoType(t *testing.T) {
	tbl := newHandleTable()
	if _, err := allocHandleResult(tbl, binary.OwnDesc{ResourceType: 1}, "not-a-rep"); err == nil {
		t.Fatal("expected an error for a non-uint32 own result")
	}
	if _, err := allocHandleResult(tbl, binary.BorrowDesc{ResourceType: 1}, "not-a-rep"); err == nil {
		t.Fatal("expected an error for a non-uint32 borrow result")
	}
}

func TestAllocHandleResult_PassThrough(t *testing.T) {
	tbl := newHandleTable()
	v, err := allocHandleResult(tbl, binary.PrimitiveDesc{Prim: "u32"}, uint32(5))
	if err != nil {
		t.Fatalf("allocHandleResult: %v", err)
	}
	if v.(uint32) != 5 {
		t.Fatalf("v = %v, want 5 (pass-through)", v)
	}
}

// ------- own/borrow through the full liftHostArgs/lowerHostResults path -------
//
// These exercise the actual production entry points (as called from
// buildHostWrapper's GoModuleFunc), not just the resolveHandleArg /
// allocHandleResult helpers, proving the handle<->rep translation is really
// wired into the host-import call boundary.

func TestLiftHostArgs_OwnHandle(t *testing.T) {
	ctx, mod := memModule(t)
	_ = ctx
	tbl := newHandleTable()
	h := tbl.NewOwn(1, 42)

	fd, resolve := synthFuncDesc([]binary.TypeDesc{binary.OwnDesc{ResourceType: 1}}, nil)
	args, _, err := liftHostArgs(fd, resolve, []uint64{uint64(h)}, mod, tbl)
	if err != nil {
		t.Fatalf("liftHostArgs: %v", err)
	}
	if len(args) != 1 || args[0].(uint32) != 42 {
		t.Fatalf("args = %#v, want [42] (the rep, not the handle)", args)
	}
	// own consumes the handle.
	if _, err := tbl.Rep(1, h); err == nil {
		t.Fatal("expected the own handle to be consumed by liftHostArgs")
	}
}

func TestLiftHostArgs_BorrowHandle(t *testing.T) {
	_, mod := memModule(t)
	tbl := newHandleTable()
	h := tbl.NewOwn(1, 42)

	fd, resolve := synthFuncDesc([]binary.TypeDesc{binary.BorrowDesc{ResourceType: 1}}, nil)
	args, _, err := liftHostArgs(fd, resolve, []uint64{uint64(h)}, mod, tbl)
	if err != nil {
		t.Fatalf("liftHostArgs: %v", err)
	}
	if len(args) != 1 || args[0].(uint32) != 42 {
		t.Fatalf("args = %#v, want [42] (the rep, not the handle)", args)
	}
	// borrow does not consume the handle.
	if rep, err := tbl.Rep(1, h); err != nil || rep != 42 {
		t.Fatalf("Rep after borrow = (%d, %v), want (42, nil)", rep, err)
	}
}

func TestLiftHostArgs_UnknownHandle(t *testing.T) {
	_, mod := memModule(t)
	tbl := newHandleTable()
	fd, resolve := synthFuncDesc([]binary.TypeDesc{binary.OwnDesc{ResourceType: 1}}, nil)
	if _, _, err := liftHostArgs(fd, resolve, []uint64{999}, mod, tbl); err == nil {
		t.Fatal("expected an error lifting an unknown handle")
	}
}

func TestLowerHostResults_OwnHandle(t *testing.T) {
	ctx, mod := memModule(t)
	tbl := newHandleTable()
	fd, resolve := synthFuncDesc(nil, []binary.TypeDesc{binary.OwnDesc{ResourceType: 2}})

	stack := make([]uint64, 1)
	if err := lowerHostResults(ctx, fd, resolve, []abi.Value{uint32(55)}, stack, mod, tbl, -1, abi.Realloc{}); err != nil {
		t.Fatalf("lowerHostResults: %v", err)
	}
	h := uint32(stack[0])
	rep, err := tbl.Rep(2, h)
	if err != nil || rep != 55 {
		t.Fatalf("Rep(2, h) = (%d, %v), want (55, nil)", rep, err)
	}
}

func TestLowerHostResults_BorrowHandle(t *testing.T) {
	ctx, mod := memModule(t)
	tbl := newHandleTable()
	fd, resolve := synthFuncDesc(nil, []binary.TypeDesc{binary.BorrowDesc{ResourceType: 2}})

	stack := make([]uint64, 1)
	if err := lowerHostResults(ctx, fd, resolve, []abi.Value{uint32(66)}, stack, mod, tbl, -1, abi.Realloc{}); err != nil {
		t.Fatalf("lowerHostResults: %v", err)
	}
	h := uint32(stack[0])
	rep, err := tbl.Rep(2, h)
	if err != nil || rep != 66 {
		t.Fatalf("Rep(2, h) = (%d, %v), want (66, nil)", rep, err)
	}
	requireErrContains(t, tbl.Drop(2, h), "borrow")
}

// ------- resourceCanonHostFunc white-box validation -------

func TestResourceCanonHostFunc_TypeIdxOutOfRange(t *testing.T) {
	comp := &binary.Component{}
	_, err := resourceCanonHostFunc(comp, newConfig(nil), newHandleTable(), "new", binary.Canon{Kind: 0x02, TypeIdx: 5})
	requireErrContains(t, err, "out of range")
}

func TestResourceCanonHostFunc_NotAResourceType(t *testing.T) {
	comp := &binary.Component{Types: []binary.Type{{Descriptor: binary.PrimitiveDesc{Prim: "u32"}}}}
	_, err := resourceCanonHostFunc(comp, newConfig(nil), newHandleTable(), "new", binary.Canon{Kind: 0x02, TypeIdx: 0})
	requireErrContains(t, err, "not a resource type")
}

func TestResourceCanonHostFunc_UnsupportedKind(t *testing.T) {
	comp := &binary.Component{Types: []binary.Type{{Descriptor: binary.ResourceDesc{}}}}
	_, err := resourceCanonHostFunc(comp, newConfig(nil), newHandleTable(), "new", binary.Canon{Kind: 0x99, TypeIdx: 0})
	requireErrContains(t, err, "unsupported resource canon kind")
}

// ------- fixture round-trip: a real component that defines a resource and
// exercises resource.new/resource.rep/resource.drop from a guest core module -------

// TestResourceRoundtrip is the M4 STEP 1 milestone proof: a real component
// that declares its own resource type wires resource.new/resource.rep/
// resource.drop as host-implemented core funcs the guest module imports and
// calls; the guest's own "roundtrip" export (new -> rep -> drop -> return the
// rep it read back) proves the whole path works end to end.
func TestResourceRoundtrip(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, resourceRoundtripWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "roundtrip", uint32(99))
	if err != nil {
		t.Fatalf("Call roundtrip: %v", err)
	}
	if len(results) != 1 || results[0].(uint32) != 99 {
		t.Fatalf("roundtrip(99) = %#v, want [99]", results)
	}
}

// TestResourceRoundtrip_Steps drives new/rep/drop individually (rather than
// through the all-in-one "roundtrip" export) to check the handle a guest
// receives from resource.new is a real, usable, independent handle.
func TestResourceRoundtrip_Steps(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, resourceRoundtripWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	newResults, err := inst.Call(ctx, "new", uint32(7))
	if err != nil {
		t.Fatalf("Call new: %v", err)
	}
	handle := newResults[0].(uint32)
	if handle == 0 {
		t.Fatal("expected a non-zero handle from resource.new")
	}

	repResults, err := inst.Call(ctx, "rep", handle)
	if err != nil {
		t.Fatalf("Call rep: %v", err)
	}
	if repResults[0].(uint32) != 7 {
		t.Fatalf("rep(handle) = %v, want 7", repResults[0])
	}

	if _, err := inst.Call(ctx, "drop", handle); err != nil {
		t.Fatalf("Call drop: %v", err)
	}
}

// TestResourceRoundtrip_DropAfterDropFails proves a dropped handle cannot be
// dropped again -- the error surfaces from the real guest call, through the
// core func's panic recovery, not a stubbed table.
func TestResourceRoundtrip_DropAfterDropFails(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, resourceRoundtripWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	newResults, err := inst.Call(ctx, "new", uint32(1))
	if err != nil {
		t.Fatalf("Call new: %v", err)
	}
	handle := newResults[0].(uint32)

	if _, err := inst.Call(ctx, "drop", handle); err != nil {
		t.Fatalf("first drop: %v", err)
	}
	_, err = inst.Call(ctx, "drop", handle)
	if err == nil {
		t.Fatal("expected the second drop of the same handle to fail")
	}
	requireErrContains(t, err, "unknown handle")
}

// TestResourceRoundtrip_RepOfDroppedHandleFails proves resource.rep on an
// already-dropped handle fails loud rather than returning a stale/zero rep.
func TestResourceRoundtrip_RepOfDroppedHandleFails(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, resourceRoundtripWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	newResults, err := inst.Call(ctx, "new", uint32(1))
	if err != nil {
		t.Fatalf("Call new: %v", err)
	}
	handle := newResults[0].(uint32)

	if _, err := inst.Call(ctx, "drop", handle); err != nil {
		t.Fatalf("drop: %v", err)
	}
	_, err = inst.Call(ctx, "rep", handle)
	if err == nil {
		t.Fatal("expected resource.rep of a dropped handle to fail")
	}
	requireErrContains(t, err, "unknown handle")
}

// TestResourceRoundtrip_UnknownHandle proves resource.rep/resource.drop on a
// handle the guest never received fails loud.
func TestResourceRoundtrip_UnknownHandle(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, resourceRoundtripWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "rep", uint32(99999)); err == nil {
		t.Fatal("expected resource.rep of an unknown handle to fail")
	}
	if _, err := inst.Call(ctx, "drop", uint32(99999)); err == nil {
		t.Fatal("expected resource.drop of an unknown handle to fail")
	}
}

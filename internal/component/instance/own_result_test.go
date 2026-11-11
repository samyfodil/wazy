package instance

import (
	"context"
	le "encoding/binary"
	"strconv"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// lift_own's checks on a handle an export returns, which the host then
// holds: a live own of the declared resource that nothing borrows.
func TestHandleTable_CheckOwn(t *testing.T) {
	tbl := newHandleTable()
	own := tbl.NewOwn(1, 10)
	lent := tbl.NewOwn(1, 11)
	if err := tbl.Lend(1, lent); err != nil {
		t.Fatal(err)
	}
	borrow := tbl.NewBorrowScoped(1, 12, &task{})
	ws := tbl.addEntry(&waitableSet{})
	same := func(tag uint32) uint32 { return tag }

	if err := tbl.checkOwn(own, 1, same); err != nil {
		t.Fatalf("a live own: %v", err)
	}
	for _, tc := range []struct {
		name string
		h    uint32
		want string
	}{
		{"unknown", 99, "unknown handle index 99"},
		{"not a resource", ws, "handle index " + strconv.FormatUint(uint64(ws), 10) + " used with the wrong type: it is not a resource handle"},
		{"other resource", own, "expected guest-defined resource but found a different guest-defined resource"},
		{"borrow", borrow, "is a borrow, where an own handle is required"},
		{"lent", lent, "cannot remove owned resource while borrowed"},
	} {
		want := uint32(1)
		if tc.name == "other resource" {
			want = 2
		}
		requireErrContains(t, tbl.checkOwn(tc.h, want, same), tc.want)
	}
}

// checkOwnResult finds an own<T> wherever an export's result type nests one,
// and leaves a host resource's handle, and anything else, alone.
func TestCheckOwnResult(t *testing.T) {
	idx := func(i uint32) binary.TypeRef { return binary.TypeRef{TypeIndex: &i} }
	ownRef := idx(0)
	types := map[uint32]binary.TypeDesc{
		0: binary.OwnDesc{ResourceType: 7},
		1: binary.OwnDesc{ResourceType: 8}, // a host resource
	}
	resolve := func(i uint32) binary.TypeDesc { return types[i] }
	in := &Instance{
		resources:       newHandleTable(),
		comp:            &binary.Component{},
		isGuestResource: func(rt uint32) bool { return rt == 7 },
	}
	good := in.resources.NewOwn(7, 1)
	wrong := in.resources.NewOwn(9, 2)
	hostRef := idx(1)

	for _, tc := range []struct {
		name string
		t    binary.TypeDesc
		v    abi.Value
		bad  bool
	}{
		{"own", types[0], good, false},
		{"own of the wrong resource", types[0], wrong, true},
		{"a host resource is not checked", types[1], wrong, false},
		{"not a handle", types[0], "x", false},
		{"list", binary.ListDesc{Element: ownRef}, []abi.Value{good, wrong}, true},
		{"record", binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: ownRef}, {Name: "b", Type: ownRef}}}, []abi.Value{good, wrong}, true},
		{"record with a missing field", binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: ownRef}, {Name: "b", Type: ownRef}}}, []abi.Value{good}, false},
		{"tuple", binary.TupleDesc{Elements: []binary.TypeRef{ownRef, hostRef}}, []abi.Value{wrong, wrong}, true},
		{"tuple of a host own", binary.TupleDesc{Elements: []binary.TypeRef{hostRef}}, []abi.Value{wrong}, false},
		{"option some", binary.OptionDesc{Element: ownRef}, abi.Some{Value: wrong}, true},
		{"option bare", binary.OptionDesc{Element: ownRef}, wrong, true},
		{"option none", binary.OptionDesc{Element: ownRef}, nil, false},
		{"result ok", binary.ResultDesc{Ok: &ownRef}, abi.ResultValue{Payload: wrong}, true},
		{"result err", binary.ResultDesc{Err: &ownRef}, abi.ResultValue{IsErr: true, Payload: wrong}, true},
		{"result with no payload type", binary.ResultDesc{Ok: &ownRef}, abi.ResultValue{IsErr: true, Payload: wrong}, false},
		{"result with no payload", binary.ResultDesc{Ok: &ownRef}, abi.ResultValue{}, false},
		{"variant", binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a", Type: &ownRef}}}, abi.VariantValue{Payload: wrong}, true},
		{"variant out of range", binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a", Type: &ownRef}}}, abi.VariantValue{Disc: 3, Payload: wrong}, false},
		{"primitive", binary.PrimitiveDesc{Prim: "u32"}, uint32(5), false},
	} {
		err := in.checkOwnResult(tc.v, tc.t, resolve, 0)
		if tc.bad != (err != nil) {
			t.Errorf("%s: %v, want an error: %v", tc.name, err, tc.bad)
		}
	}

	bad := idx(99)
	if err := in.checkOwnResult([]abi.Value{good}, binary.ListDesc{Element: bad}, func(uint32) binary.TypeDesc { return nil }, 0); err == nil {
		t.Error("an unresolvable element type passes")
	}
	if err := in.checkOwnResult(wrong, types[0], resolve, maxResourceWalkDepth+1); err != nil {
		t.Errorf("past the depth limit: %v", err)
	}
	if err := (&Instance{resources: newHandleTable()}).checkOwnResult(wrong, types[0], resolve, 0); err != nil {
		t.Errorf("an instance with no component: %v", err)
	}
}

// may_leave guards a lowered import and resource.new/drop: a post-return
// function calling one traps. resource.rep has no such check.
func TestMayLeaveGuardsImportsAndResources(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: false, resources: newHandleTable()}
	hi := &hostImport{fn: func(context.Context, []abi.Value) ([]abi.Value, error) { return nil, nil }}
	fn, _, _, err := buildHostWrapper(in, "i", "f", hi, in.resources, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requirePanicContains(t, `host import "i" "f": cannot leave component instance`, func() {
		fn.Call(context.Background(), nil, nil)
	})
	afn, _, _, err := buildAsyncHostWrapper(in, "i", "g", &hostImport{asyncFn: func(context.Context, []abi.Value, *AsyncCall) error { return nil }}, in.resources, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requirePanicContains(t, `async import "i" "g": cannot leave component instance`, func() {
		afn.Call(context.Background(), nil, []uint64{0})
	})

	comp := &binary.Component{}
	h := in.resources.NewOwn(7, 1)
	for _, tc := range []struct {
		kind byte
		name string
	}{{0x02, "resource.new"}, {0x03, "resource.drop"}} {
		def, err := resourceCanonHostFuncGraph(comp, newConfig(nil), in, in.resources, tc.name, binary.Canon{Kind: tc.kind, TypeIdx: 7})
		if err != nil {
			t.Fatal(err)
		}
		requirePanicContains(t, tc.name+": cannot leave component instance", func() {
			def.fn.Call(context.Background(), nil, []uint64{uint64(h)})
		})
	}
	def, err := resourceCanonHostFuncGraph(comp, newConfig(nil), in, in.resources, "rep", binary.Canon{Kind: 0x04, TypeIdx: 7})
	if err != nil {
		t.Fatal(err)
	}
	stack := []uint64{uint64(h)}
	def.fn.Call(context.Background(), nil, stack)
	if stack[0] != 1 {
		t.Fatalf("resource.rep = %d, want 1", stack[0])
	}
}

// A lift's memory option names the memory its values live in, which may be
// another module's than its core func's.
func TestBindFuncExportGraph_MemoryOption(t *testing.T) {
	_, funcMod := memModule(t)
	_, memMod := memModule(t)
	comp := &binary.Component{
		Types:  []binary.Type{{Descriptor: binary.FuncDesc{}}},
		Canons: []binary.Canon{{Kind: 0x00, TypeIdx: 0, Opts: []binary.CanonOpt{{Kind: 0x03, Idx: 4}, {Kind: 0x01}}}},
	}
	componentFunc := func(uint32) (bool, int, aliasTarget, error) { return true, 0, aliasTarget{}, nil }
	coreFuncTarget := func(int) (api.Module, string, error) { return funcMod, "main", nil }

	cfg := newConfig(nil)
	cfg.coreMem = func(idx int) (api.Module, error) {
		if idx != 4 {
			t.Fatalf("memory index %d, want 4", idx)
		}
		return memMod, nil
	}
	be, err := bindFuncExportGraph(comp, 0, componentFunc, coreFuncTarget, nil, "x", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if be.memMod != memMod || be.mod != funcMod || be.enc != abi.UTF16 {
		t.Fatalf("memMod %v mod %v enc %v, want the option's memory, the core func's module, utf16", be.memMod, be.mod, be.enc)
	}

	cfg.coreMem = func(int) (api.Module, error) { return nil, errBoom }
	_, err = bindFuncExportGraph(comp, 0, componentFunc, coreFuncTarget, nil, "x", cfg, nil)
	requireErrContains(t, err, `export "x": memory option: boom`)
}

// The shared-everything threads built-ins decode, but binding one says what
// it would need rather than failing as an unknown canon.
func TestComputeCanonHostFunc_SharedEverythingThreads(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	for _, kind := range []byte{binary.CanonKindThreadSpawnRef, binary.CanonKindThreadSpawnIndirect, binary.CanonKindThreadAvailableParallelism} {
		_, _, err := computeCanonHostFunc(ctx, r, &binary.Component{}, newConfig(nil), newHandleTable(), &Instance{sched: &sched{}}, binary.Canon{Kind: kind}, nil, nil, "e", nil, nil, nil)
		requireErrContains(t, err, binary.CanonKindName(kind)+", which needs the shared-everything threads proposal")
	}
}

// A destructor whose core func cannot be resolved does nothing.
func TestResourceDtor_Unresolved(t *testing.T) {
	dtor := resourceDtor(guestDtor{owner: &Instance{}, resolve: func() api.Function { return nil }})
	if err := dtor(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
}

// A zero-length buffer of a typed stream is still checked, so it needs a
// memory to be checked against.
func TestNewGuestBuffer_ZeroLengthNeedsMemory(t *testing.T) {
	_, err := newGuestBuffer(&Instance{}, nil, abi.Realloc{}, nil, binary.PrimitiveDesc{Prim: "u8"}, 1, 1, true, 0, 0)
	requireErrContains(t, err, "calling module has no memory")
}

// An own<T> in a result spilled to memory is checked like a flat one.
func TestLiftResult_SpilledOwn(t *testing.T) {
	_, mod := memModule(t)
	idx := func(i uint32) binary.TypeRef { return binary.TypeRef{TypeIndex: &i} }
	own, rec := idx(0), idx(1)
	types := map[uint32]binary.TypeDesc{
		0: binary.OwnDesc{ResourceType: 7},
		1: binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: own}, {Name: "b", Type: own}}},
	}
	resolve := func(i uint32) binary.TypeDesc { return types[i] }
	be := &boundExport{mod: mod, funcName: "main", fd: binary.FuncDesc{Results: binary.FuncResults{Unnamed: &rec}}}
	finalizeBoundExport(be, resolve, nil, nil, 0)
	in := &Instance{resources: newHandleTable(), comp: &binary.Component{}, isGuestResource: func(uint32) bool { return true }, resolve: resolve}
	good, wrong := in.resources.NewOwn(7, 1), in.resources.NewOwn(9, 2)

	mem, _ := memoryBytesOf(mod)
	put := func(a, b uint32) {
		le.LittleEndian.PutUint32(mem[64:], a)
		le.LittleEndian.PutUint32(mem[68:], b)
	}
	put(good, good)
	if _, err := in.liftResult(be, []uint64{64}, mem, true, "f"); err != nil {
		t.Fatalf("two good handles: %v", err)
	}
	put(good, wrong)
	_, err := in.liftResult(be, []uint64{64}, mem, true, "f")
	requireErrContains(t, err, `export "f" result: handle index 2 used with the wrong type`)
}

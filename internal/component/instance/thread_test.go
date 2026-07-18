package instance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/binary"
	"github.com/samyfodil/wazy/internal/internalapi"
)

// callThreadYield builds threadYieldHostFunc(in, canon) and invokes it,
// returning the i32 result (Cancelled: FALSE=0, TRUE=1).
func callThreadYield(in *Instance, canon binary.Canon) uint64 {
	def := threadYieldHostFunc(in, canon)
	stack := []uint64{0}
	def.fn.Call(context.Background(), nil, stack)
	return stack[0]
}

// TestThreadYieldHostFunc_BindSetsPromotionGate pins design §4.1/§5.4:
// binding thread.yield unconditionally sets syncTaskNeeded (so a plain sync
// lift that calls it still gets a syncImplicit task) and mayBlockSync (the
// promotion gate a bound-but-not-yet-called thread canon must widen, per
// §5.4 -- checked at BIND time, not call time, so this must hold even before
// the returned host func is ever invoked).
func TestThreadYieldHostFunc_BindSetsPromotionGate(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	if in.syncTaskNeeded || in.mayBlockSync {
		t.Fatal("BUG in test setup: flags already set before bind")
	}
	threadYieldHostFunc(in, binary.Canon{})
	if !in.syncTaskNeeded {
		t.Fatal("threadYieldHostFunc: syncTaskNeeded not set at bind time")
	}
	if !in.mayBlockSync {
		t.Fatal("threadYieldHostFunc: mayBlockSync not set at bind time")
	}
}

// TestThreadYieldHostFunc_MayLeaveFalsePanics: like every other async
// builtin, the may_leave prologue traps first, before any task/blocker
// resolution.
func TestThreadYieldHostFunc_MayLeaveFalsePanics(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: false}
	requirePanicContains(t, "may not be left", func() {
		callThreadYield(in, binary.Canon{})
	})
}

// TestThreadYieldHostFunc_TasklessSyncDoesNotTrap pins design §4.1's default
// arm / §11.2: a task-less synchronous context (t == nil -- e.g. a core
// module's instantiation-time start function) is allowed to call
// thread.yield, unlike thread.suspend/waitable-set.wait's "cannot block a
// synchronous task" trap. It runs one scheduler round and reports NOT
// cancelled (a task-less context can never observe a delivered
// cancellation).
func TestThreadYieldHostFunc_TasklessSyncDoesNotTrap(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	if got := callThreadYield(in, binary.Canon{}); got != 0 {
		t.Fatalf("thread.yield (task-less sync) = %d, want 0 (not cancelled)", got)
	}
}

// TestThreadYieldHostFunc_SyncImplicitDoesNotTrap: the syncImplicit task
// (invokeEntered's implicit task for a plain sync lift) hits the same
// yield-is-fine default arm as the task-less case -- this is exactly what
// trap-if-block-and-sync's "yield-is-fine" assertion needs (that suite
// itself stays skipped in Stage A pending thread.index/new-indirect/suspend
// binds, but the yield semantics it will exercise are pinned here directly).
func TestThreadYieldHostFunc_SyncImplicitDoesNotTrap(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{syncImplicit: true}}
	if got := callThreadYield(in, binary.Canon{}); got != 0 {
		t.Fatalf("thread.yield (syncImplicit task) = %d, want 0 (not cancelled)", got)
	}
}

// TestThreadYieldHostFunc_UnpromotedCallbackTask pins the middle arm: a
// non-syncImplicit task with no live taskBlocker (t.blocker() == nil --
// t.st and t.gt.seg both nil, i.e. a callback task's guest code on the
// driving goroutine, not currently inside a live segment). Unexercised by
// any Stage A .wast suite (design §4.1's comment: cancellable's promotion
// gate means a bound thread.yield always gets a promoted task in practice),
// but the reference never distinguishes this from the stackful case at
// Thread.yield_'s level, so it must behave the same: one scheduler round,
// not cancelled.
func TestThreadYieldHostFunc_UnpromotedCallbackTask(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{}}
	if got := callThreadYield(in, binary.Canon{}); got != 0 {
		t.Fatalf("thread.yield (unpromoted callback task) = %d, want 0 (not cancelled)", got)
	}
}

// TestThreadYieldHostFunc_UnpromotedCallbackTask_PendingCancelDelivers pins
// the middle arm's deliverPendingCancel prologue (design §4.1, mirroring
// wait_until's ~404): a PENDING_CANCEL task with cancel?=true yielding must
// flip to CANCEL_DELIVERED and report Cancelled.TRUE immediately, without
// running a scheduler round at all.
func TestThreadYieldHostFunc_UnpromotedCallbackTask_PendingCancelDelivers(t *testing.T) {
	tk := &task{state: taskPendingCancel}
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: tk}
	if got := callThreadYield(in, binary.Canon{Cancellable: true}); got != 1 {
		t.Fatalf("thread.yield (pending cancel, cancellable) = %d, want 1 (Cancelled.TRUE)", got)
	}
	if tk.state != taskCancelDelivered {
		t.Fatalf("task state = %v, want taskCancelDelivered", tk.state)
	}
}

// TestThreadYieldHostFunc_UnpromotedCallbackTask_NonCancellableIgnoresPendingCancel
// pins that a non-cancellable yield never consults cancelDeliverable/
// deliverPendingCancel -- a PENDING_CANCEL task yielding without cancel?=true
// just runs its scheduler round and returns NOT cancelled, staying
// PENDING_CANCEL (the pending cancellation is only observed at a
// cancellable suspension point).
func TestThreadYieldHostFunc_UnpromotedCallbackTask_NonCancellableIgnoresPendingCancel(t *testing.T) {
	tk := &task{state: taskPendingCancel}
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: tk}
	if got := callThreadYield(in, binary.Canon{Cancellable: false}); got != 0 {
		t.Fatalf("thread.yield (pending cancel, non-cancellable) = %d, want 0 (not cancelled)", got)
	}
	if tk.state != taskPendingCancel {
		t.Fatalf("task state = %v, want unchanged taskPendingCancel", tk.state)
	}
}

// ------- Stage C: threadTable -------

// TestThreadTable_ReservesIndexZero pins §4.3/§3's "index 0 reserved,
// mirroring the reference Table's `array = [None]`" convention: the first
// allocated slot must be index 1, never 0.
func TestThreadTable_ReservesIndexZero(t *testing.T) {
	var tt threadTable
	idx := tt.add("first")
	if idx == 0 {
		t.Fatalf("threadTable.add: first index = 0, want nonzero (index 0 is reserved)")
	}
	if got := tt.slots[idx]; got != "first" {
		t.Fatalf("threadTable.slots[%d] = %v, want %q", idx, got, "first")
	}
}

// TestThreadTable_RemoveThenReuse pins the free-list recycling discipline
// (§4.3's "Free-slot discipline still matches the reference Table").
func TestThreadTable_RemoveThenReuse(t *testing.T) {
	var tt threadTable
	idx1 := tt.add("a")
	tt.remove(idx1)
	if tt.slots[idx1] != nil {
		t.Fatalf("threadTable.slots[%d] after remove = %v, want nil", idx1, tt.slots[idx1])
	}
	idx2 := tt.add("b")
	if idx2 != idx1 {
		t.Fatalf("threadTable.add after remove: idx = %d, want recycled %d", idx2, idx1)
	}
}

// TestThreadTable_RemoveIndexZeroNoop and out-of-range removes are
// defensive no-ops (thread.go's doc: "Stage C never calls this" with an
// invalid index, but it must not panic if it ever does).
func TestThreadTable_RemoveIndexZeroNoop(t *testing.T) {
	var tt threadTable
	tt.add("x") // populates slots so index 0 truly exists as the reserved nil
	tt.remove(0)
	tt.remove(999) // out of range
	if len(tt.free) != 0 {
		t.Fatalf("threadTable.free = %v, want empty (both removes were no-ops)", tt.free)
	}
}

// ------- Stage C: thread.index -------

// callThreadIndex builds threadIndexHostFunc(in) and invokes it, returning
// the i32 result.
func callThreadIndex(in *Instance) uint64 {
	def := threadIndexHostFunc(in)
	stack := []uint64{0}
	def.fn.Call(context.Background(), nil, stack)
	return stack[0]
}

// TestThreadIndexHostFunc_BindSetsPromotionGate mirrors
// TestThreadYieldHostFunc_BindSetsPromotionGate for thread.index (§4.3/§12
// Stage C).
func TestThreadIndexHostFunc_BindSetsPromotionGate(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	threadIndexHostFunc(in)
	if !in.syncTaskNeeded {
		t.Fatal("threadIndexHostFunc: syncTaskNeeded not set at bind time")
	}
	if !in.mayBlockSync {
		t.Fatal("threadIndexHostFunc: mayBlockSync not set at bind time")
	}
}

func TestThreadIndexHostFunc_MayLeaveFalsePanics(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: false}
	requirePanicContains(t, "may not be left", func() {
		callThreadIndex(in)
	})
}

// TestThreadIndexHostFunc_NoActiveTaskPanics: unlike thread.yield, thread.
// index has no task-less default arm -- requireActiveTask traps (§4.3's
// "requireActiveTask(in, \"thread.index\")").
func TestThreadIndexHostFunc_NoActiveTaskPanics(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	requirePanicContains(t, "called outside an active async task", func() {
		callThreadIndex(in)
	})
}

// TestThreadIndexHostFunc_LazyAllocationStable pins §4.3's lazy
// implicit-slot allocation: the first call allocates a nonzero index and
// registers it on the task; every later call on the SAME task returns the
// identical index (no re-allocation).
func TestThreadIndexHostFunc_LazyAllocationStable(t *testing.T) {
	tk := &task{}
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: tk}
	first := callThreadIndex(in)
	if first == 0 {
		t.Fatalf("thread.index first call = 0, want nonzero (index 0 is reserved)")
	}
	if tk.implicitThreadIdx != uint32(first) {
		t.Fatalf("task.implicitThreadIdx = %d, want %d (the returned index)", tk.implicitThreadIdx, first)
	}
	second := callThreadIndex(in)
	if second != first {
		t.Fatalf("thread.index second call = %d, want %d (stable, no re-allocation)", second, first)
	}
	if len(in.threads.slots) != 2 { // [reserved-0, this-task's-slot]
		t.Fatalf("in.threads.slots = %v, want exactly 2 entries (index 0 reserved + 1 allocated)", in.threads.slots)
	}
}

// TestThreadIndexHostFunc_TwoTasksGetDistinctIndices: two different tasks
// each lazily registering thread.index must not collide.
func TestThreadIndexHostFunc_TwoTasksGetDistinctIndices(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	t1, t2 := &task{}, &task{}
	in.activeTask = t1
	i1 := callThreadIndex(in)
	in.activeTask = t2
	i2 := callThreadIndex(in)
	if i1 == i2 {
		t.Fatalf("two distinct tasks both got thread.index = %d, want distinct indices", i1)
	}
}

// ------- Stage C: thread.suspend -------

// callThreadSuspend builds threadSuspendHostFunc(in, canon) and invokes it.
func callThreadSuspend(in *Instance, canon binary.Canon) uint64 {
	def := threadSuspendHostFunc(in, canon)
	stack := []uint64{0}
	def.fn.Call(context.Background(), nil, stack)
	return stack[0]
}

func TestThreadSuspendHostFunc_BindSetsPromotionGate(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	threadSuspendHostFunc(in, binary.Canon{})
	if !in.syncTaskNeeded {
		t.Fatal("threadSuspendHostFunc: syncTaskNeeded not set at bind time")
	}
	if !in.mayBlockSync {
		t.Fatal("threadSuspendHostFunc: mayBlockSync not set at bind time")
	}
}

func TestThreadSuspendHostFunc_MayLeaveFalsePanics(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: false}
	requirePanicContains(t, "may not be left", func() {
		callThreadSuspend(in, binary.Canon{})
	})
}

// TestThreadSuspendHostFunc_TaskLessTraps pins §4.2/§8.3's exercised path:
// a task-less context (t == nil, e.g. a core module's instantiation-time
// start function) hits blockingTask's sync-task-block trap -- the exact
// substring trap-if-block-and-sync's trap-if-suspend asserts (wast:296).
func TestThreadSuspendHostFunc_TaskLessTraps(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	requirePanicContains(t, "cannot block a synchronous task before returning", func() {
		callThreadSuspend(in, binary.Canon{})
	})
}

// TestThreadSuspendHostFunc_SyncImplicitTraps: the syncImplicit task (a
// plain sync lift's implicit task, invokeEntered) hits the identical trap --
// this is EXACTLY trap-if-block-and-sync's trap-if-suspend shape (wast:242,
// a plain sync lift calling thread.suspend).
func TestThreadSuspendHostFunc_SyncImplicitTraps(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{syncImplicit: true}}
	requirePanicContains(t, "cannot block a synchronous task before returning", func() {
		callThreadSuspend(in, binary.Canon{})
	})
}

// TestThreadSuspendHostFunc_UnpromotedCallbackTaskFailsLoud pins §4.2's
// documented fail-loud arm: a non-syncImplicit task with no live blocker
// (t.blocker() == nil) cannot genuinely suspend-forever without deadlocking
// its own driver, so it fails loud rather than hanging. Unexercised by any
// suite; parity-only.
func TestThreadSuspendHostFunc_UnpromotedCallbackTaskFailsLoud(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{}}
	requirePanicContains(t, "not supported from an unpromoted callback task", func() {
		callThreadSuspend(in, binary.Canon{})
	})
}

// ------- Stage C: thread.new-indirect (bind path + fail-loud stub) -------

// tableModWasm is a minimal core module exporting a 2-element funcref table
// named "t" -- (module (table (export "t") 2 funcref)) -- hand-encoded
// (magic+version, table section, export section) rather than pulled in via
// a WAT toolchain, matching this package's existing shim.go convention of
// hand-encoding tiny mechanical core wasm binaries.
var tableModWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, // \0asm
	0x01, 0x00, 0x00, 0x00, // version 1
	0x04, 0x04, 0x01, 0x70, 0x00, 0x02, // table section: 1 table, funcref, min=2
	0x07, 0x05, 0x01, 0x01, 0x74, 0x01, 0x00, // export section: "t" -> table 0
}

// newTableTestModule instantiates tableModWasm and returns it as an
// api.Module, for use as threadNewIndirectHostFunc's coreTableTarget's
// resolved owner.
func newTableTestModule(t *testing.T) (context.Context, api.Module, func()) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	mod, err := r.Instantiate(ctx, tableModWasm)
	if err != nil {
		t.Fatalf("instantiate tableModWasm: %v", err)
	}
	return ctx, mod, func() { r.Close(ctx) }
}

// TestThreadNewIndirectHostFunc_BindThenNoActiveTaskPanics pins the bind/call
// split still holds in Stage D: bind succeeds (resolves the real table +
// validates the consumer's core import signature) independent of any active
// task, but the CALL body's first act is requireActiveTask (§4.4 "Call
// time") -- exactly the shape trap-if-block-and-sync's bind-but-never-call
// requirement needs (wast:131/140/145's call sites are commented out; those
// binds never reach a call at all).
func TestThreadNewIndirectHostFunc_BindThenNoActiveTaskPanics(t *testing.T) {
	_, mod, closeFn := newTableTestModule(t)
	defer closeFn()

	in := &Instance{sched: &sched{}, mayLeave: true}
	canon := binary.Canon{Kind: binary.CanonKindThreadNewIndirect, TableIdx: 0}
	neededTypes := map[string]map[string]coreFuncSig{
		"g": {"e": {params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}},
	}
	coreTableTarget := func(idx int) (api.Module, string, error) {
		if idx != 0 {
			return nil, "", context.DeadlineExceeded // any error; not exercised here
		}
		return mod, "t", nil
	}

	def, err := threadNewIndirectHostFunc(in, canon, neededTypes, "g", "e", coreTableTarget)
	if err != nil {
		t.Fatalf("threadNewIndirectHostFunc bind: %v", err)
	}
	if !in.syncTaskNeeded || !in.mayBlockSync {
		t.Fatal("threadNewIndirectHostFunc: syncTaskNeeded/mayBlockSync not set at bind time")
	}
	if len(def.params) != 2 || len(def.results) != 1 {
		t.Fatalf("def params/results = %v/%v, want the consumer's declared (i32,i32)->i32 shape", def.params, def.results)
	}
	requirePanicContains(t, "called outside an active async task", func() {
		def.fn.Call(context.Background(), nil, []uint64{0, 0})
	})
}

// TestThreadNewIndirectHostFunc_BadTableIndexFailsBind pins that a
// coreTableTarget resolution failure surfaces as a bind-time error, not a
// panic (mirroring every other computeCanonHostFunc case's error contract).
func TestThreadNewIndirectHostFunc_BadTableIndexFailsBind(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	canon := binary.Canon{Kind: binary.CanonKindThreadNewIndirect, TableIdx: 5}
	coreTableTarget := func(int) (api.Module, string, error) {
		return nil, "", fmt.Errorf("test: table index out of range")
	}
	_, err := threadNewIndirectHostFunc(in, canon, nil, "g", "e", coreTableTarget)
	if err == nil {
		t.Fatal("expected a bind-time error for an unresolvable table index")
	}
}

// TestThreadNewIndirectHostFunc_WrongSignatureFailsBind pins §4.4 step 3's
// signature validation: a consumer signature that isn't one of the two
// legal (i32,i32)->i32 / (i32,i64)->i32 shapes fails bind loudly.
func TestThreadNewIndirectHostFunc_WrongSignatureFailsBind(t *testing.T) {
	_, mod, closeFn := newTableTestModule(t)
	defer closeFn()

	in := &Instance{sched: &sched{}, mayLeave: true}
	canon := binary.Canon{Kind: binary.CanonKindThreadNewIndirect, TableIdx: 0}
	neededTypes := map[string]map[string]coreFuncSig{
		"g": {"e": {params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}}, // wrong arity
	}
	coreTableTarget := func(int) (api.Module, string, error) { return mod, "t", nil }
	_, err := threadNewIndirectHostFunc(in, canon, neededTypes, "g", "e", coreTableTarget)
	if err == nil {
		t.Fatal("expected a bind-time error for a non-legal-shape consumer signature")
	}
}

// TestThreadNewIndirectHostFunc_MissingSignatureFailsBind pins the
// no-consumer-declares-this-module-field case (mirroring the lower canon's
// identical neededTypes lookup failure, graph.go's computeCanonHostFunc).
func TestThreadNewIndirectHostFunc_MissingSignatureFailsBind(t *testing.T) {
	_, mod, closeFn := newTableTestModule(t)
	defer closeFn()

	in := &Instance{sched: &sched{}, mayLeave: true}
	canon := binary.Canon{Kind: binary.CanonKindThreadNewIndirect, TableIdx: 0}
	coreTableTarget := func(int) (api.Module, string, error) { return mod, "t", nil }
	_, err := threadNewIndirectHostFunc(in, canon, nil, "g", "e", coreTableTarget)
	if err == nil {
		t.Fatal("expected a bind-time error when neededTypes has no entry for this group/entry")
	}
}

// ======================================================================
// Stage D: guestThread runtime (design §3/§4.4/§4.5/§5.2/§6.4, §12 Stage D)
// ======================================================================

// threadFuncTableModWasm is a minimal hand-encoded core module -- magic+
// version, type/function/table/export/elem/code sections, matching this
// package's existing shim.go/tableModWasm convention of hand-encoding tiny
// mechanical core wasm binaries rather than pulling in a WAT toolchain --
// exporting a 4-element funcref table "t" populated (via an active elem
// segment) with three real funcs real thread.new-indirect calls can resolve
// and CallWithStack against:
//
//	(module
//	  (type $okty (func (param i32)))
//	  (type $wrongty (func (param i64)))
//	  (func $ok (type $okty))                      ;; index 0: (i32)->(), returns normally
//	  (func $trap (type $okty) unreachable)         ;; index 1: (i32)->(), always traps
//	  (func $wrongtype (type $wrongty))             ;; index 2: (i64)->(), WRONG shape for ft=(i32)->()
//	  ;; index 3: left null (no elem entry) -- an in-bounds-but-empty slot
//	  (table (export "t") 4 funcref)
//	  (elem (i32.const 0) $ok $trap $wrongtype)
//	)
var threadFuncTableModWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x09, 0x02, 0x60,
	0x01, 0x7f, 0x00, 0x60, 0x01, 0x7e, 0x00, 0x03, 0x04, 0x03, 0x00, 0x00,
	0x01, 0x04, 0x04, 0x01, 0x70, 0x00, 0x04, 0x07, 0x05, 0x01, 0x01, 0x74,
	0x01, 0x00, 0x09, 0x09, 0x01, 0x00, 0x41, 0x00, 0x0b, 0x03, 0x00, 0x01,
	0x02, 0x0a, 0x0b, 0x03, 0x02, 0x00, 0x0b, 0x03, 0x00, 0x00, 0x0b, 0x02,
	0x00, 0x0b,
}

// newThreadFuncTableTestModule instantiates threadFuncTableModWasm, mirroring
// newTableTestModule but with real callable table entries.
func newThreadFuncTableTestModule(t *testing.T) (context.Context, api.Module, func()) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	mod, err := r.Instantiate(ctx, threadFuncTableModWasm)
	if err != nil {
		t.Fatalf("instantiate threadFuncTableModWasm: %v", err)
	}
	return ctx, mod, func() { r.Close(ctx) }
}

// bindThreadFuncTableNewIndirect binds threadNewIndirectHostFunc against
// newThreadFuncTableTestModule's table, with the consumer declaring the
// (i32,i32)->i32 legal shape (ft=(i32)->()).
func bindThreadFuncTableNewIndirect(t *testing.T, in *Instance, mod api.Module) hostFuncDef {
	t.Helper()
	canon := binary.Canon{Kind: binary.CanonKindThreadNewIndirect, TableIdx: 0}
	neededTypes := map[string]map[string]coreFuncSig{
		"g": {"e": {params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}},
	}
	coreTableTarget := func(int) (api.Module, string, error) { return mod, "t", nil }
	def, err := threadNewIndirectHostFunc(in, canon, neededTypes, "g", "e", coreTableTarget)
	if err != nil {
		t.Fatalf("threadNewIndirectHostFunc bind: %v", err)
	}
	return def
}

// TestThreadNewIndirectHostFunc_SpawnsSuspendedThread is §4.4's "Call time"
// happy path, end to end through the real bound host func + a real table:
// resolving fi=0 ($ok) registers a SUSPENDED guestThread (never spawns a
// goroutine -- §3's "a never-resumed wazy thread costs nothing").
func TestThreadNewIndirectHostFunc_SpawnsSuspendedThread(t *testing.T) {
	_, mod, closeFn := newThreadFuncTableTestModule(t)
	defer closeFn()

	tsk := &task{}
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: tsk}
	def := bindThreadFuncTableNewIndirect(t, in, mod)

	stack := []uint64{0, 42} // fi=0 ($ok), c=42
	def.fn.Call(context.Background(), nil, stack)
	idx := uint32(stack[0])
	if idx == 0 {
		t.Fatal("thread.new-indirect returned index 0 (reserved)")
	}
	th, ok := in.threads.getThread(idx)
	if !ok {
		t.Fatalf("in.threads.getThread(%d) not found", idx)
	}
	if !th.suspendedState() {
		t.Fatalf("th.suspendedState() = false, want true (§4.4 assert(new_thread.suspended()))")
	}
	if th.arg != 42 {
		t.Fatalf("th.arg = %d, want 42 (the c argument)", th.arg)
	}
	if th.spawned {
		t.Fatal("th.spawned = true, want false (goroutine spawn is lazy, §3)")
	}
	if tsk.liveThreads != 1 {
		t.Fatalf("tsk.liveThreads = %d, want 1", tsk.liveThreads)
	}
	if !in.sched.isParked(th) {
		t.Fatal("th not registered in sched.parked")
	}
}

// TestThreadNewIndirectHostFunc_OutOfRangeIndexTraps pins call_indirect
// semantics (§4.4's "trap edges: bad fi") for an index beyond the table's
// declared size.
func TestThreadNewIndirectHostFunc_OutOfRangeIndexTraps(t *testing.T) {
	_, mod, closeFn := newThreadFuncTableTestModule(t)
	defer closeFn()
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{}}
	def := bindThreadFuncTableNewIndirect(t, in, mod)
	requirePanicContains(t, "invalid table access", func() {
		def.fn.Call(context.Background(), nil, []uint64{4, 0}) // table has only 4 slots (0-3)
	})
}

// TestThreadNewIndirectHostFunc_NullEntryTraps pins the in-bounds-but-empty
// table slot (index 3, no elem segment ever populated it).
func TestThreadNewIndirectHostFunc_NullEntryTraps(t *testing.T) {
	_, mod, closeFn := newThreadFuncTableTestModule(t)
	defer closeFn()
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{}}
	def := bindThreadFuncTableNewIndirect(t, in, mod)
	requirePanicContains(t, "invalid table access", func() {
		def.fn.Call(context.Background(), nil, []uint64{3, 0})
	})
}

// TestThreadNewIndirectHostFunc_TypeMismatchTraps pins the reference's
// trap_if(f.t != ft) (§4.4's "Call time"): index 2 holds $wrongtype,
// (i64)->() -- the WRONG shape for this bind's ft=(i32)->().
func TestThreadNewIndirectHostFunc_TypeMismatchTraps(t *testing.T) {
	_, mod, closeFn := newThreadFuncTableTestModule(t)
	defer closeFn()
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{}}
	def := bindThreadFuncTableNewIndirect(t, in, mod)
	requirePanicContains(t, "indirect call type mismatch", func() {
		def.fn.Call(context.Background(), nil, []uint64{2, 0})
	})
}

// TestThreadNewIndirectHostFunc_MayLeaveFalsePanics: requireActiveTask's
// may_leave prologue traps before the table lookup.
func TestThreadNewIndirectHostFunc_MayLeaveFalsePanics(t *testing.T) {
	_, mod, closeFn := newThreadFuncTableTestModule(t)
	defer closeFn()
	in := &Instance{sched: &sched{}, mayLeave: false, activeTask: &task{}}
	def := bindThreadFuncTableNewIndirect(t, in, mod)
	requirePanicContains(t, "may not be left", func() {
		def.fn.Call(context.Background(), nil, []uint64{0, 0})
	})
}

// fakeThreadFunc is a minimal api.Function stand-in for a guestThread's `fn`
// (the resolved indirect-table func), letting guestThread's own machinery
// (spawn/block/resumeReady/run/reap) be unit-tested without compiling a real
// wasm module for every scenario -- only thread.new-indirect's own BIND/
// table-resolution semantics (tested above) need a real module.
// internalapi.WazyOnlyType satisfies api.Function's unexported marker
// method; instance is an internal package under the same module, so this is
// a legitimate (not third-party) implementation.
type fakeThreadFunc struct {
	internalapi.WazyOnlyType
	callFn func(ctx context.Context, stack []uint64) error
}

func (f *fakeThreadFunc) Definition() api.FunctionDefinition { return nil }

func (f *fakeThreadFunc) Call(ctx context.Context, params ...uint64) ([]uint64, error) {
	stack := append([]uint64(nil), params...)
	err := f.callFn(ctx, stack)
	return stack, err
}

// CallWithStack recovers a panic into a plain error -- mirroring the REAL
// wasm engine's CallWithStack contract (a host builtin's panic, e.g.
// errStackfulAbort from a reap-driven block() abort, is turned into a
// returned error by the engine's own recover around guest/host execution;
// see stackful_task.go's block/run doc). Without this, guestThread.run's
// th.fn.CallWithStack(...) line would never return on an aborted park --
// the panic would blow straight through run() to main()'s defer, skipping
// run()'s own unregister-thread tail, which the real engine never lets
// happen.
func (f *fakeThreadFunc) CallWithStack(ctx context.Context, stack []uint64) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	return f.callFn(ctx, stack)
}

// newSuspendedGuestThread hand-builds a SUSPENDED *guestThread registered in
// in.threads/in.sched.parked/t.liveThreads -- the exact state
// threadNewIndirectHostFunc's call body leaves behind (§4.4), but without
// needing a real wasm table/func -- mirrors newPromotedGuestTask's
// no-wasm-needed pattern for guestTask.
func newSuspendedGuestThread(in *Instance, t *task, callFn func(ctx context.Context, stack []uint64) error) *guestThread {
	th := &guestThread{t: t, in: in, ctx: context.Background(), fn: &fakeThreadFunc{callFn: callFn}}
	th.parked, th.parkReady = true, nil
	in.sched.park(th)
	th.index = in.threads.add(th)
	t.liveThreads++
	return th
}

// TestGuestThread_ReadySuspendedVsWaiting pins §3's ready()/suspendedState()
// distinction: a SUSPENDED thread (parkReady == nil) is never ready() --
// invisible to sched.step -- while a WAITING thread's ready() follows its
// predicate/cancel-wake exactly like stackfulTask.ready's sparkBlock arm.
func TestGuestThread_ReadySuspendedVsWaiting(t *testing.T) {
	in := newAsyncInst()
	tsk := &task{}
	th := newSuspendedGuestThread(in, tsk, nil)

	if th.ready() {
		t.Fatal("a SUSPENDED thread must never be ready()")
	}
	if !th.suspendedState() {
		t.Fatal("suspendedState() = false right after creation, want true")
	}

	ready := false
	th.parked, th.parkReady, th.cancellable = true, func() bool { return ready }, false
	if th.suspendedState() {
		t.Fatal("suspendedState() = true with a non-nil parkReady, want false (WAITING)")
	}
	if th.ready() {
		t.Fatal("ready() = true before the predicate is satisfied")
	}
	ready = true
	if !th.ready() {
		t.Fatal("ready() = false after the predicate is satisfied")
	}

	// A cancellable WAITING thread wakes on a deliverable pending cancel even
	// when its own predicate is false (cancelDeliverable() disjunct).
	th.parked, th.parkReady, th.cancellable = true, func() bool { return false }, true
	th.t.state = taskPendingCancel
	if !th.ready() {
		t.Fatal("ready() = false for a WAITING cancellable thread with a deliverable pending cancel, want true")
	}

	// But a SUSPENDED thread (parkReady == nil) never wakes this way -- only
	// an explicit thread.yield-then-resume (or reap) can resume it (§3's
	// "deliberately scoped tighter" doc; §11.5: spawned threads are never
	// cancellation candidates).
	th.parkReady = nil
	if th.ready() {
		t.Fatal("ready() = true for a SUSPENDED thread with a pending cancel, want false")
	}
}

// newSuspendedGuestThreadSelfBlocking is newSuspendedGuestThread's two-step
// variant for a callFn that needs to call back into th.block itself (th does
// not exist yet when callFn would otherwise need to close over it).
func newSuspendedGuestThreadSelfBlocking(in *Instance, t *task, callFn func(th *guestThread, ctx context.Context, stack []uint64) error) *guestThread {
	th := &guestThread{t: t, in: in, ctx: context.Background()}
	th.fn = &fakeThreadFunc{callFn: func(ctx context.Context, stack []uint64) error {
		return callFn(th, ctx, stack)
	}}
	th.parked, th.parkReady = true, nil
	in.sched.park(th)
	th.index = in.threads.add(th)
	t.liveThreads++
	return th
}

// TestGuestThread_BlockResumeRoundtrip mirrors
// TestPromotedGuestTask_BlockParkResumeAbort for guestThread: resuming a
// suspended thread spawns its goroutine lazily (goroutine count rises);
// blocking inside its guest code parks it WAITING; satisfying the predicate
// and resuming again runs it to completion (goroutine count returns to
// baseline) -- proves the whole baton (§3's core methods, verbatim-shaped
// after stackfulTask's).
func TestGuestThread_BlockResumeRoundtrip(t *testing.T) {
	in := newAsyncInst()
	tsk := &task{state: taskResolved} // resolved up front: avoids the last-thread-unresolved trap on exit
	ready := false
	blockResult := make(chan bool, 1)
	th := newSuspendedGuestThreadSelfBlocking(in, tsk, func(th *guestThread, ctx context.Context, stack []uint64) error {
		cancelled := th.block(func() bool { return ready }, false)
		blockResult <- cancelled
		return nil
	})

	before := numGoroutineStable(t)
	if !th.suspendedState() {
		t.Fatal("th not suspended before first resume")
	}
	if err := th.resumeReady(); err != nil {
		t.Fatalf("resumeReady (spawn): %v", err)
	}
	// th blocked inside its own guest code (ready=false): WAITING, live
	// goroutine, activeThread cleared (baton back with the test goroutine).
	if !in.sched.isParked(th) {
		t.Fatal("th not parked after blocking mid-call")
	}
	if th.suspendedState() {
		t.Fatal("th.suspendedState() = true after a real block(), want false (WAITING, not SUSPENDED)")
	}
	if in.activeThread != nil {
		t.Fatalf("in.activeThread = %v after th parked, want nil", in.activeThread)
	}
	select {
	case <-blockResult:
		t.Fatal("blockResult received a value before the predicate was ever satisfied")
	default:
	}

	during := numGoroutineStable(t)
	if during <= before {
		t.Fatalf("goroutine count = %d while th is parked mid-call, want > baseline %d", during, before)
	}

	ready = true
	if !th.ready() {
		t.Fatal("ready() = false after the predicate is satisfied")
	}
	if err := th.resumeReady(); err != nil {
		t.Fatalf("resumeReady (second): %v", err)
	}
	select {
	case cancelled := <-blockResult:
		if cancelled {
			t.Fatal("block() reported cancelled, want false (a plain ready-predicate resume)")
		}
	default:
		t.Fatal("th did not run to completion synchronously within the second resumeReady")
	}
	if in.sched.isParked(th) {
		t.Fatal("th should be unparked once its thread func returns")
	}
	if _, ok := in.threads.getThread(th.index); ok {
		t.Fatal("th's table slot should be freed once its thread func returns (unregister_thread)")
	}
	if tsk.liveThreads != 0 {
		t.Fatalf("tsk.liveThreads = %d, want 0 after the last thread exits", tsk.liveThreads)
	}
	if in.activeThread != nil {
		t.Fatalf("in.activeThread = %v after th finished, want nil", in.activeThread)
	}

	after := numGoroutineStable(t)
	if after > before {
		t.Fatalf("goroutine count after th finishes = %d, want <= baseline %d (goroutine leaked)", after, before)
	}
}

// TestGuestThread_ReapAbortsParkedSpawnedThread is the design's §6.4/§7
// no-goroutine-leak acceptance test for a spawned thread: reapParkedGoroutines
// must abort a WAITING guestThread's live goroutine, matching
// TestPromotedGuestTask_ReapAbortsParkedSegment/TestStackfulReap_* exactly.
func TestGuestThread_ReapAbortsParkedSpawnedThread(t *testing.T) {
	in := newAsyncInst()
	tsk := &task{state: taskStarted}
	blockResult := make(chan bool, 1)
	th := newSuspendedGuestThreadSelfBlocking(in, tsk, func(th *guestThread, ctx context.Context, stack []uint64) error {
		cancelled := th.block(func() bool { return false }, false) // never ready on its own
		blockResult <- cancelled
		return nil
	})

	before := numGoroutineStable(t)
	if err := th.resumeReady(); err != nil {
		t.Fatalf("resumeReady: %v", err)
	}
	during := numGoroutineStable(t)
	if during <= before {
		t.Fatalf("goroutine count = %d while th is parked, want > baseline %d", during, before)
	}

	in.reapParkedGoroutines()

	if in.sched.isParked(th) {
		t.Fatal("th should be unparked after reap")
	}
	if _, ok := in.threads.getThread(th.index); ok {
		t.Fatal("th's table slot should be freed after reap")
	}
	if tsk.liveThreads != 0 {
		t.Fatalf("tsk.liveThreads = %d after reap, want 0", tsk.liveThreads)
	}
	// block() panicked errStackfulAbort inside th's goroutine (recovered by
	// main(), never reaching the "cancelled <- blockResult" send below it) --
	// the closure's own send is unreachable on the abort path.
	select {
	case <-blockResult:
		t.Fatal("blockResult received a value -- th.block returned normally instead of panicking on abort")
	default:
	}

	after := numGoroutineStable(t)
	if after > before {
		t.Fatalf("goroutine count after reap = %d, want <= baseline %d (parked spawned thread's goroutine leaked)", after, before)
	}

	// Idempotent: reaping again (nothing left parked) must not hang.
	in.reapParkedGoroutines()
}

// TestGuestThread_ReapNeverSpawnedThread pins §6.4's "!spawned: nothing to
// unwind" reap arm: a thread.new-indirect thread that is registered but
// never once resumed has no goroutine at all -- reap must still free its
// table slot and decrement liveThreads, without ever touching a channel
// (which would hang forever on a truly unspawned thread).
func TestGuestThread_ReapNeverSpawnedThread(t *testing.T) {
	in := newAsyncInst()
	tsk := &task{state: taskStarted}
	th := newSuspendedGuestThread(in, tsk, nil) // never resumed -- callFn is never invoked

	before := numGoroutineStable(t)

	done := make(chan struct{})
	go func() {
		in.reapParkedGoroutines() // must return promptly, not hang
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reapParkedGoroutines hung reaping a never-spawned guestThread")
	}

	if th.spawned {
		t.Fatal("th.spawned = true, want false (BUG in test setup)")
	}
	if !th.done {
		t.Fatal("th.done = false after reap, want true")
	}
	if _, ok := in.threads.getThread(th.index); ok {
		t.Fatal("th's table slot should be freed after reap")
	}
	if tsk.liveThreads != 0 {
		t.Fatalf("tsk.liveThreads = %d after reap, want 0", tsk.liveThreads)
	}
	after := numGoroutineStable(t)
	if after > before {
		t.Fatalf("goroutine count after reaping a never-spawned thread = %d, want <= baseline %d", after, before)
	}
}

// TestGuestThread_RunLastThreadUnresolvedTraps pins the reference's
// unregister_thread last-thread trap (§3's run doc, definitions.py:521-523):
// the LAST spawned thread of a task exiting normally while the task itself
// is not yet RESOLVED is an error.
func TestGuestThread_RunLastThreadUnresolvedTraps(t *testing.T) {
	in := newAsyncInst()
	tsk := &task{state: taskStarted} // NOT resolved
	th := newSuspendedGuestThread(in, tsk, func(ctx context.Context, stack []uint64) error { return nil })

	if err := th.resumeReady(); err == nil {
		t.Fatal("expected an error: last thread of the task exited before the task was resolved")
	} else {
		requireErrContains(t, err, "last thread of the task exited before the task was resolved")
	}
	if tsk.liveThreads != 0 {
		t.Fatalf("tsk.liveThreads = %d, want 0 (unregister still happens on this error path)", tsk.liveThreads)
	}
	if _, ok := in.threads.getThread(th.index); ok {
		t.Fatal("th's table slot should still be freed even though run() returned an error")
	}
}

// TestGuestThread_RunGuestTrapPropagatesAndPoisons pins §3's run doc: a real
// guest trap inside the spawned thread's func poisons the instance (the same
// rule as stackfulTask.run) and surfaces wrapped with the thread's index.
func TestGuestThread_RunGuestTrapPropagatesAndPoisons(t *testing.T) {
	in := newAsyncInst()
	tsk := &task{state: taskResolved}
	trapErr := fmt.Errorf("boom")
	th := newSuspendedGuestThread(in, tsk, func(ctx context.Context, stack []uint64) error { return trapErr })

	err := th.resumeReady()
	if err == nil {
		t.Fatal("expected the guest trap to propagate out of resumeReady")
	}
	requireErrContains(t, err, "boom")
	if !in.poisoned {
		t.Fatal("in.poisoned = false after a spawned thread's guest code trapped, want true")
	}
	if tsk.liveThreads != 0 {
		t.Fatalf("tsk.liveThreads = %d, want 0 (unregister still happens on trap)", tsk.liveThreads)
	}
}

// ------- threadTable.getThread -------

func TestThreadTable_GetThread_ValidGuestThread(t *testing.T) {
	in := newAsyncInst()
	tsk := &task{}
	th := newSuspendedGuestThread(in, tsk, nil)
	got, ok := in.threads.getThread(th.index)
	if !ok || got != th {
		t.Fatalf("getThread(%d) = (%v, %v), want (%v, true)", th.index, got, ok, th)
	}
}

func TestThreadTable_GetThread_RejectsImplicitMarker(t *testing.T) {
	in := newAsyncInst()
	tsk := &task{}
	idx := in.threads.add(implicitThreadMarker{t: tsk})
	if _, ok := in.threads.getThread(idx); ok {
		t.Fatal("getThread on an implicitThreadMarker slot returned ok=true, want false")
	}
}

func TestThreadTable_GetThread_BadIndex(t *testing.T) {
	var tt threadTable
	if _, ok := tt.getThread(0); ok {
		t.Fatal("getThread(0) (reserved) returned ok=true, want false")
	}
	if _, ok := tt.getThread(999); ok {
		t.Fatal("getThread(999) (out of range) returned ok=true, want false")
	}
}

// ------- currentBlocker / blockingTask / activeBlocker seam (§5.2) -------

// TestInstance_CurrentBlocker_PrefersActiveThread is the load-bearing seam
// test: with a spawned guestThread active, currentBlocker must resolve to
// IT, not the task's own implicit-thread blocker -- the exact preference
// trap-if-sync-and-waitable-set's join-during-sync-* tests depend on
// (§5.2's doc: "parking it twice would corrupt the baton").
func TestInstance_CurrentBlocker_PrefersActiveThread(t *testing.T) {
	in := newAsyncInst()
	tsk, st := newBareStackfulTask(in)
	in.activeTask = tsk

	if got := in.currentBlocker(); got != st {
		t.Fatalf("currentBlocker() = %v, want the implicit thread %v when activeThread is nil", got, st)
	}

	th := &guestThread{t: tsk, in: in}
	in.activeThread = th
	if got := in.currentBlocker(); got != taskBlocker(th) {
		t.Fatalf("currentBlocker() = %v, want the active spawned thread %v", got, th)
	}
}

func TestBlockingTask_PrefersActiveThread(t *testing.T) {
	in := newAsyncInst()
	tsk, _ := newBareStackfulTask(in)
	in.activeTask = tsk
	th := &guestThread{t: tsk, in: in}
	in.activeThread = th

	_, blk := blockingTask(in, "test")
	if blk != taskBlocker(th) {
		t.Fatalf("blockingTask blk = %v, want the active spawned thread %v", blk, th)
	}
}

func TestActiveBlocker_PrefersActiveThread(t *testing.T) {
	in := newAsyncInst()
	tsk, _ := newBareStackfulTask(in)
	in.activeTask = tsk
	th := &guestThread{t: tsk, in: in}
	in.activeThread = th

	if blk := activeBlocker(in); blk != taskBlocker(th) {
		t.Fatalf("activeBlocker = %v, want the active spawned thread %v", blk, th)
	}
}

// ------- thread.yield-then-resume (0x2b) trap arms + happy path -------

func callThreadYieldThenResume(def hostFuncDef, i uint32) uint64 {
	stack := []uint64{uint64(i)}
	def.fn.Call(context.Background(), nil, stack)
	return stack[0]
}

func TestThreadYieldThenResumeHostFunc_BindSetsPromotionGate(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	threadYieldThenResumeHostFunc(in, binary.Canon{})
	if !in.syncTaskNeeded || !in.mayBlockSync {
		t.Fatal("threadYieldThenResumeHostFunc: syncTaskNeeded/mayBlockSync not set at bind time")
	}
}

func TestThreadYieldThenResumeHostFunc_MayLeaveFalsePanics(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: false}
	def := threadYieldThenResumeHostFunc(in, binary.Canon{})
	requirePanicContains(t, "may not be left", func() { callThreadYieldThenResume(def, 1) })
}

func TestThreadYieldThenResumeHostFunc_BadIndexTraps(t *testing.T) {
	in := newAsyncInst()
	def := threadYieldThenResumeHostFunc(in, binary.Canon{})
	requirePanicContains(t, "is not a live spawned thread", func() { callThreadYieldThenResume(def, 1) })
}

func TestThreadYieldThenResumeHostFunc_ImplicitMarkerIndexTraps(t *testing.T) {
	in := newAsyncInst()
	idx := in.threads.add(implicitThreadMarker{t: &task{}})
	def := threadYieldThenResumeHostFunc(in, binary.Canon{})
	requirePanicContains(t, "is not a live spawned thread", func() { callThreadYieldThenResume(def, idx) })
}

func TestThreadYieldThenResumeHostFunc_NotSuspendedTraps(t *testing.T) {
	in := newAsyncInst()
	tsk := &task{}
	th := newSuspendedGuestThread(in, tsk, nil)
	th.parked, th.parkReady = true, func() bool { return true } // WAITING, not SUSPENDED
	def := threadYieldThenResumeHostFunc(in, binary.Canon{})
	requirePanicContains(t, "is not suspended", func() { callThreadYieldThenResume(def, th.index) })
}

func TestThreadYieldThenResumeHostFunc_TaskLessTraps(t *testing.T) {
	in := newAsyncInst()
	tsk := &task{}
	th := newSuspendedGuestThread(in, tsk, nil)
	def := threadYieldThenResumeHostFunc(in, binary.Canon{})
	// in.activeTask is nil (task-less context) -- blockingTask's sync trap.
	requirePanicContains(t, "cannot block a synchronous task before returning", func() { callThreadYieldThenResume(def, th.index) })
}

func TestThreadYieldThenResumeHostFunc_SyncImplicitTraps(t *testing.T) {
	in := newAsyncInst()
	tsk := &task{syncImplicit: true}
	in.activeTask = tsk
	th := newSuspendedGuestThread(in, tsk, nil)
	def := threadYieldThenResumeHostFunc(in, binary.Canon{})
	requirePanicContains(t, "cannot block a synchronous task before returning", func() { callThreadYieldThenResume(def, th.index) })
}

func TestThreadYieldThenResumeHostFunc_UnpromotedCallbackTaskFailsLoud(t *testing.T) {
	in := newAsyncInst()
	tsk := &task{} // no st, no gt -- blocker() == nil
	in.activeTask = tsk
	th := newSuspendedGuestThread(in, tsk, nil)
	def := threadYieldThenResumeHostFunc(in, binary.Canon{})
	requirePanicContains(t, "not supported from an unpromoted callback task", func() { callThreadYieldThenResume(def, th.index) })
}

// TestThreadYieldThenResumeHostFunc_PendingCancelDeliversWithoutSwitching
// pins the deliver-pending-cancel prologue (§4.5): a PENDING_CANCEL
// cancellable caller must flip to CANCEL_DELIVERED and return Cancelled.TRUE
// WITHOUT ever touching `other` -- other stays untouched/suspended.
func TestThreadYieldThenResumeHostFunc_PendingCancelDeliversWithoutSwitching(t *testing.T) {
	in := newAsyncInst()
	tsk, _ := newBareStackfulTask(in)
	tsk.state = taskPendingCancel
	in.activeTask = tsk
	th := newSuspendedGuestThread(in, tsk, func(context.Context, []uint64) error {
		t.Fatal("other's thread func must not run when the caller's pending cancel delivers first")
		return nil
	})
	def := threadYieldThenResumeHostFunc(in, binary.Canon{Cancellable: true})
	if got := callThreadYieldThenResume(def, th.index); got != 1 {
		t.Fatalf("thread.yield-then-resume (pending cancel) = %d, want 1 (Cancelled.TRUE)", got)
	}
	if tsk.state != taskCancelDelivered {
		t.Fatalf("task state = %v, want taskCancelDelivered", tsk.state)
	}
	if !th.suspendedState() {
		t.Fatal("other must remain SUSPENDED (never resumed) when the caller's own pending cancel delivers first")
	}
}

// newBareStackfulTask builds a minimal (*task, *stackfulTask) pair with no
// wasm-backed boundExport -- driven via bareStackfulRun(st, fn) instead of
// st.firstRun()/st.run(), mirroring newPromotedGuestTask's no-wasm-needed
// pattern (guest_task_promoted_test.go) for guestTask.
func newBareStackfulTask(in *Instance) (*task, *stackfulTask) {
	tsk := &task{inst: in, state: taskStarted}
	st := &stackfulTask{t: tsk, in: in}
	tsk.st = st
	return tsk, st
}

// bareStackfulRun spawns st's goroutine running fn (instead of st.run()) and
// performs the first baton handoff -- the shape of stackfulTask.firstRun/
// main with the payload substituted, the way guestTask.runSegment lets tests
// substitute firstRunBody.
func bareStackfulRun(st *stackfulTask, fn func() error) error {
	st.resumeCh, st.yieldCh = make(chan resumeMode), make(chan struct{})
	st.spawned = true
	go func() {
		mode := <-st.resumeCh
		defer func() {
			if r := recover(); r != nil && r != any(errStackfulAbort) {
				st.panicVal = r
			}
			st.done = true
			st.yieldCh <- struct{}{}
		}()
		if mode == resumeAbort {
			return
		}
		st.err = fn()
	}()
	return st.handoff(resumeNormal)
}

// TestThreadYieldThenResumeHostFunc_SwitchAndSelfPark is the design's §4.5
// ordering-equivalence proof exercised end to end at the host-func level (the
// exact mechanism trap-if-sync-and-waitable-set's $spawn-and-yield uses,
// wast:69-72): $Main (a stackful implicit thread) switches directly to a
// spawned thread that runs to completion, then parks itself ready-waiting;
// resuming $Main later completes the call with Cancelled.FALSE.
func TestThreadYieldThenResumeHostFunc_SwitchAndSelfPark(t *testing.T) {
	in := newAsyncInst()
	tsk, st := newBareStackfulTask(in)
	in.activeTask = tsk

	ran := make(chan struct{})
	other := newSuspendedGuestThread(in, tsk, func(ctx context.Context, stack []uint64) error {
		close(ran)
		return nil
	})
	tsk.state = taskResolved // other becomes the last thread on exit; avoid the unresolved-task trap

	before := numGoroutineStable(t)

	def := threadYieldThenResumeHostFunc(in, binary.Canon{})
	selfParked := make(chan bool, 1)
	if err := bareStackfulRun(st, func() error {
		got := callThreadYieldThenResume(def, other.index)
		selfParked <- got == 1
		return nil
	}); err != nil {
		t.Fatalf("bareStackfulRun: %v", err)
	}

	select {
	case <-ran:
	default:
		t.Fatal("the switched-to thread's func never ran")
	}
	if _, ok := in.threads.getThread(other.index); ok {
		t.Fatal("other's table slot should be freed once its thread func returns")
	}
	if tsk.liveThreads != 0 {
		t.Fatalf("tsk.liveThreads = %d, want 0", tsk.liveThreads)
	}
	if !in.sched.isParked(st) {
		t.Fatal("st (the implicit thread) should be parked ready-waiting after the switch returns")
	}
	select {
	case <-selfParked:
		t.Fatal("the host func returned before st ever got a chance to self-park -- ordering violated")
	default:
	}

	// Resume st (simulating the driver's next step): the "yield" half
	// completes, Cancelled.FALSE.
	if err := st.resumeReady(); err != nil {
		t.Fatalf("st.resumeReady: %v", err)
	}
	select {
	case cancelled := <-selfParked:
		if cancelled {
			t.Fatal("thread.yield-then-resume reported cancelled, want Cancelled.FALSE")
		}
	default:
		t.Fatal("st did not resume to completion synchronously")
	}

	after := numGoroutineStable(t)
	if after > before {
		t.Fatalf("goroutine count after everything completes = %d, want <= baseline %d (leak)", after, before)
	}
}

package instance

import (
	"context"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/binary"
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

// TestThreadNewIndirectHostFunc_BindThenFailLoudStub is the unit test the
// design (§12 Stage C) explicitly calls for: bind succeeds (resolves the
// real table + validates the consumer's core import signature), but the
// returned hostFuncDef's CALL body panics naming Stage D -- exactly the
// shape trap-if-block-and-sync's bind-but-never-call requirement needs
// (wast:131/140/145's call sites are commented out).
func TestThreadNewIndirectHostFunc_BindThenFailLoudStub(t *testing.T) {
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
	requirePanicContains(t, "execution lands in Stage D", func() {
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

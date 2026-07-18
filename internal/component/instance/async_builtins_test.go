package instance

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

func requirePanicContains(t *testing.T, substr string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic containing %q, got none", substr)
		}
		err, ok := r.(error)
		if !ok {
			t.Fatalf("expected a panic with an error value, got %T: %v", r, r)
		}
		requireErrContains(t, err, substr)
	}()
	fn()
}

func TestContextValType(t *testing.T) {
	if vt, err := contextValType(0x7f); err != nil || vt != api.ValueTypeI32 {
		t.Fatalf("contextValType(i32) = (%v, %v), want (ValueTypeI32, nil)", vt, err)
	}
	if vt, err := contextValType(0x7e); err != nil || vt != api.ValueTypeI64 {
		t.Fatalf("contextValType(i64) = (%v, %v), want (ValueTypeI64, nil)", vt, err)
	}
	if _, err := contextValType(0x7d); err == nil {
		t.Fatal("contextValType(f32-ish byte): expected an error, got nil")
	}
}

func TestContextGetSetHostFuncGraph_BadCoreValType(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	if _, err := contextGetHostFuncGraph(in, binary.Canon{CoreValType: 0x7d}); err == nil {
		t.Fatal("contextGetHostFuncGraph: expected an error for a non-i32/i64 core valtype")
	}
	if _, err := contextSetHostFuncGraph(in, binary.Canon{CoreValType: 0x7d}); err == nil {
		t.Fatal("contextSetHostFuncGraph: expected an error for a non-i32/i64 core valtype")
	}
}

// TestContextGetSetRoundTrip exercises context.get/set directly (no wasm
// needed -- these are ordinary Go host funcs closing over *Instance),
// pinning the "current task" wiring: set writes task.ctxStorage[slot], get
// reads it back.
func TestContextGetSetRoundTrip(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	tk := &task{}
	in.activeTask = tk

	setDef, err := contextSetHostFuncGraph(in, binary.Canon{CoreValType: 0x7f, Slot: 1})
	if err != nil {
		t.Fatalf("contextSetHostFuncGraph: %v", err)
	}
	getDef, err := contextGetHostFuncGraph(in, binary.Canon{CoreValType: 0x7f, Slot: 1})
	if err != nil {
		t.Fatalf("contextGetHostFuncGraph: %v", err)
	}

	setStack := []uint64{0xCAFE}
	setDef.fn.Call(context.Background(), nil, setStack)
	if tk.ctxStorage[1] != 0xCAFE {
		t.Fatalf("ctxStorage[1] = %#x, want 0xCAFE", tk.ctxStorage[1])
	}

	getStack := []uint64{0}
	getDef.fn.Call(context.Background(), nil, getStack)
	if getStack[0] != 0xCAFE {
		t.Fatalf("context.get returned %#x, want 0xCAFE", getStack[0])
	}

	// Slot 0 was never set: still zero.
	getDef0, err := contextGetHostFuncGraph(in, binary.Canon{CoreValType: 0x7f, Slot: 0})
	if err != nil {
		t.Fatalf("contextGetHostFuncGraph(slot 0): %v", err)
	}
	stack0 := []uint64{0xFFFF}
	getDef0.fn.Call(context.Background(), nil, stack0)
	if stack0[0] != 0 {
		t.Fatalf("context.get(slot 0) = %#x, want 0", stack0[0])
	}
}

func TestContextGetSetSlotOutOfRange(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{}}
	getDef, err := contextGetHostFuncGraph(in, binary.Canon{CoreValType: 0x7f, Slot: 5})
	if err != nil {
		t.Fatalf("contextGetHostFuncGraph: %v", err)
	}
	requirePanicContains(t, "slot 5 out of range", func() {
		getDef.fn.Call(context.Background(), nil, []uint64{0})
	})

	setDef, err := contextSetHostFuncGraph(in, binary.Canon{CoreValType: 0x7f, Slot: 5})
	if err != nil {
		t.Fatalf("contextSetHostFuncGraph: %v", err)
	}
	requirePanicContains(t, "slot 5 out of range", func() {
		setDef.fn.Call(context.Background(), nil, []uint64{0})
	})
}

func TestRequireActiveTask_NoActiveTaskPanics(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true}
	requirePanicContains(t, "called outside an active async task", func() {
		requireActiveTask(in, "task.return")
	})
}

func TestRequireActiveTask_MayLeaveFalsePanics(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: false, activeTask: &task{}}
	requirePanicContains(t, "may not be left", func() {
		requireActiveTask(in, "task.return")
	})
}

func TestBackpressureIncDec(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{}}
	inc := backpressureIncHostFuncGraph(in)
	dec := backpressureDecHostFuncGraph(in)

	inc.fn.Call(context.Background(), nil, nil)
	if in.backpressure != 1 {
		t.Fatalf("backpressure = %d, want 1", in.backpressure)
	}
	dec.fn.Call(context.Background(), nil, nil)
	if in.backpressure != 0 {
		t.Fatalf("backpressure = %d, want 0", in.backpressure)
	}
}

func TestBackpressureDecUnderflowPanics(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{}}
	dec := backpressureDecHostFuncGraph(in)
	requirePanicContains(t, "underflow", func() {
		dec.fn.Call(context.Background(), nil, nil)
	})
}

func TestBackpressureIncOverflowPanics(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, activeTask: &task{}, backpressure: 1<<16 - 1}
	inc := backpressureIncHostFuncGraph(in)
	requirePanicContains(t, "overflow", func() {
		inc.fn.Call(context.Background(), nil, nil)
	})
}

// ------- task.return -------

func u32TypeIdxResolver(t binary.TypeDesc) abi.Resolver {
	return func(uint32) binary.TypeDesc { return t }
}

func TestTaskReturnHostFuncGraph_NoResult(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: true, resolve: u32TypeIdxResolver(nil)}
	def, err := taskReturnHostFuncGraph(in, binary.Canon{})
	if err != nil {
		t.Fatalf("taskReturnHostFuncGraph: %v", err)
	}
	if len(def.params) != 0 {
		t.Fatalf("params = %v, want none", def.params)
	}

	var got []abi.Value
	tk := &task{onResolve: func(vals []abi.Value, _ bool) { got = vals }}
	in.activeTask = tk
	def.fn.Call(context.Background(), nil, nil)
	if tk.state != taskResolved {
		t.Fatalf("task.state = %v, want taskResolved", tk.state)
	}
	if got != nil {
		t.Fatalf("result = %v, want nil (no-result task.return)", got)
	}
}

func TestTaskReturnHostFuncGraph_U32Result(t *testing.T) {
	u32Ref := binary.TypeRef{Primitive: "u32"}
	canon := binary.Canon{TaskReturnResult: binary.FuncResults{Unnamed: &u32Ref}}
	in := &Instance{sched: &sched{}, mayLeave: true, resolve: u32TypeIdxResolver(nil)}
	def, err := taskReturnHostFuncGraph(in, canon)
	if err != nil {
		t.Fatalf("taskReturnHostFuncGraph: %v", err)
	}
	if len(def.params) != 1 || def.params[0] != api.ValueTypeI32 {
		t.Fatalf("params = %v, want [i32]", def.params)
	}

	var got []abi.Value
	tk := &task{
		be:        &boundExport{resultType: binary.PrimitiveDesc{Prim: "u32"}},
		onResolve: func(vals []abi.Value, _ bool) { got = vals },
	}
	in.activeTask = tk
	def.fn.Call(context.Background(), nil, []uint64{42})
	if len(got) != 1 || got[0].(uint32) != 42 {
		t.Fatalf("result = %v, want [42]", got)
	}

	// A second task.return on the same (now-resolved) task traps.
	requirePanicContains(t, "already resolved", func() {
		def.fn.Call(context.Background(), nil, []uint64{7})
	})
}

func TestTaskReturnHostFuncGraph_ResultTypeMismatchPanics(t *testing.T) {
	u32Ref := binary.TypeRef{Primitive: "u32"}
	canon := binary.Canon{TaskReturnResult: binary.FuncResults{Unnamed: &u32Ref}}
	in := &Instance{sched: &sched{}, mayLeave: true, resolve: u32TypeIdxResolver(nil)}
	def, err := taskReturnHostFuncGraph(in, canon)
	if err != nil {
		t.Fatalf("taskReturnHostFuncGraph: %v", err)
	}
	// The active task's export declares a DIFFERENT result type (s32) than
	// this task.return's own (u32) -- trap_if(result_type != task.ft.result).
	tk := &task{be: &boundExport{resultType: binary.PrimitiveDesc{Prim: "s32"}}}
	in.activeTask = tk
	requirePanicContains(t, "does not match", func() {
		def.fn.Call(context.Background(), nil, []uint64{1})
	})
}

func TestTaskReturnHostFuncGraph_MultipleNamedResultsError(t *testing.T) {
	canon := binary.Canon{TaskReturnResult: binary.FuncResults{Named: []binary.FuncResult{
		{Name: "a", Type: binary.TypeRef{Primitive: "u32"}},
		{Name: "b", Type: binary.TypeRef{Primitive: "u32"}},
	}}}
	in := &Instance{sched: &sched{}, resolve: u32TypeIdxResolver(nil)}
	_, err := taskReturnHostFuncGraph(in, canon)
	requireErrContains(t, err, "multiple task.return results")
}

func TestTaskReturnHostFuncGraph_FlattenError(t *testing.T) {
	ref := binary.TypeRef{TypeIndex: idxPtr(0)}
	canon := binary.Canon{TaskReturnResult: binary.FuncResults{Unnamed: &ref}}
	// resolveTypeRef succeeds (returns a FuncDesc), but abi.Flatten rejects a
	// FuncDesc outright ("cannot flatten unsupported type") -- a result type
	// task.return could never legally declare, exercised here directly since
	// no real fixture produces it.
	in := &Instance{sched: &sched{}, resolve: func(uint32) binary.TypeDesc { return binary.FuncDesc{} }}
	_, err := taskReturnHostFuncGraph(in, canon)
	requireErrContains(t, err, "flatten result type")
}

// TestTaskReturnHostFuncGraph_SpillsNoMemoryPanics exercises the >
// MaxFlatParams spill path: a result type flattening to more than 16 core
// values forces task.return's own core signature down to a single pointer
// param, loaded via linear memory -- which panics if none is available.
func TestTaskReturnHostFuncGraph_SpillsNoMemoryPanics(t *testing.T) {
	elems := make([]binary.TypeRef, abi.MaxFlatParams+1)
	for i := range elems {
		elems[i] = binary.TypeRef{Primitive: "u32"}
	}
	tupleRef := binary.TypeRef{TypeIndex: idxPtr(0)}
	canon := binary.Canon{TaskReturnResult: binary.FuncResults{Unnamed: &tupleRef}}
	in := &Instance{sched: &sched{}, mayLeave: true, resolve: func(uint32) binary.TypeDesc {
		return binary.TupleDesc{Elements: elems}
	}}
	def, err := taskReturnHostFuncGraph(in, canon)
	if err != nil {
		t.Fatalf("taskReturnHostFuncGraph: %v", err)
	}
	if len(def.params) != 1 || def.params[0] != api.ValueTypeI32 {
		t.Fatalf("params = %v, want a single i32 pointer param (spilled)", def.params)
	}
	in.activeTask = &task{be: &boundExport{resultType: binary.TupleDesc{Elements: elems}}}
	requirePanicContains(t, "requires linear memory", func() {
		def.fn.Call(context.Background(), nil, []uint64{0})
	})
}

func TestTaskReturnHostFuncGraph_ResolveTypeError(t *testing.T) {
	badRef := binary.TypeRef{TypeIndex: idxPtr(0)} // no primitive, and the resolver below returns nil
	canon := binary.Canon{TaskReturnResult: binary.FuncResults{Unnamed: &badRef}}
	in := &Instance{sched: &sched{}, resolve: func(uint32) binary.TypeDesc { return nil }}
	_, err := taskReturnHostFuncGraph(in, canon)
	requireErrContains(t, err, "resolve result type")
}

// TestReturnValues_NumBorrowsPanics pins the reference's
// trap_if(num_borrows > 0): task.return's second trap condition, reachable
// today only via a directly-constructed task since nothing in this
// milestone increments numBorrows yet (Phase 3 borrow scopes).
func TestReturnValues_NumBorrowsTraps(t *testing.T) {
	tk := &task{numBorrows: 1, onResolve: func([]abi.Value, bool) {}}
	err := tk.returnValues(nil)
	requireErrContains(t, err, "borrow handles still remain")
}

// TestTryEnter_SecondCallerParksInsteadOfTrapping pins Phase 3's forced
// change #4 (docs/component-model-async-phase3-design.md §6 #4): a second
// entry into a busy instance no longer traps with "reentrant async call" --
// tryEnter simply reports false (park at entry), exactly like a genuinely
// backpressured entry would. This replaces the retired
// TestEnterTask_ReentrantTraps.
func TestTryEnter_SecondCallerParksInsteadOfTrapping(t *testing.T) {
	in := &Instance{sched: &sched{}}
	first := &task{}
	if !in.tryEnter(first) {
		t.Fatal("tryEnter(first) = false, want true (nothing else active)")
	}
	second := &task{}
	if in.tryEnter(second) {
		t.Fatal("tryEnter(second) = true, want false (instance already entered)")
	}
}

func TestTryEnter_BackpressureParks(t *testing.T) {
	in := &Instance{sched: &sched{}, backpressure: 1}
	if in.tryEnter(&task{}) {
		t.Fatal("tryEnter = true, want false (backpressure held)")
	}
}

func TestEnterRunLeaveRun_ClearsState(t *testing.T) {
	in := &Instance{sched: &sched{}, mayEnter: true}
	tk := &task{}
	in.enterRun(tk)
	if in.activeTask != tk || !in.exclusiveHeld || in.mayEnter {
		t.Fatalf("enterRun: activeTask=%v exclusiveHeld=%v mayEnter=%v", in.activeTask, in.exclusiveHeld, in.mayEnter)
	}
	in.leaveRun()
	if in.activeTask != nil || in.exclusiveHeld || !in.mayEnter {
		t.Fatalf("leaveRun left activeTask=%v exclusiveHeld=%v mayEnter=%v, want (nil, false, true)", in.activeTask, in.exclusiveHeld, in.mayEnter)
	}
}

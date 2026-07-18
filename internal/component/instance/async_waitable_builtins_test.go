package instance

import (
	"context"
	"testing"
)

// These exercise the Phase 1c handle-table async builtins directly (ordinary
// Go host funcs closing over *Instance), the same way the task.return /
// context.* builtins are tested -- no wasm needed.

func newAsyncInst() *Instance {
	return &Instance{sched: &sched{}, mayLeave: true, resources: newHandleTable()}
}

func callBuiltin(def hostFuncDef, stack ...uint64) []uint64 {
	buf := make([]uint64, 4)
	copy(buf, stack)
	def.fn.Call(context.Background(), nil, buf)
	return buf
}

func TestWaitableSetNewHostFunc(t *testing.T) {
	in := newAsyncInst()
	out := callBuiltin(waitableSetNewHostFunc(in), 0)
	h := uint32(out[0])
	e, ok := in.resources.getEntry(h)
	if !ok {
		t.Fatalf("handle %d not in table", h)
	}
	if _, isWS := e.(*waitableSet); !isWS {
		t.Fatalf("handle %d is %T, want *waitableSet", h, e)
	}
}

func TestWaitableSetNew_MayLeaveFalseTraps(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: false, resources: newHandleTable()}
	requirePanicContains(t, "may not be left", func() { callBuiltin(waitableSetNewHostFunc(in), 0) })
}

func TestWaitableJoin_IntoSetOutOfSetAndTraps(t *testing.T) {
	in := newAsyncInst()
	st := newSubtask()
	sub := in.resources.addEntry(st)
	set := uint32(callBuiltin(waitableSetNewHostFunc(in), 0)[0])
	joinFn := waitableJoinHostFunc(in)

	// join subtask -> set
	callBuiltin(joinFn, uint64(sub), uint64(set))
	if st.wset == nil {
		t.Fatal("subtask not joined to set")
	}
	// join subtask -> 0 leaves the set
	callBuiltin(joinFn, uint64(sub), 0)
	if st.wset != nil {
		t.Fatal("subtask not removed from set by join(_, 0)")
	}
	// wi not a waitable
	res := in.resources.addEntry(&resourceEntry{})
	requirePanicContains(t, "is not a waitable", func() { callBuiltin(joinFn, uint64(res), uint64(set)) })
	// si not a waitable set
	requirePanicContains(t, "is not a waitable set", func() { callBuiltin(joinFn, uint64(sub), uint64(res)) })
}

func TestWaitableSetDrop_EmptyThenNonEmptyThenBadKind(t *testing.T) {
	in := newAsyncInst()
	dropFn := waitableSetDropHostFunc(in)

	// empty set drops and is removed
	set := uint32(callBuiltin(waitableSetNewHostFunc(in), 0)[0])
	callBuiltin(dropFn, uint64(set))
	if _, ok := in.resources.getEntry(set); ok {
		t.Fatal("waitable-set.drop did not remove the set")
	}
	// non-empty set traps
	set2 := uint32(callBuiltin(waitableSetNewHostFunc(in), 0)[0])
	st := newSubtask()
	sub := in.resources.addEntry(st)
	callBuiltin(waitableJoinHostFunc(in), uint64(sub), uint64(set2))
	requirePanicContains(t, "still joined", func() { callBuiltin(dropFn, uint64(set2)) })
	// wrong kind traps
	res := in.resources.addEntry(&resourceEntry{})
	requirePanicContains(t, "is not a waitable set", func() { callBuiltin(dropFn, uint64(res)) })
}

func TestSubtaskDrop_ResolvedThenUnresolvedThenBadKind(t *testing.T) {
	in := newAsyncInst()
	dropFn := subtaskDropHostFunc(in)

	// resolved+delivered subtask drops and is removed
	st := newSubtask()
	st.resolve(subtaskReturned, nil)
	st.deliverResolve()
	h := in.resources.addEntry(st)
	callBuiltin(dropFn, uint64(h))
	if _, ok := in.resources.getEntry(h); ok {
		t.Fatal("subtask.drop did not remove the subtask")
	}
	// unresolved subtask traps
	h2 := in.resources.addEntry(newSubtask())
	requirePanicContains(t, "not yet resolved", func() { callBuiltin(dropFn, uint64(h2)) })
	// wrong kind traps
	res := in.resources.addEntry(&resourceEntry{})
	requirePanicContains(t, "is not a subtask", func() { callBuiltin(dropFn, uint64(res)) })
}

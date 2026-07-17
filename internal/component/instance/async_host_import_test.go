package instance

import (
	"context"
	"errors"
	"testing"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// These are unit tests for the async host-import machinery
// (async_host_import.go) and the waitable-set.wait/poll builtins
// (async_builtins.go) that TestAwaitAsyncImport (async_await_import_test.go)
// exercises only through the guest-code paths it happens to take. Mirrors
// host_import_test.go's direct-call style for buildHostWrapper.

// ------- buildAsyncHostWrapper: bind-time errors -------

func TestBuildAsyncHostWrapper_TooManyParams(t *testing.T) {
	var params []binary.TypeDesc
	for range 5 { // 5 u32 flat params > maxFlatAsyncParams (4)
		params = append(params, binary.PrimitiveDesc{Prim: "u32"})
	}
	hi := &hostImport{asyncFn: func(context.Context, []abi.Value, *AsyncCall) error { return nil }, params: params}
	_, _, _, err := buildAsyncHostWrapper(&Instance{sched: &sched{}}, "i", "f", hi, newHandleTable(), nil, nil)
	requireErrContains(t, err, "exceeding the async flat limit")
}

func TestBuildAsyncHostWrapper_TooManyResults(t *testing.T) {
	hi := &hostImport{
		asyncFn: func(context.Context, []abi.Value, *AsyncCall) error { return nil },
		results: []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}, binary.PrimitiveDesc{Prim: "u32"}},
	}
	_, _, _, err := buildAsyncHostWrapper(&Instance{sched: &sched{}}, "i", "f", hi, newHandleTable(), nil, nil)
	requireErrContains(t, err, "multiple async-import results are not supported")
}

func TestBuildAsyncHostWrapper_BadParamType(t *testing.T) {
	hi := &hostImport{
		asyncFn:  func(context.Context, []abi.Value, *AsyncCall) error { return nil },
		customFD: &binary.FuncDesc{Params: []binary.FuncParam{{Type: binary.TypeRef{}}}},
	}
	_, _, _, err := buildAsyncHostWrapper(&Instance{sched: &sched{}}, "i", "f", hi, newHandleTable(), nil, nil)
	requireErrContains(t, err, "params")
}

func TestBuildAsyncHostWrapper_BadResultType(t *testing.T) {
	hi := &hostImport{
		asyncFn:  func(context.Context, []abi.Value, *AsyncCall) error { return nil },
		customFD: &binary.FuncDesc{Results: binary.FuncResults{Unnamed: &binary.TypeRef{}}},
	}
	_, _, _, err := buildAsyncHostWrapper(&Instance{sched: &sched{}}, "i", "f", hi, newHandleTable(), nil, nil)
	requireErrContains(t, err, "results")
}

// ------- buildAsyncHostWrapper: the wrapper's runtime behavior -------

func TestBuildAsyncHostWrapper_ImmediateResolve(t *testing.T) {
	_, mod := memModule(t)
	in := &Instance{sched: &sched{}, resources: newHandleTable(), mayLeave: true}
	hi := &hostImport{
		asyncFn: func(_ context.Context, args []abi.Value, call *AsyncCall) error {
			if len(args) != 0 {
				t.Fatalf("args: %#v, want none", args)
			}
			call.Resolve([]abi.Value{uint32(99)})
			return nil
		},
		results: []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}},
	}
	fn, params, results, err := buildAsyncHostWrapper(in, "i", "get", hi, in.resources, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(params) != 1 { // trailing out-pointer only (0 real params)
		t.Fatalf("params: %#v, want 1 (out-pointer)", params)
	}
	if len(results) != 1 {
		t.Fatalf("results: %#v, want 1 (packed state)", results)
	}

	stack := []uint64{0} // retptr = 0
	fn.Call(context.Background(), mod, stack)
	if stack[0] != uint64(subtaskReturned) {
		t.Fatalf("packed = %d, want RETURNED (%d)", stack[0], subtaskReturned)
	}
	got, ok := mod.Memory().ReadUint32Le(0)
	if !ok || got != 99 {
		t.Fatalf("memory[0] = %d (ok=%v), want 99", got, ok)
	}
	if len(in.resources.entries) != 0 {
		t.Fatalf("immediate resolve must not park a subtask in the table, got %d entries", len(in.resources.entries))
	}
}

func TestBuildAsyncHostWrapper_ZeroResult(t *testing.T) {
	in := &Instance{sched: &sched{}, resources: newHandleTable(), mayLeave: true}
	hi := &hostImport{asyncFn: func(_ context.Context, _ []abi.Value, call *AsyncCall) error {
		call.Resolve(nil)
		return nil
	}}
	fn, params, _, err := buildAsyncHostWrapper(in, "i", "f", hi, in.resources, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(params) != 0 {
		t.Fatalf("params: %#v, want none (no result => no out-pointer)", params)
	}
	stack := []uint64{0}
	fn.Call(context.Background(), nil, stack)
	if stack[0] != uint64(subtaskReturned) {
		t.Fatalf("packed = %d, want RETURNED", stack[0])
	}
}

func TestBuildAsyncHostWrapper_Deferred(t *testing.T) {
	_, mod := memModule(t)
	in := &Instance{sched: &sched{}, resources: newHandleTable(), mayLeave: true}
	var call *AsyncCall
	hi := &hostImport{
		asyncFn: func(_ context.Context, _ []abi.Value, c *AsyncCall) error {
			call = c
			c.Defer(func() { c.Resolve([]abi.Value{uint32(7)}) })
			return nil
		},
		results: []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}},
	}
	fn, _, _, err := buildAsyncHostWrapper(in, "i", "get", hi, in.resources, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	stack := []uint64{0}
	fn.Call(context.Background(), mod, stack)

	code := uint32(stack[0]) & 0xf
	subtaski := uint32(stack[0]) >> 4
	if code != uint32(subtaskStarted) || subtaski == 0 {
		t.Fatalf("packed = %#x, want STARTED with a nonzero subtask index", stack[0])
	}
	e, ok := in.resources.getEntry(subtaski)
	st, isSub := e.(*subtask)
	if !ok || !isSub {
		t.Fatalf("subtask %d not parked in the table", subtaski)
	}
	if st.resolved() {
		t.Fatal("subtask resolved before the deferred thunk ran")
	}

	if _, err := in.sched.step(); err != nil {
		t.Fatalf("sched.step: %v", err)
	}
	if !st.resolved() {
		t.Fatal("subtask not resolved after running the deferred thunk")
	}
	got, ok := mod.Memory().ReadUint32Le(0)
	if !ok || got != 7 {
		t.Fatalf("memory[0] = %d (ok=%v), want 7", got, ok)
	}
	if !st.hasPendingEvent() {
		t.Fatal("Resolve (parked case) must install a pending event")
	}
	ev := st.getPendingEvent()
	if ev.code != eventSubtask || ev.p1 != subtaski || ev.p2 != uint32(subtaskReturned) {
		t.Fatalf("pending event = %+v", ev)
	}
	if !st.resolveDelivered() {
		t.Fatal("delivering the pending event must deliver_resolve")
	}
	_ = call
}

func TestBuildAsyncHostWrapper_HostFuncErrorTraps(t *testing.T) {
	in := &Instance{sched: &sched{}, resources: newHandleTable(), mayLeave: true}
	hi := &hostImport{asyncFn: func(context.Context, []abi.Value, *AsyncCall) error {
		return errors.New("boom")
	}}
	fn, _, _, err := buildAsyncHostWrapper(in, "i", "f", hi, in.resources, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requirePanicContains(t, "boom", func() { fn.Call(context.Background(), nil, []uint64{0}) })
}

func TestBuildAsyncHostWrapper_BorrowArgLendsOnSubtask(t *testing.T) {
	_, mod := memModule(t)
	in := &Instance{sched: &sched{}, resources: newHandleTable(), mayLeave: true}
	const borrowRT = uint32(42)
	h := in.resources.NewOwn(borrowRT, 7) // an own<T> handle to borrow from

	var seenRep uint32
	hi := &hostImport{
		asyncFn: func(_ context.Context, args []abi.Value, call *AsyncCall) error {
			seenRep = args[0].(uint32) //nolint:errcheck
			call.Defer(func() { call.Resolve(nil) })
			return nil
		},
		customFD: &binary.FuncDesc{Params: []binary.FuncParam{
			{Name: "h", Type: binary.TypeRef{TypeIndex: ptrU32(0)}},
		}},
		customResolve: func(uint32) binary.TypeDesc { return binary.BorrowDesc{ResourceType: borrowRT} },
	}
	fn, _, _, err := buildAsyncHostWrapper(in, "i", "f", hi, in.resources, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	stack := []uint64{uint64(h)}
	fn.Call(context.Background(), mod, stack)
	if seenRep != 7 {
		t.Fatalf("host func saw rep %d, want 7 (the resolved borrow<T>)", seenRep)
	}

	subtaski := uint32(stack[0]) >> 4
	e, ok := in.resources.getEntry(subtaski)
	st := e.(*subtask) //nolint:errcheck
	if !ok || len(st.lenders) != 1 {
		t.Fatalf("subtask.lenders = %#v, want exactly the borrowed entry", st.lenders)
	}
	entry, _ := in.resources.entryAt(h)
	re := entry.(*resourceEntry) //nolint:errcheck
	if re.lendCount != 1 {
		t.Fatalf("lendCount = %d, want 1 (from addLender, not a double count)", re.lendCount)
	}

	if _, err := in.sched.step(); err != nil {
		t.Fatal(err)
	}
	st.getPendingEvent() // delivers the resolve, releasing the lend
	if re.lendCount != 0 {
		t.Fatalf("lendCount after deliverResolve = %d, want 0", re.lendCount)
	}
}

func ptrU32(v uint32) *uint32 { return &v }

// ------- AsyncCall.Resolve: the two structural guards -------

func TestAsyncCall_Resolve_TwiceTraps(t *testing.T) {
	st := newSubtask()
	st.applyResolve = func([]abi.Value) error { return nil }
	ac := &AsyncCall{in: &Instance{sched: &sched{}}, st: st, inCall: true}
	ac.Resolve(nil)
	requirePanicContains(t, "Resolve called twice", func() { ac.Resolve(nil) })
}

func TestAsyncCall_Resolve_OutsideSchedulerTraps(t *testing.T) {
	st := newSubtask()
	st.applyResolve = func([]abi.Value) error { return nil }
	ac := &AsyncCall{in: &Instance{sched: &sched{}}, st: st} // inCall=false, sched.pumping=false
	requirePanicContains(t, "external completion requires CallAsync", func() { ac.Resolve(nil) })
}

func TestAsyncCall_Resolve_ApplyResolveErrorTraps(t *testing.T) {
	st := newSubtask()
	st.applyResolve = func([]abi.Value) error { return errors.New("bad result") }
	ac := &AsyncCall{in: &Instance{sched: &sched{}}, st: st, inCall: true}
	requirePanicContains(t, "bad result", func() { ac.Resolve(nil) })
}

// ------- AsyncCall.OnCancel / ResolveCancelled -------

func TestAsyncCall_OnCancel_SetsSubtaskOnCancel(t *testing.T) {
	st := newSubtask()
	ac := &AsyncCall{in: &Instance{sched: &sched{}}, st: st}
	called := false
	ac.OnCancel(func() { called = true })
	if st.onCancel == nil {
		t.Fatal("OnCancel did not set st.onCancel")
	}
	if err := st.onCancel(); err != nil {
		t.Fatalf("st.onCancel(): %v", err)
	}
	if !called {
		t.Fatal("the registered fn was not invoked")
	}
}

func TestAsyncCall_ResolveCancelled_RequiresCancellationRequested(t *testing.T) {
	st := newSubtask()
	ac := &AsyncCall{in: &Instance{sched: &sched{}}, st: st, inCall: true}
	requirePanicContains(t, "without a prior subtask.cancel request", func() { ac.ResolveCancelled() })
}

func TestAsyncCall_ResolveCancelled_Immediate(t *testing.T) {
	st := newSubtask()
	st.cancellationRequested = true
	ac := &AsyncCall{in: &Instance{sched: &sched{}}, st: st, inCall: true}
	ac.ResolveCancelled()
	if st.state != subtaskCancelledBeforeReturned {
		t.Fatalf("state = %v, want subtaskCancelledBeforeReturned", st.state)
	}
	// Immediate path (inCall): no pending event installed -- the wrapper's
	// own epilogue observes st.resolved() and takes the fast path.
	if st.hasPendingEvent() {
		t.Fatal("a pending event was installed on the immediate (inCall) path")
	}
}

func TestAsyncCall_ResolveCancelled_Parked(t *testing.T) {
	st := newSubtask()
	st.cancellationRequested = true
	ac := &AsyncCall{in: &Instance{sched: &sched{}}, st: st, subtaski: 5}
	ac.in.sched.pumping = true // "called from a scheduler thunk"
	ac.ResolveCancelled()
	if !st.hasPendingEvent() {
		t.Fatal("expected a pending event on the parked path")
	}
	ev := st.getPendingEvent()
	if ev.code != eventSubtask || ev.p1 != 5 || subtaskState(ev.p2) != subtaskCancelledBeforeReturned {
		t.Fatalf("event = %+v, want (SUBTASK, 5, CANCELLED_BEFORE_RETURNED)", ev)
	}
}

func TestAsyncCall_ResolveCancelled_TwiceTraps(t *testing.T) {
	st := newSubtask()
	st.cancellationRequested = true
	ac := &AsyncCall{in: &Instance{sched: &sched{}}, st: st, inCall: true}
	ac.ResolveCancelled()
	requirePanicContains(t, "called twice", func() { ac.ResolveCancelled() })
}

func TestAsyncCall_ResolveCancelled_OutsideSchedulerTraps(t *testing.T) {
	st := newSubtask()
	st.cancellationRequested = true
	ac := &AsyncCall{in: &Instance{sched: &sched{}}, st: st} // inCall=false, sched.pumping=false
	requirePanicContains(t, "external completion requires CallAsync", func() { ac.ResolveCancelled() })
}

// ------- mapArgsToProvider / mapResultsFromProvider -------

func TestMapArgsToProvider_BorrowArgIsCallScopedInProviderTable(t *testing.T) {
	sub := &Instance{resources: newHandleTable()}
	// A provider that does NOT itself own the resource (isGuestResource
	// false, or nil): the cross-instance scoped-mint path applies (§4.1).
	tgt := &guestAsyncTarget{sub: sub, be: &boundExport{paramTypes: []binary.TypeDesc{binary.BorrowDesc{ResourceType: 1}}}}
	scope := &task{}
	out, err := mapArgsToProvider(tgt, []abi.Value{uint32(42)}, scope)
	if err != nil {
		t.Fatalf("mapArgsToProvider: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d value(s), want 1", len(out))
	}
	h, ok := out[0].(uint32)
	if !ok {
		t.Fatalf("result is %T, want uint32 (a minted handle)", out[0])
	}
	if scope.numBorrows != 1 {
		t.Fatalf("scope.numBorrows = %d, want 1 (call-scoped mint)", scope.numBorrows)
	}
	rep, err := sub.resources.Rep(1, h)
	if err != nil || rep != 42 {
		t.Fatalf("Rep(1, h) = (%d, %v), want (42, nil)", rep, err)
	}
}

func TestMapArgsToProvider_ProviderOwnsResource_NoScope(t *testing.T) {
	sub := &Instance{resources: newHandleTable(), isGuestResource: func(uint32) bool { return true }}
	tgt := &guestAsyncTarget{sub: sub, be: &boundExport{paramTypes: []binary.TypeDesc{binary.BorrowDesc{ResourceType: 1}}}}
	scope := &task{}
	if _, err := mapArgsToProvider(tgt, []abi.Value{uint32(9)}, scope); err != nil {
		t.Fatalf("mapArgsToProvider: %v", err)
	}
	if scope.numBorrows != 0 {
		t.Fatalf("scope.numBorrows = %d, want 0 (same-instance exemption: provider owns the resource)", scope.numBorrows)
	}
}

func TestMapResultsFromProvider_OwnResult(t *testing.T) {
	sub := &Instance{resources: newHandleTable()}
	h := sub.resources.NewOwn(2, 77)
	tgt := &guestAsyncTarget{sub: sub, be: &boundExport{hasResult: true, resultType: binary.OwnDesc{ResourceType: 2}}}
	out, err := mapResultsFromProvider(tgt, []abi.Value{h})
	if err != nil {
		t.Fatalf("mapResultsFromProvider: %v", err)
	}
	if len(out) != 1 || out[0].(uint32) != 77 {
		t.Fatalf("got %v, want [77] (the rep, own consumed)", out)
	}
	if _, err := sub.resources.Rep(2, h); err == nil {
		t.Fatal("expected the own handle to have been consumed (TakeOwn)")
	}
}

// ------- WithAsyncImport -------

func TestWithAsyncImport_Registers(t *testing.T) {
	fn := func(context.Context, []abi.Value, *AsyncCall) error { return nil }
	c := newConfig([]Option{WithAsyncImport("get", "", fn, nil, nil)})
	hi, ok := c.imports[mkImportKey("get", "")]
	if !ok || hi.asyncFn == nil || hi.fn != nil {
		t.Fatalf("WithAsyncImport did not register an async-only hostImport: %#v", hi)
	}
}

// ------- waitable-set.wait / waitable-set.poll -------

func TestWaitableSetWaitHostFunc_DrivesToPendingEvent(t *testing.T) {
	_, mod := memModule(t)
	in := newAsyncInst()
	in.activeTask = &task{}
	st := newSubtask()
	sub := in.resources.addEntry(st)
	set := uint32(callBuiltin(waitableSetNewHostFunc(in), 0)[0])
	callBuiltin(waitableJoinHostFunc(in), uint64(sub), uint64(set))

	in.sched.enqueue(func() error {
		st.setPendingEvent(func() eventTuple { return eventTuple{code: eventSubtask, p1: sub, p2: uint32(subtaskReturned)} })
		return nil
	})

	waitFn := waitableSetWaitHostFunc(in, binary.Canon{})
	stack := []uint64{uint64(set), 0} // ptr=0
	waitFn.fn.Call(context.Background(), mod, stack)
	if eventCode(stack[0]) != eventSubtask {
		t.Fatalf("event code = %d, want SUBTASK", stack[0])
	}
	p1, _ := mod.Memory().ReadUint32Le(0)
	p2, _ := mod.Memory().ReadUint32Le(4)
	if p1 != sub || p2 != uint32(subtaskReturned) {
		t.Fatalf("stored event tuple = (%d,%d), want (%d,%d)", p1, p2, sub, subtaskReturned)
	}
}

func TestWaitableSetWaitHostFunc_DeadlockTraps(t *testing.T) {
	_, mod := memModule(t)
	in := newAsyncInst()
	in.activeTask = &task{}
	set := uint32(callBuiltin(waitableSetNewHostFunc(in), 0)[0])
	waitFn := waitableSetWaitHostFunc(in, binary.Canon{})
	requirePanicContains(t, "deadlock", func() {
		waitFn.fn.Call(context.Background(), mod, []uint64{uint64(set), 0})
	})
}

func TestWaitableSetWaitHostFunc_BadKindTraps(t *testing.T) {
	in := newAsyncInst()
	in.activeTask = &task{}
	res := in.resources.addEntry(&resourceEntry{})
	waitFn := waitableSetWaitHostFunc(in, binary.Canon{})
	requirePanicContains(t, "is not a waitable set", func() {
		waitFn.fn.Call(context.Background(), nil, []uint64{uint64(res), 0})
	})
}

func TestWaitableSetWaitHostFunc_MayLeaveFalseTraps(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: false, resources: newHandleTable()}
	waitFn := waitableSetWaitHostFunc(in, binary.Canon{})
	requirePanicContains(t, "may not be left", func() {
		waitFn.fn.Call(context.Background(), nil, []uint64{0, 0})
	})
}

func TestWaitableSetPollHostFunc_NoneThenEvent(t *testing.T) {
	_, mod := memModule(t)
	in := newAsyncInst()
	in.activeTask = &task{}
	st := newSubtask()
	sub := in.resources.addEntry(st)
	set := uint32(callBuiltin(waitableSetNewHostFunc(in), 0)[0])
	callBuiltin(waitableJoinHostFunc(in), uint64(sub), uint64(set))

	pollFn := waitableSetPollHostFunc(in, binary.Canon{})
	stack := []uint64{uint64(set), 0}
	pollFn.fn.Call(context.Background(), mod, stack)
	if eventCode(stack[0]) != eventNone {
		t.Fatalf("event code = %d, want NONE (nothing pending)", stack[0])
	}

	st.setPendingEvent(func() eventTuple { return eventTuple{code: eventSubtask, p1: sub, p2: uint32(subtaskReturned)} })
	stack = []uint64{uint64(set), 0}
	pollFn.fn.Call(context.Background(), mod, stack)
	if eventCode(stack[0]) != eventSubtask {
		t.Fatalf("event code = %d, want SUBTASK", stack[0])
	}
}

func TestWaitableSetPollHostFunc_BadKindTraps(t *testing.T) {
	in := newAsyncInst()
	in.activeTask = &task{}
	res := in.resources.addEntry(&resourceEntry{})
	pollFn := waitableSetPollHostFunc(in, binary.Canon{})
	requirePanicContains(t, "is not a waitable set", func() {
		pollFn.fn.Call(context.Background(), nil, []uint64{uint64(res), 0})
	})
}

func TestWaitableSetPollHostFunc_MayLeaveFalseTraps(t *testing.T) {
	in := &Instance{sched: &sched{}, mayLeave: false, resources: newHandleTable()}
	pollFn := waitableSetPollHostFunc(in, binary.Canon{})
	requirePanicContains(t, "may not be left", func() {
		pollFn.fn.Call(context.Background(), nil, []uint64{0, 0})
	})
}

// ------- invokeAsyncCallback's callbackWait arm: bad-kind + deadlock -------

func TestWaitableSetDrop_StillWaitedOnTraps(t *testing.T) {
	ws := &waitableSet{numWaiting: 1}
	err := ws.dropSet()
	requireErrContains(t, err, "waiter(s) still blocked")
}

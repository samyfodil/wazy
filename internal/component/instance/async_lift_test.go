package instance

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

func TestUnpackCallbackResult(t *testing.T) {
	code, si, err := unpackCallbackResult(0x25) // 0010_0101: low nibble 5 -- invalid
	if err == nil {
		t.Fatalf("unpackCallbackResult(0x25) = (%v, %v, nil), want an error (code 5 > MAX)", code, si)
	}

	code, si, err = unpackCallbackResult(0x21) // low nibble 1 (YIELD), si = 2
	if err != nil {
		t.Fatalf("unpackCallbackResult(0x21): %v", err)
	}
	if code != callbackYield || si != 2 {
		t.Fatalf("unpackCallbackResult(0x21) = (%v, %v), want (YIELD, 2)", code, si)
	}
}

// firstLightAsyncExport instantiates first_light.wasm and returns both the
// Instance and its "run-async" boundExport, for tests that need a REAL
// resolved coreFn/callbackFn to mutate a copy of (invokeAsyncCallback's
// early guard checks all run before the core func is ever called, so a
// mutated copy never actually executes the real wasm).
func firstLightAsyncExport(t *testing.T) (*Instance, *boundExport) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	inst, err := Instantiate(ctx, r, firstLightAsyncWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { inst.Close(ctx) })
	be, ok := inst.exports["run-async"]
	if !ok {
		t.Fatal(`exports["run-async"] missing`)
	}
	return inst, be
}

func TestInvokeAsyncCallback_ArityMismatch(t *testing.T) {
	inst, be := firstLightAsyncExport(t)
	_, err := inst.invokeAsyncCallback(context.Background(), be, "run-async", []abi.Value{uint32(1)})
	requireErrContains(t, err, "takes 0 parameter(s), got 1")
}

func TestInvokeAsyncCallback_MissingCoreFn(t *testing.T) {
	inst, be := firstLightAsyncExport(t)
	beCopy := *be
	beCopy.coreFn = nil
	_, err := inst.invokeAsyncCallback(context.Background(), &beCopy, "run-async", nil)
	requireErrContains(t, err, "no exported function")
}

func TestInvokeAsyncCallback_MissingCallbackFn(t *testing.T) {
	inst, be := firstLightAsyncExport(t)
	beCopy := *be
	beCopy.callbackFn = nil
	_, err := inst.invokeAsyncCallback(context.Background(), &beCopy, "run-async", nil)
	requireErrContains(t, err, "callback option")
}

func TestInvokeAsyncCallback_FlattenErr(t *testing.T) {
	inst, be := firstLightAsyncExport(t)
	beCopy := *be
	beCopy.flattenErr = errBoom
	_, err := inst.invokeAsyncCallback(context.Background(), &beCopy, "run-async", nil)
	requireErrContains(t, err, "boom")
}

func TestInvokeAsyncCallback_ZeroCoreResults(t *testing.T) {
	inst, be := firstLightAsyncExport(t)
	beCopy := *be
	beCopy.coreResultCount = 0
	_, err := inst.invokeAsyncCallback(context.Background(), &beCopy, "run-async", nil)
	requireErrContains(t, err, "must return a single packed i32")
}

func TestInvokeAsyncCallback_NotReenterable(t *testing.T) {
	inst, be := firstLightAsyncExport(t)
	inst.mayEnter = false
	_, err := inst.invokeAsyncCallback(context.Background(), be, "run-async", nil)
	requireErrContains(t, err, "not reenterable")
}

func TestInvokeAsyncCallback_EnterTaskError(t *testing.T) {
	inst, be := firstLightAsyncExport(t)
	inst.backpressure = 1 // enterTask rejects entry while backpressure is held
	_, err := inst.invokeAsyncCallback(context.Background(), be, "run-async", nil)
	requireErrContains(t, err, "backpressure")
	// mayEnter must still be restored (the defer runs even on this error path).
	if !inst.mayEnter {
		t.Fatal("mayEnter was left false after an enterTask failure")
	}
}

// TestInvokeAsyncCallback_ParamsSpillUnsupported exercises the "whole-
// parameter-list spilling" guard by declaring a param count wider than what
// coreParamsWant actually holds (lowerParams would append into coreArgs
// according to fd.Params, but be.coreParamsWant here is deliberately wrong).
func TestInvokeAsyncCallback_ParamsSpillUnsupported(t *testing.T) {
	inst, be := firstLightAsyncExport(t)
	beCopy := *be
	beCopy.coreParamsWant = []string{"i32"} // fd.Params is still empty -> lowerParams produces 0 args
	_, err := inst.invokeAsyncCallback(context.Background(), &beCopy, "run-async", nil)
	requireErrContains(t, err, "spilling to memory is not supported")
}

// TestResolveCallbackFuncGraph_CrossInstanceMismatch mirrors
// TestResolvePostReturnFuncGraph_CrossInstanceMismatch: a callback option
// targeting a different core instance than the lift's own core func is
// rejected (this package has no cross-instance calling), using two real
// api.Module identities from an actual instantiated component.
func TestResolveCallbackFuncGraph_CrossInstanceMismatch(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	inst, err := Instantiate(ctx, r, realHelloWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	liftMod := inst.exports["wasi:cli/run@0.2.3#run"].mod
	canon := binary.Canon{Opts: []binary.CanonOpt{{Kind: 0x07, Idx: 0}}}
	coreFuncTarget := func(int) (api.Module, string, error) { return nil, "other", nil } // nil != liftMod
	_, err = resolveCallbackFuncGraph(canon, coreFuncTarget, liftMod)
	requireErrContains(t, err, "cross-instance callback is not supported")
}

func TestResolveCallbackFuncGraph_TargetError(t *testing.T) {
	canon := binary.Canon{Opts: []binary.CanonOpt{{Kind: 0x07, Idx: 5}}}
	coreFuncTarget := func(int) (api.Module, string, error) { return nil, "", errBoom }
	_, err := resolveCallbackFuncGraph(canon, coreFuncTarget, nil)
	requireErrContains(t, err, "callback")
}

func TestResolveCallbackFuncGraph_NoOpt(t *testing.T) {
	name, err := resolveCallbackFuncGraph(binary.Canon{}, nil, nil)
	if err != nil || name != "" {
		t.Fatalf("got (%q, %v), want (\"\", nil)", name, err)
	}
}

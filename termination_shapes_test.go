package wazy_test

import (
	"context"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// Guest code can run unboundedly without ever crossing a `loop` back-edge, which
// used to be the only place WithCloseOnContextDone emitted its termination check.
// Both shapes below ran forever under a cancelled context until the check was
// also emitted at function entry.
//
// Wasm's structured control flow makes `loop` the only backward branch within a
// function, so the call graph is the only other way to build one.

const (
	opNop        = 0x01
	opEnd        = 0x0b
	opCall       = 0x10
	opReturnCall = 0x12
)

func voidFuncType() wasm.FunctionType {
	return wasm.FunctionType{ParamNumInUint64: 0, ResultNumInUint64: 0}
}

// tailRecursionModule is infinite `return_call` self-recursion. The tail call
// reuses the frame, so this runs forever at a constant stack depth with no
// `loop` opcode anywhere.
func tailRecursionModule() []byte {
	return binaryencoding.EncodeModule(&wasm.Module{
		TypeSection:     []wasm.FunctionType{voidFuncType()},
		FunctionSection: []wasm.Index{0},
		ExportSection:   []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 0}},
		CodeSection: []wasm.Code{
			{Body: []byte{opReturnCall, 0x00, opEnd}},
		},
	})
}

// callTreeModule builds a loop-free exponential call tree: f_i calls f_{i+1}
// twice, so f_0 performs 2^(n-1) calls while never exceeding a stack depth of n.
func callTreeModule(n int) []byte {
	funcs := make([]wasm.Index, n)
	code := make([]wasm.Code, n)
	for i := 0; i < n; i++ {
		funcs[i] = 0
		if i == n-1 {
			code[i] = wasm.Code{Body: []byte{opNop, opEnd}}
			continue
		}
		callee := byte(i + 1) // n stays below 128, so a single-byte LEB128 index is fine.
		code[i] = wasm.Code{Body: []byte{opCall, callee, opCall, callee, opEnd}}
	}
	return binaryencoding.EncodeModule(&wasm.Module{
		TypeSection:     []wasm.FunctionType{voidFuncType()},
		FunctionSection: funcs,
		ExportSection:   []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 0}},
		CodeSection:     code,
	})
}

// requireInterrupted calls "run" under a context that expires after timeout and
// requires the call to come back with the cancellation error. A regression fails
// the test at hardLimit rather than hanging it.
func requireInterrupted(t *testing.T, newCfg func() wazy.RuntimeConfig, bin []byte) {
	t.Helper()

	const timeout = 200 * time.Millisecond
	const hardLimit = 30 * time.Second

	cfg := newCfg().
		WithCoreFeatures(api.CoreFeaturesV2 | api.CoreFeatureTailCall).
		WithCloseOnContextDone(true)

	r := wazy.NewRuntimeWithConfig(context.Background(), cfg)
	defer r.Close(context.Background())

	mod, err := r.Instantiate(context.Background(), bin)
	require.NoError(t, err)
	run := mod.ExportedFunction("run")
	require.NotNil(t, run)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, callErr := run.Call(ctx)
		done <- callErr
	}()

	select {
	case callErr := <-done:
		require.Error(t, callErr)
		require.Contains(t, callErr.Error(), "module closed with context deadline exceeded")
	case <-time.After(hardLimit):
		t.Fatal("call was never interrupted: the termination check is not reached on this shape")
	}
}

// hostCallLoopModule is `loop (call $cb) (br 0)`: an infinite loop whose body
// leaves compiled code on every iteration. It is the shape that breaks a naive
// fuel counter: a Go call restores the counter that compiled code was holding in
// a register, so a counter that is REFILLED rather than PRESERVED across an exit
// to Go would start every iteration with a full budget and never run out -- and
// the module-closed check, which only happens when it does, would never run.
func hostCallLoopModule() []byte {
	return binaryencoding.EncodeModule(&wasm.Module{
		TypeSection:     []wasm.FunctionType{voidFuncType()},
		ImportSection:   []wasm.Import{{Module: "env", Name: "cb", Type: wasm.ExternTypeFunc, DescFunc: 0}},
		FunctionSection: []wasm.Index{0},
		ExportSection:   []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 1}},
		CodeSection: []wasm.Code{
			// loop (call 0) (br 0) end
			{Body: []byte{0x03, 0x40, opCall, 0x00, 0x0c, 0x00, opEnd, opEnd}},
		},
	})
}

// TestEnsureTerminationHostCallLoop is requireInterrupted for that shape. It
// needs its own runner because the guest imports a host function.
func TestEnsureTerminationHostCallLoop(t *testing.T) {
	requireCompiler(t)

	const timeout = 200 * time.Millisecond
	const hardLimit = 30 * time.Second

	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithCloseOnContextDone(true))
	defer r.Close(ctx)

	_, err := wazy.HostProc0(r.NewHostModuleBuilder("env").NewFunctionBuilder(), func(context.Context, api.Module) {}).
		Export("cb").
		Instantiate(ctx)
	require.NoError(t, err)

	mod, err := r.Instantiate(ctx, hostCallLoopModule())
	require.NoError(t, err)
	run := mod.ExportedFunction("run")
	require.NotNil(t, run)

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, callErr := run.Call(callCtx)
		done <- callErr
	}()

	select {
	case callErr := <-done:
		require.Error(t, callErr)
		require.Contains(t, callErr.Error(), "module closed with context deadline exceeded")
	case <-time.After(hardLimit):
		t.Fatal("call was never interrupted: a host call is resetting the termination check")
	}
}

func TestEnsureTerminationWithoutLoops(t *testing.T) {
	engines := []struct {
		name   string
		newCfg func() wazy.RuntimeConfig
	}{
		{"compiler", wazy.NewRuntimeConfigCompiler},
		{"interpreter", wazy.NewRuntimeConfigInterpreter},
	}

	shapes := []struct {
		name string
		bin  []byte
	}{
		{"exponential call tree", callTreeModule(45)},
		{"tail call self recursion", tailRecursionModule()},
	}

	for _, eng := range engines {
		for _, sh := range shapes {
			t.Run(eng.name+"/"+sh.name, func(t *testing.T) {
				if eng.name == "compiler" {
					requireCompiler(t)
				}
				t.Parallel()
				requireInterrupted(t, eng.newCfg, sh.bin)
			})
		}
	}
}

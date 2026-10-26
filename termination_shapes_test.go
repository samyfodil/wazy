package wazy_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// Guest code can run unboundedly without ever crossing a `loop` back-edge, which
// used to be the only place a termination check was emitted. Both shapes below
// ran forever under a cancelled context until functions that can do so were
// checked on entry too.
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

// moduleChainModules returns n modules for "m0" to "m<n-1>", and one importing
// from the last whose "run" calls it once. m0 exports an empty function f, and
// each later module an f calling the f of the one before it twice, or through a
// table of its own if table: alone, each is a call tree of two calls, but together
// they make 2^n.
func moduleChainModules(n int, table bool) (deps [][]byte, run []byte) {
	imported := func(k int, body []byte, name string) *wasm.Module {
		m := &wasm.Module{
			TypeSection:     []wasm.FunctionType{voidFuncType()},
			ImportSection:   []wasm.Import{{Module: fmt.Sprintf("m%d", k), Name: "f", Type: wasm.ExternTypeFunc}},
			FunctionSection: []wasm.Index{0},
			CodeSection:     []wasm.Code{{Body: body}},
			ExportSection:   []wasm.Export{{Name: name, Type: wasm.ExternTypeFunc, Index: 1}},
		}
		if table {
			m.TableSection = []wasm.Table{{Min: 1, Type: wasm.RefTypeFuncref}}
			m.ElementSection = []wasm.ElementSegment{{
				OffsetExpr: wasm.NewConstantExpressionFromI32(0), Init: []wasm.Index{0},
				Type: wasm.RefTypeFuncref, Mode: wasm.ElementModeActive,
			}}
		}
		return m
	}
	call := []byte{opCall, 0}
	if table {
		call = []byte{wasm.OpcodeI32Const, 0, wasm.OpcodeCallIndirect, 0, 0}
	}
	deps = append(deps, binaryencoding.EncodeModule(&wasm.Module{
		TypeSection:     []wasm.FunctionType{voidFuncType()},
		FunctionSection: []wasm.Index{0},
		CodeSection:     []wasm.Code{{Body: []byte{opNop, opEnd}}},
		ExportSection:   []wasm.Export{{Name: "f", Type: wasm.ExternTypeFunc, Index: 0}},
	}))
	for k := 1; k < n; k++ {
		deps = append(deps, binaryencoding.EncodeModule(imported(k-1, append(append(append([]byte{}, call...), call...), opEnd), "f")))
	}
	return deps, binaryencoding.EncodeModule(imported(n-1, append(append([]byte{}, call...), opEnd), "run"))
}

// fillTreeModule is callTreeModule(n) whose leaf fills a memory of 128 MiB, which
// takes milliseconds, rather than doing nothing: validation counts the fill as the
// few bytes of code it is, so it alone does not bound the work between two checks.
func fillTreeModule(n int) []byte {
	const size = 128 << 20
	funcs := make([]wasm.Index, n)
	code := make([]wasm.Code, n)
	for i := range n - 1 {
		callee := byte(i + 1)
		code[i] = wasm.Code{Body: []byte{opCall, callee, opCall, callee, opEnd}}
	}
	leaf := []byte{wasm.OpcodeI32Const, 0, wasm.OpcodeI32Const, 0, wasm.OpcodeI32Const}
	leaf = append(leaf, leb128.EncodeInt32(size)...)
	code[n-1] = wasm.Code{Body: append(leaf, wasm.OpcodeMiscPrefix, wasm.OpcodeMiscMemoryFill, 0, opEnd)}
	return binaryencoding.EncodeModule(&wasm.Module{
		TypeSection:     []wasm.FunctionType{voidFuncType()},
		FunctionSection: funcs,
		MemorySection:   []wasm.Memory{{Min: size >> 16, Cap: size >> 16, Max: size >> 16, IsMaxEncoded: true}},
		ExportSection:   []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 0}},
		CodeSection:     code,
	})
}

// requireInterrupted calls "run" under a context that expires after timeout and
// requires the call to come back with the cancellation error. A regression fails
// the test at hardLimit rather than hanging it. deps are instantiated first, as
// "m0", "m1" and so on.
func requireInterrupted(t *testing.T, newCfg func() wazy.RuntimeConfig, bin []byte, deps ...[]byte) {
	t.Helper()

	const timeout = 200 * time.Millisecond
	const hardLimit = 30 * time.Second

	cfg := newCfg().WithCoreFeatures(api.CoreFeaturesV2 | api.CoreFeatureTailCall)

	r := wazy.NewRuntimeWithConfig(context.Background(), cfg)
	defer r.Close(context.Background())

	for i, dep := range deps {
		_, err := r.InstantiateWithConfig(context.Background(), dep, wazy.NewModuleConfig().WithName(fmt.Sprintf("m%d", i)))
		require.NoError(t, err)
	}
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
		require.True(t, errors.Is(callErr, context.DeadlineExceeded), "got %v", callErr)
	case <-time.After(hardLimit):
		t.Fatal("call was never interrupted: the termination check is not reached on this shape")
	}
}

// hostCallLoopModule is `loop (call $cb) (br 0)`: an infinite loop whose body
// leaves compiled code on every iteration. Go code in the host function serves
// any preemption request before compiled code can see it, so this call has to
// be ended from the host-call path instead.
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
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
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
		require.True(t, errors.Is(callErr, context.DeadlineExceeded), "got %v", callErr)
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

	type shape struct {
		name string
		bin  []byte
		deps [][]byte
	}
	shapes := []shape{
		{name: "exponential call tree", bin: callTreeModule(45)},
		{name: "tail call self recursion", bin: tailRecursionModule()},
		{name: "call tree of fills", bin: fillTreeModule(45)},
	}
	for _, table := range []bool{false, true} {
		deps, run := moduleChainModules(45, table)
		name := "call tree across modules"
		if table {
			name += " through tables"
		}
		shapes = append(shapes, shape{name: name, bin: run, deps: deps})
	}

	for _, eng := range engines {
		for _, sh := range shapes {
			t.Run(eng.name+"/"+sh.name, func(t *testing.T) {
				if eng.name == "compiler" {
					requireCompiler(t)
				}
				t.Parallel()
				requireInterrupted(t, eng.newCfg, sh.bin, sh.deps...)
			})
		}
	}
}

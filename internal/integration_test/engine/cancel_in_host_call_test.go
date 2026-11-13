package adhoc

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

func TestCancelInHostCall_Compiler(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	testCancelInHostCall(t, wazy.NewRuntimeConfigCompiler(), true)
}

func TestCancelInHostCall_Interpreter(t *testing.T) {
	testCancelInHostCall(t, wazy.NewRuntimeConfigInterpreter(), false)
}

type cancelKey struct{}

// testCancelInHostCall cancels the call's context from inside a host function,
// where Go code, not the guest, is running when the watcher asks the goroutine
// to stop -- and Go serves that request itself. However the guest goes on from
// there, the call must end the way it would have without the cancel, except
// that recursion, which would otherwise run until the stack overflows, ends
// with the context's error: it reaches no host call, but it grows the stack.
//
// The watcher fires on a goroutine of its own, whenever that gets to run, so
// under the compiler, whose calls are watched, the host function waits for it.
//
// Each function first makes 300 host calls, so that the watcher is registered
// (on the 256th) before the cancel rather than left for a yield that a call this
// short never makes.
func testCancelInHostCall(t *testing.T, config wazy.RuntimeConfig, watched bool) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, config)
	defer r.Close(ctx)

	fired := make(chan struct{}, 1)
	onFire := func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	}
	wasm.AfterWatchFire.Store(&onFire)
	t.Cleanup(func() { wasm.AfterWatchFire.Store(nil) })

	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	b := r.NewHostModuleBuilder("host")
	wazy.HostProc0(b.NewFunctionBuilder(), func(context.Context, api.Module) {}).Export("tick")
	wazy.HostProc0(b.NewFunctionBuilder(), func(ctx context.Context, m api.Module) {
		ctx.Value(cancelKey{}).(context.CancelFunc)()
		if watched {
			<-fired
		}
		// Serves the preemption request the watcher made, as Go does at
		// whatever function call comes next.
		runtime.Gosched()
	}).Export("cancel")
	wazy.HostProc0(b.NewFunctionBuilder(), func(context.Context, api.Module) {
		panic("host panic")
	}).Export("panic")
	_, err := b.Instantiate(ctx)
	require.NoError(t, err)

	const tick, cancel, hostPanic, procExit, recurse = 0, 1, 2, 3, 8
	call := func(idx byte) []byte { return []byte{wasm.OpcodeCall, idx} }
	// for i := 0; i < 300; i++ { tick() }, in local 0.
	ticks := []byte{
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeCall, tick,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalTee, 0,
		wasm.OpcodeI32Const, 0xac, 0x02, wasm.OpcodeI32LtU, wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd,
	}
	body := func(parts ...[]byte) wasm.Code {
		out := append([]byte(nil), ticks...)
		for _, p := range parts {
			out = append(out, p...)
		}
		return wasm.Code{LocalTypes: []wasm.ValueType{i32}, Body: append(out, wasm.OpcodeEnd)}
	}
	mod := &wasm.Module{
		TypeSection: []wasm.FunctionType{{}, {Params: []wasm.ValueType{i32}}},
		ImportSection: []wasm.Import{
			{Module: "host", Name: "tick", Type: wasm.ExternTypeFunc, DescFunc: 0},
			{Module: "host", Name: "cancel", Type: wasm.ExternTypeFunc, DescFunc: 0},
			{Module: "host", Name: "panic", Type: wasm.ExternTypeFunc, DescFunc: 0},
			{Module: wasi_snapshot_preview1.ModuleName, Name: "proc_exit", Type: wasm.ExternTypeFunc, DescFunc: 1},
		},
		FunctionSection: []wasm.Index{0, 0, 0, 0, 0, 0},
		MemorySection:   []wasm.Memory{{Min: 1, Cap: 1, Max: 1}},
		CodeSection: []wasm.Code{
			body(call(cancel), []byte{wasm.OpcodeI32Const, 7}, call(procExit)), // 4: exit
			body(call(cancel), []byte{wasm.OpcodeUnreachable}),                 // 5: trap
			body(call(cancel), call(hostPanic)),                                // 6: panic
			body(call(cancel), call(recurse)),                                  // 7: overflow
			{Body: []byte{wasm.OpcodeCall, recurse, wasm.OpcodeEnd}},           // 8: recurse
			body(call(cancel)), // 9: return
		},
		ExportSection: []wasm.Export{
			{Name: "exit", Type: wasm.ExternTypeFunc, Index: 4},
			{Name: "trap", Type: wasm.ExternTypeFunc, Index: 5},
			{Name: "panic", Type: wasm.ExternTypeFunc, Index: 6},
			{Name: "overflow", Type: wasm.ExternTypeFunc, Index: 7},
			{Name: "return", Type: wasm.ExternTypeFunc, Index: 9},
		},
	}
	require.NoError(t, mod.Validate(api.CoreFeaturesV2))
	compiled, err := r.CompileModule(ctx, binaryencoding.EncodeModule(mod))
	require.NoError(t, err)

	for fn, wantErr := range map[string]string{
		"exit":     "module closed with exit_code(7)",
		"trap":     "wasm error: unreachable",
		"panic":    "host panic (recovered by wazy)",
		"overflow": context.Canceled.Error(),
		"return":   "",
	} {
		t.Run(fn, func(t *testing.T) {
			callCtx, cancelFn := context.WithCancel(ctx)
			defer cancelFn()
			callCtx = context.WithValue(callCtx, cancelKey{}, cancelFn)

			m, err := r.InstantiateModule(ctx, compiled, wazy.NewModuleConfig().WithName(t.Name()))
			require.NoError(t, err)
			defer m.Close(ctx)

			_, err = m.ExportedFunction(fn).Call(callCtx)
			switch {
			case wantErr == "":
				require.NoError(t, err)
			case fn == "overflow":
				require.True(t, errors.Is(err, context.Canceled), "got %v", err)
			default:
				require.Error(t, err)
				require.Contains(t, err.Error(), wantErr)
			}
		})
	}
}

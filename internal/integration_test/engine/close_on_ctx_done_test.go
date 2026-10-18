package adhoc

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

func TestCloseOnContextDoneReleasesResources_Compiler(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	testCloseOnContextDoneReleasesResources(t, wazy.NewRuntimeConfigCompiler())
}

func TestCloseOnContextDoneReleasesResources_Interpreter(t *testing.T) {
	testCloseOnContextDoneReleasesResources(t, wazy.NewRuntimeConfigInterpreter())
}

type cancelKey struct{}

type countingCloseNotifier struct{ n atomic.Int32 }

func (c *countingCloseNotifier) CloseNotify(context.Context, uint32) { c.n.Add(1) }

// testCloseOnContextDoneReleasesResources pins wazero#2558: with
// WithCloseOnContextDone, a context done while the guest is in a host call
// closes the module but defers releasing its resources to the next
// FailIfClosed. However the guest then leaves -- trapping, calling proc_exit,
// a host function panicking, overflowing the stack, or returning normally --
// the memory and the fds must be released by the time the call returns, and
// exactly once.
func testCloseOnContextDoneReleasesResources(t *testing.T, config wazy.RuntimeConfig) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, config.WithCloseOnContextDone(true))
	defer r.Close(ctx)

	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	b := r.NewHostModuleBuilder("host")
	// Cancels the caller's context and waits for the watcher to close the
	// module, so the deferred-release state is set before the guest leaves.
	wazy.HostProc0(b.NewFunctionBuilder(), func(ctx context.Context, m api.Module) {
		ctx.Value(cancelKey{}).(context.CancelFunc)()
		for !m.IsClosed() {
			time.Sleep(time.Millisecond)
		}
	}).Export("cancel")
	wazy.HostProc0(b.NewFunctionBuilder(), func(context.Context, api.Module) {
		panic("host panic")
	}).Export("panic")
	_, err := b.Instantiate(ctx)
	require.NoError(t, err)

	const cancel, hostPanic, procExit, recurse = 0, 1, 2, 7
	call := func(idx byte) []byte { return []byte{wasm.OpcodeCall, idx} }
	body := func(parts ...[]byte) wasm.Code {
		var out []byte
		for _, p := range parts {
			out = append(out, p...)
		}
		return wasm.Code{Body: append(out, wasm.OpcodeEnd)}
	}
	mod := &wasm.Module{
		TypeSection: []wasm.FunctionType{{}, {Params: []wasm.ValueType{i32}}},
		ImportSection: []wasm.Import{
			{Module: "host", Name: "cancel", Type: wasm.ExternTypeFunc, DescFunc: 0},
			{Module: "host", Name: "panic", Type: wasm.ExternTypeFunc, DescFunc: 0},
			{Module: wasi_snapshot_preview1.ModuleName, Name: "proc_exit", Type: wasm.ExternTypeFunc, DescFunc: 1},
		},
		FunctionSection: []wasm.Index{0, 0, 0, 0, 0, 0},
		MemorySection:   []wasm.Memory{{Min: 1, Cap: 1, Max: 1}},
		CodeSection: []wasm.Code{
			body(call(cancel), []byte{wasm.OpcodeI32Const, 7}, call(procExit)), // 3: exit
			body(call(cancel), []byte{wasm.OpcodeUnreachable}),                 // 4: trap
			body(call(cancel), call(hostPanic)),                                // 5: panic
			body(call(cancel), call(recurse)),                                  // 6: overflow
			body(call(recurse)),                                                // 7: recurse
			body(call(cancel)),                                                 // 8: return
		},
		ExportSection: []wasm.Export{
			{Name: "exit", Type: wasm.ExternTypeFunc, Index: 3},
			{Name: "trap", Type: wasm.ExternTypeFunc, Index: 4},
			{Name: "panic", Type: wasm.ExternTypeFunc, Index: 5},
			{Name: "overflow", Type: wasm.ExternTypeFunc, Index: 6},
			{Name: "return", Type: wasm.ExternTypeFunc, Index: 8},
		},
	}
	require.NoError(t, mod.Validate(api.CoreFeaturesV2))
	compiled, err := r.CompileModule(ctx, binaryencoding.EncodeModule(mod))
	require.NoError(t, err)

	for _, fn := range []string{"exit", "trap", "panic", "overflow", "return"} {
		t.Run(fn, func(t *testing.T) {
			notifier := &countingCloseNotifier{}
			callCtx, cancelFn := context.WithCancel(api.WithCloseNotifier(ctx, notifier))
			defer cancelFn()
			callCtx = context.WithValue(callCtx, cancelKey{}, cancelFn)

			cfg := wazy.NewModuleConfig().WithName(t.Name()).
				WithFSConfig(wazy.NewFSConfig().WithDirMount(t.TempDir(), "/"))
			m, err := r.InstantiateModule(callCtx, compiled, cfg)
			require.NoError(t, err)
			inst := m.(*wasm.ModuleInstance)
			require.NotNil(t, inst.Sys)
			require.NotEqual(t, uint32(0), m.Memory().Size())

			_, err = m.ExportedFunction(fn).Call(callCtx)
			require.Error(t, err)

			require.True(t, m.IsClosed())
			require.Equal(t, uint32(0), m.Memory().Size(), "memory not released when the call returned")
			require.Nil(t, inst.Sys, "fds not released when the call returned")
			require.Equal(t, int32(1), notifier.n.Load())

			// A later Close must not release a second time.
			require.NoError(t, m.Close(ctx))
			require.Equal(t, int32(1), notifier.n.Load())
		})
	}
}

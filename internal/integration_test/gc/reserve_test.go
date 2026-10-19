package gc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// Sources: testdata/reserve_callee.wat, testdata/reserve_caller.wat, testdata/reserve_late.wat.
var (
	//go:embed testdata/reserve_callee.wasm
	reserveCalleeWasm []byte
	//go:embed testdata/reserve_caller.wasm
	reserveCallerWasm []byte
	//go:embed testdata/reserve_late.wasm
	reserveLateWasm []byte
)

// TestRootRegionIsTheEngines: a safepoint spills its roots into a region at the bottom of the wasm stack it runs
// on, which is the stack of whichever exported function started the call, not of the module the safepoint is in.
// Sized from the caller's module, which has no safepoint of its own, the region did not exist, and the callee's
// spill went below the start of the buffer, over whatever Go object came before it.
func TestRootRegionIsTheEngines(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	const n = 300_000
	const want = 7 + n*(n+1)/2

	setup := func(t *testing.T, prepare func(context.Context, wazy.Runtime)) (caller, callee api.Module, live func() int) {
		ctx := context.Background()
		r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithCoreFeatures(v128GCFeatures))
		t.Cleanup(func() { r.Close(ctx) })
		_, err := wazy.HostProc0(r.NewHostModuleBuilder("env").NewFunctionBuilder(),
			func(ctx context.Context, _ api.Module) { prepare(ctx, r) }).
			Export("prepare").Instantiate(ctx)
		require.NoError(t, err)
		callee, err = r.InstantiateWithConfig(ctx, reserveCalleeWasm, wazy.NewModuleConfig().WithName("callee"))
		require.NoError(t, err)
		caller, err = r.InstantiateWithConfig(ctx, reserveCallerWasm, wazy.NewModuleConfig().WithName("caller"))
		require.NoError(t, err)
		return caller, callee, callee.(*wasm.ModuleInstance).GCLiveObjects
	}

	// The callee's function, imported: its spills must land in the caller's stack, and its garbage must
	// still be reclaimed.
	t.Run("imported", func(t *testing.T) {
		caller, _, live := setup(t, func(context.Context, wazy.Runtime) {})
		for range 3 {
			res, err := caller.ExportedFunction("direct").Call(context.Background(), n)
			require.NoError(t, err)
			require.Equal(t, uint64(want), res[0])
		}
		require.True(t, live() < n, "nothing was reclaimed: %d live", live())
	})

	// A stack that grows in the middle of the call keeps the region it started with, header and all.
	t.Run("stack grown during the call", func(t *testing.T) {
		_, callee, live := setup(t, func(context.Context, wazy.Runtime) {})
		for range 3 {
			res, err := callee.ExportedFunction("deep").Call(context.Background(), 5000, n)
			require.NoError(t, err)
			require.Equal(t, uint64(want), res[0])
		}
		require.True(t, live() < n, "nothing was reclaimed: %d live", live())
	})

	// A module instantiated in the middle of the call, needing more room than any module before it, reached
	// through the caller's table. The region cannot grow under a running call, so its safepoints say they
	// could not spill, and the collector reclaims nothing rather than miss their roots.
	t.Run("instantiated during the call", func(t *testing.T) {
		var instantiated bool
		caller, _, live := setup(t, func(ctx context.Context, r wazy.Runtime) {
			if instantiated {
				return
			}
			instantiated = true
			_, err := r.Instantiate(ctx, reserveLateWasm)
			require.NoError(t, err)
		})
		call := func() {
			res, err := caller.ExportedFunction("indirect").Call(context.Background(), n)
			require.NoError(t, err)
			require.Equal(t, uint64(want), res[0])
		}
		call()
		require.True(t, live() >= n, "reclaimed with roots missing: %d live", live())
		// A call that starts after the module was instantiated gets a region that fits it.
		call()
		require.True(t, live() < n, "nothing was reclaimed: %d live", live())
	})
}

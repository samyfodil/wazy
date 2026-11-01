package gc

import (
	"context"
	_ "embed"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

//go:embed testdata/park.wasm
var parkWasm []byte

//go:embed testdata/burst.wasm
var burstWasm []byte

const parkFeatures = v128GCFeatures | api.CoreFeatureTailCall | experimental.CoreFeaturesThreads

// parkTimeout is how long a collection may take to stop a call that is parkable. Either way of failing
// these tests is a collection that never stops it, which would wait out the guest instead.
const parkTimeout = 5 * time.Second

func forEachEngine(t *testing.T, test func(t *testing.T, config wazy.RuntimeConfig)) {
	t.Run("interpreter", func(t *testing.T) {
		test(t, wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(parkFeatures))
	})
	t.Run("compiler", func(t *testing.T) {
		if !platform.CompilerSupported() {
			t.Skip()
		}
		test(t, wazy.NewRuntimeConfigCompiler().WithCoreFeatures(parkFeatures))
	})
}

// TestParkInTailCallCycle runs a guest that repeats through return_call alone while another asks for a
// collection. Such a cycle never passes a loop header, so it has to park somewhere else, or the collection,
// and every other call on the store with it, waits until the cycle ends.
func TestParkInTailCallCycle(t *testing.T) {
	forEachEngine(t, func(t *testing.T, config wazy.RuntimeConfig) {
		ctx := context.Background()
		r := wazy.NewRuntimeWithConfig(ctx, config)
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, parkWasm)
		require.NoError(t, err)

		spinCtx, cancel := context.WithCancel(ctx)
		spun := make(chan error, 1)
		go func() {
			_, err := mod.ExportedFunction("spin").Call(spinCtx, 1<<62)
			spun <- err
		}()
		defer func() {
			cancel()
			<-spun
		}()
		time.Sleep(50 * time.Millisecond)

		churned := make(chan error, 1)
		go func() {
			_, err := mod.ExportedFunction("churn").Call(ctx, 5_000)
			churned <- err
		}()
		select {
		case err := <-churned:
			require.NoError(t, err)
		case <-time.After(parkTimeout):
			t.Fatal("the collection did not stop the return_call cycle")
		}
	})
}

// TestParkWhileWaiting asks for a collection while another call is blocked in memory.atomic.wait. The waiter's
// stack cannot move until it is woken, so it is parked; were it not, the collection would wait for it while the
// call that is to wake it waits for the collection.
func TestParkWhileWaiting(t *testing.T) {
	forEachEngine(t, func(t *testing.T, config wazy.RuntimeConfig) {
		ctx := context.Background()
		r := wazy.NewRuntimeWithConfig(ctx, config)
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, parkWasm)
		require.NoError(t, err)

		call := func(out chan<- uint64, name string, args ...uint64) {
			res, err := mod.ExportedFunction(name).Call(ctx, args...)
			if err != nil {
				t.Error(err)
				out <- ^uint64(0)
				return
			}
			out <- res[0]
		}
		waited := make(chan uint64, 1)
		go call(waited, "wait")
		time.Sleep(100 * time.Millisecond)

		woke := make(chan uint64, 1)
		go call(woke, "churn_then_wake", 5_000)
		select {
		case n := <-woke:
			require.Equal(t, uint64(1), n, "the waiter was not woken")
			require.Equal(t, uint64(0), <-waited, "the wait did not end with \"ok\"")
		case <-time.After(parkTimeout):
			t.Error("the collection waited for a call blocked in memory.atomic.wait")
			<-woke // the wait times out after 20 seconds, which lets the collection finish
			<-waited
		}
	})
}

// TestRequestOutlivesCaller asks for a collection from a call that returns without reaching a parking point.
// The request outlives it, and later calls have to take it on: otherwise every allocation after it finds a
// collection already wanted and asks for none, and the heap grows without bound.
func TestRequestOutlivesCaller(t *testing.T) {
	forEachEngine(t, func(t *testing.T, config wazy.RuntimeConfig) {
		ctx := context.Background()
		r := wazy.NewRuntimeWithConfig(ctx, config)
		defer r.Close(ctx)
		burst, err := r.InstantiateWithConfig(ctx, burstWasm, wazy.NewModuleConfig().WithName("burst"))
		require.NoError(t, err)
		mod, err := r.Instantiate(ctx, parkWasm)
		require.NoError(t, err)

		_, err = burst.ExportedFunction("burst").Call(ctx)
		require.NoError(t, err)
		const churn = 200_000
		_, err = mod.ExportedFunction("churn").Call(ctx, churn)
		require.NoError(t, err)
		live := mod.(*wasm.ModuleInstance).GCLiveObjects()
		require.True(t, live < churn/2, "%d objects live after churning %d", live, churn)
	})
}

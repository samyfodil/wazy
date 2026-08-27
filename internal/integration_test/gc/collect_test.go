package gc

import (
	"context"
	_ "embed"
	"fmt"
	"sync"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

//go:embed testdata/gcloop.wasm
var gcLoopWasm []byte

// TestCollector exercises reclamation: a guest that allocates in a loop must not grow the heap without bound,
// and what it keeps -- through a local, a global, a table or another object's field -- must survive.
func TestCollector(t *testing.T) {
	t.Run("interpreter", func(t *testing.T) {
		runCollector(t, wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(v128GCFeatures))
	})
	t.Run("compiler", func(t *testing.T) {
		if !platform.CompilerSupported() {
			t.Skip()
		}
		runCollector(t, wazy.NewRuntimeConfigCompiler().WithCoreFeatures(v128GCFeatures))
	})
}

func runCollector(t *testing.T, config wazy.RuntimeConfig) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, config)
	defer r.Close(ctx)

	mod, err := r.Instantiate(ctx, gcLoopWasm)
	require.NoError(t, err)
	// Nothing about reclamation is observable through the public API, which is the point; a test has to
	// reach the store's heap directly to see a collection happen.
	heap := mod.(*wasm.ModuleInstance).GCLiveObjects

	call := func(name string, args ...uint64) []uint64 {
		t.Helper()
		res, err := mod.ExportedFunction(name).Call(ctx, args...)
		require.NoError(t, err, name)
		return res
	}

	const churn = 200_000

	t.Run("allocating in a loop does not grow the heap without bound", func(t *testing.T) {
		call("churn", churn)
		live := heap()
		require.True(t, live < churn/2,
			"expected the heap to be collected, but %d of %d allocations are still live", live, churn)
	})

	t.Run("what a global and a table name survives", func(t *testing.T) {
		call("churn_keeping", churn)
		require.Equal(t, []uint64{churn - 1}, call("kept"))
		live := heap()
		require.True(t, live < churn/2, "expected collection, but %d objects are live", live)
	})

	t.Run("a chain reachable through fields survives whole", func(t *testing.T) {
		const links = 5_000
		call("clear")
		call("churn", churn) // push everything else out of the heap first
		call("chain", links)
		require.Equal(t, []uint64{links}, call("chain_len"))
	})

	t.Run("dropping the last root lets the whole chain go", func(t *testing.T) {
		before := heap()
		call("clear")
		call("churn", churn)
		require.True(t, heap() < before, "expected the chain to be reclaimed once nothing named it")
	})
}

// TestCollectorUnderConcurrency runs several goroutines allocating in the same store at once. A collection has
// to stop all of them, so the point of this is that it neither deadlocks nor loses an object one of them is
// still holding. Run it with -race to see the stack scan reading a stack that is genuinely stopped.
func TestCollectorUnderConcurrency(t *testing.T) {
	t.Run("interpreter", func(t *testing.T) {
		runCollectorConcurrently(t, wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(v128GCFeatures))
	})
	t.Run("compiler", func(t *testing.T) {
		if !platform.CompilerSupported() {
			t.Skip()
		}
		runCollectorConcurrently(t, wazy.NewRuntimeConfigCompiler().WithCoreFeatures(v128GCFeatures))
	})
}

func runCollectorConcurrently(t *testing.T, config wazy.RuntimeConfig) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, config)
	defer r.Close(ctx)

	compiled, err := r.CompileModule(ctx, gcLoopWasm)
	require.NoError(t, err)

	const goroutines = 8
	const links = 500

	// Every goroutine gets its own instance, so they share a store -- and therefore a heap and one
	// collector -- but not a global.
	mods := make([]api.Module, goroutines)
	for i := range mods {
		mods[i], err = r.InstantiateModule(ctx, compiled,
			wazy.NewModuleConfig().WithName(fmt.Sprintf("m%d", i)))
		require.NoError(t, err)
	}

	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mod := mods[i]
			for round := 0; round < 20; round++ {
				if _, err := mod.ExportedFunction("churn").Call(ctx, 5_000); err != nil {
					errs[i] = err
					return
				}
				// Build a chain and read it back: whatever this goroutine is holding has to survive
				// every other goroutine's collections.
				if _, err := mod.ExportedFunction("chain").Call(ctx, links); err != nil {
					errs[i] = err
					return
				}
				res, err := mod.ExportedFunction("chain_len").Call(ctx)
				if err != nil {
					errs[i] = err
					return
				}
				if res[0] != links {
					errs[i] = fmt.Errorf("round %d: chain has %d links, want %d", round, res[0], links)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}
}

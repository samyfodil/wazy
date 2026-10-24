package experimental_test

import (
	"context"
	_ "embed"
	"runtime"
	"sync"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// exnrefRetainedWasm keeps a caught exnref in an exnref-typed global, so it
// outlives the call that produced it, then re-throws it after other exceptions
// have been thrown and collected. Source: testdata/exnref_retained.wat.
//
//go:embed testdata/exnref_retained.wasm
var exnrefRetainedWasm []byte

// TestExnRef_retainedAcrossCollection is a regression test for a use-after-free
// of the shape of https://github.com/tetratelabs/wazero/issues/2522.
//
// An exnref used to be the address of the runtime's Exception object, handed to
// guest code as an integer. Nothing the collector can see roots an exception
// named only from wasm state, so a guest holding an exnref past the point where
// the runtime stopped naming it held a dangling pointer: throw_ref then read
// reclaimed Go heap, and the throw path installed that pointer as a GC root.
// Before the fix this reported the wrong tag, or failed to match any handler,
// on the large majority of runs in both engines.
func TestExnRef_retainedAcrossCollection(t *testing.T) {
	for _, tc := range []struct {
		name         string
		needCompiler bool
		config       func() wazy.RuntimeConfig
	}{
		{name: "compiler", needCompiler: true, config: wazy.NewRuntimeConfigCompiler},
		{name: "interpreter", config: wazy.NewRuntimeConfigInterpreter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needCompiler && !platform.CompilerSupported() {
				t.Skip("compiler not supported here, e.g. executable mmap is unavailable")
			}
			ctx := context.Background()
			r := wazy.NewRuntimeWithConfig(ctx,
				tc.config().WithCoreFeatures(api.CoreFeaturesV2|api.CoreFeatureExceptionHandling))
			defer r.Close(ctx)

			mod, err := r.Instantiate(ctx, exnrefRetainedWasm)
			require.NoError(t, err)

			// Retain a $t0 by reference, in a global that outlives the call.
			_, err = mod.ExportedFunction("catch_t0_ref").Call(ctx)
			require.NoError(t, err)

			// The exception is caught, so nothing the runtime tracks is still
			// in flight. Only the guest's exnref names it.
			runtime.GC()

			// Throw and catch a few thousand others, which reuse any memory
			// the retained one occupied if it was freed.
			_, err = mod.ExportedFunction("spray").Call(ctx, 4000)
			require.NoError(t, err)
			runtime.GC()

			// Per the spec it is still a $t0, so it matches the first clause.
			results, err := mod.ExportedFunction("rethrow_stashed").Call(ctx)
			require.NoError(t, err)
			require.Equal(t, uint64(0), results[0],
				"retained exnref matched the wrong tag: its exception was reused")
		})
	}
}

// exnrefCollectWasm throws and catches by reference in a loop, optionally
// holding one exnref across the churn. Source: testdata/exnref_collect.wat.
//
//go:embed testdata/exnref_collect.wasm
var exnrefCollectWasm []byte

// exnrefStructWasm throws a struct inside an exception, keeps only the exnref,
// and reads the struct back after enough allocation to force collections.
// Source: testdata/exnref_struct.wat.
//
//go:embed testdata/exnref_struct.wasm
var exnrefStructWasm []byte

// forEachEngine runs fn against both engines with features enabled.
func forEachEngine(t *testing.T, features api.CoreFeatures, fn func(t *testing.T, r wazy.Runtime)) {
	for _, tc := range []struct {
		name         string
		needCompiler bool
		config       func() wazy.RuntimeConfig
	}{
		{name: "compiler", needCompiler: true, config: wazy.NewRuntimeConfigCompiler},
		{name: "interpreter", config: wazy.NewRuntimeConfigInterpreter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needCompiler && !platform.CompilerSupported() {
				t.Skip("compiler not supported here, e.g. executable mmap is unavailable")
			}
			ctx := context.Background()
			r := wazy.NewRuntimeWithConfig(ctx, tc.config().WithCoreFeatures(features))
			defer r.Close(ctx)
			fn(t, r)
		})
	}
}

// TestExnRef_collected: an exception nothing names any more is reclaimed. A
// store used to keep every exception it ever handed an exnref out for until it
// was closed -- two for each throw a guest caught -- so a long-running guest
// that uses exceptions for control flow, as compiled C++ does, grew without
// bound. Holding one exnref through the churn checks the other half: what is
// still named survives every collection along the way.
func TestExnRef_collected(t *testing.T) {
	forEachEngine(t, api.CoreFeaturesV2|api.CoreFeatureExceptionHandling, func(t *testing.T, r wazy.Runtime) {
		ctx := context.Background()
		mod, err := r.Instantiate(ctx, exnrefCollectWasm)
		require.NoError(t, err)

		const churn = 300_000
		// Warm up, so the measurement below sees steady state rather than
		// first-call allocations.
		_, err = mod.ExportedFunction("churn").Call(ctx, 1000)
		require.NoError(t, err)

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, err = mod.ExportedFunction("churn").Call(ctx, churn)
		require.NoError(t, err)
		runtime.GC()
		runtime.ReadMemStats(&after)
		// Retaining them costs well over 100 bytes per throw; reclaiming them
		// leaves the heap where it was.
		grown := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		require.True(t, grown < churn*16, "heap grew %d bytes over %d caught exceptions", grown, churn)

		res, err := mod.ExportedFunction("hold").Call(ctx, churn)
		require.NoError(t, err)
		require.Equal(t, uint64(42), res[0], "held exnref lost its exception")
	})
}

// TestExnRef_keepsParams: an exception's params are roots for as long as the
// exception is. A struct thrown in one, then reachable only through the
// exnref a catch_ref produced, used to be invisible to the collector and was
// reclaimed under the guest.
func TestExnRef_keepsParams(t *testing.T) {
	forEachEngine(t, api.CoreFeaturesV2|api.CoreFeatureExceptionHandling|api.CoreFeatureGC, func(t *testing.T, r wazy.Runtime) {
		ctx := context.Background()
		mod, err := r.Instantiate(ctx, exnrefStructWasm)
		require.NoError(t, err)
		res, err := mod.ExportedFunction("run").Call(ctx, 200_000)
		require.NoError(t, err)
		require.Equal(t, uint64(7), res[0])
	})
}

// exnrefStashWasm keeps an exnref in a global, and exnrefCallerWasm calls its check without touching an
// exnref itself. Sources: testdata/exnref_stash.wat, testdata/exnref_caller.wat.
var (
	//go:embed testdata/exnref_stash.wasm
	exnrefStashWasm []byte
	//go:embed testdata/exnref_caller.wasm
	exnrefCallerWasm []byte
)

// TestExnRef_heldByImportedFunction: an imported wasm function runs on the stack of the call into the module
// that imports it, so the safepoints it parks at spill their roots into that stack. Here it holds an exnref
// read from a global while it spins, allocating nothing, and collections run on another goroutine. The
// importer's own code has no safepoint, and when the region a safepoint spills into was sized from the
// importer alone, the spill landed below the stack buffer, over whatever Go object came before it.
func TestExnRef_heldByImportedFunction(t *testing.T) {
	forEachEngine(t, api.CoreFeaturesV2|api.CoreFeatureExceptionHandling, func(t *testing.T, r wazy.Runtime) {
		ctx := context.Background()
		stash, err := r.InstantiateWithConfig(ctx, exnrefStashWasm, wazy.NewModuleConfig().WithName("stash"))
		require.NoError(t, err)
		caller, err := r.Instantiate(ctx, exnrefCallerWasm)
		require.NoError(t, err)

		for range 3 {
			_, err = stash.ExportedFunction("stash").Call(ctx)
			require.NoError(t, err)

			done := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				churn := stash.ExportedFunction("churn")
				for {
					select {
					case <-done:
						return
					default:
					}
					if _, err := churn.Call(ctx, 10_000); err != nil {
						panic(err)
					}
				}
			}()
			res, err := caller.ExportedFunction("check").Call(ctx, 20_000_000)
			close(done)
			wg.Wait()
			require.NoError(t, err)
			require.Equal(t, uint64(42), res[0], "exnref held by an imported function was reclaimed")
		}
	})
}

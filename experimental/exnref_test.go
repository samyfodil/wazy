package experimental_test

import (
	"context"
	_ "embed"
	"runtime"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
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
		name   string
		config wazy.RuntimeConfig
	}{
		{name: "compiler", config: wazy.NewRuntimeConfigCompiler()},
		{name: "interpreter", config: wazy.NewRuntimeConfigInterpreter()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntimeWithConfig(ctx,
				tc.config.WithCoreFeatures(api.CoreFeaturesV2|api.CoreFeatureExceptionHandling))
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

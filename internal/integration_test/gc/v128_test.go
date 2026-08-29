package gc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// A vector is the one storage type that does not fit in a GCObject's word, so a struct field or array element
// of type v128 takes two. Nothing in the WebAssembly/gc conformance suite declares one -- it is the corner
// where that suite and the SIMD proposal meet -- so this covers it directly.
//
//go:embed testdata/v128gc.wasm
var v128GCWasm []byte

const v128GCFeatures = api.CoreFeaturesV2 | api.CoreFeatureTypedFunctionReferences | api.CoreFeatureGC

func TestV128InGCTypes(t *testing.T) {
	t.Run("interpreter", func(t *testing.T) {
		runV128GC(t, wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(v128GCFeatures))
	})
	t.Run("compiler", func(t *testing.T) {
		if !platform.CompilerSupported() {
			t.Skip()
		}
		runV128GC(t, wazy.NewRuntimeConfigCompiler().WithCoreFeatures(v128GCFeatures))
	})
}

func runV128GC(t *testing.T, config wazy.RuntimeConfig) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, config)
	defer r.Close(ctx)

	mod, err := r.Instantiate(ctx, v128GCWasm)
	require.NoError(t, err)

	call := func(name string, args ...uint64) []uint64 {
		t.Helper()
		res, err := mod.ExportedFunction(name).Call(ctx, args...)
		require.NoError(t, err, name)
		return res
	}

	t.Run("a vector field round-trips both halves", func(t *testing.T) {
		require.Equal(t, []uint64{0xdeadbeef, 0xfeedface},
			call("struct_roundtrip", 0xdeadbeef, 0xfeedface))
	})

	t.Run("the fields on either side of a vector keep their own slots", func(t *testing.T) {
		require.Equal(t, []uint64{7, 9}, call("struct_neighbours"))
	})

	t.Run("a vector array element round-trips, and len counts elements not words", func(t *testing.T) {
		require.Equal(t, []uint64{0x1122, 0x1122, 4}, call("array_roundtrip", 2, 0x1122))
	})

	t.Run("new, fill and copy all move whole vector elements", func(t *testing.T) {
		// new [(5,6) x3]; fill elements 1..2 with (8,9); copy element 2 to element 0.
		require.Equal(t, []uint64{8, 9, 8}, call("array_new_fill_copy"))
	})

	t.Run("new_fixed lays out each vector element in two words", func(t *testing.T) {
		require.Equal(t, []uint64{12, 13}, call("array_fixed"))
	})
}

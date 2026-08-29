package gc

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
)

// BenchmarkGCCall measures what a GC-enabled module pays per call and per allocation, which is where the
// collector's own costs land: a root buffer per call, and a safepoint poll at every loop header.
func BenchmarkGCCall(b *testing.B) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx,
		wazy.NewRuntimeConfigCompiler().WithCoreFeatures(v128GCFeatures))
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, gcLoopWasm)
	if err != nil {
		b.Fatal(err)
	}

	// An empty call: all of this is per-call overhead, none of it collector work.
	b.Run("call", func(b *testing.B) {
		f := mod.ExportedFunction("clear")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := f.Call(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})

	// Allocating in a loop: safepoint polls, collections, and the root writes that go with them.
	for _, n := range []uint64{100, 10_000} {
		b.Run("churn", func(b *testing.B) {
			f := mod.ExportedFunction("churn")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := f.Call(ctx, n); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

var _ = api.CoreFeaturesV2

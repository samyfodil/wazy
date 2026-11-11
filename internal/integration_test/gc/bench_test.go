package gc

import (
	"context"
	"errors"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
)

// BenchmarkGCCall measures what a GC-enabled module pays per call and per allocation, which is where the
// collector's own costs land: registering every call, and parking it for a collection.
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

	// Allocating in a loop: collections, and parking for them.
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

	// Walking a chain: a ref.cast and a struct.get per link and no allocation at all, which is the heap's
	// read path on its own.
	b.Run("walk", func(b *testing.B) {
		if _, err := mod.ExportedFunction("chain").Call(ctx, 1000); err != nil {
			b.Fatal(err)
		}
		f := mod.ExportedFunction("chain_len")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := f.Call(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkGCParkLatency churns beside a call that only loops. Every collection the churn asks for waits for that
// call to park, so this is how long parking keeps a collection waiting.
func BenchmarkGCParkLatency(b *testing.B) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithCoreFeatures(parkFeatures))
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, parkWasm)
	if err != nil {
		b.Fatal(err)
	}

	countCtx, cancel := context.WithCancel(ctx)
	counted := make(chan error, 1)
	go func() {
		_, err := mod.ExportedFunction("count").Call(countCtx, 1<<62)
		counted <- err
	}()
	defer func() {
		cancel()
		if err := <-counted; !errors.Is(err, context.Canceled) {
			b.Fatal(err)
		}
	}()

	f := mod.ExportedFunction("churn")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.Call(ctx, 10_000); err != nil {
			b.Fatal(err)
		}
	}
}

var _ = api.CoreFeaturesV2

package vswazero

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/tetratelabs/wazero"
)

// constAddrWasm is a hot loop of constant-address i32 loads (see
// testdata/constaddr.wat). It is the workload that exercises wazy C21 /
// wazero #2514 — bounds-check elision for constant addresses within the
// memory minimum — which caseWasm's pure-compute fibonacci export does not.
//
//go:embed testdata/constaddr.wasm
var constAddrWasm []byte

// constAddrIters is the loop trip count per Call: large enough that per-load
// bounds-check overhead (eliminated by the optimization) dominates the fixed
// call cost.
const constAddrIters = uint64(2000)

// BenchmarkConstAddrLoads runs the constant-address-load kernel on wazy and
// wazero. Compare runtime=wazy before/after C21 for the "vs main" delta, and
// wazy vs wazero for standing on a workload upstream wazero does not yet elide.
func BenchmarkConstAddrLoads(b *testing.B) {
	ctx := context.Background()

	b.Run("runtime=wazy", func(b *testing.B) {
		r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, constAddrWasm)
		if err != nil {
			b.Fatal(err)
		}
		fn := mod.ExportedFunction("work")
		stack := make([]uint64, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0] = constAddrIters
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("runtime=wazero", func(b *testing.B) {
		r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, constAddrWasm)
		if err != nil {
			b.Fatal(err)
		}
		fn := mod.ExportedFunction("work")
		stack := make([]uint64, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0] = constAddrIters
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	})
}

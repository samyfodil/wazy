package vswazero

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/tetratelabs/wazero"
)

// dynAddrWasm is a hot loop of dynamic-address i32 loads (see
// testdata/dynaddr.wat): 8 distinct bounds-check sites per iteration, none
// elidable by #2514/C21, all sharing one trap island under #2515/C22. This
// is the workload that exercises the shared-trap-island change; caseWasm's
// fibonacci export has no memory ops and constAddrWasm's checks are elided.
//
//go:embed testdata/dynaddr.wasm
var dynAddrWasm []byte

const dynAddrIters = uint64(2000)

// BenchmarkDynAddrLoads runs the dynamic-address-load kernel on wazy and
// wazero. Compare runtime=wazy vs runtime=wazero for standing on a
// bounds-check-heavy workload.
func BenchmarkDynAddrLoads(b *testing.B) {
	ctx := context.Background()

	b.Run("runtime=wazy", func(b *testing.B) {
		r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, dynAddrWasm)
		if err != nil {
			b.Fatal(err)
		}
		fn := mod.ExportedFunction("work")
		stack := make([]uint64, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0] = dynAddrIters
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("runtime=wazero", func(b *testing.B) {
		r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, dynAddrWasm)
		if err != nil {
			b.Fatal(err)
		}
		fn := mod.ExportedFunction("work")
		stack := make([]uint64, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0] = dynAddrIters
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	})
}

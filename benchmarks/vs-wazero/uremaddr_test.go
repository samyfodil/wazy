package vswazero

import (
	"context"
	_ "embed"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/samyfodil/wazy"
)

// uremAddrWasm is a hot loop of unsigned-remainder-bounded i32 loads (see
// testdata/uremaddr.wat). F1 proves the remainder's upper bound and removes the
// otherwise-required memory bounds check.
//
//go:embed testdata/uremaddr.wasm
var uremAddrWasm []byte

const uremAddrIters = uint64(2000)

func BenchmarkURemAddrLoads(b *testing.B) {
	ctx := context.Background()

	b.Run("runtime=wazy", func(b *testing.B) {
		r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, uremAddrWasm)
		if err != nil {
			b.Fatal(err)
		}
		fn := mod.ExportedFunction("work")
		stack := make([]uint64, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0] = uremAddrIters
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("runtime=wazero", func(b *testing.B) {
		r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, uremAddrWasm)
		if err != nil {
			b.Fatal(err)
		}
		fn := mod.ExportedFunction("work")
		stack := make([]uint64, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0] = uremAddrIters
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	})
}

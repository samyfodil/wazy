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

// uremAddrRustWasm is optimized Rust/LLVM output from uremaddr_rust.rs. LLVM
// lowers the indexed byte load to `i32.load8_u(1048576 + (index % 8191))`, the
// producer-shaped address expression F1 must recognize to have practical use.
//
// Regenerate with:
//
//	rustc testdata/uremaddr_rust.rs --target wasm32-unknown-unknown \
//	  -C opt-level=3 -C panic=abort --crate-type=cdylib \
//	  -o testdata/uremaddr_rust.wasm
//
//go:embed testdata/uremaddr_rust.wasm
var uremAddrRustWasm []byte

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

func BenchmarkURemAddrRust(b *testing.B) {
	ctx := context.Background()
	const iterations = uint64(4096)

	b.Run("runtime=wazy", func(b *testing.B) {
		r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, uremAddrRustWasm)
		if err != nil {
			b.Fatal(err)
		}
		fn := mod.ExportedFunction("sum_mod")
		stack := []uint64{0, iterations}
		if err := fn.CallWithStack(ctx, stack); err != nil {
			b.Fatal(err)
		}
		if stack[0] != iterations {
			b.Fatalf("sum_mod = %d, want %d", stack[0], iterations)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0], stack[1] = 0, iterations
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("runtime=wazero", func(b *testing.B) {
		r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, uremAddrRustWasm)
		if err != nil {
			b.Fatal(err)
		}
		fn := mod.ExportedFunction("sum_mod")
		stack := []uint64{0, iterations}
		if err := fn.CallWithStack(ctx, stack); err != nil {
			b.Fatal(err)
		}
		if stack[0] != iterations {
			b.Fatalf("sum_mod = %d, want %d", stack[0], iterations)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0], stack[1] = 0, iterations
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	})
}

package vswazero

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/samyfodil/wazy"
)

// BenchmarkExecute3Heavy runs the SAME kernels/wasm as BenchmarkExecute3 but
// with iteration counts cranked up so each call runs ~10ms. At that duration
// the wasmtime-go CGo boundary (~2.6us/call) is <0.1% noise, so this is a true
// Cranelift-native vs wazy-interpreter engine comparison rather than a per-call
// call-overhead one (which BenchmarkExecute3, at ~1-150us/call, partly is).
// Trip counts stay within int32 (the non-is64 kernels take an i32 arg).
func execKernelsHeavy() []execKernel {
	return []execKernel{
		{"constaddr", constAddrWasm, "work", 12_000_000, false},
		{"dynaddr", dynAddrWasm, "work", 1_200_000, false},
		{"dispatch_mono", dispatchWasm, "mono", 7_000_000, false},
		{"dispatch_poly", dispatchWasm, "poly", 7_000_000, false},
		{"spin", spinWasm, "spin", 67_000_000, true},
	}
}

func BenchmarkExecute3Heavy(b *testing.B) {
	ctx := context.Background()
	for _, k := range execKernelsHeavy() {
		b.Run("kernel="+k.name+"/runtime=wazy", func(b *testing.B) {
			r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, k.wasm)
			if err != nil {
				b.Fatal(err)
			}
			fn := mod.ExportedFunction(k.fn)
			stack := make([]uint64, 1)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stack[0] = k.arg
				if err := fn.CallWithStack(ctx, stack); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("kernel="+k.name+"/runtime=wazero", func(b *testing.B) {
			r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, k.wasm)
			if err != nil {
				b.Fatal(err)
			}
			fn := mod.ExportedFunction(k.fn)
			stack := make([]uint64, 1)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stack[0] = k.arg
				if err := fn.CallWithStack(ctx, stack); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("kernel="+k.name+"/runtime=wasmtime", func(b *testing.B) {
			store, inst := wtInstance(b, k.wasm)
			fn := inst.GetFunc(store, k.fn)
			if fn == nil {
				b.Fatalf("no export %q", k.fn)
			}
			var arg any = int32(k.arg)
			if k.is64 {
				arg = int64(k.arg)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := fn.Call(store, arg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

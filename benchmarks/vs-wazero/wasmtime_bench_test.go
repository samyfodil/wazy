package vswazero

import (
	"context"
	"testing"

	wasmtime "github.com/bytecodealliance/wasmtime-go/v34"
	"github.com/samyfodil/wazy"
	"github.com/tetratelabs/wazero"
)

// Three-way in-process comparison: wazy vs wazero vs wasmtime (Cranelift,
// default opts). Execute uses the import-free compute kernels (caseWasm needs
// WASI+env imports, out of scope here); compile uses caseWasm (NewModule
// compiles without resolving imports). wasmtime is measured in-process and
// amortized, apples-to-apples with the wazy/wazero arms.

func wtCompile(tb testing.TB, engine *wasmtime.Engine, wasm []byte) *wasmtime.Module {
	m, err := wasmtime.NewModule(engine, wasm)
	if err != nil {
		tb.Fatal(err)
	}
	return m
}

func wtInstance(tb testing.TB, wasm []byte) (*wasmtime.Store, *wasmtime.Instance) {
	engine := wasmtime.NewEngine()
	store := wasmtime.NewStore(engine)
	inst, err := wasmtime.NewInstance(store, wtCompile(tb, engine, wasm), nil)
	if err != nil {
		tb.Fatal(err)
	}
	return store, inst
}

// BenchmarkCompile3 compiles caseWasm (90 funcs) on all three runtimes.
func BenchmarkCompile3(b *testing.B) {
	ctx := context.Background()
	b.Run("runtime=wazy", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
			if _, err := r.CompileModule(ctx, caseWasm); err != nil {
				b.Fatal(err)
			}
			r.Close(ctx)
		}
	})
	b.Run("runtime=wazero", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
			if _, err := r.CompileModule(ctx, caseWasm); err != nil {
				b.Fatal(err)
			}
			r.Close(ctx)
		}
	})
	b.Run("runtime=wasmtime", func(b *testing.B) {
		engine := wasmtime.NewEngine()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := wasmtime.NewModule(engine, caseWasm); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// execKernel is an import-free compute kernel: single-arg loop returning a value.
type execKernel struct {
	name string
	wasm []byte
	fn   string
	arg  uint64 // passed as-is to wazy/wazero stack; typed below for wasmtime
	is64 bool   // param/result i64 (else i32)
}

func execKernels() []execKernel {
	return []execKernel{
		{"constaddr", constAddrWasm, "work", constAddrIters, false},
		{"dynaddr", dynAddrWasm, "work", dynAddrIters, false},
		{"dispatch_mono", dispatchWasm, "mono", dispatchIters, false},
		{"dispatch_poly", dispatchWasm, "poly", dispatchIters, false},
		{"spin", spinWasm, "spin", spinIters, true},
	}
}

func BenchmarkExecute3(b *testing.B) {
	ctx := context.Background()
	for _, k := range execKernels() {
		k := k
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
			var arg interface{} = int32(k.arg)
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

package vswazero

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/tetratelabs/wazero"
)

// dispatchWasm is a call_indirect dispatch kernel (see testdata/dispatch.wat):
// eight small same-typed "virtual methods" in a funcref table, driven by three
// hot loops — mono (always slot 0), poly (slot i&7), direct (direct call $m0).
// (mono - direct) is the ceiling a monomorphic inline cache can recover; poly
// is the megamorphic case an IC must not regress.
//
//go:embed testdata/dispatch.wasm
var dispatchWasm []byte

const dispatchIters = uint64(100000)

func benchDispatch(b *testing.B, export string) {
	ctx := context.Background()
	b.Run("runtime=wazy", func(b *testing.B) {
		r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, dispatchWasm)
		if err != nil {
			b.Fatal(err)
		}
		fn := mod.ExportedFunction(export)
		stack := make([]uint64, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0] = dispatchIters
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("runtime=wazero", func(b *testing.B) {
		r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, dispatchWasm)
		if err != nil {
			b.Fatal(err)
		}
		fn := mod.ExportedFunction(export)
		stack := make([]uint64, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0] = dispatchIters
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkDispatchMono(b *testing.B)        { benchDispatch(b, "mono") }
func BenchmarkDispatchPoly(b *testing.B)        { benchDispatch(b, "poly") }
func BenchmarkDispatchDirect(b *testing.B)      { benchDispatch(b, "direct") }
func BenchmarkDispatchMonoHeavy(b *testing.B)   { benchDispatch(b, "mono_heavy") }
func BenchmarkDispatchDirectHeavy(b *testing.B) { benchDispatch(b, "direct_heavy") }

package vswazero

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/samyfodil/wazy"
)

// BenchmarkCase3Interp is BenchmarkCase3 restricted to the *interpreter* engine
// (both runtimes), so it measures wazy's interpreter tier against wazero's on
// the same real Go-compiled exports. No wasmtime line: wasmtime is compiled
// only. Correctness/host-call framing is the same as BenchmarkCase3 (see
// TestCase3Parity and the caseExports doc).
func BenchmarkCase3Interp(b *testing.B) {
	ctx := context.Background()
	for _, e := range caseExports {
		e := e
		b.Run("fn="+e.name+"/runtime=wazy", func(b *testing.B) {
			r := newCaseRuntimeWazyCfg(b, wazy.NewRuntimeConfigInterpreter())
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, caseWasm)
			if err != nil {
				b.Fatal(err)
			}
			fn := mod.ExportedFunction(e.name)
			stack := make([]uint64, 1)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stack[0] = uint64(uint32(e.arg))
				if err := fn.CallWithStack(ctx, stack); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("fn="+e.name+"/runtime=wazero", func(b *testing.B) {
			r := newCaseRuntimeWazeroCfg(b, wazero.NewRuntimeConfigInterpreter())
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, caseWasm)
			if err != nil {
				b.Fatal(err)
			}
			fn := mod.ExportedFunction(e.name)
			stack := make([]uint64, 1)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stack[0] = uint64(uint32(e.arg))
				if err := fn.CallWithStack(ctx, stack); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

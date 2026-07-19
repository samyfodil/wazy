package bench

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/wasm"
)

func BenchmarkMemoryGrowNativeFastPath(b *testing.B) {
	if !platform.CompilerSupported() {
		b.Skip()
	}
	const growsPerCall = 100
	body := make([]byte, 0, growsPerCall*5+1)
	for range growsPerCall {
		body = append(body,
			wasm.OpcodeI32Const, 0,
			wasm.OpcodeMemoryGrow, 0,
			wasm.OpcodeDrop,
		)
	}
	body = append(body, wasm.OpcodeEnd)

	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithMemoryCapacityFromMax(true))
	b.Cleanup(func() { r.Close(ctx) })
	mod, err := r.Instantiate(ctx, binaryencoding.EncodeModule(&wasm.Module{
		TypeSection:     []wasm.FunctionType{{}},
		FunctionSection: []wasm.Index{0},
		MemorySection:   &wasm.Memory{Min: 1, Cap: 2, Max: 2, IsMaxEncoded: true},
		ExportSection:   []wasm.Export{{Name: "grow", Type: wasm.ExternTypeFunc, Index: 0}},
		CodeSection:     []wasm.Code{{Body: body}},
	}))
	if err != nil {
		b.Fatal(err)
	}
	fn := mod.ExportedFunction("grow")
	stack := make([]uint64, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err = fn.CallWithStack(ctx, stack); err != nil {
			b.Fatal(err)
		}
	}
}

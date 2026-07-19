package bench

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/wasm"
)

// memoryGrowRustWasm is optimized rustc/LLVM output from
// testdata/memory_grow_rust.rs.
//
//go:embed testdata/memory_grow_rust.wasm
var memoryGrowRustWasm []byte

const (
	rustAllocationChunkSize = 64 << 10
	rustAllocationChunks    = 64
	rustAllocationChecksum  = rustAllocationChunks * (rustAllocationChunks - 1) / 2
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

func TestMemoryGrowRustAllocatorFixture(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithMemoryCapacityFromMax(true))
	t.Cleanup(func() {
		if err := r.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	mod, err := r.Instantiate(ctx, memoryGrowRustWasm)
	if err != nil {
		t.Fatal(err)
	}
	initialSize := mod.Memory().Size()
	results, err := mod.ExportedFunction("allocate").Call(ctx, rustAllocationChunkSize, rustAllocationChunks)
	if err != nil {
		t.Fatal(err)
	}
	if want, have := uint64(rustAllocationChecksum), results[0]; want != have {
		t.Fatalf("unexpected checksum: want %d, have %d", want, have)
	}
	if grownSize := mod.Memory().Size(); grownSize <= initialSize {
		t.Fatalf("Rust allocator did not grow memory: initial=%d, final=%d", initialSize, grownSize)
	}
}

func BenchmarkMemoryGrowRustAllocator(b *testing.B) {
	if !platform.CompilerSupported() {
		b.Skip()
	}
	for _, tc := range []struct {
		name   string
		config wazy.RuntimeConfig
	}{
		{name: "reserve_pages=0", config: wazy.NewRuntimeConfigCompiler()},
		{name: "reserve_pages=16", config: wazy.NewRuntimeConfigCompiler().WithMemoryCapacityReservePages(16)},
		{name: "reserve_pages=64", config: wazy.NewRuntimeConfigCompiler().WithMemoryCapacityReservePages(64)},
		{name: "reserve_pages=128", config: wazy.NewRuntimeConfigCompiler().WithMemoryCapacityReservePages(128)},
		{name: "reserve_max", config: wazy.NewRuntimeConfigCompiler().WithMemoryCapacityFromMax(true)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchmarkMemoryGrowRustAllocator(b, tc.config)
		})
	}
}

func benchmarkMemoryGrowRustAllocator(b *testing.B, config wazy.RuntimeConfig) {
	ctx := context.Background()
	b.ReportAllocs()
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		r := wazy.NewRuntimeWithConfig(ctx, config)
		mod, err := r.Instantiate(ctx, memoryGrowRustWasm)
		if err != nil {
			b.Fatal(err)
		}
		allocate := mod.ExportedFunction("allocate")

		b.StartTimer()
		results, err := allocate.Call(ctx, rustAllocationChunkSize, rustAllocationChunks)
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
		if want, have := uint64(rustAllocationChecksum), results[0]; want != have {
			b.Fatalf("unexpected checksum: want %d, have %d", want, have)
		}
		if err = r.Close(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

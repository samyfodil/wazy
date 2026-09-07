package vswazero

import (
	"context"
	_ "embed"
	"fmt"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/samyfodil/wazy"
)

// memFillWasm holds memory.fill kernels over a 5 MiB memory; see
// testdata/memfill.wat. It covers the three lowering paths: constant-size
// inline stores, the runtime-length loop, and large zero fills.
//
//go:embed testdata/memfill.wasm
var memFillWasm []byte

// memFillSizes are the dynamic fill lengths swept by BenchmarkMemoryFill.
// They straddle every threshold the lowering has: the byte tail, the 16-byte
// loop, the 64-byte main loop, and the memclr cutover.
// Sizes straddling every threshold, including n%16 == 15 (79, 127), where the
// byte-at-a-time tail an overlapping vector store replaces was longest.
var memFillSizes = []uint32{15, 31, 64, 79, 100, 127, 256, 512, 768, 1024, 4096, 65536, 1 << 20, 4 << 20}

// memFillIters keeps each Call long enough to swamp the call overhead without
// making the multi-megabyte rows take minutes.
func memFillIters(size uint32) uint32 {
	switch {
	case size >= 1<<20:
		return 4
	case size >= 4096:
		return 64
	default:
		return 2000
	}
}

func BenchmarkMemoryFill(b *testing.B) {
	ctx := context.Background()

	// value 0 takes the zero-fill fast path; 0xAB is the non-zero path.
	for _, v := range []uint32{0, 0xAB} {
		for _, size := range memFillSizes {
			iters := memFillIters(size)
			name := fmt.Sprintf("value=%#x/size=%d", v, size)
			b.Run(name, func(b *testing.B) {
				b.Run("runtime=wazy", func(b *testing.B) {
					r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
					defer r.Close(ctx)
					mod, err := r.Instantiate(ctx, memFillWasm)
					if err != nil {
						b.Fatal(err)
					}
					benchFill(b, mod.ExportedFunction("fill").CallWithStack, size, v, iters)
				})
				b.Run("runtime=wazero", func(b *testing.B) {
					r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
					defer r.Close(ctx)
					mod, err := r.Instantiate(ctx, memFillWasm)
					if err != nil {
						b.Fatal(err)
					}
					benchFill(b, mod.ExportedFunction("fill").CallWithStack, size, v, iters)
				})
			})
		}
	}
}

// BenchmarkMemoryFillUnaligned repeats the sweep at a misaligned destination,
// where an implementation that assumes 16-byte alignment would show up.
func BenchmarkMemoryFillUnaligned(b *testing.B) {
	ctx := context.Background()
	for _, size := range []uint32{64, 100, 1024, 65536} {
		iters := memFillIters(size)
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			b.Run("runtime=wazy", func(b *testing.B) {
				r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
				defer r.Close(ctx)
				mod, err := r.Instantiate(ctx, memFillWasm)
				if err != nil {
					b.Fatal(err)
				}
				benchFill(b, mod.ExportedFunction("fill_unaligned").CallWithStack, size, 0, iters)
			})
			b.Run("runtime=wazero", func(b *testing.B) {
				r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
				defer r.Close(ctx)
				mod, err := r.Instantiate(ctx, memFillWasm)
				if err != nil {
					b.Fatal(err)
				}
				benchFill(b, mod.ExportedFunction("fill_unaligned").CallWithStack, size, 0, iters)
			})
		})
	}
}

// BenchmarkMemoryFillConst covers the constant-size clears a producer emits
// for struct and array zero-init, which take the inline-store lowering.
func BenchmarkMemoryFillConst(b *testing.B) {
	ctx := context.Background()
	const iters = uint32(2000)
	for _, export := range []string{"fill_c16", "fill_c64", "fill_c128", "fill_c256", "fill_c1024"} {
		b.Run(export, func(b *testing.B) {
			b.Run("runtime=wazy", func(b *testing.B) {
				r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
				defer r.Close(ctx)
				mod, err := r.Instantiate(ctx, memFillWasm)
				if err != nil {
					b.Fatal(err)
				}
				benchFillConst(b, mod.ExportedFunction(export).CallWithStack, iters)
			})
			b.Run("runtime=wazero", func(b *testing.B) {
				r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
				defer r.Close(ctx)
				mod, err := r.Instantiate(ctx, memFillWasm)
				if err != nil {
					b.Fatal(err)
				}
				benchFillConst(b, mod.ExportedFunction(export).CallWithStack, iters)
			})
		})
	}
}

type callWithStack = func(context.Context, []uint64) error

func benchFill(b *testing.B, call callWithStack, size, value, iters uint32) {
	b.Helper()
	stack := make([]uint64, 3)
	b.SetBytes(int64(size) * int64(iters))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stack[0], stack[1], stack[2] = uint64(size), uint64(value), uint64(iters)
		if err := call(context.Background(), stack); err != nil {
			b.Fatal(err)
		}
	}
}

func benchFillConst(b *testing.B, call callWithStack, iters uint32) {
	b.Helper()
	stack := make([]uint64, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stack[0] = uint64(iters)
		if err := call(context.Background(), stack); err != nil {
			b.Fatal(err)
		}
	}
}

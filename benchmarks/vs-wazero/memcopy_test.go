package vswazero

import (
	"context"
	_ "embed"
	"fmt"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/samyfodil/wazy"
)

// memCopyWasm holds memory.copy kernels; see testdata/memcopy.wat. memory.copy
// reaches the Go runtime's memmove through the same call sequence memory.fill
// uses for memclr, so this guards that shared path.
//
//go:embed testdata/memcopy.wasm
var memCopyWasm []byte

var memCopySizes = []uint32{16, 64, 256, 1024, 4096, 65536, 1 << 20}

func memCopyIters(size uint32) uint32 {
	switch {
	case size >= 1<<20:
		return 8
	case size >= 4096:
		return 64
	default:
		return 2000
	}
}

func BenchmarkMemoryCopy(b *testing.B) {
	ctx := context.Background()
	for _, size := range memCopySizes {
		iters := memCopyIters(size)
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			b.Run("runtime=wazy", func(b *testing.B) {
				r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
				defer r.Close(ctx)
				mod, err := r.Instantiate(ctx, memCopyWasm)
				if err != nil {
					b.Fatal(err)
				}
				benchCopy(b, mod.ExportedFunction("copy").CallWithStack, size, iters)
			})
			b.Run("runtime=wazero", func(b *testing.B) {
				r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
				defer r.Close(ctx)
				mod, err := r.Instantiate(ctx, memCopyWasm)
				if err != nil {
					b.Fatal(err)
				}
				benchCopy(b, mod.ExportedFunction("copy").CallWithStack, size, iters)
			})
		})
	}
}

// BenchmarkMemoryCopyConst covers the constant-size copies a producer emits.
func BenchmarkMemoryCopyConst(b *testing.B) {
	ctx := context.Background()
	const iters = uint32(2000)
	for _, export := range []string{"copy_c64", "copy_c4096"} {
		b.Run(export, func(b *testing.B) {
			b.Run("runtime=wazy", func(b *testing.B) {
				r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
				defer r.Close(ctx)
				mod, err := r.Instantiate(ctx, memCopyWasm)
				if err != nil {
					b.Fatal(err)
				}
				benchFillConst(b, mod.ExportedFunction(export).CallWithStack, iters)
			})
			b.Run("runtime=wazero", func(b *testing.B) {
				r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
				defer r.Close(ctx)
				mod, err := r.Instantiate(ctx, memCopyWasm)
				if err != nil {
					b.Fatal(err)
				}
				benchFillConst(b, mod.ExportedFunction(export).CallWithStack, iters)
			})
		})
	}
}

func benchCopy(b *testing.B, call callWithStack, size, iters uint32) {
	b.Helper()
	stack := make([]uint64, 2)
	b.SetBytes(int64(size) * int64(iters))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stack[0], stack[1] = uint64(size), uint64(iters)
		if err := call(context.Background(), stack); err != nil {
			b.Fatal(err)
		}
	}
}

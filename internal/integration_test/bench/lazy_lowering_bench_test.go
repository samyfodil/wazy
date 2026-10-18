package bench

import (
	"context"
	"os"
	"runtime"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"
)

// BenchmarkInterpreterLazyLowering compares eager and lazy lowering on WASI
// programs that run one short command, so only part of their code executes.
// Each iteration compiles the module in a new interpreter runtime and runs it.
// retained-KiB is the heap held after the run while the compiled module is
// alive. Set WAZY_LAZY_BENCH_WASM to a wasip1 binary to add it as "extra".
func BenchmarkInterpreterLazyLowering(b *testing.B) {
	modules := []struct{ name, path string }{
		{"zig-cc", "../../../imports/wasi_snapshot_preview1/testdata/zig-cc/wasi.wasm"},
		{"tinygo", "../../../imports/wasi_snapshot_preview1/testdata/tinygo/wasi.wasm"},
		{"cargo-wasi", "../../../imports/wasi_snapshot_preview1/testdata/cargo-wasi/wasi.wasm"},
	}
	if p := os.Getenv("WAZY_LAZY_BENCH_WASM"); p != "" {
		modules = append(modules, struct{ name, path string }{"extra", p})
	}
	modes := []struct {
		name string
		ctx  context.Context
	}{
		{"eager", testCtx},
		{"lazy", api.WithInterpreterLazyLowering(testCtx)},
	}
	for _, m := range modules {
		bin, err := os.ReadFile(m.path)
		if err != nil {
			b.Fatal(err)
		}
		for _, mode := range modes {
			b.Run(m.name+"/"+mode.name, func(b *testing.B) {
				ctx := mode.ctx
				root := b.TempDir()

				// Measure retained memory in an untimed run, as it forces GCs.
				before := heapInUse()
				r, compiled := compileAndRunWASI(b, ctx, bin, root)
				retained := heapInUse() - before
				runtime.KeepAlive(compiled)
				_ = r.Close(ctx)

				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					r, _ := compileAndRunWASI(b, ctx, bin, root)
					_ = r.Close(ctx)
				}
				b.StopTimer()
				b.ReportMetric(float64(retained)/1024, "retained-KiB")
			})
		}
	}
}

// compileAndRunWASI compiles bin in a new interpreter runtime and runs its
// "stat" command with root mounted as "/".
func compileAndRunWASI(b *testing.B, ctx context.Context, bin []byte, root string) (wazy.Runtime, wazy.CompiledModule) {
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	compiled, err := r.CompileModule(ctx, bin)
	if err != nil {
		b.Fatal(err)
	}
	cfg := wazy.NewModuleConfig().WithArgs("wasi", "stat").WithFSConfig(wazy.NewFSConfig().WithDirMount(root, "/"))
	if _, err = r.InstantiateModule(ctx, compiled, cfg); err != nil {
		b.Fatal(err)
	}
	return r, compiled
}

func heapInUse() int64 {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return int64(stats.HeapAlloc)
}

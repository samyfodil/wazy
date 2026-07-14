package filecache

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/wasm"
)

// benchCacheModule builds a module with many distinct function-type signatures
// so that the per-type entry-preamble work (A10) is a meaningful fraction of a
// cache-hit reconstruction.
func benchCacheModule() []byte {
	const nTypes = 16
	types := make([]wasm.FunctionType, nTypes)
	for i := range types {
		params := make([]wasm.ValueType, i%8)
		for j := range params {
			params[j] = wasm.ValueTypeI32
		}
		results := make([]wasm.ValueType, i%4)
		for j := range results {
			results[j] = wasm.ValueTypeI64
		}
		types[i] = wasm.FunctionType{
			Params:  params,
			Results: results,
		}
	}

	const nFuncs = 128
	funcs := make([]wasm.Index, nFuncs)
	code := make([]wasm.Code, nFuncs)
	for i := range funcs {
		funcs[i] = wasm.Index(i % nTypes)
		typ := types[i%nTypes]
		body := []byte{}
		for range typ.Results {
			body = append(body, wasm.OpcodeI64Const, 0)
		}
		body = append(body, wasm.OpcodeEnd)
		code[i] = wasm.Code{Body: body}
	}

	return binaryencoding.EncodeModule(&wasm.Module{
		TypeSection:     types,
		FunctionSection: funcs,
		CodeSection:     code,
	})
}

// BenchmarkCompileModuleCacheHit measures a file-cache hit: a fresh runtime
// (cold in-memory cache) compiling a module already present in the on-disk
// cache. This is the A10 path -- deserialize + entry-preamble reconstruction.
// allocs/op is the low-noise signal: A10 removes the per-hit SSA builder +
// backend compiler + machine allocations.
func BenchmarkCompileModuleCacheHit(b *testing.B) {
	if !platform.CompilerSupported() {
		b.Skip()
	}
	ctx := context.Background()
	dir := b.TempDir()
	bin := benchCacheModule()
	config := wazy.NewRuntimeConfigCompiler()

	// Warm the on-disk cache once.
	{
		cc, err := wazy.NewCompilationCacheWithDir(dir)
		if err != nil {
			b.Fatal(err)
		}
		r := wazy.NewRuntimeWithConfig(ctx, config.WithCompilationCache(cc))
		if _, err := r.CompileModule(ctx, bin); err != nil {
			b.Fatal(err)
		}
		_ = r.Close(ctx)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cc, err := wazy.NewCompilationCacheWithDir(dir)
		if err != nil {
			b.Fatal(err)
		}
		r := wazy.NewRuntimeWithConfig(ctx, config.WithCompilationCache(cc))
		if _, err := r.CompileModule(ctx, bin); err != nil {
			b.Fatal(err)
		}
		_ = r.Close(ctx)
	}
}

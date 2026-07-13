package bench

// Benchmark for finding A1: mod.ExportedFunction(name).Call(ctx, ...) without
// caching the api.Function handle across iterations. This is the single most
// common per-request call shape (e.g. an HTTP handler doing
// mod.ExportedFunction("handle").Call(ctx, ...) on every request), and
// ModuleInstance.ExportedFunction always calls Engine.NewFunction fresh --
// wazevo's moduleEngine.NewFunction builds a brand new *callEngine on every
// such call, which before A1 unconditionally allocated a fresh ~10KB wasm
// stack (among other things) in (*callEngine).init on every single call. See
// internal/engine/wazevo/stack_pool.go for the pool that replaces that.
//
// B/op and allocs/op are what matter here (allocs/op is load-immune, unlike
// ns/op, which is why OPTIMIZATIONS.md's A1 entry uses it as the reliable
// measure): immediately before A1, this benchmark measured 11784 B/op,
// 3 allocs/op (callEngine struct + the wasm stack + the param/result slice,
// one make() each -- see (*callEngine).Call and moduleEngine.NewFunction).
// After A1 (pooling the wasm stack -- see stack_pool.go), it measures
// ~1551 B/op, 2 allocs/op: the callEngine struct and the param/result slice
// remain (this pass deliberately did not pool the callEngine struct itself,
// only its dominant-cost stack -- see stack_pool.go's package doc), but the
// ~10KB stack allocation is gone from the steady state.

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/wasm"
)

// BenchmarkExportedFunctionCall_FreshHandle is the win-metric benchmark: a
// trivial leaf function (identity on an i32), so the measured cost is
// dominated by ExportedFunction+Call's own overhead -- callEngine
// construction, the wasm stack (de)allocation, and the param/result slice --
// rather than actual wasm execution time.
func BenchmarkExportedFunctionCall_FreshHandle(b *testing.B) {
	if !platform.CompilerSupported() {
		b.Skip()
	}
	mod := instantiateExportedFunctionCallBenchModule(b)

	// Built once, spread (not passed as individual literal args) at each call
	// below: Call's params parameter is variadic (...uint64), and Call
	// itself is invoked through the api.Function *interface* (dynamic
	// dispatch, so the compiler can't devirtualize and prove the argument
	// doesn't escape). Spreading a pre-existing slice with `params...`
	// passes it through as-is with no new allocation; passing individual
	// literal args like `Call(ctx, 42)` instead would make the compiler
	// synthesize a fresh backing array for them on *every* call (a
	// benchmark-harness artifact, not a cost every real caller pays -- most
	// real call sites either already hold a []uint64 or, like this
	// benchmark, can hoist construction of one out of the hot path).
	params := []uint64{42}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Deliberately re-resolving the export every iteration (rather than
		// caching the api.Function handle, as BenchmarkInvocation's fib
		// benches and BenchmarkHostFunctionCall do to isolate the *already
		// warm* callEngine path) -- this is the actual pattern A1 targets.
		res, err := mod.ExportedFunction("f").Call(testCtx, params...)
		if err != nil {
			b.Fatal(err)
		}
		if res[0] != 42 {
			b.Fatalf("unexpected result: %d", res[0])
		}
	}
}

func instantiateExportedFunctionCallBenchModule(tb testing.TB) *wasm.ModuleInstance {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	tb.Cleanup(func() { r.Close(ctx) })

	bin := binaryencoding.EncodeModule(&wasm.Module{
		TypeSection: []wasm.FunctionType{{
			Params:  []wasm.ValueType{wasm.ValueTypeI32},
			Results: []wasm.ValueType{wasm.ValueTypeI32},
		}},
		FunctionSection: []wasm.Index{0},
		ExportSection:   []wasm.Export{{Name: "f", Type: wasm.ExternTypeFunc, Index: 0}},
		Exports: map[string]*wasm.Export{
			"f": {Name: "f", Type: wasm.ExternTypeFunc, Index: 0},
		},
		CodeSection: []wasm.Code{
			{Body: []byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeEnd}},
		},
	})

	mod, err := r.Instantiate(ctx, bin)
	if err != nil {
		tb.Fatal(err)
	}
	return mod.(*wasm.ModuleInstance)
}

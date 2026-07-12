package vswazero

import (
	"context"
	"math"
	"testing"

	"github.com/samyfodil/wazy"
	wazyapi "github.com/samyfodil/wazy/api"
	wazywasi "github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"
)

// setupHostCallWazy builds the host-call fixture on wazy. The "go" host
// function is registered with WithGoModuleFunction (raw stack); "go-typed" is
// registered with wazy.HostFunc1, the compile-time-typed, reflection-free
// helper that is wazy's headline addition over upstream.
func setupHostCallWazy(tb testing.TB) hostCallEnv {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())

	const i32, f32 = wazyapi.ValueTypeI32, wazyapi.ValueTypeF32
	hb := r.NewHostModuleBuilder("host")
	hb.NewFunctionBuilder().WithGoModuleFunction(wazyapi.GoModuleFunc(func(ctx context.Context, mod wazyapi.Module, stack []uint64) {
		v, ok := mod.Memory().ReadUint32Le(uint32(stack[0]))
		if !ok {
			panic("couldn't read memory")
		}
		stack[0] = uint64(v)
	}), []wazyapi.ValueType{i32}, []wazyapi.ValueType{f32}).Export("go")
	wazy.HostFunc1(hb.NewFunctionBuilder(), func(ctx context.Context, m wazyapi.Module, pos uint32) float32 {
		v, ok := m.Memory().ReadUint32Le(pos)
		if !ok {
			panic("couldn't read memory")
		}
		return math.Float32frombits(v)
	}).Export("go-typed")
	if _, err := hb.Instantiate(ctx); err != nil {
		tb.Fatal(err)
	}

	mod, err := r.Instantiate(ctx, hostcallWasm)
	if err != nil {
		tb.Fatal(err)
	}
	return hostCallEnv{
		fns: map[string]benchFn{
			"gomodule": mod.ExportedFunction(callGoHostName),
			"typed":    mod.ExportedFunction(callGoTypedHostName),
		},
		writeMem: func(offset, v uint32) {
			if !mod.Memory().WriteUint32Le(offset, v) {
				tb.Fatal("couldn't write memory")
			}
		},
		close: func() { r.Close(ctx) },
	}
}

// newCaseRuntimeWazy returns a wazy runtime (compiler engine) with the imports
// caseWasm needs: a no-op env.get_random_string stub and WASI. The stub is
// never invoked by the benchmarked exports (fibonacci is pure); it only has to
// exist so instantiation resolves the import.
func newCaseRuntimeWazy(tb testing.TB) wazy.Runtime {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	hb := r.NewHostModuleBuilder("env")
	hb.NewFunctionBuilder().WithGoModuleFunction(wazyapi.GoModuleFunc(func(ctx context.Context, mod wazyapi.Module, stack []uint64) {
	}), []wazyapi.ValueType{wazyapi.ValueTypeI32, wazyapi.ValueTypeI32}, nil).Export("get_random_string")
	if _, err := hb.Instantiate(ctx); err != nil {
		tb.Fatal(err)
	}
	wazywasi.MustInstantiate(ctx, r)
	return r
}

// benchCompileWazy compiles caseWasm on a fresh runtime each iteration (no
// compilation cache) so every iteration truly recompiles.
func benchCompileWazy(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
		if _, err := r.CompileModule(ctx, caseWasm); err != nil {
			b.Fatal(err)
		}
		r.Close(ctx)
	}
}

// anonWazyConfig is the module config used for instantiation: WithName("")
// keeps each instance anonymous (so it can be instantiated repeatedly) and
// WithStartFunctions() (no names) skips _start so only instantiation is timed.
func anonWazyConfig() wazy.ModuleConfig {
	return wazy.NewModuleConfig().WithName("").WithStartFunctions()
}

// benchInstantiateWazy measures per-instance cost: caseWasm is compiled once,
// then instantiated repeatedly.
func benchInstantiateWazy(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	r := newCaseRuntimeWazy(b)
	defer r.Close(ctx)
	compiled, err := r.CompileModule(ctx, caseWasm)
	if err != nil {
		b.Fatal(err)
	}
	cfg := anonWazyConfig()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mod, err := r.InstantiateModule(ctx, compiled, cfg)
		if err != nil {
			b.Fatal(err)
		}
		mod.Close(ctx)
	}
}

// setupExecuteWazy instantiates caseWasm (running _start for TinyGo init) and
// returns its fibonacci export for the pure-execution benchmark.
func setupExecuteWazy(tb testing.TB) (benchFn, func()) {
	ctx := context.Background()
	r := newCaseRuntimeWazy(tb)
	mod, err := r.Instantiate(ctx, caseWasm)
	if err != nil {
		r.Close(ctx)
		tb.Fatal(err)
	}
	return mod.ExportedFunction("fibonacci"), func() { r.Close(ctx) }
}

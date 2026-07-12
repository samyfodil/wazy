package vswazero

import (
	"context"
	"math"
	"testing"

	"github.com/tetratelabs/wazero"
	wazeroapi "github.com/tetratelabs/wazero/api"
	wazerowasi "github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// setupHostCallWazero builds the host-call fixture on upstream wazero. The "go"
// host function uses WithGoModuleFunction (identical to wazy); "go-typed" uses
// WithFunc, wazero's reflection-based typed registration. This is the direct
// counterpart to wazy's HostFunc1 and the point of the headline comparison.
func setupHostCallWazero(tb testing.TB) hostCallEnv {
	ctx := context.Background()
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())

	const i32, f32 = wazeroapi.ValueTypeI32, wazeroapi.ValueTypeF32
	hb := r.NewHostModuleBuilder("host")
	hb.NewFunctionBuilder().WithGoModuleFunction(wazeroapi.GoModuleFunc(func(ctx context.Context, mod wazeroapi.Module, stack []uint64) {
		v, ok := mod.Memory().ReadUint32Le(uint32(stack[0]))
		if !ok {
			panic("couldn't read memory")
		}
		stack[0] = uint64(v)
	}), []wazeroapi.ValueType{i32}, []wazeroapi.ValueType{f32}).Export("go")
	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m wazeroapi.Module, pos uint32) float32 {
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

// newCaseRuntimeWazero is the wazero counterpart of newCaseRuntimeWazy.
func newCaseRuntimeWazero(tb testing.TB) wazero.Runtime {
	ctx := context.Background()
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
	hb := r.NewHostModuleBuilder("env")
	hb.NewFunctionBuilder().WithGoModuleFunction(wazeroapi.GoModuleFunc(func(ctx context.Context, mod wazeroapi.Module, stack []uint64) {
	}), []wazeroapi.ValueType{wazeroapi.ValueTypeI32, wazeroapi.ValueTypeI32}, nil).Export("get_random_string")
	if _, err := hb.Instantiate(ctx); err != nil {
		tb.Fatal(err)
	}
	wazerowasi.MustInstantiate(ctx, r)
	return r
}

func benchCompileWazero(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
		if _, err := r.CompileModule(ctx, caseWasm); err != nil {
			b.Fatal(err)
		}
		r.Close(ctx)
	}
}

// anonWazeroConfig mirrors anonWazyConfig for wazero. See its doc comment.
func anonWazeroConfig() wazero.ModuleConfig {
	return wazero.NewModuleConfig().WithName("").WithStartFunctions()
}

func benchInstantiateWazero(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	r := newCaseRuntimeWazero(b)
	defer r.Close(ctx)
	compiled, err := r.CompileModule(ctx, caseWasm)
	if err != nil {
		b.Fatal(err)
	}
	cfg := anonWazeroConfig()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mod, err := r.InstantiateModule(ctx, compiled, cfg)
		if err != nil {
			b.Fatal(err)
		}
		mod.Close(ctx)
	}
}

func setupExecuteWazero(tb testing.TB) (benchFn, func()) {
	ctx := context.Background()
	r := newCaseRuntimeWazero(tb)
	mod, err := r.Instantiate(ctx, caseWasm)
	if err != nil {
		r.Close(ctx)
		tb.Fatal(err)
	}
	return mod.ExportedFunction("fibonacci"), func() { r.Close(ctx) }
}

package bench

import (
	"context"
	_ "embed"
	"encoding/binary"
	"math"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

const (
	// callGoHostName is the name of exported function which calls the
	// Go-implemented host function registered via WithGoModuleFunction.
	callGoHostName = "call_go_host"
	// callGoTypedHostName is the name of exported function which calls the
	// Go-implemented host function registered via wazy.HostFunc1.
	callGoTypedHostName = "call_go_typed_host"
)

// BenchmarkHostFunctionCall measures the cost of host function calls whose target functions are either
// Go-implemented or Wasm-implemented, and compare the results between them.
func BenchmarkHostFunctionCall(b *testing.B) {
	if !platform.CompilerSupported() {
		b.Skip()
	}

	m := setupHostCallBench(func(err error) {
		if err != nil {
			b.Fatal(err)
		}
	})

	const offset = uint64(100)
	const val = float32(1.1234)

	binary.LittleEndian.PutUint32(m.MemoryInstance.Buffer[offset:], math.Float32bits(val))

	for _, fn := range []string{callGoHostName, callGoTypedHostName} {
		b.Run(fn, func(b *testing.B) {
			ce := getCallEngine(m, fn)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := ce.Call(testCtx, offset)
				if err != nil {
					b.Fatal(err)
				}
				if uint32(res[0]) != math.Float32bits(val) {
					b.Fail()
				}
			}
		})

		b.Run(fn+"_with_stack", func(b *testing.B) {
			ce := getCallEngine(m, fn)

			b.ResetTimer()
			stack := make([]uint64, 1)
			for i := 0; i < b.N; i++ {
				stack[0] = offset
				err := ce.CallWithStack(testCtx, stack)
				if err != nil {
					b.Fatal(err)
				}
				if uint32(stack[0]) != math.Float32bits(val) {
					b.Fail()
				}
			}
		})
	}
}

func TestBenchmarkFunctionCall(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}

	m := setupHostCallBench(func(err error) {
		require.NoError(t, err)
	})

	callGoHost := getCallEngine(m, callGoHostName)
	callGoTypedHost := getCallEngine(m, callGoTypedHostName)

	require.NotNil(t, callGoHost)
	require.NotNil(t, callGoTypedHost)

	tests := []struct {
		offset uint32
		val    float32
	}{
		{offset: 0, val: math.Float32frombits(0xffffffff)},
		{offset: 100, val: 1.12314},
		{offset: wasm.MemoryPageSize - 4, val: 1.12314},
	}

	mem := m.MemoryInstance.Buffer

	for _, f := range []struct {
		name string
		ce   api.Function
	}{
		{name: "go", ce: callGoHost},
		{name: "go-typed", ce: callGoTypedHost},
	} {
		t.Run(f.name, func(t *testing.T) {
			for _, tc := range tests {
				binary.LittleEndian.PutUint32(mem[tc.offset:], math.Float32bits(tc.val))
				res, err := f.ce.Call(context.Background(), uint64(tc.offset))
				require.NoError(t, err)
				require.Equal(t, math.Float32bits(tc.val), uint32(res[0]))
			}
		})
	}
}

func getCallEngine(m *wasm.ModuleInstance, name string) (ce api.Function) {
	exp := m.Exports[name]
	ce = m.Engine.NewFunction(exp.Index)
	return
}

func setupHostCallBench(requireNoError func(error)) *wasm.ModuleInstance {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)

	const i32, f32 = api.ValueTypeI32, api.ValueTypeF32
	hostBuilder := r.NewHostModuleBuilder("host").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		ret, ok := mod.Memory().ReadUint32Le(uint32(stack[0]))
		if !ok {
			panic("couldn't read memory")
		}
		stack[0] = uint64(ret)
	}), []api.ValueType{i32}, []api.ValueType{f32}).Export("go")
	wazy.HostFunc1(hostBuilder.NewFunctionBuilder(), func(ctx context.Context, m api.Module, pos uint32) float32 {
		ret, ok := m.Memory().ReadUint32Le(pos)
		if !ok {
			panic("couldn't read memory")
		}
		return math.Float32frombits(ret)
	}).Export("go-typed")
	_, err := hostBuilder.Instantiate(ctx)
	requireNoError(err)

	// Build the importing module.
	importingModuleBin := binaryencoding.EncodeModule(&wasm.Module{
		TypeSection: []wasm.FunctionType{{
			Params:  []wasm.ValueType{wasm.ValueType(i32)},
			Results: []wasm.ValueType{wasm.ValueType(f32)},
		}},
		ImportSection: []wasm.Import{
			// Placeholders for imports from hostModule.
			{Type: wasm.ExternTypeFunc, Module: "host", Name: "go"},
			{Type: wasm.ExternTypeFunc, Module: "host", Name: "go-typed"},
		},
		FunctionSection: []wasm.Index{0, 0},
		ExportSection: []wasm.Export{
			{Name: callGoHostName, Type: wasm.ExternTypeFunc, Index: 2},
			{Name: callGoTypedHostName, Type: wasm.ExternTypeFunc, Index: 3},
		},
		Exports: map[string]*wasm.Export{
			callGoHostName:      {Name: callGoHostName, Type: wasm.ExternTypeFunc, Index: 2},
			callGoTypedHostName: {Name: callGoTypedHostName, Type: wasm.ExternTypeFunc, Index: 3},
		},
		CodeSection: []wasm.Code{
			{Body: []byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeCall, 0, wasm.OpcodeEnd}}, // Calling the index 0 = host.go.
			{Body: []byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeCall, 1, wasm.OpcodeEnd}}, // Calling the index 1 = host.go-typed.
		},
		MemorySection: &wasm.Memory{Min: 1},
	})

	importing, err := r.Instantiate(ctx, importingModuleBin)
	requireNoError(err)
	return importing.(*wasm.ModuleInstance)
}

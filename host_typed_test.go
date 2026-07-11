package wazero

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/testing/binaryencoding"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasm"
)

// TestHostFunc_equivalence registers the same logical host functions two
// ways: through HostFunc0-HostFunc8/HostProc0-HostProc8, and by hand through
// WithGoModuleFunction. Both are exported from a single host module,
// instantiated through a real Runtime, and called with identical raw
// parameters; results must match exactly.
//
// This is the correctness backstop for the typed API: if decodeHostValue or
// encodeHostValue ever drifted from the wasm value conventions documented on
// api.ValueType, this test catches it independent of the zero-allocation or
// benchmark checks.
func TestHostFunc_equivalence(t *testing.T) {
	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close(ctx)

	b := r.NewHostModuleBuilder("host")

	i32, i64, f32, f64, extern := api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeF32, api.ValueTypeF64, api.ValueTypeExternref

	// ---- arity 0, one per HostValue kind ----
	HostFunc0(b.NewFunctionBuilder(), func(context.Context, api.Module) uint32 { return 0xDEADBEEF }).Export("u32_0")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		stack[0] = uint64(uint32(0xDEADBEEF))
	}), nil, []api.ValueType{i32}).Export("u32_0_manual")

	HostFunc0(b.NewFunctionBuilder(), func(context.Context, api.Module) int32 { return -12345 }).Export("i32_0")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		x := int32(-12345)
		stack[0] = uint64(int64(x))
	}), nil, []api.ValueType{i32}).Export("i32_0_manual")

	HostFunc0(b.NewFunctionBuilder(), func(context.Context, api.Module) uint64 { return 0x0123456789ABCDEF }).Export("u64_0")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		stack[0] = uint64(0x0123456789ABCDEF)
	}), nil, []api.ValueType{i64}).Export("u64_0_manual")

	HostFunc0(b.NewFunctionBuilder(), func(context.Context, api.Module) int64 { return math.MinInt64 }).Export("i64_0")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		x := int64(math.MinInt64)
		stack[0] = uint64(x)
	}), nil, []api.ValueType{i64}).Export("i64_0_manual")

	HostFunc0(b.NewFunctionBuilder(), func(context.Context, api.Module) float32 { return float32(math.Pi) }).Export("f32_0")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		stack[0] = uint64(math.Float32bits(float32(math.Pi)))
	}), nil, []api.ValueType{f32}).Export("f32_0_manual")

	HostFunc0(b.NewFunctionBuilder(), func(context.Context, api.Module) float64 { return -math.E }).Export("f64_0")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		stack[0] = math.Float64bits(-math.E)
	}), nil, []api.ValueType{f64}).Export("f64_0_manual")

	HostFunc0(b.NewFunctionBuilder(), func(context.Context, api.Module) uintptr { return 0xCAFEBABE }).Export("uintptr_0")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		stack[0] = uint64(uintptr(0xCAFEBABE))
	}), nil, []api.ValueType{extern}).Export("uintptr_0_manual")

	// ---- arity 1, identity functions to probe raw edge bit patterns ----
	HostFunc1(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, x uint32) uint32 { return x }).Export("u32_1")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		stack[0] = uint64(uint32(stack[0]))
	}), []api.ValueType{i32}, []api.ValueType{i32}).Export("u32_1_manual")

	HostFunc1(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, x int32) int32 { return x }).Export("i32_1")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		stack[0] = uint64(int64(int32(stack[0])))
	}), []api.ValueType{i32}, []api.ValueType{i32}).Export("i32_1_manual")

	HostFunc1(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, x uint64) uint64 { return x }).Export("u64_1")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		// identity: a uint64 stack slot is already in wire format.
	}), []api.ValueType{i64}, []api.ValueType{i64}).Export("u64_1_manual")

	HostFunc1(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, x int64) int64 { return x }).Export("i64_1")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		stack[0] = uint64(int64(stack[0]))
	}), []api.ValueType{i64}, []api.ValueType{i64}).Export("i64_1_manual")

	HostFunc1(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, x float32) float32 { return x }).Export("f32_1")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		x := math.Float32frombits(uint32(stack[0]))
		stack[0] = uint64(math.Float32bits(x))
	}), []api.ValueType{f32}, []api.ValueType{f32}).Export("f32_1_manual")

	HostFunc1(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, x float64) float64 { return x }).Export("f64_1")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		x := math.Float64frombits(stack[0])
		stack[0] = math.Float64bits(x)
	}), []api.ValueType{f64}, []api.ValueType{f64}).Export("f64_1_manual")

	HostFunc1(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, x uintptr) uintptr { return x }).Export("uintptr_1")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		stack[0] = uint64(uintptr(stack[0]))
	}), []api.ValueType{extern}, []api.ValueType{extern}).Export("uintptr_1_manual")

	// ---- arity 2 ----
	HostFunc2(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, x, y uint32) uint32 { return x + y }).Export("u32_2")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		x, y := uint32(stack[0]), uint32(stack[1])
		stack[0] = uint64(x + y)
	}), []api.ValueType{i32, i32}, []api.ValueType{i32}).Export("u32_2_manual")

	HostFunc2(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, x, y float64) float64 { return x * y }).Export("f64_2")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		x, y := math.Float64frombits(stack[0]), math.Float64frombits(stack[1])
		stack[0] = math.Float64bits(x * y)
	}), []api.ValueType{f64, f64}, []api.ValueType{f64}).Export("f64_2_manual")

	// ---- arity 4, confirms correct stack offsets 0..3 ----
	HostFunc4(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, a, c, d, e uint32) uint32 { return a ^ c ^ d ^ e }).Export("u32_4")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		a, c, d, e := uint32(stack[0]), uint32(stack[1]), uint32(stack[2]), uint32(stack[3])
		stack[0] = uint64(a ^ c ^ d ^ e)
	}), []api.ValueType{i32, i32, i32, i32}, []api.ValueType{i32}).Export("u32_4_manual")

	// ---- arity 8, confirms correct stack offsets 0..7 ----
	HostFunc8(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, a, c, d, e, f, g, h, i uint32) uint32 {
		return a ^ c ^ d ^ e ^ f ^ g ^ h ^ i
	}).Export("u32_8")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		var v uint32
		for idx := range 8 {
			v ^= uint32(stack[idx])
		}
		stack[0] = uint64(v)
	}), []api.ValueType{i32, i32, i32, i32, i32, i32, i32, i32}, []api.ValueType{i32}).Export("u32_8_manual")

	// ---- HostProc (no result); side effects prove the closure ran ----
	var typedSum, manualSum int
	HostProc1(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, x uint32) { typedSum += int(x) }).Export("proc_1")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		manualSum += int(uint32(stack[0]))
	}), []api.ValueType{i32}, nil).Export("proc_1_manual")

	// Bypass HostModuleBuilder.Instantiate's convenience wrapping (which
	// forbids ExportedFunction on host modules) by compiling then
	// instantiating directly: this lets the test call exported host
	// functions the same way a wasm-defined importing module would, through
	// a real Runtime.
	compiled, err := b.Compile(ctx)
	require.NoError(t, err)
	mod, err := r.InstantiateModule(ctx, compiled, NewModuleConfig())
	require.NoError(t, err)
	defer mod.Close(ctx)

	tests := []struct {
		name   string
		params []uint64
	}{
		{name: "u32_0"},
		{name: "i32_0"},
		{name: "u64_0"},
		{name: "i64_0"},
		{name: "f32_0"},
		{name: "f64_0"},
		{name: "uintptr_0"},

		{name: "u32_1", params: []uint64{0}},
		{name: "u32_1", params: []uint64{0xFFFFFFFF}},
		{name: "i32_1", params: []uint64{0xFFFFFFFF}}, // -1: negative i32, raw slot all-ones
		{name: "i32_1", params: []uint64{0x80000000}}, // math.MinInt32
		{name: "u64_1", params: []uint64{0xFFFFFFFFFFFFFFFF}},
		{name: "i64_1", params: []uint64{0x8000000000000000}}, // math.MinInt64: i64 sign bit
		{name: "f32_1", params: []uint64{0x7fc00000}},         // quiet NaN bit pattern
		{name: "f32_1", params: []uint64{uint64(math.Float32bits(1.5))}},
		{name: "f64_1", params: []uint64{0x7ff8000000000000}}, // quiet NaN bit pattern
		{name: "uintptr_1", params: []uint64{0xDEADBEEFCAFEBABE}},

		{name: "u32_2", params: []uint64{40, 2}},
		{name: "f64_2", params: []uint64{uint64(math.Float64bits(1.5)), uint64(math.Float64bits(-2.0))}},

		{name: "u32_4", params: []uint64{1, 2, 3, 4}},
		{name: "u32_8", params: []uint64{1, 2, 3, 4, 5, 6, 7, 8}},

		{name: "proc_1", params: []uint64{7}},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s%v", tc.name, tc.params), func(t *testing.T) {
			typed := mod.ExportedFunction(tc.name)
			manual := mod.ExportedFunction(tc.name + "_manual")
			require.NotNil(t, typed)
			require.NotNil(t, manual)

			gotTyped, err := typed.Call(ctx, tc.params...)
			require.NoError(t, err)
			gotManual, err := manual.Call(ctx, tc.params...)
			require.NoError(t, err)

			require.Equal(t, gotManual, gotTyped)
		})
	}

	require.Equal(t, manualSum, typedSum)
	require.Equal(t, 7, typedSum)
}

// TestHostFunc_zeroAllocs guards the headline win of the typed API: calling
// the GoModuleFunction produced by HostFunc1 must not allocate. This proves
// decodeHostValue/encodeHostValue resolve entirely through their literal
// type switches, never touching the allocating reflection machinery.
//
// This calls the registered api.GoModuleFunction directly with a reused
// stack: it is simpler than round-tripping through a wasm-defined importing
// module and just as reliable, since the GoModuleFunction is exactly what
// the real call engine invokes.
func TestHostFunc_zeroAllocs(t *testing.T) {
	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close(ctx)

	// A minimal wasm-defined module supplies a real, memory-backed
	// api.Module to pass to the GoModuleFunction, matching what an actual
	// call originating from Wasm would provide.
	memMod, err := r.Instantiate(ctx, binaryencoding.EncodeModule(&wasm.Module{MemorySection: &wasm.Memory{Min: 1}}))
	require.NoError(t, err)
	defer memMod.Close(ctx)

	const offset, val = uint32(100), float32(1.5)
	require.True(t, memMod.Memory().WriteUint32Le(offset, math.Float32bits(val)))

	b := r.NewHostModuleBuilder("host")
	typed := HostFunc1(b.NewFunctionBuilder(), func(_ context.Context, mod api.Module, offset uint32) float32 {
		ret, ok := mod.Memory().ReadUint32Le(offset)
		if !ok {
			panic("couldn't read memory")
		}
		return math.Float32frombits(ret)
	})
	fn := goModuleFuncFor(t, typed)

	stack := make([]uint64, 1)
	allocs := testing.AllocsPerRun(1000, func() {
		stack[0] = uint64(offset) // the result overwrites stack[0], so reset it each iteration.
		fn.Call(ctx, memMod, stack)
		if uint32(stack[0]) != math.Float32bits(val) {
			t.Fatal("unexpected result")
		}
	})
	require.Zero(t, allocs, "typed host function Call must not allocate")
}

// goModuleFuncFor extracts the api.GoModuleFunction registered by a
// HostFunc0-HostFunc8/HostProc0-HostProc8 call, so tests can invoke it
// directly (bypassing Export/Compile/Instantiate) with a reused stack.
func goModuleFuncFor(t *testing.T, b HostFunctionBuilder) api.GoModuleFunction {
	t.Helper()
	hb, ok := b.(*hostFunctionBuilder)
	require.True(t, ok)
	hostFn, ok := hb.fn.(*wasm.HostFunc)
	require.True(t, ok)
	fn, ok := hostFn.Code.GoFunc.(api.GoModuleFunction)
	require.True(t, ok)
	return fn
}

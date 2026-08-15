package wazy

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestHostFunc_equivalence registers the same logical host functions two
// ways: through HostFunc0-HostFunc16/HostProc0-HostProc16, and by hand through
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

	// ---- arity 9, the first arity past the original HostFunc8 ceiling ----
	HostFunc9(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, a, c, d, e, f, g, h, i, j uint32) uint32 {
		return a ^ c ^ d ^ e ^ f ^ g ^ h ^ i ^ j
	}).Export("u32_9")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		var v uint32
		for idx := range 9 {
			v ^= uint32(stack[idx])
		}
		stack[0] = uint64(v)
	}), i32s(9), []api.ValueType{i32}).Export("u32_9_manual")

	// ---- arity 14, the shape wasi-http's new-outgoing-request needs ----
	HostFunc14(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, a, c, d, e, f, g, h, i, j, k, l, m, n, o uint32) uint32 {
		return a ^ c ^ d ^ e ^ f ^ g ^ h ^ i ^ j ^ k ^ l ^ m ^ n ^ o
	}).Export("u32_14")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		var v uint32
		for idx := range 14 {
			v ^= uint32(stack[idx])
		}
		stack[0] = uint64(v)
	}), i32s(14), []api.ValueType{i32}).Export("u32_14_manual")

	// ---- arity 16, the ceiling: confirms stack offsets 0..15 ----
	HostFunc16(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, a, c, d, e, f, g, h, i, j, k, l, m, n, o, p, q uint32) uint32 {
		return a ^ c ^ d ^ e ^ f ^ g ^ h ^ i ^ j ^ k ^ l ^ m ^ n ^ o ^ p ^ q
	}).Export("u32_16")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		var v uint32
		for idx := range 16 {
			v ^= uint32(stack[idx])
		}
		stack[0] = uint64(v)
	}), i32s(16), []api.ValueType{i32}).Export("u32_16_manual")

	// ---- arity 12, mixed HostValue kinds: an XOR of uint32s would pass even
	// if a high-offset parameter decoded with the wrong type, so mix every
	// kind and place the widest ones last.
	HostFunc12(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module,
		p1 uint32, p2 int32, p3 uint64, p4 int64, p5 float32, p6 float64,
		p7 uintptr, p8 uint32, p9 int32, p10 int64, p11 float32, p12 float64) float64 {
		return float64(p1) + float64(p2) + float64(p3) + float64(p4) + float64(p5) + p6 +
			float64(p7) + float64(p8) + float64(p9) + float64(p10) + float64(p11) + p12
	}).Export("mixed_12")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		sum := float64(uint32(stack[0])) + float64(int32(stack[1])) + float64(stack[2]) +
			float64(int64(stack[3])) + float64(math.Float32frombits(uint32(stack[4]))) + math.Float64frombits(stack[5]) +
			float64(uintptr(stack[6])) + float64(uint32(stack[7])) + float64(int32(stack[8])) +
			float64(int64(stack[9])) + float64(math.Float32frombits(uint32(stack[10]))) + math.Float64frombits(stack[11])
		stack[0] = math.Float64bits(sum)
	}), []api.ValueType{i32, i32, i64, i64, f32, f64, extern, i32, i32, i64, f32, f64}, []api.ValueType{f64}).Export("mixed_12_manual")

	// ---- HostProc (no result); side effects prove the closure ran ----
	var typedSum, manualSum int
	HostProc1(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, x uint32) { typedSum += int(x) }).Export("proc_1")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		manualSum += int(uint32(stack[0]))
	}), []api.ValueType{i32}, nil).Export("proc_1_manual")

	var typedProc16, manualProc16 uint32
	HostProc16(b.NewFunctionBuilder(), func(_ context.Context, _ api.Module, a, c, d, e, f, g, h, i, j, k, l, m, n, o, p, q uint32) {
		typedProc16 = a ^ c ^ d ^ e ^ f ^ g ^ h ^ i ^ j ^ k ^ l ^ m ^ n ^ o ^ p ^ q
	}).Export("proc_16")
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		for idx := range 16 {
			manualProc16 ^= uint32(stack[idx])
		}
	}), i32s(16), nil).Export("proc_16_manual")

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
		{name: "u32_9", params: []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9}},
		{name: "u32_14", params: []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}},
		{name: "u32_16", params: []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
		// Distinct values per slot: a swapped pair would still XOR equal.
		{name: "u32_16", params: []uint64{0x1, 0x2, 0x4, 0x8, 0x10, 0x20, 0x40, 0x80,
			0x100, 0x200, 0x400, 0x800, 0x1000, 0x2000, 0x4000, 0x8000}},

		{name: "mixed_12", params: []uint64{
			0xFFFFFFFF,                    // uint32 max
			0xFFFFFFFF,                    // int32 -1
			0xFFFFFFFFFFFFFFFF,            // uint64 max
			0x8000000000000000,            // int64 min
			uint64(math.Float32bits(1.5)), // float32
			math.Float64bits(-2.25),       // float64
			0xDEADBEEF,                    // uintptr
			42,                            // uint32
			0x80000000,                    // int32 min
			7,                             // int64
			uint64(math.Float32bits(0.5)), // float32
			math.Float64bits(math.Pi),     // float64
		}},

		{name: "proc_1", params: []uint64{7}},
		{name: "proc_16", params: []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
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
	require.Equal(t, manualProc16, typedProc16)
	require.Equal(t, uint32(1^2^3^4^5^6^7^8^9^10^11^12^13^14^15^16), typedProc16)
}

// i32s returns n i32 value types, for the wide manual signatures above.
func i32s(n int) []api.ValueType {
	types := make([]api.ValueType, n)
	for i := range types {
		types[i] = api.ValueTypeI32
	}
	return types
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
// HostFunc0-HostFunc16/HostProc0-HostProc16 call, so tests can invoke it
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

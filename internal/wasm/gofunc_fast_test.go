package wasm

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/testing/require"
)

// reflectFallback builds the general reflect-based adapter (reflectGoFunction
// or reflectGoModuleFunction) for fn, bypassing the fast-path type switch in
// fastGoFunc. It mirrors the precompute done by parseGoReflectFunc so the
// fallback we compare against is exactly the one production would use if the
// signature were not in the fast-path matrix.
func reflectFallback(t *testing.T, fn interface{}) interface{} {
	t.Helper()
	fnV := reflect.ValueOf(fn)
	p := fnV.Type()

	pk, err := kind(p)
	require.NoError(t, err)

	pOffset := 0
	switch pk {
	case paramsKindContext:
		pOffset = 1
	case paramsKindContextModule:
		pOffset = 2
	}

	pCount := p.NumIn() - pOffset
	var paramTypes []reflect.Type
	var paramKinds []reflect.Kind
	if pCount > 0 {
		paramTypes = make([]reflect.Type, pCount)
		paramKinds = make([]reflect.Kind, pCount)
	}
	for i := 0; i < pCount; i++ {
		pI := p.In(i + pOffset)
		paramTypes[i] = pI
		paramKinds[i] = pI.Kind()
	}

	var resultKinds []reflect.Kind
	if rCount := p.NumOut(); rCount > 0 {
		resultKinds = make([]reflect.Kind, rCount)
		for i := 0; i < rCount; i++ {
			resultKinds[i] = p.Out(i).Kind()
		}
	}

	if pk == paramsKindContextModule {
		return &reflectGoModuleFunction{
			fn: &fnV, numIn: p.NumIn(),
			paramTypes: paramTypes, paramKinds: paramKinds, resultKinds: resultKinds,
		}
	}
	return &reflectGoFunction{
		pk: pk, fn: &fnV, numIn: p.NumIn(),
		paramTypes: paramTypes, paramKinds: paramKinds, resultKinds: resultKinds,
	}
}

// callGoFuncValue invokes a Code.GoFunc value regardless of whether it is a
// module-aware (api.GoModuleFunction) or plain (api.GoFunction) adapter.
func callGoFuncValue(gf interface{}, mod api.Module, stack []uint64) {
	switch f := gf.(type) {
	case api.GoModuleFunction:
		f.Call(testCtx, mod, stack)
	case api.GoFunction:
		f.Call(testCtx, stack)
	default:
		panic("unexpected Code.GoFunc type")
	}
}

// isReflectFallback reports whether gf is one of the general reflect adapters,
// i.e. the fast path did not match.
func isReflectFallback(gf interface{}) bool {
	switch gf.(type) {
	case *reflectGoFunction, *reflectGoModuleFunction:
		return true
	default:
		return false
	}
}

// Bit patterns that stress the decode/encode conversions: sign bits, all-ones,
// signaling and quiet NaNs, and junk in the unused upper 32 bits of an i32/f32
// slot (which both paths must mask off identically).
const (
	slotI32Neg  = 0xdeadbeef_ffffffff // low 32 bits -> int32(-1); upper bits are junk
	slotI32Min  = 0x00000000_80000000 // low 32 bits -> int32 minimum
	slotU32Max  = 0xcafef00d_ffffffff // low 32 bits -> uint32 maximum; upper bits are junk
	slotI64Sign = 0x80000000_00000000 // int64 minimum / sign bit set
	slotU64Max  = 0xffffffff_ffffffff
	slotF32sNaN = 0x0badf00d_7f800001 // signaling NaN f32 in the low 32 bits
	slotF32qNaN = 0x00000000_7fc00000
	slotF64sNaN = 0x7ff00000_00000001 // signaling NaN f64
	slotF64qNaN = 0x7ff80000_00000000
)

// Test_fastGoFunc_equivalence asserts that, for a representative func of every
// fast-path shape, the generated zero-reflection adapter produces byte-for-byte
// the same result stack as the general reflect fallback, including for
// pathological i32/i64 sign bits, NaN payloads, and junk upper bits. If these
// ever diverge, WithFunc host calls would silently change behavior.
func Test_fastGoFunc_equivalence(t *testing.T) {
	tests := []struct {
		name  string
		fn    interface{}
		stack []uint64 // input stack; length must cover both params and results
		want  []uint64 // optional: explicit expected output for deterministic cases
	}{
		// --- (context.Context, api.Module, ...) shapes ---
		{
			name:  "ctxmod void->void",
			fn:    func(context.Context, api.Module) {},
			stack: nil,
		},
		{
			name:  "ctxmod void->u32",
			fn:    func(context.Context, api.Module) uint32 { return 0xcafebabe },
			stack: []uint64{0},
			want:  []uint64{0xcafebabe},
		},
		{
			name:  "ctxmod i32->i32 negative",
			fn:    func(_ context.Context, _ api.Module, x int32) int32 { return x },
			stack: []uint64{slotI32Neg},
			want:  []uint64{slotU64Max}, // uint64(int64(int32(-1)))
		},
		{
			name:  "ctxmod i32->i64 min",
			fn:    func(_ context.Context, _ api.Module, x int32) int64 { return int64(x) },
			stack: []uint64{slotI32Min},
			want:  []uint64{0xffffffff_80000000}, // sign-extended MinInt32
		},
		{
			name:  "ctxmod u32->u64 mask upper bits",
			fn:    func(_ context.Context, _ api.Module, x uint32) uint64 { return uint64(x) },
			stack: []uint64{slotU32Max},
			want:  []uint64{0xffffffff},
		},
		{
			name:  "ctxmod u64->u64 max",
			fn:    func(_ context.Context, _ api.Module, x uint64) uint64 { return x },
			stack: []uint64{slotU64Max},
			want:  []uint64{slotU64Max},
		},
		{
			name:  "ctxmod i64->i64 sign bit",
			fn:    func(_ context.Context, _ api.Module, x int64) int64 { return x },
			stack: []uint64{slotI64Sign},
			want:  []uint64{slotI64Sign},
		},
		{
			name:  "ctxmod 2u32->u64 combine",
			fn:    func(_ context.Context, _ api.Module, a, b uint32) uint64 { return uint64(a)<<32 | uint64(b) },
			stack: []uint64{slotU32Max, 0x11223344_55667788},
			want:  []uint64{0xffffffff_55667788},
		},
		{
			name:  "ctxmod f32->f32 signaling NaN",
			fn:    func(_ context.Context, _ api.Module, x float32) float32 { return x },
			stack: []uint64{slotF32sNaN},
		},
		{
			name:  "ctxmod f32->f32 quiet NaN",
			fn:    func(_ context.Context, _ api.Module, x float32) float32 { return x },
			stack: []uint64{slotF32qNaN},
		},
		{
			name:  "ctxmod f64->f64 signaling NaN",
			fn:    func(_ context.Context, _ api.Module, x float64) float64 { return x },
			stack: []uint64{slotF64sNaN},
		},
		{
			name:  "ctxmod f64->f64 quiet NaN",
			fn:    func(_ context.Context, _ api.Module, x float64) float64 { return x },
			stack: []uint64{slotF64qNaN},
		},
		{
			name:  "ctxmod f64f64->f64 add",
			fn:    func(_ context.Context, _ api.Module, a, b float64) float64 { return a + b },
			stack: []uint64{math.Float64bits(1.5), math.Float64bits(2.25)},
			want:  []uint64{math.Float64bits(3.75)},
		},
		{
			name:  "ctxmod 4u32->u32",
			fn:    func(_ context.Context, _ api.Module, a, b, c, d uint32) uint32 { return a ^ b ^ c ^ d },
			stack: []uint64{0x1, 0x2, 0x4, 0x8},
			want:  []uint64{0xf},
		},
		{
			name: "ctxmod 3u32->f64 fresh signaling NaN result",
			fn: func(context.Context, api.Module, uint32, uint32, uint32) float64 {
				return math.Float64frombits(slotF64sNaN)
			},
			stack: []uint64{0, 0, 0},
		},

		// --- (context.Context, ...) shapes ---
		{
			name:  "ctx void->void",
			fn:    func(context.Context) {},
			stack: nil,
		},
		{
			name:  "ctx u32->f32 signaling NaN round-trip",
			fn:    func(_ context.Context, x uint32) float32 { return math.Float32frombits(x) },
			stack: []uint64{slotF32sNaN},
		},
		{
			name:  "ctx i32i32->i32 subtract",
			fn:    func(_ context.Context, a, b int32) int32 { return a - b },
			stack: []uint64{0x2, slotI32Neg}, // 2 - (-1) = 3
			want:  []uint64{0x3},
		},
		{
			name:  "ctx i32->i64 min",
			fn:    func(_ context.Context, x int32) int64 { return int64(x) },
			stack: []uint64{slotI32Min},
			want:  []uint64{0xffffffff_80000000},
		},
		{
			name:  "ctx u64u64->u64",
			fn:    func(_ context.Context, a, b uint64) uint64 { return a - b },
			stack: []uint64{0x0, 0x1}, // 0 - 1 = MaxUint64
			want:  []uint64{slotU64Max},
		},

		// --- no-context shapes ---
		{
			name:  "none void->void",
			fn:    func() {},
			stack: nil,
		},
		{
			name:  "none void->i32",
			fn:    func() int32 { return -1 },
			stack: []uint64{0},
			want:  []uint64{slotU64Max},
		},
		{
			name:  "none u32->u32 identity mask",
			fn:    func(x uint32) uint32 { return x },
			stack: []uint64{slotU32Max},
			want:  []uint64{0xffffffff},
		},
		{
			name:  "none i64->i64 sign bit",
			fn:    func(x int64) int64 { return x },
			stack: []uint64{slotI64Sign},
			want:  []uint64{slotI64Sign},
		},
		{
			name:  "none f32f32->f32 signaling NaN operand",
			fn:    func(a, b float32) float32 { return a + b },
			stack: []uint64{slotF32sNaN, 0}, // slot 1 low 32 bits are 0.0f
		},
		{
			name:  "none 3u32->u64",
			fn:    func(a, b, c uint32) uint64 { return uint64(a) + uint64(b) + uint64(c) },
			stack: []uint64{slotU32Max, 0x1, 0x0},
			want:  []uint64{0x1_00000000}, // MaxUint32 + 1 + 0
		},
	}

	// A non-nil module is required: the reflect fallback places the module
	// argument only when mod != nil, as is always the case in production.
	mod := &ModuleInstance{}

	for _, tt := range tests {
		tc := tt
		t.Run(tc.name, func(t *testing.T) {
			_, _, code, err := parseGoReflectFunc(tc.fn)
			require.NoError(t, err)

			// Guard: the whole test is only meaningful if the fast path was
			// actually taken for this shape.
			require.False(t, isReflectFallback(code.GoFunc),
				"expected a fast-path adapter, got the reflect fallback")

			fallback := reflectFallback(t, tc.fn)
			require.True(t, isReflectFallback(fallback))

			fastStack := append([]uint64(nil), tc.stack...)
			slowStack := append([]uint64(nil), tc.stack...)
			callGoFuncValue(code.GoFunc, mod, fastStack)
			callGoFuncValue(fallback, mod, slowStack)

			// The core contract: the fast path is byte-identical to reflect.
			require.Equal(t, slowStack, fastStack)

			// Optional: lock in the expected result slots (the leading
			// len(want) slots; any trailing entries are leftover params).
			if tc.want != nil {
				require.Equal(t, tc.want, fastStack[:len(tc.want)])
			}
		})
	}
}

// Test_fastGoFunc_zeroAllocs guards the headline win: a fast-path adapter Call
// must not allocate. The equivalent reflect fallback is asserted to allocate,
// so this test fails loudly if a change ever routes a fast-path signature back
// through reflection.
func Test_fastGoFunc_zeroAllocs(t *testing.T) {
	// Representative shape, matching the go-reflect host benchmark:
	// (context.Context, api.Module, uint32) float32.
	fn := func(_ context.Context, _ api.Module, x uint32) float32 { return math.Float32frombits(x) }

	_, _, code, err := parseGoReflectFunc(fn)
	require.NoError(t, err)
	require.False(t, isReflectFallback(code.GoFunc),
		"expected a fast-path adapter, got the reflect fallback")
	fast := code.GoFunc.(api.GoModuleFunction)

	mod := &ModuleInstance{}
	stack := []uint64{0x3f800000}
	fastAllocs := testing.AllocsPerRun(100, func() {
		fast.Call(testCtx, mod, stack)
	})
	require.Zero(t, fastAllocs, "fast-path adapter Call must not allocate")

	// Sanity check that the guard has teeth: the reflect fallback for the same
	// signature does allocate (reflect.Value.Call builds argument values).
	slow := reflectFallback(t, fn).(api.GoModuleFunction)
	slowAllocs := testing.AllocsPerRun(100, func() {
		slow.Call(testCtx, mod, stack)
	})
	require.True(t, slowAllocs > 0, "reflect fallback is expected to allocate")
}

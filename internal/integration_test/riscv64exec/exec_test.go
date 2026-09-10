//go:build riscv64

package riscv64exec

import (
	"context"
	"math"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestRiscv64_compiledExecution runs real compiled RISC-V code.
//
// It lives in its own package deliberately. internal/engine/native's TestMain
// calls os.Exit(0) when platform.CompilerSupported() is false, and that asks
// about CoreFeaturesV2 -- which includes SIMD, which riscv64 does not lower
// yet. So every test in that binary exits silently on riscv64 and a green run
// there proves nothing about this backend.
//
// This test asks for V1 instead, the feature set the riscv64 gate does admit,
// and asserts the compiler really was selected before running anything.
// Without that assertion it could quietly pass on the interpreter and still
// tell us nothing.
func TestRiscv64_compiledExecution(t *testing.T) {
	require.True(t, platform.CompilerSupports(api.CoreFeaturesV1),
		"the riscv64 compiler must be selected for CoreFeaturesV1, or this test proves nothing")

	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV1))
	defer r.Close(ctx)

	mod, err := r.Instantiate(ctx, riscvExecModule())
	require.NoError(t, err)

	i32 := func(name string, args ...uint64) uint32 {
		t.Helper()
		res, err := mod.ExportedFunction(name).Call(ctx, args...)
		require.NoError(t, err)
		return uint32(res[0])
	}
	i64 := func(name string, args ...uint64) uint64 {
		t.Helper()
		res, err := mod.ExportedFunction(name).Call(ctx, args...)
		require.NoError(t, err)
		return res[0]
	}

	t.Run("i32 arithmetic", func(t *testing.T) {
		require.Equal(t, uint32(7), i32("add", 3, 4))
		require.Equal(t, uint32(0xfffffffe), i32("add", 0xffffffff, 0xffffffff))
		require.Equal(t, uint32(6), i32("mul", 2, 3))
		require.Equal(t, uint32(0xffffffff), i32("sub", 0, 1))
	})

	t.Run("i32 shifts respect the modulo-32 rule", func(t *testing.T) {
		// The whole point of using the word-form shifts: a 64-bit shift would
		// take six count bits and return 0 here.
		require.Equal(t, uint32(1), i32("shr_s", 1, 32))
		require.Equal(t, uint32(2), i32("shl", 1, 33))
		require.Equal(t, uint32(0xffffffff), i32("shr_s", 0x80000000, 31))
		require.Equal(t, uint32(1), i32("shr_u", 0x80000000, 31))
	})

	t.Run("unsigned comparison of sign-extended i32", func(t *testing.T) {
		// 0x80000000 must compare *above* 1 unsigned, which is the property
		// the sign-extended representation relies on.
		require.Equal(t, uint32(0), i32("lt_u", 0x80000000, 1))
		require.Equal(t, uint32(1), i32("lt_u", 1, 0x80000000))
		require.Equal(t, uint32(1), i32("lt_s", 0x80000000, 1))
	})

	t.Run("bit counting", func(t *testing.T) {
		require.Equal(t, uint32(32), i32("clz", 0))
		require.Equal(t, uint32(0), i32("clz", 0x80000000))
		require.Equal(t, uint32(31), i32("clz", 1))
		require.Equal(t, uint32(32), i32("ctz", 0))
		require.Equal(t, uint32(3), i32("ctz", 8))
		require.Equal(t, uint32(32), i32("popcnt", 0xffffffff))
		require.Equal(t, uint32(0), i32("popcnt", 0))
		require.Equal(t, uint32(16), i32("popcnt", 0xaaaaaaaa))
	})

	t.Run("i64 constants across the whole range", func(t *testing.T) {
		require.Equal(t, uint64(0x7fffffff), i64("k_7fffffff"))
		require.Equal(t, uint64(0x7ffff800), i64("k_7ffff800"))
		require.Equal(t, uint64(0x123456789abcdef), i64("k_big"))
		require.Equal(t, uint64(0xffffffffffffffff), i64("k_neg1"))
	})

	t.Run("memory", func(t *testing.T) {
		require.Equal(t, uint32(0xdeadbeef), i32("roundtrip32", 0xdeadbeef))
		require.Equal(t, uint64(0x0123456789abcdef), i64("roundtrip64", 0x0123456789abcdef))
	})

	t.Run("calls and loops", func(t *testing.T) {
		require.Equal(t, uint32(55), i32("fib", 10))
		require.Equal(t, uint32(3628800), i32("fact", 10))
	})

	t.Run("floats", func(t *testing.T) {
		f64of := func(name string, args ...uint64) float64 {
			t.Helper()
			res, err := mod.ExportedFunction(name).Call(ctx, args...)
			require.NoError(t, err)
			return math.Float64frombits(res[0])
		}
		require.Equal(t, 3.5, f64of("f64_const"))
		require.Equal(t, 7.0, f64of("f64_add", math.Float64bits(3.0), math.Float64bits(4.0)))
		require.Equal(t, 12.0, f64of("f64_mul", math.Float64bits(3.0), math.Float64bits(4.0)))
	})

	t.Run("negative memory index traps", func(t *testing.T) {
		// An i32 index lives sign-extended, so -1 is 0xffffffffffffffff. A
		// bounds check that compares it as a signed 64-bit value lets it
		// through, and the address computation then reads *below* the memory
		// base. wasm requires a trap.
		for _, idx := range []uint64{uint64(uint32(0xffffffff)), uint64(uint32(0x80000000))} {
			_, err := mod.ExportedFunction("load_at").Call(ctx, idx)
			require.Error(t, err, "load at index %#x must trap", idx)
		}
		// ...while an in-range index still works. Address 1000 is untouched
		// by the earlier subtests, which write at 0.
		require.Equal(t, uint32(0), i32("load_at", 1000))
	})

	t.Run("traps", func(t *testing.T) {
		_, err := mod.ExportedFunction("div_s").Call(ctx, 1, 0)
		require.Error(t, err, "division by zero must trap")
		_, err = mod.ExportedFunction("div_s").Call(ctx, uint64(uint32(0x80000000)), uint64(uint32(0xffffffff)))
		require.Error(t, err, "INT32_MIN / -1 must trap")
		// ...but the matching remainder is 0 and must not trap.
		require.Equal(t, uint32(0), i32("rem_s", uint64(uint32(0x80000000)), uint64(uint32(0xffffffff))))
	})
}

// riscvExecModule builds the module the test above drives.
func riscvExecModule() []byte {
	const (
		i32t = wasm.ValueTypeI32
		i64t = wasm.ValueTypeI64
	)
	bin2i32 := wasm.FunctionType{Params: []wasm.ValueType{i32t, i32t}, Results: []wasm.ValueType{i32t}}
	un1i32 := wasm.FunctionType{Params: []wasm.ValueType{i32t}, Results: []wasm.ValueType{i32t}}
	noneI64 := wasm.FunctionType{Results: []wasm.ValueType{i64t}}
	un1i64 := wasm.FunctionType{Params: []wasm.ValueType{i64t}, Results: []wasm.ValueType{i64t}}
	noneF64 := wasm.FunctionType{Results: []wasm.ValueType{wasm.ValueTypeF64}}
	bin2f64 := wasm.FunctionType{
		Params:  []wasm.ValueType{wasm.ValueTypeF64, wasm.ValueTypeF64},
		Results: []wasm.ValueType{wasm.ValueTypeF64},
	}

	type fn struct {
		name string
		typ  wasm.Index
		body []byte
	}
	lg0, lg1 := []byte{wasm.OpcodeLocalGet, 0}, []byte{wasm.OpcodeLocalGet, 1}
	bin := func(op byte) []byte {
		b := append([]byte{}, lg0...)
		b = append(b, lg1...)
		return append(b, op, wasm.OpcodeEnd)
	}
	un := func(op byte) []byte {
		return append(append([]byte{}, lg0...), op, wasm.OpcodeEnd)
	}
	i64const := func(v int64) []byte {
		b := []byte{wasm.OpcodeI64Const}
		b = append(b, leb128Signed(v)...)
		return append(b, wasm.OpcodeEnd)
	}

	fns := []fn{
		{"add", 0, bin(wasm.OpcodeI32Add)},
		{"sub", 0, bin(wasm.OpcodeI32Sub)},
		{"mul", 0, bin(wasm.OpcodeI32Mul)},
		{"shl", 0, bin(wasm.OpcodeI32Shl)},
		{"shr_s", 0, bin(wasm.OpcodeI32ShrS)},
		{"shr_u", 0, bin(wasm.OpcodeI32ShrU)},
		{"lt_u", 0, bin(wasm.OpcodeI32LtU)},
		{"lt_s", 0, bin(wasm.OpcodeI32LtS)},
		{"div_s", 0, bin(wasm.OpcodeI32DivS)},
		{"rem_s", 0, bin(wasm.OpcodeI32RemS)},
		{"clz", 1, un(wasm.OpcodeI32Clz)},
		{"ctz", 1, un(wasm.OpcodeI32Ctz)},
		{"popcnt", 1, un(wasm.OpcodeI32Popcnt)},
		{"k_7fffffff", 2, i64const(0x7fffffff)},
		{"k_7ffff800", 2, i64const(0x7ffff800)},
		{"k_big", 2, i64const(0x123456789abcdef)},
		{"k_neg1", 2, i64const(-1)},
		// roundtrip32: store the argument to memory then load it back.
		{"roundtrip32", 1, []byte{
			wasm.OpcodeI32Const, 0, wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Store, 0x02, 0x00,
			wasm.OpcodeI32Const, 0, wasm.OpcodeI32Load, 0x02, 0x00, wasm.OpcodeEnd,
		}},
		{"load_at", 1, []byte{
			wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load, 0x02, 0x00, wasm.OpcodeEnd,
		}},
		{"roundtrip64", 3, []byte{
			wasm.OpcodeI32Const, 0, wasm.OpcodeLocalGet, 0, wasm.OpcodeI64Store, 0x03, 0x00,
			wasm.OpcodeI32Const, 0, wasm.OpcodeI64Load, 0x03, 0x00, wasm.OpcodeEnd,
		}},
	}

	f64const := func(v float64) []byte {
		b := []byte{wasm.OpcodeF64Const}
		var raw [8]byte
		bits := math.Float64bits(v)
		for i := 0; i < 8; i++ {
			raw[i] = byte(bits >> (8 * uint(i)))
		}
		b = append(b, raw[:]...)
		return append(b, wasm.OpcodeEnd)
	}
	fns = append(fns,
		fn{"f64_const", 4, f64const(3.5)},
		fn{"f64_add", 5, bin(wasm.OpcodeF64Add)},
		fn{"f64_mul", 5, bin(wasm.OpcodeF64Mul)},
	)

	types := []wasm.FunctionType{bin2i32, un1i32, noneI64, un1i64, noneF64, bin2f64}
	m := &wasm.Module{
		TypeSection:     types,
		MemorySection:   []wasm.Memory{{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true}},
		FunctionSection: make([]wasm.Index, 0, len(fns)+2),
		CodeSection:     make([]wasm.Code, 0, len(fns)+2),
		ExportSection:   make([]wasm.Export, 0, len(fns)+2),
	}
	for i, f := range fns {
		m.FunctionSection = append(m.FunctionSection, f.typ)
		m.CodeSection = append(m.CodeSection, wasm.Code{Body: f.body})
		m.ExportSection = append(m.ExportSection, wasm.Export{
			Name: f.name, Type: wasm.ExternTypeFunc, Index: wasm.Index(i),
		})
	}

	// fib(n): recursive, to exercise calls, branches and the frame layout.
	fibIdx := wasm.Index(len(fns))
	m.FunctionSection = append(m.FunctionSection, 1)
	m.CodeSection = append(m.CodeSection, wasm.Code{Body: []byte{
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 2, wasm.OpcodeI32LtS,
		wasm.OpcodeIf, 0x40,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeReturn,
		wasm.OpcodeEnd,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Sub, wasm.OpcodeCall, byte(fibIdx),
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 2, wasm.OpcodeI32Sub, wasm.OpcodeCall, byte(fibIdx),
		wasm.OpcodeI32Add, wasm.OpcodeEnd,
	}})
	m.ExportSection = append(m.ExportSection, wasm.Export{Name: "fib", Type: wasm.ExternTypeFunc, Index: fibIdx})

	// fact(n): a loop, to exercise back-edges and block arguments.
	factIdx := fibIdx + 1
	m.FunctionSection = append(m.FunctionSection, 1)
	m.CodeSection = append(m.CodeSection, wasm.Code{
		LocalTypes: []wasm.ValueType{i32t},
		Body: []byte{
			wasm.OpcodeI32Const, 1, wasm.OpcodeLocalSet, 1,
			wasm.OpcodeBlock, 0x40,
			wasm.OpcodeLoop, 0x40,
			wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Eqz, wasm.OpcodeBrIf, 1,
			wasm.OpcodeLocalGet, 1, wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Mul, wasm.OpcodeLocalSet, 1,
			wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Sub, wasm.OpcodeLocalSet, 0,
			wasm.OpcodeBr, 0,
			wasm.OpcodeEnd,
			wasm.OpcodeEnd,
			wasm.OpcodeLocalGet, 1, wasm.OpcodeEnd,
		},
	})
	m.ExportSection = append(m.ExportSection, wasm.Export{Name: "fact", Type: wasm.ExternTypeFunc, Index: factIdx})

	return binaryencoding.EncodeModule(m)
}

// leb128Signed encodes v as a signed LEB128, which is how i64.const carries
// its immediate.
func leb128Signed(v int64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

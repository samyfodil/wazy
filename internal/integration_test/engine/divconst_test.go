package adhoc

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestDivByConst runs every integer division and remainder by constants of every kind the compiler
// strength-reduces, and the ones it leaves alone, against the interpreter, on dividends at the edges
// and at random: results and traps must agree.
func TestDivByConst(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	d32 := []int64{
		0, 1, -1, 2, -2, 3, -3, 4, 5, 6, 7, -7, 10, -10, 12, 16, -16, 24, 25, 100, 255, 641, 1000, -1000, 7919,
		1 << 16, 1<<20 + 1, 1<<30 + 3, math.MaxInt32, math.MinInt32, math.MinInt32 + 1, -(1 << 30),
	}
	d64 := []int64{
		0, 1, -1, 2, -2, 3, -3, 7, 10, -10, 12, 24, 100, 1 << 20, 1<<32 + 1, 1_000_000_000_039, -1_000_000_000_039,
		// MinInt64+1 is 0x8000000000000001 unsigned, above 2^63, as -2 is just below 2^64.
		math.MaxInt64, math.MinInt64, math.MinInt64 + 1, 1 << 62, 3 << 61,
	}
	type fn struct {
		name string
		is64 bool
	}
	var fns []fn
	var types []wasm.FunctionType
	types = append(types,
		wasm.FunctionType{Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32}},
		wasm.FunctionType{Params: []wasm.ValueType{wasm.ValueTypeI64}, Results: []wasm.ValueType{wasm.ValueTypeI64}})
	m := &wasm.Module{TypeSection: types, Exports: map[string]*wasm.Export{}}
	add := func(name string, is64 bool, body []byte) {
		idx := wasm.Index(len(m.FunctionSection))
		typ := wasm.Index(0)
		if is64 {
			typ = 1
		}
		m.FunctionSection = append(m.FunctionSection, typ)
		m.CodeSection = append(m.CodeSection, wasm.Code{Body: body})
		e := wasm.Export{Name: name, Type: wasm.ExternTypeFunc, Index: idx}
		m.ExportSection = append(m.ExportSection, e)
		m.Exports[name] = &e
		fns = append(fns, fn{name, is64})
	}
	ops32 := []wasm.Opcode{wasm.OpcodeI32DivS, wasm.OpcodeI32DivU, wasm.OpcodeI32RemS, wasm.OpcodeI32RemU}
	ops64 := []wasm.Opcode{wasm.OpcodeI64DivS, wasm.OpcodeI64DivU, wasm.OpcodeI64RemS, wasm.OpcodeI64RemU}
	for _, d := range d32 {
		for _, op := range ops32 {
			body := append([]byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const}, leb128.EncodeInt32(int32(d))...)
			add(fmt.Sprintf("%s_%d", wasm.InstructionName(op), d), false, append(body, op, wasm.OpcodeEnd))
		}
	}
	for _, d := range d64 {
		for _, op := range ops64 {
			body := append([]byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeI64Const}, leb128.EncodeInt64(d)...)
			add(fmt.Sprintf("%s_%d", wasm.InstructionName(op), d), true, append(body, op, wasm.OpcodeEnd))
		}
	}
	bin := binaryencoding.EncodeModule(m)

	ctx := context.Background()
	compiler := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer compiler.Close(ctx)
	interp := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
	defer interp.Close(ctx)
	mc, err := compiler.Instantiate(ctx, bin)
	require.NoError(t, err)
	mi, err := interp.Instantiate(ctx, bin)
	require.NoError(t, err)

	rng := rand.New(rand.NewSource(1))
	edges32 := []uint64{0, 1, 2, 3, math.MaxInt32, 1 << 31, 1<<31 + 1, math.MaxUint32, math.MaxUint32 - 1, 24, 23, 25, 99, 100, 101, 1000, 7918, 7919, 7920}
	edges64 := []uint64{0, 1, 2, 3, math.MaxInt64, 1 << 63, 1<<63 + 1, math.MaxUint64, math.MaxUint64 - 1, 1 << 32, 1<<32 + 1, 999_999_999_999, 1_000_000_000_039, 3 << 61}
	for _, f := range fns {
		inputs := edges32
		if f.is64 {
			inputs = edges64
		}
		inputs = append([]uint64(nil), inputs...)
		for i := 0; i < 300; i++ {
			v := rng.Uint64()
			switch i % 3 {
			case 1:
				v >>= rng.Intn(64)
			case 2:
				v = -(v >> rng.Intn(64))
			}
			if !f.is64 {
				v = uint64(uint32(v))
			}
			inputs = append(inputs, v)
		}
		for _, x := range inputs {
			rc, errc := mc.ExportedFunction(f.name).Call(ctx, x)
			ri, erri := mi.ExportedFunction(f.name).Call(ctx, x)
			require.Equal(t, erri == nil, errc == nil, "%s(%#x): compiler %v, interpreter %v", f.name, x, errc, erri)
			if erri == nil {
				require.Equal(t, ri[0], rc[0], "%s(%#x)", f.name, x)
			}
		}
	}
}

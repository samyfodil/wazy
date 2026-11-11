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

// TestImmOperands runs integer arithmetic and comparisons against constants at the edges of what an
// immediate can hold, with the other operand in a register and loaded from memory, against the
// interpreter: a 64-bit instruction sign-extends its 32-bit immediate, and a multiplication by a
// constant reads its other operand from memory directly.
func TestImmOperands(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	consts := []int64{
		0, 1, -1, 2, 3, 7, -7, 100, -392, 127, 128, -128, -129, 1 << 16, math.MaxInt32, math.MinInt32,
		math.MaxInt32 + 1, math.MinInt32 - 1, math.MaxUint32, 1 << 32, -1 << 32, math.MaxInt64, math.MinInt64,
	}
	ops32 := []wasm.Opcode{
		wasm.OpcodeI32Mul, wasm.OpcodeI32Add, wasm.OpcodeI32Sub, wasm.OpcodeI32And, wasm.OpcodeI32Or,
		wasm.OpcodeI32Xor, wasm.OpcodeI32Eq, wasm.OpcodeI32LtS, wasm.OpcodeI32LtU, wasm.OpcodeI32GtU,
	}
	ops64 := []wasm.Opcode{
		wasm.OpcodeI64Mul, wasm.OpcodeI64Add, wasm.OpcodeI64Sub, wasm.OpcodeI64And, wasm.OpcodeI64Or,
		wasm.OpcodeI64Xor, wasm.OpcodeI64Eq, wasm.OpcodeI64LtS, wasm.OpcodeI64LtU, wasm.OpcodeI64GtU,
	}

	m := &wasm.Module{
		TypeSection:   []wasm.FunctionType{{Params: []wasm.ValueType{wasm.ValueTypeI64}, Results: []wasm.ValueType{wasm.ValueTypeI64}}},
		MemorySection: []wasm.Memory{{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true}},
		Exports:       map[string]*wasm.Export{},
	}
	var names []string
	add := func(name string, body []byte) {
		idx := wasm.Index(len(m.FunctionSection))
		m.FunctionSection = append(m.FunctionSection, 0)
		m.CodeSection = append(m.CodeSection, wasm.Code{Body: body})
		e := wasm.Export{Name: name, Type: wasm.ExternTypeFunc, Index: idx}
		m.ExportSection = append(m.ExportSection, e)
		m.Exports[name] = &e
		names = append(names, name)
	}
	store := []byte{wasm.OpcodeI32Const, 8, wasm.OpcodeLocalGet, 0, wasm.OpcodeI64Store, 3, 0}
	for _, k := range consts {
		for _, fromMem := range []bool{false, true} {
			for _, op := range ops32 {
				var body []byte
				if fromMem {
					body = append(append(body, store...), wasm.OpcodeI32Const, 8, wasm.OpcodeI32Load, 2, 0)
				} else {
					body = append(body, wasm.OpcodeLocalGet, 0, wasm.OpcodeI32WrapI64)
				}
				body = append(append(body, wasm.OpcodeI32Const), leb128.EncodeInt32(int32(k))...)
				body = append(body, op, wasm.OpcodeI64ExtendI32U)
				add(fmt.Sprintf("%s_%d_%v", wasm.InstructionName(op), k, fromMem), append(body, wasm.OpcodeEnd))
			}
			for _, op := range ops64 {
				var body []byte
				if fromMem {
					body = append(append(body, store...), wasm.OpcodeI32Const, 8, wasm.OpcodeI64Load, 3, 0)
				} else {
					body = append(body, wasm.OpcodeLocalGet, 0)
				}
				body = append(append(body, wasm.OpcodeI64Const), leb128.EncodeInt64(k)...)
				body = append(body, op)
				if op == wasm.OpcodeI64Eq || op == wasm.OpcodeI64LtS || op == wasm.OpcodeI64LtU || op == wasm.OpcodeI64GtU {
					body = append(body, wasm.OpcodeI64ExtendI32U)
				}
				add(fmt.Sprintf("%s_%d_%v", wasm.InstructionName(op), k, fromMem), append(body, wasm.OpcodeEnd))
			}
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
	inputs := []uint64{0, 1, math.MaxUint64, math.MaxInt64, 1 << 63, math.MaxUint32, 1 << 31, 1<<31 - 1, 1 << 32, 0xffffffff00000000}
	for range 40 {
		inputs = append(inputs, rng.Uint64(), rng.Uint64()>>rng.Intn(64))
	}
	for _, name := range names {
		for _, x := range inputs {
			rc, err := mc.ExportedFunction(name).Call(ctx, x)
			require.NoError(t, err)
			ri, err := mi.ExportedFunction(name).Call(ctx, x)
			require.NoError(t, err)
			require.Equal(t, ri[0], rc[0], "%s(%#x)", name, x)
		}
	}
}

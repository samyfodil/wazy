package frontend

import (
	"bytes"
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

func Test_straightLineBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		ok   bool
	}{
		{name: "empty", body: []byte{wasm.OpcodeEnd}, ok: true},
		{
			name: "locals, constants, numerics, memory, drop, select, global.get",
			body: []byte{
				wasm.OpcodeLocalGet, 0, wasm.OpcodeLocalTee, 1, wasm.OpcodeLocalSet, 2,
				wasm.OpcodeI32Const, 0x7f, wasm.OpcodeI64Const, 0x80, 0x01,
				wasm.OpcodeF32Const, 0, 0, 0, 0, wasm.OpcodeF64Const, 0, 0, 0, 0, 0, 0, 0, 0,
				wasm.OpcodeI64Mul, wasm.OpcodeI32Eqz, wasm.OpcodeI64Extend32S, wasm.OpcodeDrop, wasm.OpcodeSelect,
				wasm.OpcodeGlobalGet, 0,
				wasm.OpcodeI32Load, 2, 0x80, 0x01, wasm.OpcodeI64Store32, 2, 8,
				wasm.OpcodeEnd,
			},
			ok: true,
		},
		{name: "memory index", body: []byte{wasm.OpcodeI32Load, 0x42, 1, 0, wasm.OpcodeEnd}},
		{name: "division", body: []byte{wasm.OpcodeI32DivU, wasm.OpcodeEnd}},
		{name: "remainder", body: []byte{wasm.OpcodeI64RemS, wasm.OpcodeEnd}},
		{name: "truncation", body: []byte{wasm.OpcodeI64TruncF64U, wasm.OpcodeEnd}},
		{name: "call", body: []byte{wasm.OpcodeCall, 0, wasm.OpcodeEnd}},
		{name: "block", body: []byte{wasm.OpcodeBlock, 0x40, wasm.OpcodeEnd, wasm.OpcodeEnd}},
		{name: "global.set", body: []byte{wasm.OpcodeI32Const, 0, wasm.OpcodeGlobalSet, 0, wasm.OpcodeEnd}},
		{name: "bytes after end", body: []byte{wasm.OpcodeEnd, wasm.OpcodeNop}},
		{name: "no end", body: []byte{wasm.OpcodeDrop}},
		{name: "truncated immediate", body: []byte{wasm.OpcodeLocalGet, 0x80}},
		{name: "truncated offset", body: []byte{wasm.OpcodeI32Load, 2, 0x80}},
		{name: "too large", body: append(bytes.Repeat([]byte{wasm.OpcodeDrop}, inlineLeafMaxBody), wasm.OpcodeEnd)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.ok, straightLineBody(tc.body))
		})
	}
}

func Test_zeroOf(t *testing.T) {
	b := ssa.NewBuilder()
	b.SetCurrentBlock(b.AllocateBasicBlock())
	for _, typ := range []ssa.Type{ssa.TypeI32, ssa.TypeI64, ssa.TypeF32, ssa.TypeF64, ssa.TypeV128} {
		v := zeroOf(b, typ)
		require.Equal(t, typ, v.Type())
		instr := b.InstructionOfValue(v)
		if typ == ssa.TypeV128 {
			lo, hi := instr.VconstData()
			require.Equal(t, [2]uint64{}, [2]uint64{lo, hi})
		} else {
			require.True(t, instr.Constant())
			require.Equal(t, uint64(0), instr.ConstantVal())
		}
	}
}

func TestCompiler_foldInlined(t *testing.T) {
	c := NewFrontendCompiler(&wasm.Module{}, ssa.NewBuilder(), nil, false, false)
	b := c.ssaBuilder
	b.SetCurrentBlock(b.AllocateBasicBlock())
	x := b.CurrentBlock().AddParam(b, ssa.TypeI64)
	zero := b.AllocateInstruction().AsIconst64(0).Insert(b).Return()
	one := b.AllocateInstruction().AsIconst64(1).Insert(b).Return()
	two := b.AllocateInstruction().AsIconst64(2).Insert(b).Return()

	_, ok := c.foldInlined(wasm.OpcodeI64Add, x, zero)
	require.False(t, ok, "outside an inlined body nothing folds")

	c.inlinedCallee = 1
	for _, tc := range []struct {
		op   wasm.Opcode
		x, y ssa.Value
		want ssa.Value
	}{
		{wasm.OpcodeI64Add, x, zero, x},
		{wasm.OpcodeI64Add, zero, x, x},
		{wasm.OpcodeI64Or, zero, x, x},
		{wasm.OpcodeI64Or, x, zero, x},
		{wasm.OpcodeI64Mul, x, zero, zero},
		{wasm.OpcodeI64Mul, zero, x, zero},
		{wasm.OpcodeI64Mul, one, x, x},
		{wasm.OpcodeI64Mul, x, one, x},
	} {
		got, ok := c.foldInlined(tc.op, tc.x, tc.y)
		require.True(t, ok)
		require.Equal(t, tc.want, got)
	}
	for _, tc := range [][2]ssa.Value{{x, two}, {two, x}} {
		_, ok := c.foldInlined(wasm.OpcodeI64Mul, tc[0], tc[1])
		require.False(t, ok)
		_, ok = c.foldInlined(wasm.OpcodeI64Add, tc[0], tc[1])
		require.False(t, ok)
	}
	_, ok = c.foldInlined(wasm.OpcodeI64Sub, x, zero)
	require.False(t, ok)
}

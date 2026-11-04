package amd64

import (
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// Test_aliasZeroExtended32 covers the pass that lets an unsigned 32->64 extend emit nothing when
// the operand's producer is lowered to an instruction writing a 32-bit register. If zeroExtends32
// ever claimed an opcode that leaves garbage in the upper half, the cases that keep their movzx
// below would lose it and the extend would read whatever the producer left there.
func Test_aliasZeroExtended32(t *testing.T) {
	setup := func(producer func(b ssa.Builder, arg ssa.Value) *ssa.Instruction, signed bool, from, to byte) (*mockCompiler, *machine, *ssa.Instruction) {
		ctx, b, m := newSetupWithMockContext()
		param := b.CurrentBlock().AddParam(b, ssa.TypeI32)
		ctx.vRegMap[param] = raxVReg
		ctx.definitions[param] = backend.SSAValueDefinition{V: param}

		v := param
		if producer != nil {
			p := producer(b, param)
			b.InsertInstruction(p)
			v = p.Return()
			ctx.vRegMap[v] = rcxVReg
			ctx.definitions[v] = backend.SSAValueDefinition{V: v, Instr: p}
		}

		ext := b.AllocateInstruction()
		if signed {
			ext.AsSExtend(v, from, to)
		} else {
			ext.AsUExtend(v, from, to)
		}
		b.InsertInstruction(ext)
		ctx.vRegMap[ext.Return()] = rdxVReg

		m.StartBlock(b.CurrentBlock())
		m.lowerExtend(v, ext.Return(), from, to, signed)
		return ctx, m, ext
	}
	iadd := func(b ssa.Builder, arg ssa.Value) *ssa.Instruction { return b.AllocateInstruction().AsIadd(arg, arg) }

	for name, producer := range map[string]func(b ssa.Builder, arg ssa.Value) *ssa.Instruction{
		"iadd": iadd,
		"isub": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction { return b.AllocateInstruction().AsIsub(arg, arg) },
		"imul": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction { return b.AllocateInstruction().AsImul(arg, arg) },
		"band": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction { return b.AllocateInstruction().AsBand(arg, arg) },
		"bxor": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
			i := b.AllocateInstruction()
			i.AsBxor(arg, arg)
			return i
		},
		"ishl": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction { return b.AllocateInstruction().AsIshl(arg, arg) },
		"sshr": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction { return b.AllocateInstruction().AsSshr(arg, arg) },
		"rotr": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
			i := b.AllocateInstruction()
			i.AsRotr(arg, arg)
			return i
		},
		"load": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
			return b.AllocateInstruction().AsLoad(arg, 0, ssa.TypeI32)
		},
		"uload16": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
			return b.AllocateInstruction().AsExtLoad(ssa.OpcodeUload16, arg, 0, false)
		},
	} {
		t.Run(name+" is aliased", func(t *testing.T) {
			ctx, m, ext := setup(producer, false, 32, 64)
			require.Equal(t, rcxVReg, ctx.VRegOf(ext.Return()))
			require.Equal(t, "nop", formatEmittedInstructionsInCurrentBlock(m))
		})
	}
	t.Run("select is not aliased", func(t *testing.T) {
		ctx, m, ext := setup(func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
			return b.AllocateInstruction().AsSelect(arg, arg, arg)
		}, false, 32, 64)
		require.Equal(t, rdxVReg, ctx.VRegOf(ext.Return()))
		require.Equal(t, "movzx.lq %rcx, %rdx\nnop", formatEmittedInstructionsInCurrentBlock(m))
	})
	t.Run("signed extend is not aliased", func(t *testing.T) {
		ctx, m, ext := setup(iadd, true, 32, 64)
		require.Equal(t, rdxVReg, ctx.VRegOf(ext.Return()))
		require.Equal(t, "movsx.lq %rcx, %rdx\nnop", formatEmittedInstructionsInCurrentBlock(m))
	})
	t.Run("narrower source is not aliased", func(t *testing.T) {
		ctx, m, ext := setup(iadd, false, 16, 64)
		require.Equal(t, rdxVReg, ctx.VRegOf(ext.Return()))
		require.Equal(t, "movzx.wq %rcx, %rdx\nnop", formatEmittedInstructionsInCurrentBlock(m))
	})
	t.Run("parameter is not aliased", func(t *testing.T) {
		// Nothing vouches for the upper half of a value with no defining instruction.
		ctx, m, ext := setup(nil, false, 32, 64)
		require.Equal(t, rdxVReg, ctx.VRegOf(ext.Return()))
		require.Equal(t, "movzx.lq %rax, %rdx\nnop", formatEmittedInstructionsInCurrentBlock(m))
	})
}

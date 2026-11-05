package amd64

import (
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// Test_aliasZeroExtended32 covers the pass that lets an unsigned 32->64 extend emit nothing, since
// every i32 in a register has its upper half zero (TestI32UpperHalf checks that per producer).
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
	t.Run("select is aliased", func(t *testing.T) {
		ctx, m, ext := setup(func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
			return b.AllocateInstruction().AsSelect(arg, arg, arg)
		}, false, 32, 64)
		require.Equal(t, rcxVReg, ctx.VRegOf(ext.Return()))
		require.Equal(t, "nop", formatEmittedInstructionsInCurrentBlock(m))
	})
	t.Run("constant is extended where it is used", func(t *testing.T) {
		ctx, m, ext := setup(func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
			return b.AllocateInstruction().AsIconst32(0xfffffff8)
		}, false, 32, 64)
		require.Equal(t, rdxVReg, ctx.VRegOf(ext.Return()))
		require.Equal(t, "movabsq $4294967288, %rdx\nnop", formatEmittedInstructionsInCurrentBlock(m))
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
	t.Run("parameter is aliased", func(t *testing.T) {
		// Parameters arrive zero-extended too: from Go through movl, from wasm as any other i32.
		ctx, m, ext := setup(nil, false, 32, 64)
		require.Equal(t, raxVReg, ctx.VRegOf(ext.Return()))
		require.Equal(t, "nop", formatEmittedInstructionsInCurrentBlock(m))
	})
}

// TestMachine_LowerConditionalBranch_orOfIcmps checks a branch on either of two comparisons: a jump on each
// to the target, and for a brz, which branches where neither holds, a jump past the second where the first
// holds and one to the target where the second does not.
func TestMachine_LowerConditionalBranch_orOfIcmps(t *testing.T) {
	for _, tc := range []struct {
		name string
		brz  bool
		exp  string
	}{
		{name: "brnz", exp: "cmpl $1, %eax\njl L1\ncmpl %ecx, %eax\njnle L1"},
		{name: "brz", brz: true, exp: "cmpl $1, %eax\njl L2\ncmpl %ecx, %eax\njle L1\nnop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, m := newSetupWithMockContext()
			m.StartLoweringFunction(1)
			entry := b.CurrentBlock()
			x, y := entry.AddParam(b, ssa.TypeI32), entry.AddParam(b, ssa.TypeI32)
			ctx.vRegMap[x], ctx.vRegMap[y] = raxVReg, rcxVReg
			ctx.definitions[x], ctx.definitions[y] = backend.SSAValueDefinition{V: x}, backend.SSAValueDefinition{V: y}
			def := func(i *ssa.Instruction) ssa.Value {
				b.InsertInstruction(i)
				ctx.definitions[i.Return()] = backend.SSAValueDefinition{Instr: i, V: i.Return()}
				return i.Return()
			}
			one := def(b.AllocateInstruction().AsIconst32(1))
			c1 := def(b.AllocateInstruction().AsIcmp(x, one, ssa.IntegerCmpCondSignedLessThan))
			c2 := def(b.AllocateInstruction().AsIcmp(x, y, ssa.IntegerCmpCondSignedGreaterThan))
			or := b.AllocateInstruction()
			or.AsBor(c1, c2)
			either := def(or)
			br := b.AllocateInstruction()
			if tc.brz {
				br.AsBrz(either, ssa.ValuesNil, b.AllocateBasicBlock())
			} else {
				br.AsBrnz(either, ssa.ValuesNil, b.AllocateBasicBlock())
			}
			b.InsertInstruction(br)
			m.LowerConditionalBranch(br)
			require.Equal(t, tc.exp, formatEmittedInstructionsInCurrentBlock(m))
		})
	}
}

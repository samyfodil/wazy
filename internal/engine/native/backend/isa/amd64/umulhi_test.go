package amd64

import (
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestMachine_lowerUmulhi(t *testing.T) {
	ctx, b, m := newSetupWithMockContext()
	x := b.CurrentBlock().AddParam(b, ssa.TypeI64)
	y := b.CurrentBlock().AddParam(b, ssa.TypeI64)
	ctx.vRegMap[x], ctx.vRegMap[y] = r8VReg, r9VReg
	ctx.definitions[x], ctx.definitions[y] = backend.SSAValueDefinition{V: x}, backend.SSAValueDefinition{V: y}
	mul := b.AllocateInstruction().AsUmulhi(x, y)
	b.InsertInstruction(mul)
	ctx.vRegMap[mul.Return()] = r10VReg

	m.lowerUmulhi(mul)
	require.Equal(t, `
movq %r8, %rax
mulq %r9
movq %rdx, %r10
`, "\n"+formatEmittedInstructionsInCurrentBlock(m)+"\n")

	mulHi := m.allocateInstr().asMulHi(newOperandReg(r9VReg), false, true)
	var regs []regalloc.VReg
	require.Equal(t, []regalloc.VReg{raxVReg, rdxVReg}, mulHi.Defs(&regs))
	require.Equal(t, []regalloc.VReg{raxVReg, r9VReg}, mulHi.Uses(&regs))
	mulHi.AssignUse(1, r11VReg)
	require.Equal(t, "mulq %r11", mulHi.String())
}

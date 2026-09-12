package riscv64

import (
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestMachine_LowerInstr_exitIfTrueWithCode_sourceOffset pins the dispatch
// between the two forms of a conditional trap. Sharing one exit sequence per
// trap kind costs the per-site trap address, which is what a DWARF backtrace
// maps back to source, so a site that carries a source offset must keep its
// sequence inline. Without the guard every trap in a debug build reports the
// island's address instead.
func TestMachine_LowerInstr_exitIfTrueWithCode_sourceOffset(t *testing.T) {
	lower := func(t *testing.T, offset ssa.SourceOffset) *machine {
		t.Helper()
		ctx, b, m := newSetupWithMockContext()
		m.maxSSABlockID, m.nextLabel = 1, 1
		ctx.vRegCounter = 10
		ctx.typeOf = map[regalloc.VRegID]ssa.Type{intToVReg(1).ID(): ssa.TypeI64}

		blk := b.CurrentBlock()
		execCtx := blk.AddParam(b, ssa.TypeI64)
		c := blk.AddParam(b, ssa.TypeI32)
		ctx.vRegMap[execCtx], ctx.vRegMap[c] = intToVReg(1), intToVReg(2)
		ctx.definitions[execCtx] = backend.SSAValueDefinition{V: execCtx}
		ctx.definitions[c] = backend.SSAValueDefinition{V: c}

		b.SetCurrentSourceOffset(offset)
		instr := b.AllocateInstruction()
		instr.AsExitIfTrueWithCode(execCtx, c, nativeapi.ExitCodeUnreachable)
		b.InsertInstruction(instr)
		require.Equal(t, offset, instr.SourceOffset())

		m.LowerInstr(instr)
		return m
	}

	t.Run("no source offset shares an island", func(t *testing.T) {
		m := lower(t, ssa.SourceOffset(-1))
		require.Equal(t, 1, len(m.trapIslands))
		require.False(t, strings.Contains(formatEmittedInstructionsInCurrentBlock(m), "exit_sequence"))
	})

	t.Run("source offset keeps the sequence inline", func(t *testing.T) {
		m := lower(t, ssa.SourceOffset(0x1234))
		require.Zero(t, len(m.trapIslands))
		require.True(t, strings.Contains(formatEmittedInstructionsInCurrentBlock(m), "exit_sequence"))
	})
}

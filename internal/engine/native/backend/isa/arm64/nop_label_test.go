package arm64

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestMachine_unlabeledNopDoesNotClaimLabelZero pins the distinction between
// "this nop anchors no label" and "this nop anchors label 0".
//
// Label 0 is a real label -- it is SSA block 0, the entry block -- so a nop
// created without one cannot leave its label field zero and be told apart. It
// used to, and every block-boundary nop therefore reported itself as anchoring
// L0, so the last one laid out overwrote the entry block's resolved offset.
// That offset feeds CompiledBlockOffsets, which builds the exception side
// table's body ranges and landing-pad offsets.
func TestMachine_unlabeledNopDoesNotClaimLabelZero(t *testing.T) {
	_, _, m := newSetupWithMockContext()
	_, ok := m.allocateNop().nop0Label()
	require.False(t, ok, "a nop created without a label must not anchor one")

	// A nop that *does* carry a label still reports it, L0 included -- the
	// sentinel must not cost us the ability to anchor the entry block.
	labelled := m.allocateInstr()
	labelled.asNop0WithLabel(0)
	l, ok := labelled.nop0Label()
	require.True(t, ok)
	require.Equal(t, label(0), l)
}

// TestMachine_entryBlockOffsetSurvivesLayout is the observable consequence:
// after laying out a function whose later blocks contain real instructions,
// the entry block must still start at offset 0.
func TestMachine_entryBlockOffsetSurvivesLayout(t *testing.T) {
	_, ssaB, m := newSetupWithMockContext()
	entry := ssaB.CurrentBlock()
	second := ssaB.AllocateBasicBlock()
	m.StartLoweringFunction(second.ID())

	m.StartBlock(entry)
	m.EndBlock()

	// The second block carries real, non-zero-width instructions, so if its
	// head nop claims L0 the entry offset moves off zero.
	m.StartBlock(second)
	for i := 0; i < 4; i++ {
		nop := m.allocateInstr()
		nop.asMove64(x1VReg, x2VReg)
		m.insert(nop)
	}
	m.FlushPendingInstructions()
	m.EndBlock()
	m.LinkAdjacentBlocks(entry, second)

	m.resolveRelativeAddresses(context.Background())

	require.Equal(t, int64(0), m.labelPositionPool.Get(0).binaryOffset,
		"entry block (L0) must start at offset 0")
}

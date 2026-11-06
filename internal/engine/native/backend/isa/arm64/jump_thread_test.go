package arm64

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestMachine_threadJumps_invertOverLabels checks that a conditional branch over an unconditional one
// becomes the opposite branch where the labels between them are ones nothing branches to any more, and
// stays where a branch still goes to one: it would land on code without the unconditional branch.
func TestMachine_threadJumps_invertOverLabels(t *testing.T) {
	for _, tc := range []struct {
		name       string
		branchedTo bool
	}{
		{name: "a label nothing branches to"},
		{name: "a label a branch goes to", branchedTo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, m := newSetupWithMockContext()
			m.nextLabel = 1
			after, afterLabel := m.allocateBrTarget()
			target, targetLabel := m.allocateBrTarget()
			// A label of its own, not one a label position begins at.
			mid := m.allocateInstr()
			midLabel := m.nextLabel
			mid.asNop0WithLabel(midLabel)
			m.nextLabel++
			cond := m.allocateInstr()
			cond.asCondBr(eq.asCond(), afterLabel, false)
			jump := m.allocateInstr()
			jump.asBr(targetLabel)
			ret := m.allocateInstr()
			ret.asRet()
			chain := []*instruction{cond, mid, jump, after, ret, target}
			if tc.branchedTo {
				other := m.allocateInstr()
				other.asBr(midLabel)
				chain = append(chain, other)
			}
			m.rootInstr = cond
			for i := 0; i+1 < len(chain); i++ {
				chain[i].next, chain[i+1].prev = chain[i+1], chain[i]
			}
			m.threadJumps()
			if tc.branchedTo {
				require.Equal(t, eq.asCond(), cond.condBrCond())
				require.Equal(t, afterLabel, cond.condBrLabel())
				require.Equal(t, br, jump.kind)
			} else {
				require.Equal(t, ne.asCond(), cond.condBrCond())
				require.Equal(t, targetLabel, cond.condBrLabel())
				require.Equal(t, nop0, jump.kind)
			}
		})
	}
}

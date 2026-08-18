package ssa

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestInstruction_InvertConditionalBrx(t *testing.T) {
	i := &Instruction{opcode: OpcodeBrnz}
	i.InvertBrx()
	require.Equal(t, OpcodeBrz, i.opcode)
	i.InvertBrx()
	require.Equal(t, OpcodeBrnz, i.opcode)
}

// TestIcmpImmRoundTrip pins the field layout AsIcmpImm and IcmpImmData share.
// Nothing constructs OpcodeIcmpImm today, so a backend reading it has no other
// way to know where the condition and the immediate live: if the two ever
// disagree the result is a silently wrong comparison rather than a crash.
func TestIcmpImmRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		imm  uint64
		cond IntegerCmpCond
	}{
		{"zero/eq", 0, IntegerCmpCondEqual},
		{"small/slt", 7, IntegerCmpCondSignedLessThan},
		{"negative as unsigned/uge", ^uint64(0), IntegerCmpCondUnsignedGreaterThanOrEqual},
		{"wide/ne", 0x1234_5678_9abc_def0, IntegerCmpCondNotEqual},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := &Instruction{}
			x := Value(3)
			i.AsIcmpImm(x, tc.imm, tc.cond)

			require.Equal(t, OpcodeIcmpImm, i.Opcode())
			require.Equal(t, TypeI32, i.typ)
			gotX, gotImm, gotCond := i.IcmpImmData()
			require.Equal(t, x, gotX)
			require.Equal(t, tc.imm, gotImm)
			require.Equal(t, tc.cond, gotCond)
		})
	}
}

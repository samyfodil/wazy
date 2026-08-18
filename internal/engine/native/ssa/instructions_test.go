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

// TestAsIreduce covers the guards AsIreduce applies when the instruction is
// built. There are only two integer types, so the sole meaningful narrowing is
// i64 to i32; rejecting the rest here means a backend never has to see one, and
// a bad caller is named by the panic instead of a shape deep in lowering.
func TestAsIreduce(t *testing.T) {
	// A Value carries its type in its high bits, so one can be made directly.
	val := func(id ValueID, typ Type) Value { return Value(id).setType(typ) }

	t.Run("narrowing is accepted", func(t *testing.T) {
		i := &Instruction{}
		i.AsIreduce(val(1, TypeI64), TypeI32)
		require.Equal(t, OpcodeIreduce, i.Opcode())
		require.Equal(t, TypeI32, i.typ)
	})

	t.Run("same width is accepted", func(t *testing.T) {
		// Not a narrowing, but not malformed either: it degenerates into a move,
		// and the backends lower it as one.
		i := &Instruction{}
		i.AsIreduce(val(1, TypeI64), TypeI64)
		require.Equal(t, TypeI64, i.typ)
	})

	t.Run("widening is rejected", func(t *testing.T) {
		err := require.CapturePanic(func() {
			(&Instruction{}).AsIreduce(val(1, TypeI32), TypeI64)
		})
		require.EqualError(t, err, "BUG: Ireduce widens i32 to i64, use AsUExtend or AsSExtend")
	})

	t.Run("non-integer destination is rejected", func(t *testing.T) {
		err := require.CapturePanic(func() {
			(&Instruction{}).AsIreduce(val(1, TypeI64), TypeF64)
		})
		require.EqualError(t, err, "BUG: Ireduce is defined on integers, got i64 to f64")
	})

	t.Run("non-integer source is rejected", func(t *testing.T) {
		err := require.CapturePanic(func() {
			(&Instruction{}).AsIreduce(val(1, TypeF32), TypeI32)
		})
		require.EqualError(t, err, "BUG: Ireduce is defined on integers, got f32 to i32")
	})
}

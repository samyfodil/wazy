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

// TestAsBitcast covers the guard AsBitcast applies to a bitcast that crosses
// register classes. Only the four wasm reinterpret pairs move bits between a
// general register and a vector one, and arm64 lowers anything else to a mov of
// the wrong width instead of failing, so the caller is named here instead. A
// bitcast within one class is a register copy both backends implement, so it
// stays constructible.
func TestAsBitcast(t *testing.T) {
	val := func(id ValueID, typ Type) Value { return Value(id).setType(typ) }

	t.Run("the reinterpret pairs are accepted", func(t *testing.T) {
		for _, tc := range []struct{ src, dst Type }{
			{TypeF32, TypeI32},
			{TypeI32, TypeF32},
			{TypeF64, TypeI64},
			{TypeI64, TypeF64},
		} {
			i := &Instruction{}
			i.AsBitcast(val(1, tc.src), tc.dst)
			require.Equal(t, OpcodeBitcast, i.Opcode())
			require.Equal(t, tc.dst, i.typ)
		}
	})

	t.Run("same register class is accepted", func(t *testing.T) {
		// Both ends live in one register, so these lower to a plain copy; the
		// arm64 backend implements and tests them.
		for _, tc := range []struct{ src, dst Type }{
			{TypeI64, TypeI64},
			{TypeI32, TypeI32},
			{TypeI64, TypeI32},
			{TypeF64, TypeF64},
			{TypeF32, TypeF32},
			{TypeV128, TypeV128},
		} {
			i := &Instruction{}
			i.AsBitcast(val(1, tc.src), tc.dst)
			require.Equal(t, tc.dst, i.typ)
		}
	})

	t.Run("a width change across classes is rejected", func(t *testing.T) {
		for _, tc := range []struct {
			src, dst Type
			expected string
		}{
			{TypeF32, TypeI64, "f32 to i64"},
			{TypeI64, TypeF32, "i64 to f32"},
			{TypeI32, TypeF64, "i32 to f64"},
			{TypeF64, TypeI32, "f64 to i32"},
			{TypeV128, TypeI64, "v128 to i64"},
			{TypeI32, TypeV128, "i32 to v128"},
		} {
			err := require.CapturePanic(func() {
				(&Instruction{}).AsBitcast(val(1, tc.src), tc.dst)
			})
			require.EqualError(t, err, "BUG: Bitcast cannot change width, got "+tc.expected+
				", bitcast at the same width first, then AsUExtend, AsSExtend or AsIreduce")
		}
	})
}

// TestAsSqmulRoundSat covers the guard on the lane of a Q15 saturating rounding
// multiplication. The operation exists on i16x8 only, and the backends disagree
// about any other lane instead of rejecting it: amd64 ignores the lane and
// always emits pmulhrsw, arm64 emits sqrdmulh at the arrangement it was given.
func TestAsSqmulRoundSat(t *testing.T) {
	val := func(id ValueID, typ Type) Value { return Value(id).setType(typ) }
	x, y := val(1, TypeV128), val(2, TypeV128)

	t.Run("i16x8 is accepted", func(t *testing.T) {
		i := &Instruction{}
		i.AsSqmulRoundSat(x, y, VecLaneI16x8)
		require.Equal(t, OpcodeSqmulRoundSat, i.Opcode())
		require.Equal(t, TypeV128, i.typ)
		gotX, gotY, gotLane := i.Arg2WithLane()
		require.Equal(t, x, gotX)
		require.Equal(t, y, gotY)
		require.Equal(t, VecLaneI16x8, gotLane)
	})

	t.Run("any other lane is rejected", func(t *testing.T) {
		for _, lane := range []VecLane{VecLaneI8x16, VecLaneI32x4, VecLaneI64x2, VecLaneF32x4, VecLaneF64x2, VecLaneInvalid} {
			err := require.CapturePanic(func() {
				(&Instruction{}).AsSqmulRoundSat(x, y, lane)
			})
			require.EqualError(t, err, "BUG: SqmulRoundSat is defined on i16x8 lanes, got "+lane.String())
		}
	})

	// The zero value of VecLane is not VecLaneInvalid, which is 1, so
	// VecLane.String panics on it; the message is formatted rather than
	// concatenated so that such a lane still names the constructor instead of
	// panicking a second time inside the panic.
	t.Run("the zero lane is rejected without a nested panic", func(t *testing.T) {
		err := require.CapturePanic(func() {
			(&Instruction{}).AsSqmulRoundSat(x, y, VecLane(0))
		})
		require.Contains(t, err.Error(), "BUG: SqmulRoundSat is defined on i16x8 lanes")
	})
}

// The guards in AsIcmp and AsVZeroExtLoad are compiled under
// nativeapi.SSAValidationEnabled, which is a constant false, so the Go compiler
// removes them from every build including this test binary. They cannot be
// triggered from a test while that constant is false, and there is no build tag
// that flips it, so they are deliberately left uncovered here rather than
// covered by a test that would only assert the valid path.

package arm64

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// zzFloatVReg is the RegTypeFloat counterpart of intToVReg in util_test.go.
func zzFloatVReg(i int) regalloc.VReg {
	return regalloc.VReg(i).SetRegType(regalloc.RegTypeFloat)
}

// TestMachine_LowerInstr_Ireduce covers the OpcodeIreduce arm of LowerInstr,
// whose i64 destination used to be a TODO panic.
//
// Expected instructions are derived from the Arm ARM: MOV (register) is an
// alias of ORR <Xd|Wd>, <XZR|WZR>, <Xm|Wm>, and a write to a W register zeroes
// bits [63:32] of the X register (Arm ARM B1.2.1), which is what makes the
// 32-bit move the truncation. Reducing to i64 keeps every bit, so it must be
// the 64-bit move rather than the 32-bit one.
func TestMachine_LowerInstr_Ireduce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src, dst ssa.Type
		exp      string
	}{
		{name: "i64 to i32", src: ssa.TypeI64, dst: ssa.TypeI32, exp: "mov w2?, w1?"},
		{name: "i32 to i32", src: ssa.TypeI32, dst: ssa.TypeI32, exp: "mov w2?, w1?"},
		// No truncation, so the whole X register moves.
		{name: "i64 to i64", src: ssa.TypeI64, dst: ssa.TypeI64, exp: "mov x2?, x1?"},
		// i32 to i64 is absent on purpose: AsIreduce rejects a widening reduce at
		// construction, so the lowering's guard for it can no longer be reached
		// from outside the ssa package. TestAsIreduce covers the rejection.
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, m := newSetupWithMockContext()
			v := b.CurrentBlock().AddParam(b, tc.src)
			ctx.vRegMap[v] = intToVReg(1)
			ctx.definitions[v] = backend.SSAValueDefinition{V: v}

			ireduce := b.AllocateInstruction()
			ireduce.AsIreduce(v, tc.dst)
			b.InsertInstruction(ireduce)
			ctx.vRegMap[ireduce.Return()] = intToVReg(2)

			m.LowerInstr(ireduce)
			require.Equal(t, tc.exp, formatEmittedInstructionsInCurrentBlock(m))
		})
	}

	// A non-integer destination is rejected by AsIreduce now, so the lowering's
	// own guard is unreachable from here; TestAsIreduce covers it instead.
}

// TestMachine_lowerBitcast_sameRegisterClass covers the two arms of
// lowerBitcast that used to be a single `panic("TODO?BUG?")`: a bitcast whose
// source and destination live in the same register class, which is a plain
// copy of the destination width.
//
// Expected instructions from the Arm ARM: MOV (register) is ORR Xd, XZR, Xm /
// ORR Wd, WZR, Wm for the general registers, and MOV (vector) is ORR
// Vd.<T>, Vn.<T>, Vn.<T> with T = 16B (Q=1, all 128 bits) or 8B (Q=0, which
// zeroes the upper half), so anything at most 64 bits wide uses the .8b form.
func TestMachine_lowerBitcast_sameRegisterClass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src, dst ssa.Type
		float    bool
		exp      string
	}{
		// Int to Int.
		{name: "i64 to i64", src: ssa.TypeI64, dst: ssa.TypeI64, exp: "mov x2?, x1?"},
		{name: "i32 to i32", src: ssa.TypeI32, dst: ssa.TypeI32, exp: "mov w2?, w1?"},
		{name: "i64 to i32", src: ssa.TypeI64, dst: ssa.TypeI32, exp: "mov w2?, w1?"},
		// Float to Float / vector to vector.
		{name: "f64 to f64", src: ssa.TypeF64, dst: ssa.TypeF64, float: true, exp: "mov v2?.8b, v1?.8b"},
		{name: "f32 to f32", src: ssa.TypeF32, dst: ssa.TypeF32, float: true, exp: "mov v2?.8b, v1?.8b"},
		{name: "v128 to v128", src: ssa.TypeV128, dst: ssa.TypeV128, float: true, exp: "mov v2?.16b, v1?.16b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, m := newSetupWithMockContext()
			v := b.CurrentBlock().AddParam(b, tc.src)
			src, dst := intToVReg(1), intToVReg(2)
			if tc.float {
				src, dst = zzFloatVReg(1), zzFloatVReg(2)
			}
			ctx.vRegMap[v] = src
			ctx.definitions[v] = backend.SSAValueDefinition{V: v}

			bitcast := b.AllocateInstruction()
			bitcast.AsBitcast(v, tc.dst)
			b.InsertInstruction(bitcast)
			ctx.vRegMap[bitcast.Return()] = dst

			m.LowerInstr(bitcast)
			require.Equal(t, tc.exp, formatEmittedInstructionsInCurrentBlock(m))
		})
	}
}

// TestMachine_InsertMove_defaultType covers the default arm of InsertMove,
// which used to be `panic("TODO")`. A type outside the five ssa.Type values
// falls back to the register class: the full X register for a general
// register, the full 128-bit vector register otherwise.
//
// This pins that the class decides the move, not which operand the class is
// read from: a move only makes sense between registers of the same class, so
// there is no meaningful case where the source and the destination disagree and
// nothing to assert about preferring one over the other.
func TestMachine_InsertMove_defaultType(t *testing.T) {
	// ssa.Type(0) is ssa.typeInvalid, the only value the switch above does not name.
	const unnamedType = ssa.Type(0)

	t.Run("int register", func(t *testing.T) {
		_, _, m := newSetupWithMockContext()
		m.InsertMove(intToVReg(1), intToVReg(2), unnamedType)
		require.Equal(t, "mov x1?, x2?", formatEmittedInstructionsInCurrentBlock(m))
	})
	t.Run("float register", func(t *testing.T) {
		_, _, m := newSetupWithMockContext()
		m.InsertMove(zzFloatVReg(1), zzFloatVReg(2), unnamedType)
		require.Equal(t, "mov v1?.16b, v2?.16b", formatEmittedInstructionsInCurrentBlock(m))
	})
}

// TestMachine_lowerIcmpToFlag_mixedWidths covers the branch that used to be
// `panic("TODO(maybe): support icmp with different types")`.
//
// SUBS has a 32-bit and a 64-bit form only (Arm ARM C6.2.x, selected by sf), so
// a mixed-width comparison must happen at the wider width, with the narrower
// operand extended into it: SXTW for a signed comparison and UXTW for an
// unsigned one, both of which preserve the ordering of the original values.
func TestMachine_lowerIcmpToFlag_mixedWidths(t *testing.T) {
	for _, tc := range []struct {
		name         string
		xType, yType ssa.Type
		signed       bool
		exp          []string
	}{
		// Same width: unchanged behaviour, no extension at all.
		{
			name: "i32/i32 signed", xType: ssa.TypeI32, yType: ssa.TypeI32, signed: true,
			exp: []string{"subs wzr, w1?, w2?"},
		},
		{
			name: "i64/i64 unsigned", xType: ssa.TypeI64, yType: ssa.TypeI64, signed: false,
			exp: []string{"subs xzr, x1?, x2?"},
		},
		// x is the narrow one: it is the first operand, which must be a pure register,
		// so it is widened by getOperand_NR before the 64-bit subs.
		{
			name: "i32/i64 signed", xType: ssa.TypeI32, yType: ssa.TypeI64, signed: true,
			exp: []string{"sxtw x11?, w1?", "subs xzr, x11?, x2?"},
		},
		{
			name: "i32/i64 unsigned", xType: ssa.TypeI32, yType: ssa.TypeI64, signed: false,
			exp: []string{"uxtw x11?, w1?", "subs xzr, x11?, x2?"},
		},
		// y is the narrow one: it takes the same widening, not the imm12/ER/SR form.
		{
			name: "i64/i32 signed", xType: ssa.TypeI64, yType: ssa.TypeI32, signed: true,
			exp: []string{"sxtw x11?, w2?", "subs xzr, x1?, x11?"},
		},
		{
			name: "i64/i32 unsigned", xType: ssa.TypeI64, yType: ssa.TypeI32, signed: false,
			exp: []string{"uxtw x11?, w2?", "subs xzr, x1?, x11?"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, m := newSetupWithMockContext()
			ctx.vRegCounter = 10 // Keep the allocated temporaries clear of x1?/x2?.
			blk := b.CurrentBlock()
			x := blk.AddParam(b, tc.xType)
			y := blk.AddParam(b, tc.yType)
			ctx.vRegMap[x], ctx.vRegMap[y] = intToVReg(1), intToVReg(2)
			ctx.definitions[x] = backend.SSAValueDefinition{V: x}
			ctx.definitions[y] = backend.SSAValueDefinition{V: y}

			m.lowerIcmpToFlag(x, y, tc.signed)
			require.Equal(t, strings.Join(tc.exp, "\n"), formatEmittedInstructionsInCurrentBlock(m))
		})
	}

	// A narrower y must not be folded into the shifted-register operand: SUBS
	// (shifted register) has no extend field, so folding the shift would compare
	// a 32-bit value against a 64-bit one without extending it.
	t.Run("narrow y that could be a shifted register is not folded", func(t *testing.T) {
		ctx, b, m := newSetupWithMockContext()
		ctx.vRegCounter = 10
		blk := b.CurrentBlock()
		x := blk.AddParam(b, ssa.TypeI64)
		p := blk.AddParam(b, ssa.TypeI32)
		ctx.vRegMap[x], ctx.vRegMap[p] = intToVReg(1), intToVReg(2)
		ctx.definitions[x] = backend.SSAValueDefinition{V: x}
		ctx.definitions[p] = backend.SSAValueDefinition{V: p}

		amount := b.AllocateInstruction().AsIconst32(3).Insert(b)
		ctx.definitions[amount.Return()] = backend.SSAValueDefinition{Instr: amount, V: amount.Return()}

		ishl := b.AllocateInstruction()
		ishl.AsIshl(p, amount.Return())
		b.InsertInstruction(ishl)
		y := ishl.Return()
		ctx.vRegMap[y] = intToVReg(3)
		ctx.definitions[y] = backend.SSAValueDefinition{Instr: ishl, V: y}

		m.lowerIcmpToFlag(x, y, true)
		require.Equal(t, "sxtw x11?, w3?\nsubs xzr, x1?, x11?",
			formatEmittedInstructionsInCurrentBlock(m))
		// The shift stays a real instruction, lowered on its own.
		require.False(t, ishl.Lowered())
	})
}

// TestMachine_lowerFcmpToFlag_mixedPrecision covers the branch that used to be
// `panic("TODO(maybe): support icmp with different types")` in lowerFcmpToFlag.
//
// FCMP (Arm ARM C7.2.51) has an Sn, Sm form and a Dn, Dm form and nothing
// mixed, so the single-precision side is promoted with FCVT (f32 -> f64 is
// exact, so the comparison result is unchanged) and the comparison is then the
// double-precision one.
func TestMachine_lowerFcmpToFlag_mixedPrecision(t *testing.T) {
	for _, tc := range []struct {
		name         string
		xType, yType ssa.Type
		exp          []string
	}{
		{name: "f32/f32", xType: ssa.TypeF32, yType: ssa.TypeF32, exp: []string{"fcmp s1?, s2?"}},
		{name: "f64/f64", xType: ssa.TypeF64, yType: ssa.TypeF64, exp: []string{"fcmp d1?, d2?"}},
		{
			name: "f32/f64", xType: ssa.TypeF32, yType: ssa.TypeF64,
			exp: []string{"fcvt d11?, s1?", "fcmp d11?, d2?"},
		},
		{
			name: "f64/f32", xType: ssa.TypeF64, yType: ssa.TypeF32,
			exp: []string{"fcvt d11?, s2?", "fcmp d1?, d11?"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, m := newSetupWithMockContext()
			ctx.vRegCounter = 10
			blk := b.CurrentBlock()
			x := blk.AddParam(b, tc.xType)
			y := blk.AddParam(b, tc.yType)
			ctx.vRegMap[x], ctx.vRegMap[y] = zzFloatVReg(1), zzFloatVReg(2)
			ctx.definitions[x] = backend.SSAValueDefinition{V: x}
			ctx.definitions[y] = backend.SSAValueDefinition{V: y}

			m.lowerFcmpToFlag(x, y)
			require.Equal(t, strings.Join(tc.exp, "\n"), formatEmittedInstructionsInCurrentBlock(m))
		})
	}

	t.Run("non-float operand panics", func(t *testing.T) {
		ctx, b, m := newSetupWithMockContext()
		blk := b.CurrentBlock()
		x := blk.AddParam(b, ssa.TypeI32)
		y := blk.AddParam(b, ssa.TypeF64)
		ctx.vRegMap[x], ctx.vRegMap[y] = intToVReg(1), zzFloatVReg(2)
		ctx.definitions[x] = backend.SSAValueDefinition{V: x}
		ctx.definitions[y] = backend.SSAValueDefinition{V: y}

		err := require.CapturePanic(func() { m.lowerFcmpToFlag(x, y) })
		require.EqualError(t, err,
			fmt.Sprintf("BUG: fcmp on non-float types: v%d=i32, v%d=f64", x.ID(), y.ID()))
	})
}

// Test_invertTrapCond covers the new invertTrapCond helper. Inverting the
// condition of a conditional trap is what lets the inline form branch over the
// exit sequence: CBZ is the inverse of CBNZ (Arm ARM C6.2.44/C6.2.45) and each
// B.cond flag has the inverse listed in the condition-code table (C1.2.4).
func Test_invertTrapCond(t *testing.T) {
	r := intToVReg(7)
	require.Equal(t, registerAsRegZeroCond(r), invertTrapCond(registerAsRegNotZeroCond(r)))
	require.Equal(t, registerAsRegNotZeroCond(r), invertTrapCond(registerAsRegZeroCond(r)))

	for _, tc := range []struct{ in, exp condFlag }{
		{eq, ne},
		{ne, eq},
		{hs, lo},
		{lo, hs},
		{mi, pl},
		{pl, mi},
		{vs, vc},
		{vc, vs},
		{hi, ls},
		{ls, hi},
		{ge, lt},
		{lt, ge},
		{gt, le},
		{le, gt},
		// al and nv are deliberately absent. 0b1110 is AL and 0b1111 is NV, and
		// the Arm ARM gives NV only so 0b1111 disassembles; it behaves as
		// "always" exactly like AL, so neither inverts the other. condFlag.invert
		// pairs them anyway, which is fine because lowerTrapCond only ever
		// returns a flag from condFlagFromSSAIntegerCmpCond, i.e. one of the
		// pairs above.
	} {
		t.Run(tc.in.String(), func(t *testing.T) {
			require.Equal(t, tc.exp.asCond(), invertTrapCond(tc.in.asCond()))
		})
	}
}

// TestMachine_lowerExitIfTrueWithCode_nonIcmpCondition covers the fallback that
// used to be `panic("TODO: OpcodeExitIfTrueWithCode must come after Icmp ...")`
// in both the shared-island and the inline form of a conditional trap.
//
// When the comparison is no longer foldable the condition is already a 0-or-1
// value in a register, so the branch is CBNZ (Arm ARM C6.2.45) on the taken
// path and its inverse CBZ when branching over an inlined exit sequence.
func TestMachine_lowerExitIfTrueWithCode_nonIcmpCondition(t *testing.T) {
	// setup registers a condition value that is a plain block parameter, i.e. one
	// that does not come from an instruction at all, so MatchInstr cannot fold it.
	setup := func(ctx *mockCompiler, b ssa.Builder, m *machine) ssa.Value {
		m.maxSSABlockID, m.nextLabel = 1, 1
		ctx.vRegCounter = 10
		ctx.typeOf = map[regalloc.VRegID]ssa.Type{intToVReg(1).ID(): ssa.TypeI64}
		c := b.CurrentBlock().AddParam(b, ssa.TypeI32)
		ctx.vRegMap[c] = intToVReg(2)
		ctx.definitions[c] = backend.SSAValueDefinition{V: c}
		return c
	}

	t.Run("shared island", func(t *testing.T) {
		ctx, b, m := newSetupWithMockContext()
		c := setup(ctx, b, m)
		m.lowerExitIfTrueWithCodeShared(intToVReg(1), c, nativeapi.ExitCodeUnreachable)
		require.Equal(t, `
mov x27, x1?
cbnz w2?, L1
`, "\n"+formatEmittedInstructionsInCurrentBlock(m)+"\n")
	})

	t.Run("inline", func(t *testing.T) {
		ctx, b, m := newSetupWithMockContext()
		c := setup(ctx, b, m)
		m.lowerExitIfTrueWithCode(intToVReg(1), c, nativeapi.ExitCodeUnreachable)
		// ExitCodeUnreachable == 3, hence the movz of #0x3.
		require.Equal(t, `
mov x11?, x1?
cbz w2?, (L1)
movz x12?, #0x3, lsl 0
str w12?, [x11?]
mov x13?, sp
str x13?, [x11?, #0x38]
adr x14?, #0x0
str x14?, [x11?, #0x30]
exit_sequence x11?
L1:
`, "\n"+formatEmittedInstructionsInCurrentBlock(m)+"\n")
	})
}

// TestMachine_lowerExitIfTrueWithCode_icmpCondition pins the foldable shape,
// which the fallback above must not have changed: the comparison sets the flags
// and the branch is a B.cond, inverted for the inline form.
func TestMachine_lowerExitIfTrueWithCode_icmpCondition(t *testing.T) {
	setup := func(ctx *mockCompiler, b ssa.Builder, m *machine) (ssa.Value, *ssa.Instruction) {
		m.maxSSABlockID, m.nextLabel = 1, 1
		ctx.vRegCounter = 10
		ctx.typeOf = map[regalloc.VRegID]ssa.Type{intToVReg(3).ID(): ssa.TypeI64}
		blk := b.CurrentBlock()
		x := blk.AddParam(b, ssa.TypeI32)
		y := blk.AddParam(b, ssa.TypeI32)
		ctx.vRegMap[x], ctx.vRegMap[y] = intToVReg(1), intToVReg(2)
		ctx.definitions[x] = backend.SSAValueDefinition{V: x}
		ctx.definitions[y] = backend.SSAValueDefinition{V: y}

		icmp := b.AllocateInstruction()
		icmp.AsIcmp(x, y, ssa.IntegerCmpCondEqual)
		b.InsertInstruction(icmp)
		c := icmp.Return()
		ctx.vRegMap[c] = intToVReg(4)
		ctx.definitions[c] = backend.SSAValueDefinition{Instr: icmp, V: c}
		return c, icmp
	}

	t.Run("shared island", func(t *testing.T) {
		ctx, b, m := newSetupWithMockContext()
		c, icmp := setup(ctx, b, m)
		m.lowerExitIfTrueWithCodeShared(intToVReg(3), c, nativeapi.ExitCodeUnreachable)
		require.True(t, icmp.Lowered())
		require.Equal(t, `
subs wzr, w1?, w2?
mov x27, x3?
b.eq L1
`, "\n"+formatEmittedInstructionsInCurrentBlock(m)+"\n")
	})

	t.Run("inline", func(t *testing.T) {
		ctx, b, m := newSetupWithMockContext()
		c, icmp := setup(ctx, b, m)
		m.lowerExitIfTrueWithCode(intToVReg(3), c, nativeapi.ExitCodeUnreachable)
		require.True(t, icmp.Lowered())
		require.Equal(t, `
subs wzr, w1?, w2?
mov x11?, x3?
b.ne L1
movz x12?, #0x3, lsl 0
str w12?, [x11?]
mov x13?, sp
str x13?, [x11?, #0x38]
adr x14?, #0x0
str x14?, [x11?, #0x30]
exit_sequence x11?
L1:
`, "\n"+formatEmittedInstructionsInCurrentBlock(m)+"\n")
	})
}

// TestMachine_lowerSelect_nonIntegerCondition covers the arms that used to be
// `panic("TODO?BUG?: support select with non-integer condition")`.
//
// The condition holds when it is non-zero. For a float the zero to compare
// against is built with INS Vd.D[0], XZR (Arm ARM C7.2.x MOV (general)), which
// zeroes the low 64 bits and so covers both the S and the D view; the compare
// is then FCMP. For a vector the test is "any bit set", which UMAXP Vd.16B,
// Vn.16B, Vn.16B folds into the low 8 bytes, so D[0] is zero iff the whole
// vector is, exactly as OpcodeVanyTrue is lowered.
func TestMachine_lowerSelect_nonIntegerCondition(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cType   ssa.Type
		xyType  ssa.Type
		floatXY bool
		exp     []string
	}{
		// Integer conditions: unchanged behaviour.
		{
			name: "i32 condition", cType: ssa.TypeI32, xyType: ssa.TypeI32,
			exp: []string{"subs wzr, w1?, wzr", "csel w4?, w2?, w3?, ne"},
		},
		{
			name: "i64 condition", cType: ssa.TypeI64, xyType: ssa.TypeI32,
			exp: []string{"subs xzr, x1?, xzr", "csel w4?, w2?, w3?, ne"},
		},
		// Newly implemented: float conditions.
		{
			name: "f32 condition", cType: ssa.TypeF32, xyType: ssa.TypeI32,
			exp: []string{"ins v11?.d[0], xzr", "fcmp s1?, s11?", "csel w4?, w2?, w3?, ne"},
		},
		{
			name: "f64 condition", cType: ssa.TypeF64, xyType: ssa.TypeI32,
			exp: []string{"ins v11?.d[0], xzr", "fcmp d1?, d11?", "csel w4?, w2?, w3?, ne"},
		},
		// Newly implemented: a vector condition, selecting between two floats.
		{
			name: "v128 condition", cType: ssa.TypeV128, xyType: ssa.TypeF64, floatXY: true,
			exp: []string{
				"umaxp v11?.16b, v1?.16b, v1?.16b",
				"mov x12?, v11?.d[0]",
				"subs xzr, x12?, xzr",
				"fcsel d4?, d2?, d3?, ne",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, m := newSetupWithMockContext()
			ctx.vRegCounter = 10
			blk := b.CurrentBlock()

			c := blk.AddParam(b, tc.cType)
			if tc.cType.IsInt() {
				ctx.vRegMap[c] = intToVReg(1)
			} else {
				ctx.vRegMap[c] = zzFloatVReg(1)
			}
			ctx.definitions[c] = backend.SSAValueDefinition{V: c}

			x := blk.AddParam(b, tc.xyType)
			y := blk.AddParam(b, tc.xyType)
			xReg, yReg, rdReg := intToVReg(2), intToVReg(3), intToVReg(4)
			if tc.floatXY {
				xReg, yReg, rdReg = zzFloatVReg(2), zzFloatVReg(3), zzFloatVReg(4)
			}
			ctx.vRegMap[x], ctx.vRegMap[y] = xReg, yReg
			ctx.definitions[x] = backend.SSAValueDefinition{V: x}
			ctx.definitions[y] = backend.SSAValueDefinition{V: y}

			sel := b.AllocateInstruction()
			sel.AsSelect(c, x, y)
			b.InsertInstruction(sel)
			ctx.vRegMap[sel.Return()] = rdReg

			m.lowerSelect(c, x, y, sel.Return())
			require.Equal(t, strings.Join(tc.exp, "\n"), formatEmittedInstructionsInCurrentBlock(m))
		})
	}
}

// TestMachine_lowerBitwiseNot covers lowerBitwiseNot, the new OpcodeBnot arm.
//
// ssa has no AsBnot builder (nothing in the frontend emits OpcodeBnot today),
// and the opcode field is unexported, so the instruction is shaped with AsClz,
// another single-argument op of the same type: lowerBitwiseNot reads only
// Arg() and Return().
//
// The expected encoding is MVN <Xd>, <Xm>, the alias of ORN <Xd>, XZR, <Xm>.
// "Logical (shifted register)" is sf|opc(2)|01010|shift(2)|N|Rm(5)|imm6(6)|
// Rn(5)|Rd(5) with opc=01 and N=1 for ORN, so `mvn x3, x1` is
// 1_01_01010_00_1_00001_000000_11111_00011 = 0xaa2103e3, and the 32-bit form
// `mvn w3, w1` is the same with sf=0, i.e. 0x2a2103e3.
func TestMachine_lowerBitwiseNot(t *testing.T) {
	for _, tc := range []struct {
		name          string
		typ           ssa.Type
		expectedAsm   string
		expectedBytes string
	}{
		{name: "64-bit", typ: ssa.TypeI64, expectedAsm: "orn x3, xzr, x1", expectedBytes: "e30321aa"},
		{name: "32-bit", typ: ssa.TypeI32, expectedAsm: "orn w3, wzr, w1", expectedBytes: "e303212a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, m := newSetupWithMockContext()
			v := b.CurrentBlock().AddParam(b, tc.typ)
			ctx.vRegMap[v] = x1VReg
			ctx.definitions[v] = backend.SSAValueDefinition{V: v}

			not := b.AllocateInstruction()
			not.AsClz(v)
			b.InsertInstruction(not)
			ctx.vRegMap[not.Return()] = x3VReg

			m.lowerBitwiseNot(not)
			require.Equal(t, tc.expectedAsm, formatEmittedInstructionsInCurrentBlock(m))

			m.FlushPendingInstructions()
			m.encode(m.perBlockHead)
			require.Equal(t, tc.expectedBytes, hex.EncodeToString(m.compiler.Buf()))
		})
	}

	t.Run("non-integer argument panics", func(t *testing.T) {
		ctx, b, m := newSetupWithMockContext()
		v := b.CurrentBlock().AddParam(b, ssa.TypeF64)
		ctx.vRegMap[v] = zzFloatVReg(1)
		ctx.definitions[v] = backend.SSAValueDefinition{V: v}

		not := b.AllocateInstruction()
		not.AsClz(v)
		b.InsertInstruction(not)
		ctx.vRegMap[not.Return()] = zzFloatVReg(2)

		err := require.CapturePanic(func() { m.lowerBitwiseNot(not) })
		require.EqualError(t, err, "BUG: Bnot on f64")
	})
}

// Test_operandER_subWordTarget covers the `to < 32` case of operandER, which
// used to be `panic("TODO?BUG?: when we need to extend to less than 32 bits?")`.
//
// The extended-register form has no result narrower than a W register: its only
// width control is sf (ADD (extended register), Arm ARM C6.2.5), which
// encodeAluRRRExtend sets only for to == 64. An 8- or 16-bit target therefore
// has to be represented as the 32-bit one.
func Test_operandER_subWordTarget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		to    byte
		expTo byte
		eop   extendOp
	}{
		{name: "to 8", to: 8, expTo: 32, eop: extendOpSXTB},
		{name: "to 16", to: 16, expTo: 32, eop: extendOpSXTH},
		{name: "to 32 unchanged", to: 32, expTo: 32, eop: extendOpUXTB},
		{name: "to 64 unchanged", to: 64, expTo: 64, eop: extendOpSXTW},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := operandER(intToVReg(1), tc.eop, tc.to)
			r, eop, to := op.er()
			require.Equal(t, operandKindER, op.kind)
			require.Equal(t, intToVReg(1), r)
			require.Equal(t, tc.eop, eop)
			require.Equal(t, tc.expTo, to)
		})
	}

	// An 8 -> 16 extension merged with extModeNone is the shape that reaches the
	// widened case through getOperand_ER_SR_NR.
	t.Run("merged 8->16 extension", func(t *testing.T) {
		ctx, b, m := newSetupWithMockContext()
		v := b.CurrentBlock().AddParam(b, ssa.TypeI64)
		ext := b.AllocateInstruction()
		ext.AsSExtend(v, 8, 16)
		b.InsertInstruction(ext)
		ctx.vRegMap[ext.Arg()] = intToVReg(10)
		ctx.definitions[v] = backend.SSAValueDefinition{V: ext.Arg()}

		def := backend.SSAValueDefinition{Instr: ext, V: ext.Return()}
		require.Equal(t, operandER(intToVReg(10), extendOpSXTB, 32),
			m.getOperand_ER_SR_NR(def, extModeNone))
		require.True(t, ext.Lowered())
		require.Equal(t, "", formatEmittedInstructionsInCurrentBlock(m))
	})

	// And the widening has to produce the 32-bit ADD (extended register):
	// sf=0|op=0|S=0|01011|00|1|Rm|option|imm3|Rn|Rd with option=100 for SXTB,
	// i.e. `add w3, w1, w2, sxtb` = 0000_1011_0010_0010_1000_0000_0010_0011.
	t.Run("encodes as the 32-bit extended-register form", func(t *testing.T) {
		_, _, m := newSetupWithMockContext()
		alu := m.allocateInstr()
		alu.asALU(aluOpAdd, x3VReg, operandNR(x1VReg), operandER(x2VReg, extendOpSXTB, 8), false)
		m.insert(alu)
		require.Equal(t, "add w3, w1, w2 SXTB", formatEmittedInstructionsInCurrentBlock(m))

		m.FlushPendingInstructions()
		m.encode(m.perBlockHead)
		require.Equal(t, "2380220b", hex.EncodeToString(m.compiler.Buf()))
	})
}

// TestMachine_lowerSubOrAdd_mulAccumulate covers the MADD/MSUB fold: a multiply
// feeding an add or a subtract becomes one instruction instead of two.
//
// Expected encodings are the "Data-processing (3 source)" forms of the Arm ARM:
// MADD <Xd>, <Xn>, <Xm>, <Xa> computes Xa + Xn*Xm and MSUB computes Xa - Xn*Xm,
// which is why only the right-hand operand of a subtract can be the multiply.
func TestMachine_lowerSubOrAdd_mulAccumulate(t *testing.T) {
	// setup builds the multiply feeding an add/sub and lowers the add/sub.
	setup := func(typ ssa.Type, add, mulLeft bool, mulRefCount uint32, accIsConst bool) *machine {
		ctx, b, m := newSetupWithMockContext()
		blk := b.CurrentBlock()

		mkParam := func(r regalloc.VReg) ssa.Value {
			v := blk.AddParam(b, typ)
			ctx.vRegMap[v] = r
			ctx.definitions[v] = backend.SSAValueDefinition{V: v}
			return v
		}
		a, c := mkParam(x2VReg), mkParam(x3VReg)

		var acc ssa.Value
		if accIsConst {
			k := b.AllocateInstruction()
			k.AsIconst64(0x20)
			b.InsertInstruction(k)
			acc = k.Return()
			ctx.vRegMap[acc] = x1VReg
			ctx.definitions[acc] = backend.SSAValueDefinition{Instr: k, V: acc}
		} else {
			acc = mkParam(x1VReg)
		}

		mul := b.AllocateInstruction()
		mul.AsImul(a, c)
		b.InsertInstruction(mul)
		mulVal := mul.Return()
		ctx.vRegMap[mulVal] = x4VReg
		ctx.definitions[mulVal] = backend.SSAValueDefinition{Instr: mul, V: mulVal, RefCount: mulRefCount}

		alu := b.AllocateInstruction()
		switch {
		case mulLeft:
			alu.AsIadd(mulVal, acc)
		case add:
			alu.AsIadd(acc, mulVal)
		default:
			alu.AsIsub(acc, mulVal)
		}
		b.InsertInstruction(alu)
		ctx.vRegMap[alu.Return()] = x5VReg

		m.lowerSubOrAdd(alu, add)
		return m
	}

	for _, tc := range []struct {
		name          string
		typ           ssa.Type
		add, mulLeft  bool
		expectedAsm   string
		expectedBytes string
	}{
		{
			name: "add i64", typ: ssa.TypeI64, add: true,
			expectedAsm: "madd x5, x2, x3, x1", expectedBytes: "4504039b",
		},
		{
			name: "add i32", typ: ssa.TypeI32, add: true,
			expectedAsm: "madd w5, w2, w3, w1", expectedBytes: "4504031b",
		},
		{
			// x - a*c is MSUB; the accumulator stays the left operand.
			name: "sub i64", typ: ssa.TypeI64,
			expectedAsm: "msub x5, x2, x3, x1", expectedBytes: "4584039b",
		},
		{
			name: "sub i32", typ: ssa.TypeI32,
			expectedAsm: "msub w5, w2, w3, w1", expectedBytes: "4584031b",
		},
		{
			// Addition commutes, so the multiply may be the left operand too.
			name: "add i64 with the multiply on the left", typ: ssa.TypeI64, add: true, mulLeft: true,
			expectedAsm: "madd x5, x2, x3, x1", expectedBytes: "4504039b",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := setup(tc.typ, tc.add, tc.mulLeft, 0, false)
			require.Equal(t, tc.expectedAsm, formatEmittedInstructionsInCurrentBlock(m))

			m.FlushPendingInstructions()
			m.encode(m.perBlockHead)
			require.Equal(t, tc.expectedBytes, hex.EncodeToString(m.compiler.Buf()))
		})
	}

	// The cases that must NOT fold. Each keeps the plain two-instruction shape,
	// so the multiply is lowered on its own and only the ALU op appears here.
	t.Run("multiply on the left of a subtract", func(t *testing.T) {
		// (a*c) - x has no MSUB form: MSUB subtracts the product, not from it.
		ctx, b, m := newSetupWithMockContext()
		blk := b.CurrentBlock()
		mkParam := func(r regalloc.VReg) ssa.Value {
			v := blk.AddParam(b, ssa.TypeI64)
			ctx.vRegMap[v] = r
			ctx.definitions[v] = backend.SSAValueDefinition{V: v}
			return v
		}
		a, c, x := mkParam(x2VReg), mkParam(x3VReg), mkParam(x1VReg)

		mul := b.AllocateInstruction()
		mul.AsImul(a, c)
		b.InsertInstruction(mul)
		ctx.vRegMap[mul.Return()] = x4VReg
		ctx.definitions[mul.Return()] = backend.SSAValueDefinition{Instr: mul, V: mul.Return()}

		sub := b.AllocateInstruction()
		sub.AsIsub(mul.Return(), x)
		b.InsertInstruction(sub)
		ctx.vRegMap[sub.Return()] = x5VReg

		m.lowerSubOrAdd(sub, false)
		require.Equal(t, "sub x5, x4, x1", formatEmittedInstructionsInCurrentBlock(m))
		require.False(t, mul.Lowered())
	})

	t.Run("multiply used more than once", func(t *testing.T) {
		m := setup(ssa.TypeI64, true, false, 2, false)
		require.Equal(t, "add x5, x1, x4", formatEmittedInstructionsInCurrentBlock(m))
	})

	t.Run("constant accumulator keeps the immediate form", func(t *testing.T) {
		// (a*c) + 0x20: folding would have to materialize 0x20 into a register,
		// while the plain form keeps it as the imm12 of the ADD.
		m := setup(ssa.TypeI64, true, true, 0, true)
		require.Equal(t, "add x5, x4, #0x20", formatEmittedInstructionsInCurrentBlock(m))
	})
}

// Test_aliasZeroExtended32 covers the pass that lets an unsigned 32->64 extend
// emit nothing: when the operand's producer already zeroed bits [63:32], the
// extend's result is given the operand's own virtual register.
//
// The two halves have to stay in step. If zeroExtends32 ever claimed an opcode
// that leaves garbage in the upper half, or if aliasZeroExtended32 stopped
// running before lowering, the "no alias" cases below would silently lose their
// uxtw and the extend would read whatever the producer left there.
func Test_aliasZeroExtended32(t *testing.T) {
	// setup builds `v = <producer>` followed by an extend of v, runs the alias
	// pass over the block, then lowers just the extend.
	setup := func(t *testing.T, producer func(b ssa.Builder, arg ssa.Value) *ssa.Instruction, signed bool, from, to byte) (*mockCompiler, *machine, *ssa.Instruction) {
		ctx, b, m := newSetupWithMockContext()
		param := b.CurrentBlock().AddParam(b, ssa.TypeI32)
		ctx.vRegMap[param] = intToVReg(1)
		ctx.definitions[param] = backend.SSAValueDefinition{V: param}

		p := producer(b, param)
		b.InsertInstruction(p)
		v := p.Return()
		ctx.vRegMap[v] = intToVReg(2)
		ctx.definitions[v] = backend.SSAValueDefinition{V: v, Instr: p}

		ext := b.AllocateInstruction()
		if signed {
			ext.AsSExtend(v, from, to)
		} else {
			ext.AsUExtend(v, from, to)
		}
		b.InsertInstruction(ext)
		ctx.vRegMap[ext.Return()] = intToVReg(3)

		m.StartBlock(b.CurrentBlock()) // also pins that StartBlock is where the alias pass runs.
		m.lowerExtend(v, ext.Return(), from, to, signed)
		return ctx, m, ext
	}

	iadd32 := func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
		return b.AllocateInstruction().AsIadd(arg, arg)
	}
	imul32 := func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
		return b.AllocateInstruction().AsImul(arg, arg)
	}

	t.Run("zero-extending producer: aliased, nothing emitted", func(t *testing.T) {
		ctx, m, ext := setup(t, iadd32, false, 32, 64)
		require.Equal(t, intToVReg(2), ctx.VRegOf(ext.Return()))
		require.Equal(t, "nop0", formatEmittedInstructionsInCurrentBlock(m))
	})
	t.Run("other producer: not aliased, uxtw emitted", func(t *testing.T) {
		ctx, m, ext := setup(t, imul32, false, 32, 64)
		require.Equal(t, intToVReg(3), ctx.VRegOf(ext.Return()))
		require.Equal(t, "uxtw x3?, w2?\nnop0", formatEmittedInstructionsInCurrentBlock(m))
	})
	t.Run("signed extend is never aliased", func(t *testing.T) {
		ctx, m, ext := setup(t, iadd32, true, 32, 64)
		require.Equal(t, intToVReg(3), ctx.VRegOf(ext.Return()))
		require.Equal(t, "sxtw x3?, w2?\nnop0", formatEmittedInstructionsInCurrentBlock(m))
	})
	t.Run("narrower source is never aliased", func(t *testing.T) {
		ctx, m, ext := setup(t, iadd32, false, 8, 64)
		require.Equal(t, intToVReg(3), ctx.VRegOf(ext.Return()))
		require.Equal(t, "uxtb x3?, w2?\nnop0", formatEmittedInstructionsInCurrentBlock(m))
	})
	t.Run("operand with no producer is never aliased", func(t *testing.T) {
		ctx, b, m := newSetupWithMockContext()
		// A function/block parameter has no defining instruction, so nothing vouches for its
		// upper half -- the entry ABI is free to hand over a full 64-bit register.
		param := b.CurrentBlock().AddParam(b, ssa.TypeI32)
		ctx.vRegMap[param] = intToVReg(1)
		ctx.definitions[param] = backend.SSAValueDefinition{V: param}
		ext := b.AllocateInstruction()
		ext.AsUExtend(param, 32, 64)
		b.InsertInstruction(ext)
		ctx.vRegMap[ext.Return()] = intToVReg(3)

		m.StartBlock(b.CurrentBlock())
		m.lowerExtend(param, ext.Return(), 32, 64, false)
		require.Equal(t, intToVReg(3), ctx.VRegOf(ext.Return()))
		require.Equal(t, "uxtw x3?, w1?\nnop0", formatEmittedInstructionsInCurrentBlock(m))
	})
	t.Run("every trusted producer is aliased", func(t *testing.T) {
		for name, producer := range map[string]func(b ssa.Builder, arg ssa.Value) *ssa.Instruction{
			"band": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
				return b.AllocateInstruction().AsBand(arg, arg)
			},
			"ishl": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
				return b.AllocateInstruction().AsIshl(arg, arg)
			},
			"ushr": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
				return b.AllocateInstruction().AsUshr(arg, arg)
			},
			"load": func(b ssa.Builder, arg ssa.Value) *ssa.Instruction {
				return b.AllocateInstruction().AsLoad(arg, 0, ssa.TypeI32)
			},
		} {
			t.Run(name, func(t *testing.T) {
				ctx, m, ext := setup(t, producer, false, 32, 64)
				require.Equal(t, intToVReg(2), ctx.VRegOf(ext.Return()))
				require.Equal(t, "nop0", formatEmittedInstructionsInCurrentBlock(m))
			})
		}
	})
}

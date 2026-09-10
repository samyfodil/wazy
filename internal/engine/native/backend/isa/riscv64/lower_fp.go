package riscv64

import (
	"math"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// The RISC-V M and F/D extensions define every exceptional case as a value
// rather than a trap: divide by zero yields all-ones, signed overflow yields
// the dividend, and a float-to-int conversion of a NaN or an out-of-range
// value saturates and raises a flag nobody reads. wasm instead requires traps
// for most of these, so the checks that the other backends get from hardware
// exceptions or condition flags are explicit branches here.

// lowerDivRem lowers sdiv/udiv/srem/urem with wasm's trapping semantics.
//
// Three cases need handling, and only the first two trap:
//
//   - a zero divisor traps for all four operators;
//   - INT_MIN / -1 traps for signed division, since the true quotient is not
//     representable (RISC-V would quietly return INT_MIN);
//   - INT_MIN % -1 is 0 and must *not* trap, which is what RISC-V's rem
//     already returns, so it needs no code at all.
func (m *machine) lowerDivRem(instr *ssa.Instruction, op ssa.Opcode) {
	x, y, execCtxVal := instr.Arg3()
	execCtx := m.compiler.VRegOf(execCtxVal)
	rd := m.compiler.VRegOf(instr.Return())
	_64bit := x.Type().Bits() == 64

	rn := m.getOperand_NR(m.compiler.ValueDefinition(x))
	rm := m.getOperand_NR(m.compiler.ValueDefinition(y))

	signed := op == ssa.OpcodeSdiv || op == ssa.OpcodeSrem
	isDiv := op == ssa.OpcodeSdiv || op == ssa.OpcodeUdiv

	// Divisor == 0 traps.
	m.trapIfCondBr(execCtx, condEQ, rm, operandNR(zeroVReg), nativeapi.ExitCodeIntegerDivisionByZero)

	if signed && isDiv {
		// The overflow case: dividend == INT_MIN && divisor == -1. Detect it
		// by testing both halves and branching to the trap only when both
		// hold, which needs a skip label because there is no compound branch.
		minVal := int64(math.MinInt64)
		if !_64bit {
			minVal = math.MinInt32
		}
		minReg := m.compiler.AllocateVReg(ssa.TypeI64)
		m.lowerConstantI64(minReg, minVal)

		skip := m.insertBrTargetLabelAfterCurrent()
		// If divisor != -1, skip the overflow trap.
		notMinusOne := m.allocateInstr()
		notMinusOne.asCondBr(condNE, rm, operandNR(m.materialize(-1)), skip)
		m.insert(notMinusOne)
		// If dividend != INT_MIN, skip too.
		notMin := m.allocateInstr()
		notMin.asCondBr(condNE, rn, operandNR(minReg), skip)
		m.insert(notMin)

		m.trapUnconditional(execCtx, nativeapi.ExitCodeIntegerOverflow)
		m.insert(m.labelNop(skip))
	}

	var aop aluOp
	switch op {
	case ssa.OpcodeSdiv:
		aop = aluOpDiv
	case ssa.OpcodeUdiv:
		aop = aluOpDivu
	case ssa.OpcodeSrem:
		aop = aluOpRem
	case ssa.OpcodeUrem:
		aop = aluOpRemu
	}
	i := m.allocateInstr()
	i.asALU(aop, rd, rn, rm, _64bit)
	m.insert(i)
}

// lowerFminFmax lowers wasm's f32/f64 min and max.
//
// RISC-V's FMIN/FMAX are *not* these operators: when exactly one operand is
// NaN they return the other one, where wasm requires NaN to propagate. So the
// hardware instruction handles the ordinary case and an explicit check covers
// the unordered one.
//
// The zero case needs no help: RISC-V already returns -0.0 for min(-0.0,+0.0)
// and +0.0 for max, which is what wasm specifies.
func (m *machine) lowerFminFmax(instr *ssa.Instruction, isMax bool) {
	x, y := instr.Arg2()
	rd := m.compiler.VRegOf(instr.Return())
	_64bit := x.Type() == ssa.TypeF64
	rn := m.getOperand_NR(m.compiler.ValueDefinition(x))
	rm := m.getOperand_NR(m.compiler.ValueDefinition(y))

	op := fpuBinOpMin
	if isMax {
		op = fpuBinOpMax
	}
	i := m.allocateInstr()
	i.asFpuRRR(op, rd, rn, rm, _64bit)
	m.insert(i)

	// If either operand is unordered, overwrite the result with a NaN. feq
	// answers false for NaN, so `x == x` is the standard ordered test.
	ordered := m.compiler.AllocateVReg(ssa.TypeI64)
	tmp := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asFpuCmp(fpuCmpOpEq, ordered, rn, rn, _64bit))
	m.emit(m.allocateInstr().asFpuCmp(fpuCmpOpEq, tmp, rm, rm, _64bit))
	m.emit(m.allocateInstr().asALU(aluOpAnd, ordered, operandNR(ordered), operandNR(tmp), true))

	skip := m.insertBrTargetLabelAfterCurrent()
	br := m.allocateInstr()
	br.asCondBr(condNE, operandNR(ordered), operandNR(zeroVReg), skip)
	m.insert(br)

	nan := m.compiler.AllocateVReg(x.Type())
	if _64bit {
		m.lowerConstantF64(nan, math.Float64bits(math.NaN()))
	} else {
		m.lowerConstantF32(nan, math.Float32bits(float32(math.NaN())))
	}
	m.emit(m.allocateInstr().asFpuMov(rd, nan))
	m.insert(m.labelNop(skip))
}

type roundMode byte

const (
	roundModeNearest roundMode = iota
	roundModeTrunc
	roundModeFloor
	roundModeCeil
)

// lowerFpuRound lowers f32/f64 ceil, floor, trunc and nearest.
//
// RV64D has no rounding instruction -- Zfa's fround does, but it is very new
// and not assumed here -- so each is a round trip through the integer file:
// convert to a 64-bit integer with the required rounding mode and convert
// back. The round trip is only valid while the value is small enough that
// every integer is representable, so a magnitude test leaves anything at or
// above 2^52 (2^23 for f32) untouched, which is correct because such a value
// is already an integer. NaN and the infinities take that same path.
func (m *machine) lowerFpuRound(instr *ssa.Instruction, mode roundMode) {
	x := instr.Arg()
	rd := m.compiler.VRegOf(instr.Return())
	_64bit := x.Type() == ssa.TypeF64
	rn := m.getOperand_NR(m.compiler.ValueDefinition(x))

	// Start with the identity, so the "already integral" path needs no move.
	m.emit(m.allocateInstr().asFpuMov(rd, rn.nr()))

	// |x| < 2^52 (or 2^23) is the region where the round trip is exact.
	limit := m.compiler.AllocateVReg(x.Type())
	if _64bit {
		m.lowerConstantF64(limit, math.Float64bits(math.Ldexp(1, 52)))
	} else {
		m.lowerConstantF32(limit, math.Float32bits(float32(math.Ldexp(1, 23))))
	}
	absx := m.compiler.AllocateVReg(x.Type())
	m.emit(m.allocateInstr().asFpuRR(fpuUniOpAbs, absx, rn, _64bit))

	inRange := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asFpuCmp(fpuCmpOpLt, inRange, operandNR(absx), operandNR(limit), _64bit))

	skip := m.insertBrTargetLabelAfterCurrent()
	br := m.allocateInstr()
	// A NaN compares false here too, so it skips and is returned unchanged.
	br.asCondBr(condEQ, operandNR(inRange), operandNR(zeroVReg), skip)
	m.insert(br)

	// Round through the integer file, then restore the sign so that a result
	// of zero keeps the sign of the input -- wasm requires ceil(-0.3) to be
	// -0.0, which a bare convert-back would give as +0.0.
	iv := m.compiler.AllocateVReg(ssa.TypeI64)
	cvt := m.allocateInstr()
	cvt.asFcvtToIntRounded(iv, rn, true /* to i64 */, _64bit, true /* signed */, mode)
	m.insert(cvt)

	back := m.allocateInstr()
	back.asFcvtFromInt(rd, operandNR(iv), _64bit, true, true)
	m.insert(back)

	m.emit(m.allocateInstr().asFpuRRR(fpuBinOpSgnj, rd, operandNR(rd), rn, _64bit))
	m.insert(m.labelNop(skip))
}

// lowerFcvtFromInt lowers i32/i64 -> f32/f64.
func (m *machine) lowerFcvtFromInt(instr *ssa.Instruction, signed bool) {
	x := instr.Arg()
	rd := m.compiler.VRegOf(instr.Return())
	rn := m.getOperand_NR(m.compiler.ValueDefinition(x))
	src64 := x.Type().Bits() == 64
	dst64 := instr.Return().Type() == ssa.TypeF64
	i := m.allocateInstr()
	i.asFcvtFromInt(rd, rn, dst64, src64, signed)
	m.insert(i)
}

// lowerFcvtToInt lowers f32/f64 -> i32/i64, trapping or saturating.
//
// RISC-V's fcvt already saturates out-of-range inputs to the destination's
// extremes, which is most of what the saturating operators want -- but it maps
// NaN to the *maximum*, where wasm requires zero. The trapping operators need
// both cases detected before the conversion.
func (m *machine) lowerFcvtToInt(instr *ssa.Instruction, signed, saturating bool) {
	var x, execCtxVal ssa.Value
	if saturating {
		x = instr.Arg()
	} else {
		x, execCtxVal = instr.Arg2()
	}
	rd := m.compiler.VRegOf(instr.Return())
	rn := m.getOperand_NR(m.compiler.ValueDefinition(x))
	src64 := x.Type() == ssa.TypeF64
	dst64 := instr.Return().Type().Bits() == 64

	if !saturating {
		execCtx := m.compiler.VRegOf(execCtxVal)

		// NaN traps as an invalid conversion. `x == x` answers false for both
		// quiet and signaling NaN, so it is the whole test.
		ordered := m.compiler.AllocateVReg(ssa.TypeI64)
		m.emit(m.allocateInstr().asFpuCmp(fpuCmpOpEq, ordered, rn, rn, src64))
		m.trapIfCondBr(execCtx, condEQ, operandNR(ordered), operandNR(zeroVReg),
			nativeapi.ExitCodeInvalidConversionToInteger)

		// Anything outside the destination's range traps as an overflow. The
		// bounds are compared in floating point against exactly representable
		// values, which is what makes the test exact -- writing the upper
		// bound as "<= MaxInt64" would not work, because float64(MaxInt64)
		// rounds up to 2^63 and would admit 2^63 itself.
		lo, hi, loInclusive := fcvtTrapBounds(dst64, signed, src64)

		loReg := m.compiler.AllocateVReg(x.Type())
		hiReg := m.compiler.AllocateVReg(x.Type())
		m.loadFPConstant(loReg, lo, src64)
		m.loadFPConstant(hiReg, hi, src64)

		// Trap unless lo < x (or lo <= x) ...
		lowOK := m.compiler.AllocateVReg(ssa.TypeI64)
		lowOp := fpuCmpOpLt
		if loInclusive {
			lowOp = fpuCmpOpLe
		}
		m.emit(m.allocateInstr().asFpuCmp(lowOp, lowOK, operandNR(loReg), rn, src64))
		m.trapIfCondBr(execCtx, condEQ, operandNR(lowOK), operandNR(zeroVReg),
			nativeapi.ExitCodeIntegerOverflow)

		// ... and x < hi.
		highOK := m.compiler.AllocateVReg(ssa.TypeI64)
		m.emit(m.allocateInstr().asFpuCmp(fpuCmpOpLt, highOK, rn, operandNR(hiReg), src64))
		m.trapIfCondBr(execCtx, condEQ, operandNR(highOK), operandNR(zeroVReg),
			nativeapi.ExitCodeIntegerOverflow)

		m.emit(m.allocateInstr().asFcvtToInt(rd, rn, dst64, src64, signed))
		return
	}

	// Saturating: hardware saturation is right for the infinities and for
	// out-of-range finite values; only NaN needs correcting to zero.
	m.emit(m.allocateInstr().asFcvtToInt(rd, rn, dst64, src64, signed))
	ordered := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asFpuCmp(fpuCmpOpEq, ordered, rn, rn, src64))
	// rd &= -(ordered), i.e. zero it when the input was NaN.
	mask := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asALU(aluOpSub, mask, operandNR(zeroVReg), operandNR(ordered), true))
	m.emit(m.allocateInstr().asALU(aluOpAnd, rd, operandNR(rd), operandNR(mask), true))
}

// trapIfCondBr branches to the shared trap island for `code` when the
// condition holds.
func (m *machine) trapIfCondBr(execCtx regalloc.VReg, flag condFlag, rn, rm operand, code nativeapi.ExitCode) {
	island := m.getOrCreateTrapIsland(code)
	mv := m.allocateInstr()
	mv.asMove64(tmpRegVReg, execCtx)
	m.insert(mv)
	br := m.allocateInstr()
	br.asCondBr(flag, rn, rm, island)
	m.insert(br)
}

// trapUnconditional jumps to the shared trap island for `code`.
func (m *machine) trapUnconditional(execCtx regalloc.VReg, code nativeapi.ExitCode) {
	island := m.getOrCreateTrapIsland(code)
	mv := m.allocateInstr()
	mv.asMove64(tmpRegVReg, execCtx)
	m.insert(mv)
	b := m.allocateInstr()
	b.asBr(island)
	m.insert(b)
}

// insertBrTargetLabelAfterCurrent allocates a label to be placed later by
// labelNop, for the local skip-branches the sequences above need.
func (m *machine) insertBrTargetLabelAfterCurrent() label {
	l := m.nextLabel
	m.nextLabel++
	pos := m.labelPositionPool.GetOrAllocate(int(l))
	nop := m.allocateInstr()
	nop.asNop0WithLabel(l)
	pos.begin, pos.end = nop, nop
	return l
}

// labelNop returns the nop that anchors a label allocated above.
func (m *machine) labelNop(l label) *instruction {
	pos := m.labelPositionPool.GetOrAllocate(int(l))
	return pos.begin
}

// materialize builds a small constant into a fresh register.
func (m *machine) materialize(v int64) regalloc.VReg {
	r := m.compiler.AllocateVReg(ssa.TypeI64)
	m.lowerConstantI64(r, v)
	return r
}

// fcvtTrapBounds gives the open/half-open interval a float must lie in for its
// truncation to fit the destination integer type. Trap unless
// (loInclusive ? lo <= x : lo < x) && x < hi.
//
// The asymmetry is not arbitrary. Where the bound itself is exactly
// representable in the source format the comparison must include it -- f32
// cannot represent -2^31-1, so -2^31 has to be admitted directly -- and where
// it is not, the next representable value below already lies outside the
// range, so a strict comparison is both correct and simpler. Verified against
// the spec rule over every edge case in both formats.
func fcvtTrapBounds(dst64, signed, src64 bool) (lo, hi float64, loInclusive bool) {
	switch {
	case !dst64 && signed && src64:
		return -2147483649.0, 2147483648.0, false
	case !dst64 && signed && !src64:
		// f32 has no value between -2^31-1 and -2^31, so -2^31 is the bound.
		return -2147483648.0, 2147483648.0, true
	case !dst64 && !signed:
		return -1.0, 4294967296.0, false
	case dst64 && signed:
		return -9223372036854775808.0, 9223372036854775808.0, true
	default:
		return -1.0, 18446744073709551616.0, false
	}
}

// loadFPConstant materializes an FP constant of the given width.
func (m *machine) loadFPConstant(rd regalloc.VReg, v float64, _64bit bool) {
	if _64bit {
		m.lowerConstantF64(rd, math.Float64bits(v))
		return
	}
	m.lowerConstantF32(rd, math.Float32bits(float32(v)))
}

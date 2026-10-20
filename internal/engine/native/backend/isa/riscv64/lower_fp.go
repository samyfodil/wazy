package riscv64

import (
	"math"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/moremath"
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
		// Overflow is dividend == INT_MIN && divisor == -1, and RISC-V has no
		// compound branch. Rather than branch over a trap -- which would put a
		// forward jump inside this basic block, where the register allocator
		// assumes straight-line code and may place a spill the jump skips --
		// fold both halves into one value that is zero exactly when both hold:
		//
		//	(x ^ INT_MIN) | (y ^ -1) == 0  iff  x == INT_MIN && y == -1
		//
		// and branch once, to the trap island, which never returns.
		minVal := int64(math.MinInt64)
		if !_64bit {
			minVal = math.MinInt32
		}
		minReg := m.compiler.AllocateVReg(ssa.TypeI64)
		m.lowerConstantI64(minReg, minVal)

		acc := m.compiler.AllocateVReg(ssa.TypeI64)
		t := m.compiler.AllocateVReg(ssa.TypeI64)
		m.emit(m.allocateInstr().asALU(aluOpXor, acc, rn, operandNR(minReg), true))
		m.emit(m.allocateInstr().asALU(aluOpXor, t, rm, operandImm(-1), true))
		m.emit(m.allocateInstr().asALU(aluOpOr, acc, operandNR(acc), operandNR(t), true))
		m.trapIfCondBr(execCtx, condEQ, operandNR(acc), operandNR(zeroVReg),
			nativeapi.ExitCodeIntegerOverflow)
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
	hw := m.compiler.AllocateVReg(x.Type())
	m.emit(m.allocateInstr().asFpuRRR(op, hw, rn, rm, _64bit))

	// Blend in a NaN when either operand is unordered. feq answers false for
	// NaN, so `v == v` is the ordered test, and the two results are selected
	// with a mask rather than a branch: a forward jump here would sit inside
	// the basic block, where the register allocator assumes straight-line code
	// and may place a spill or reload that the jump skips.
	ordered := m.compiler.AllocateVReg(ssa.TypeI64)
	t := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asFpuCmp(fpuCmpOpEq, ordered, rn, rn, _64bit))
	m.emit(m.allocateInstr().asFpuCmp(fpuCmpOpEq, t, rm, rm, _64bit))
	m.emit(m.allocateInstr().asALU(aluOpAnd, ordered, operandNR(ordered), operandNR(t), true))

	// The *canonical* NaN, not Go's. math.NaN() is 0x7ff8000000000001 -- its
	// payload is 1, not zero -- and wasm requires min/max to produce a
	// canonical NaN, which the spec suite checks bit for bit. Using Go's
	// constant here fails 136 assertions on a single mantissa bit while
	// reporting, unhelpfully, "have NaN want NaN".
	nan := m.compiler.AllocateVReg(x.Type())
	if _64bit {
		m.lowerConstantF64(nan, moremath.F64CanonicalNaNBits)
	} else {
		m.lowerConstantF32(nan, moremath.F32CanonicalNaNBits)
	}
	m.blendFP(rd, hw, nan, m.maskFromBool(operandNR(ordered)), _64bit)
}

// maskFromBool turns a 0/1 value into 0 or all-ones, the form blendFP wants.
func (m *machine) maskFromBool(cond operand) regalloc.VReg {
	mask := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asALU(aluOpSub, mask, operandNR(zeroVReg), cond, true))
	return mask
}

// blendFP computes `rd = mask ? whenTrue : whenFalse` for floating-point
// values, without branching.
//
// The masking operations only exist in the integer file, so both values cross
// over, get blended as bit patterns, and cross back. For f32 the moves are the
// single-precision ones: fmv.x.w and fmv.w.x, so that the NaN boxing RV64D
// requires is re-established on the way back rather than left to chance.
func (m *machine) blendFP(rd, whenTrue, whenFalse, mask regalloc.VReg, _64bit bool) {
	bt := m.compiler.AllocateVReg(ssa.TypeI64)
	bf := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asFmvToInt(bt, operandNR(whenTrue), _64bit))
	m.emit(m.allocateInstr().asFmvToInt(bf, operandNR(whenFalse), _64bit))

	diff := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asALU(aluOpXor, diff, operandNR(bt), operandNR(bf), true))
	m.emit(m.allocateInstr().asALU(aluOpAnd, diff, operandNR(diff), operandNR(mask), true))
	m.emit(m.allocateInstr().asALU(aluOpXor, diff, operandNR(bf), operandNR(diff), true))
	m.emit(m.allocateInstr().asFmvFromInt(rd, operandNR(diff), _64bit))
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

	// Round through the integer file under the requested mode, then put the
	// sign back: wasm requires ceil(-0.3) to be -0.0, where converting back
	// from the integer 0 gives +0.0.
	iv := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asFcvtToIntRounded(iv, rn, true /* to i64 */, _64bit, true, mode))
	rounded := m.compiler.AllocateVReg(x.Type())
	m.emit(m.allocateInstr().asFcvtFromInt(rounded, operandNR(iv), _64bit, true, true))
	m.emit(m.allocateInstr().asFpuRRR(fpuBinOpSgnj, rounded, operandNR(rounded), rn, _64bit))

	// That round trip is only exact while every integer in range is
	// representable, i.e. |x| < 2^52 (2^23 for f32). At or above it the value
	// is already integral and must be returned untouched -- as must NaN and
	// the infinities, which the comparison below reports as out of range
	// because flt is false for unordered operands.
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

	// The out-of-range operand is the input itself, but passed through an
	// addition of zero rather than used raw. Two reasons, and only the second
	// is obvious: adding zero is exact for every value that reaches this path
	// (they are all integral, or infinite), and it *quiets* a signaling NaN,
	// which wasm requires of every arithmetic operator. Returning the input
	// untouched leaves a signaling NaN signaling, which the spec suite catches
	// with the singularly unhelpful "have NaN want NaN".
	zero := m.compiler.AllocateVReg(x.Type())
	m.emit(m.allocateInstr().asFmvFromInt(zero, operandNR(zeroVReg), _64bit))
	passthrough := m.compiler.AllocateVReg(x.Type())
	m.emit(m.allocateInstr().asFpuRRR(fpuBinOpAdd, passthrough, rn, operandNR(zero), _64bit))

	m.blendFP(rd, rounded, passthrough, m.maskFromBool(operandNR(inRange)), _64bit)
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

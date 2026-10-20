package riscv64

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// LowerInstr implements backend.Machine.
func (m *machine) LowerInstr(instr *ssa.Instruction) {
	if l := instr.SourceOffset(); l.Valid() {
		info := m.allocateInstr()
		info.asSourceOffsetInfo(l)
		m.insert(info)
	}

	switch op := instr.Opcode(); op {
	case ssa.OpcodeBrz, ssa.OpcodeBrnz, ssa.OpcodeJump, ssa.OpcodeBrTable:
		panic("BUG: branches are lowered by LowerSingleBranch/LowerConditionalBranch")
	case ssa.OpcodeReturn:
		panic("BUG: return must be handled by backend.Compiler")

	// Constants are inlined at their use sites by getOperand_NR.
	case ssa.OpcodeIconst, ssa.OpcodeF32const, ssa.OpcodeF64const:

	case ssa.OpcodeIadd, ssa.OpcodeIsub:
		m.lowerAddSub(instr, op == ssa.OpcodeIadd)
	case ssa.OpcodeImul:
		m.lowerIntBinOp(instr, aluOpMul)
	case ssa.OpcodeBand:
		m.lowerIntBinOp(instr, aluOpAnd)
	case ssa.OpcodeBor:
		m.lowerIntBinOp(instr, aluOpOr)
	case ssa.OpcodeBxor:
		m.lowerIntBinOp(instr, aluOpXor)
	case ssa.OpcodeBnot:
		// RISC-V has no `not`; it is xori with -1.
		x := instr.Arg()
		rd := m.compiler.VRegOf(instr.Return())
		rn := m.getOperand_NR(m.compiler.ValueDefinition(x))
		i := m.allocateInstr()
		i.asALU(aluOpXor, rd, rn, operandImm(-1), true)
		m.insert(i)
		// xori works on the full 64 bits, which preserves the sign-extended
		// form of an i32 because both halves are complemented together.

	case ssa.OpcodeIshl:
		m.lowerShift(instr, aluOpSll)
	case ssa.OpcodeUshr:
		m.lowerShift(instr, aluOpSrl)
	case ssa.OpcodeSshr:
		m.lowerShift(instr, aluOpSra)
	case ssa.OpcodeRotl, ssa.OpcodeRotr:
		m.lowerRotate(instr, op == ssa.OpcodeRotl)

	case ssa.OpcodeSdiv, ssa.OpcodeUdiv, ssa.OpcodeSrem, ssa.OpcodeUrem:
		m.lowerDivRem(instr, op)

	case ssa.OpcodeClz:
		m.lowerBitCount(instr, m.lowerClz)
	case ssa.OpcodeCtz:
		m.lowerBitCount(instr, m.lowerCtz)
	case ssa.OpcodePopcnt:
		m.lowerBitCount(instr, m.lowerPopcnt)

	case ssa.OpcodeIcmp:
		x, y, c := instr.IcmpData()
		m.lowerIcmpToReg(x, y, c, m.compiler.VRegOf(instr.Return()))
	case ssa.OpcodeFcmp:
		x, y, c := instr.FcmpData()
		m.lowerFcmpToReg(x, y, c, m.compiler.VRegOf(instr.Return()))

	case ssa.OpcodeSExtend, ssa.OpcodeUExtend:
		from, to, signed := instr.ExtendData()
		m.lowerExtend(instr.Arg(), instr.Return(), from, to, signed)
	case ssa.OpcodeIreduce:
		// Narrowing to i32 must re-establish the sign-extended form: the
		// source's upper half is meaningless once truncated, and leaving it
		// would break every later comparison. addiw does exactly this.
		rn := m.getOperand_NR(m.compiler.ValueDefinition(instr.Arg()))
		rd := m.compiler.VRegOf(instr.Return())
		i := m.allocateInstr()
		i.asALU(aluOpAdd, rd, rn, operandImm(0), false /* addiw */)
		m.insert(i)
	case ssa.OpcodeBitcast:
		m.lowerBitcast(instr)

	case ssa.OpcodeFadd:
		m.lowerFpuBinOp(instr, fpuBinOpAdd)
	case ssa.OpcodeFsub:
		m.lowerFpuBinOp(instr, fpuBinOpSub)
	case ssa.OpcodeFmul:
		m.lowerFpuBinOp(instr, fpuBinOpMul)
	case ssa.OpcodeFdiv:
		m.lowerFpuBinOp(instr, fpuBinOpDiv)
	case ssa.OpcodeFmin, ssa.OpcodeFmax:
		m.lowerFminFmax(instr, op == ssa.OpcodeFmax)
	case ssa.OpcodeFabs:
		m.lowerFpuUniOp(instr, fpuUniOpAbs)
	case ssa.OpcodeFneg:
		m.lowerFpuUniOp(instr, fpuUniOpNeg)
	case ssa.OpcodeSqrt:
		m.lowerFpuUniOp(instr, fpuUniOpSqrt)
	case ssa.OpcodeFcopysign:
		x, y := instr.Arg2()
		rd := m.compiler.VRegOf(instr.Return())
		rx := m.getOperand_NR(m.compiler.ValueDefinition(x))
		ry := m.getOperand_NR(m.compiler.ValueDefinition(y))
		// fsgnj is copysign; it is the primitive here, not an emulation.
		i := m.allocateInstr()
		i.asFpuRRR(fpuBinOpSgnj, rd, rx, ry, x.Type() == ssa.TypeF64)
		m.insert(i)

	case ssa.OpcodeCeil:
		m.lowerFpuRound(instr, roundModeCeil)
	case ssa.OpcodeFloor:
		m.lowerFpuRound(instr, roundModeFloor)
	case ssa.OpcodeTrunc:
		m.lowerFpuRound(instr, roundModeTrunc)
	case ssa.OpcodeNearest:
		m.lowerFpuRound(instr, roundModeNearest)

	case ssa.OpcodeFdemote:
		rn := m.getOperand_NR(m.compiler.ValueDefinition(instr.Arg()))
		i := m.allocateInstr()
		i.asFcvtSD(m.compiler.VRegOf(instr.Return()), rn, false)
		m.insert(i)
	case ssa.OpcodeFpromote:
		rn := m.getOperand_NR(m.compiler.ValueDefinition(instr.Arg()))
		i := m.allocateInstr()
		i.asFcvtSD(m.compiler.VRegOf(instr.Return()), rn, true)
		m.insert(i)

	case ssa.OpcodeFcvtFromSint, ssa.OpcodeFcvtFromUint:
		m.lowerFcvtFromInt(instr, op == ssa.OpcodeFcvtFromSint)
	case ssa.OpcodeFcvtToSint, ssa.OpcodeFcvtToUint:
		m.lowerFcvtToInt(instr, op == ssa.OpcodeFcvtToSint, false /* trapping */)
	case ssa.OpcodeFcvtToSintSat, ssa.OpcodeFcvtToUintSat:
		m.lowerFcvtToInt(instr, op == ssa.OpcodeFcvtToSintSat, true /* saturating */)

	case ssa.OpcodeLoad:
		ptr, offset, typ := instr.LoadData()
		m.lowerLoad(ptr, offset, typ, instr.Return())
	case ssa.OpcodeUload8, ssa.OpcodeUload16, ssa.OpcodeUload32,
		ssa.OpcodeSload8, ssa.OpcodeSload16, ssa.OpcodeSload32:
		ptr, offset, _ := instr.LoadData()
		ret := m.compiler.VRegOf(instr.Return())
		var bits byte
		var signed bool
		switch op {
		case ssa.OpcodeUload8:
			bits, signed = 8, false
		case ssa.OpcodeUload16:
			bits, signed = 16, false
		case ssa.OpcodeUload32:
			bits, signed = 32, false
		case ssa.OpcodeSload8:
			bits, signed = 8, true
		case ssa.OpcodeSload16:
			bits, signed = 16, true
		case ssa.OpcodeSload32:
			bits, signed = 32, true
		}
		m.lowerExtLoad(ptr, offset, bits, signed, ret)
	case ssa.OpcodeStore, ssa.OpcodeIstore8, ssa.OpcodeIstore16, ssa.OpcodeIstore32:
		m.lowerStore(instr)

	case ssa.OpcodeSelect:
		c, x, y := instr.SelectData()
		m.lowerSelect(c, x, y, instr.Return())

	case ssa.OpcodeCall, ssa.OpcodeCallIndirect:
		m.lowerCall(instr)
	case ssa.OpcodeTailCallReturnCall, ssa.OpcodeTailCallReturnCallIndirect:
		m.lowerTailCall(instr)

	case ssa.OpcodeExitWithCode:
		execCtx, code := instr.ExitWithCodeData()
		m.lowerExitWithCode(m.compiler.VRegOf(execCtx), code)
	case ssa.OpcodeExitIfTrueWithCode:
		execCtx, c, code := instr.ExitIfTrueWithCodeData()
		if instr.SourceOffset().Valid() {
			// Sharing one exit sequence per trap kind costs the per-site trap
			// address, which is what a DWARF backtrace maps back to source. A
			// site that carries a source offset keeps its sequence inline so
			// the address it records is its own.
			m.lowerExitIfTrueWithCodeInline(m.compiler.VRegOf(execCtx), c, code)
		} else {
			m.lowerExitIfTrueWithCode(m.compiler.VRegOf(execCtx), c, code)
		}
	case ssa.OpcodeUndefined:
		m.insert(m.allocateInstr().asUDF())

	case ssa.OpcodeFence:
		m.insert(m.allocateInstr().asFence())
	case ssa.OpcodeAtomicRmw:
		m.lowerAtomicRmw(instr)
	case ssa.OpcodeAtomicCas:
		m.lowerAtomicCas(instr)
	case ssa.OpcodeAtomicLoad:
		m.lowerAtomicLoad(instr)
	case ssa.OpcodeAtomicStore:
		m.lowerAtomicStore(instr)

	// ---- SIMD ----
	case ssa.OpcodeVIadd:
		m.lowerVecRRR(vfunctAdd, opivv, instr)
	case ssa.OpcodeVIsub:
		m.lowerVecRRR(vfunctSub, opivv, instr)
	case ssa.OpcodeVImul:
		m.lowerVecRRR(vfunctMul, opmvv, instr)
	case ssa.OpcodeVband:
		m.lowerVecRRRBits(vfunctAnd, instr)
	case ssa.OpcodeVbor:
		m.lowerVecRRRBits(vfunctOr, instr)
	case ssa.OpcodeVbxor:
		m.lowerVecRRRBits(vfunctXor, instr)
	case ssa.OpcodeVbnot:
		m.lowerVbnot(instr)
	case ssa.OpcodeVIneg:
		m.lowerVIneg(instr)
	case ssa.OpcodeVImin:
		m.lowerVecRRR(vfunctMin, opivv, instr)
	case ssa.OpcodeVImax:
		m.lowerVecRRR(vfunctMax, opivv, instr)
	case ssa.OpcodeVUmin:
		m.lowerVecRRR(vfunctMinu, opivv, instr)
	case ssa.OpcodeVUmax:
		m.lowerVecRRR(vfunctMaxu, opivv, instr)
	case ssa.OpcodeVSaddSat:
		m.lowerVecRRR(vfunctSadd, opivv, instr)
	case ssa.OpcodeVUaddSat:
		m.lowerVecRRR(vfunctSaddu, opivv, instr)
	case ssa.OpcodeVSsubSat:
		m.lowerVecRRR(vfunctSsub, opivv, instr)
	case ssa.OpcodeVUsubSat:
		m.lowerVecRRR(vfunctSsubu, opivv, instr)
	case ssa.OpcodeVAvgRound:
		m.lowerVecRRR(vfunctAaddu, opmvv, instr)
	case ssa.OpcodeVIshl:
		m.lowerVecShift(vfunctSll, instr)
	case ssa.OpcodeVUshr:
		m.lowerVecShift(vfunctSrl, instr)
	case ssa.OpcodeVSshr:
		m.lowerVecShift(vfunctSra, instr)
	case ssa.OpcodeVIcmp:
		m.lowerVIcmp(instr)
	case ssa.OpcodeVFcmp:
		m.lowerVFcmp(instr)
	case ssa.OpcodeVFadd:
		m.lowerVecRRR(vfunctFadd, opfvv, instr)
	case ssa.OpcodeVFsub:
		m.lowerVecRRR(vfunctFsub, opfvv, instr)
	case ssa.OpcodeVFmul:
		m.lowerVecRRR(vfunctFmul, opfvv, instr)
	case ssa.OpcodeVFdiv:
		m.lowerVecRRR(vfunctFdiv, opfvv, instr)
	case ssa.OpcodeVSqrt:
		m.lowerVecRR(vfunctFunary1, vsubFsqrt, opfvv, instr)
	case ssa.OpcodeVFneg:
		m.lowerVFneg(instr)
	case ssa.OpcodeVFabs:
		m.lowerVFabs(instr)
	case ssa.OpcodeVIabs:
		m.lowerVIabs(instr)
	case ssa.OpcodeVbandnot:
		m.lowerVbandnot(instr)
	case ssa.OpcodeVbitselect:
		m.lowerVbitselect(instr)
	case ssa.OpcodeVanyTrue:
		m.lowerVanyTrue(instr)
	case ssa.OpcodeVallTrue:
		m.lowerVallTrue(instr)
	case ssa.OpcodeVhighBits:
		m.lowerVhighBits(instr)
	case ssa.OpcodeSplat:
		m.lowerSplat(instr)
	case ssa.OpcodeVFmin:
		m.lowerVFminFmax(instr, false)
	case ssa.OpcodeVFmax:
		m.lowerVFminFmax(instr, true)
	case ssa.OpcodeVMinPseudo:
		m.lowerVMinMaxPseudo(instr, false)
	case ssa.OpcodeVMaxPseudo:
		m.lowerVMinMaxPseudo(instr, true)
	case ssa.OpcodeVFcvtFromSint:
		m.lowerVFcvtFromInt(instr, true)
	case ssa.OpcodeVFcvtFromUint:
		m.lowerVFcvtFromInt(instr, false)
	case ssa.OpcodeVFcvtToSintSat:
		m.lowerVFcvtToIntSat(instr, true)
	case ssa.OpcodeVFcvtToUintSat:
		m.lowerVFcvtToIntSat(instr, false)
	case ssa.OpcodeFvdemote:
		m.lowerFvdemote(instr)
	case ssa.OpcodeFvpromoteLow:
		m.lowerFvpromoteLow(instr)
	case ssa.OpcodeSwidenLow:
		m.lowerVwiden(instr, true, false)
	case ssa.OpcodeSwidenHigh:
		m.lowerVwiden(instr, true, true)
	case ssa.OpcodeUwidenLow:
		m.lowerVwiden(instr, false, false)
	case ssa.OpcodeUwidenHigh:
		m.lowerVwiden(instr, false, true)
	case ssa.OpcodeSwizzle:
		m.lowerSwizzle(instr)
	case ssa.OpcodeSqmulRoundSat:
		m.lowerSqmulRoundSat(instr)
	case ssa.OpcodeExtractlane:
		m.lowerExtractlane(instr)
	case ssa.OpcodeInsertlane:
		m.lowerInsertlane(instr)
	case ssa.OpcodeSnarrow:
		m.lowerNarrow(instr, true)
	case ssa.OpcodeUnarrow:
		m.lowerNarrow(instr, false)
	case ssa.OpcodeVconst:
		m.lowerVconst(instr)
	case ssa.OpcodeVCeil:
		m.lowerVecRound(instr, roundModeCeil)
	case ssa.OpcodeVFloor:
		m.lowerVecRound(instr, roundModeFloor)
	case ssa.OpcodeVTrunc:
		m.lowerVecRound(instr, roundModeTrunc)
	case ssa.OpcodeVNearest:
		m.lowerVecRound(instr, roundModeNearest)
	case ssa.OpcodeVIpopcnt:
		m.lowerVIpopcnt(instr)
	case ssa.OpcodeExtIaddPairwise:
		m.lowerExtIaddPairwise(instr)
	case ssa.OpcodeWideningPairwiseDotProductS:
		m.lowerWideningPairwiseDotProduct(instr)
	case ssa.OpcodeVZeroExtLoad:
		m.lowerVZeroExtLoad(instr)
	case ssa.OpcodeLoadSplat:
		m.lowerLoadSplat(instr)
	case ssa.OpcodeShuffle:
		m.lowerShuffle(instr)

	default:
		panic("BUG: unimplemented lowering for opcode " + op.String() +
			" on riscv64. The remaining vector opcodes are gated off by " +
			"riscv64CompilerSupports until their lowering lands.")
	}
}

// lowerAddSub lowers i32/i64 addition and subtraction.
func (m *machine) lowerAddSub(instr *ssa.Instruction, add bool) {
	x, y := instr.Arg2()
	rd := m.compiler.VRegOf(instr.Return())
	_64bit := x.Type().Bits() == 64
	rn := m.getOperand_NR(m.compiler.ValueDefinition(x))

	if add {
		// Only addition has an immediate form; sub does not, so a constant
		// subtrahend is folded by negating it into an addi instead.
		rm := m.getOperand_Imm12_NR(m.compiler.ValueDefinition(y))
		i := m.allocateInstr()
		i.asALU(aluOpAdd, rd, rn, rm, _64bit)
		m.insert(i)
		return
	}

	yd := m.compiler.ValueDefinition(y)
	if v, ok := m.constantForImm12Negation(yd); ok {
		yd.Instr.MarkLowered()
		i := m.allocateInstr()
		i.asALU(aluOpAdd, rd, rn, operandImm(-v), _64bit)
		m.insert(i)
		return
	}
	rm := m.getOperand_NR(yd)
	i := m.allocateInstr()
	i.asALU(aluOpSub, rd, rn, rm, _64bit)
	m.insert(i)
}

// constantForImm12Negation reports a constant whose negation fits the 12-bit
// immediate field, so that `x - c` can be folded into `addi rd, x, -c`. -2048
// is excluded: its negation is 2048, which does not fit.
func (m *machine) constantForImm12Negation(def backend.SSAValueDefinition) (int64, bool) {
	if !def.IsFromInstr() || !def.Instr.Constant() {
		return 0, false
	}
	v, ok := asImm12Constant(def.Instr)
	if !ok || !fitsInSignedImm12(-v) {
		return 0, false
	}
	return v, true
}

// lowerIntBinOp lowers a register-register integer operation.
func (m *machine) lowerIntBinOp(instr *ssa.Instruction, op aluOp) {
	x, y := instr.Arg2()
	rd := m.compiler.VRegOf(instr.Return())
	_64bit := x.Type().Bits() == 64
	rn := m.getOperand_NR(m.compiler.ValueDefinition(x))

	// The bitwise ops have immediate forms and no word variant: they act on
	// all 64 bits, which preserves a sign-extended i32 because both halves are
	// combined consistently.
	switch op {
	case aluOpAnd, aluOpOr, aluOpXor:
		rm := m.getOperand_Imm12_NR(m.compiler.ValueDefinition(y))
		i := m.allocateInstr()
		i.asALU(op, rd, rn, rm, true)
		m.insert(i)
		return
	}

	rm := m.getOperand_NR(m.compiler.ValueDefinition(y))
	i := m.allocateInstr()
	i.asALU(op, rd, rn, rm, _64bit)
	m.insert(i)
}

// lowerShift lowers shl/shr_u/shr_s.
//
// The word forms take their shift amount modulo 32 and the doubleword forms
// modulo 64, which is exactly wasm's rule, so no masking of the amount is
// needed. Using the wide form for an i32 would take six count bits and make
// i32.shr_s(1, 32) return 0 where wasm requires 1.
func (m *machine) lowerShift(instr *ssa.Instruction, op aluOp) {
	x, y := instr.Arg2()
	rd := m.compiler.VRegOf(instr.Return())
	_64bit := x.Type().Bits() == 64
	rn := m.getOperand_NR(m.compiler.ValueDefinition(x))

	yd := m.compiler.ValueDefinition(y)
	if yd.IsFromInstr() && yd.Instr.Constant() {
		amount := int64(yd.Instr.ConstantVal())
		if _64bit {
			amount &= 63
		} else {
			amount &= 31
		}
		yd.Instr.MarkLowered()
		i := m.allocateInstr()
		i.asShiftImm(op, rd, rn, amount, _64bit)
		m.insert(i)
		return
	}

	rm := m.getOperand_NR(yd)
	i := m.allocateInstr()
	i.asALU(op, rd, rn, rm, _64bit)
	m.insert(i)
}

// lowerRotate lowers rotl/rotr, which RV64G lacks (Zbb has rol/ror).
//
// A rotate is two shifts and an or, but the shift amounts must be reduced
// modulo the width first: the complement `width - amount` is 64 or 32 when the
// amount is zero, and a shift by the full width is not zero on RISC-V, it is
// the identity, which would double-count the value.
func (m *machine) lowerRotate(instr *ssa.Instruction, left bool) {
	x, y := instr.Arg2()
	rd := m.compiler.VRegOf(instr.Return())
	_64bit := x.Type().Bits() == 64
	width := int64(32)
	mask := int64(31)
	if _64bit {
		width, mask = 64, 63
	}

	rn := m.getOperand_NR(m.compiler.ValueDefinition(x))
	src := rn
	if !_64bit {
		// The low half must not carry the replicated sign bits into the
		// right-shifted term.
		src = m.zeroExtend32(rn)
	}

	yd := m.compiler.ValueDefinition(y)
	if yd.IsFromInstr() && yd.Instr.Constant() {
		amount := int64(yd.Instr.ConstantVal()) & mask
		yd.Instr.MarkLowered()
		if amount == 0 {
			mv := m.allocateInstr()
			mv.asALU(aluOpAdd, rd, rn, operandImm(0), !_64bit)
			m.insert(mv)
			return
		}
		l, r := amount, width-amount
		if !left {
			l, r = r, l
		}
		hi := m.compiler.AllocateVReg(ssa.TypeI64)
		lo := m.compiler.AllocateVReg(ssa.TypeI64)
		m.emit(m.allocateInstr().asShiftImm(aluOpSll, hi, src, l, true))
		m.emit(m.allocateInstr().asShiftImm(aluOpSrl, lo, src, r, true))
		or := m.allocateInstr()
		or.asALU(aluOpOr, rd, operandNR(hi), operandNR(lo), true)
		m.insert(or)
		if !_64bit {
			// Re-establish the i32 invariant on the combined value.
			sx := m.allocateInstr()
			sx.asALU(aluOpAdd, rd, operandNR(rd), operandImm(0), false /* addiw */)
			m.insert(sx)
		}
		return
	}

	// Variable amount: n &= mask; c = width - n (also masked, so that n == 0
	// gives c == 0 rather than the full width).
	rm := m.getOperand_NR(yd)
	n := m.compiler.AllocateVReg(ssa.TypeI64)
	c := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asALU(aluOpAnd, n, rm, operandImm(mask), true))
	m.emit(m.allocateInstr().asALU(aluOpSub, c, operandNR(zeroVReg), operandNR(n), true))
	m.emit(m.allocateInstr().asALU(aluOpAnd, c, operandNR(c), operandImm(mask), true))

	sl, sr := n, c
	if !left {
		sl, sr = c, n
	}
	hi := m.compiler.AllocateVReg(ssa.TypeI64)
	lo := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asALU(aluOpSll, hi, src, operandNR(sl), true))
	m.emit(m.allocateInstr().asALU(aluOpSrl, lo, src, operandNR(sr), true))
	or := m.allocateInstr()
	or.asALU(aluOpOr, rd, operandNR(hi), operandNR(lo), true)
	m.insert(or)
	if !_64bit {
		sx := m.allocateInstr()
		sx.asALU(aluOpAdd, rd, operandNR(rd), operandImm(0), false)
		m.insert(sx)
	}
}

// lowerBitCount is the shared shape of clz/ctz/popcnt.
func (m *machine) lowerBitCount(instr *ssa.Instruction, f func(operand, regalloc.VReg, bool)) {
	x := instr.Arg()
	rn := m.getOperand_NR(m.compiler.ValueDefinition(x))
	rd := m.compiler.VRegOf(instr.Return())
	f(rn, rd, x.Type().Bits() == 64)
}

// lowerExtend lowers i32->i64 widening and the sign-extending narrow forms.
func (m *machine) lowerExtend(arg, ret ssa.Value, from, to byte, signed bool) {
	rd := m.compiler.VRegOf(ret)
	rn := m.getOperand_NR(m.compiler.ValueDefinition(arg))

	if !signed {
		// Zero extension: clear the bits above `from`. For 32 this is the
		// canonical shift pair; for 8 and 16 an andi suffices.
		switch from {
		case 8:
			i := m.allocateInstr()
			i.asALU(aluOpAnd, rd, rn, operandImm(0xff), true)
			m.insert(i)
		case 16:
			// 0xffff does not fit imm12, so the shift pair is used here too.
			m.emit(m.allocateInstr().asShiftImm(aluOpSll, rd, rn, 48, true))
			m.emit(m.allocateInstr().asShiftImm(aluOpSrl, rd, operandNR(rd), 48, true))
		case 32:
			m.emit(m.allocateInstr().asShiftImm(aluOpSll, rd, rn, 32, true))
			m.emit(m.allocateInstr().asShiftImm(aluOpSrl, rd, operandNR(rd), 32, true))
		default:
			panic(fmt.Sprintf("BUG: unsupported zero extension from %d bits", from))
		}
		return
	}

	switch from {
	case 8, 16:
		// Sign extension without Zbb is a shift left then arithmetic right.
		sh := int64(64 - from)
		m.emit(m.allocateInstr().asShiftImm(aluOpSll, rd, rn, sh, true))
		m.emit(m.allocateInstr().asShiftImm(aluOpSra, rd, operandNR(rd), sh, true))
	case 32:
		// addiw is the one-instruction sign-extend-word.
		i := m.allocateInstr()
		i.asALU(aluOpAdd, rd, rn, operandImm(0), false)
		m.insert(i)
	default:
		panic(fmt.Sprintf("BUG: unsupported sign extension from %d bits", from))
	}
	_ = to
}

// lowerBitcast reinterprets bits between the integer and float files.
func (m *machine) lowerBitcast(instr *ssa.Instruction) {
	v, dstType := instr.BitcastData()
	srcType := v.Type()
	rn := m.getOperand_NR(m.compiler.ValueDefinition(v))
	rd := m.compiler.VRegOf(instr.Return())

	switch {
	case srcType.IsInt() && dstType == ssa.TypeF32:
		m.insert(m.allocateInstr().asFmvFromInt(rd, rn, false))
	case srcType.IsInt() && dstType == ssa.TypeF64:
		m.insert(m.allocateInstr().asFmvFromInt(rd, rn, true))
	case srcType == ssa.TypeF32 && dstType.IsInt():
		// fmv.x.w sign-extends its 32-bit result, which is exactly the i32
		// representation this backend maintains.
		m.insert(m.allocateInstr().asFmvToInt(rd, rn, false))
	case srcType == ssa.TypeF64 && dstType.IsInt():
		m.insert(m.allocateInstr().asFmvToInt(rd, rn, true))
	default:
		panic(fmt.Sprintf("BUG: unsupported bitcast %s -> %s", srcType, dstType))
	}
}

// lowerFpuBinOp lowers the FP arithmetic that maps one-to-one.
func (m *machine) lowerFpuBinOp(instr *ssa.Instruction, op fpuBinOp) {
	x, y := instr.Arg2()
	rd := m.compiler.VRegOf(instr.Return())
	rn := m.getOperand_NR(m.compiler.ValueDefinition(x))
	rm := m.getOperand_NR(m.compiler.ValueDefinition(y))
	i := m.allocateInstr()
	i.asFpuRRR(op, rd, rn, rm, x.Type() == ssa.TypeF64)
	m.insert(i)
}

func (m *machine) lowerFpuUniOp(instr *ssa.Instruction, op fpuUniOp) {
	x := instr.Arg()
	rd := m.compiler.VRegOf(instr.Return())
	rn := m.getOperand_NR(m.compiler.ValueDefinition(x))
	i := m.allocateInstr()
	i.asFpuRR(op, rd, rn, x.Type() == ssa.TypeF64)
	m.insert(i)
}

// lowerSelect lowers `cond ? x : y`.
//
// There is no conditional move before Zicond, so this is the branch-free mask
// select: turn the condition into 0 or all-ones and blend. A branch would cost
// an unpredictable edge on what is often a very short value.
func (m *machine) lowerSelect(c, x, y, ret ssa.Value) {
	rc := m.getOperand_NR(m.compiler.ValueDefinition(c))
	rd := m.compiler.VRegOf(ret)

	// mask = (c != 0) ? ~0 : 0
	mask := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asALU(aluOpSltu, mask, operandNR(zeroVReg), rc, true))
	m.emit(m.allocateInstr().asALU(aluOpSub, mask, operandNR(zeroVReg), operandNR(mask), true))

	if x.Type().IsInt() {
		rx := m.getOperand_NR(m.compiler.ValueDefinition(x))
		ry := m.getOperand_NR(m.compiler.ValueDefinition(y))
		diff := m.compiler.AllocateVReg(ssa.TypeI64)
		m.emit(m.allocateInstr().asALU(aluOpXor, diff, rx, ry, true))
		m.emit(m.allocateInstr().asALU(aluOpAnd, diff, operandNR(diff), operandNR(mask), true))
		m.emit(m.allocateInstr().asALU(aluOpXor, rd, ry, operandNR(diff), true))
		return
	}

	if x.Type() == ssa.TypeV128 {
		// The same blend, one register wider. vmerge would want the condition
		// in v0 as a lane mask; the scalar mask is already all-ones or zero,
		// so vand.vx applies it to every lane at once.
		rx := m.getOperand_NR(m.compiler.ValueDefinition(x))
		ry := m.getOperand_NR(m.compiler.ValueDefinition(y))
		diff := m.compiler.AllocateVReg(ssa.TypeV128)
		m.emit(m.allocateInstr().asVecRRR(vfunctXor, opivv, diff, rx, ry, vsew64))
		m.emit(m.allocateInstr().asVecRX(vfunctAnd, diff, operandNR(diff), operandNR(mask), vsew64))
		m.emit(m.allocateInstr().asVecRRR(vfunctXor, opivv, rd, operandNR(diff), ry, vsew64))
		return
	}

	// The FP case blends in the integer file and moves the result back, since
	// the masking operations only exist there.
	rx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	ry := m.getOperand_NR(m.compiler.ValueDefinition(y))
	bx := m.compiler.AllocateVReg(ssa.TypeI64)
	by := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asFmvToInt(bx, rx, true))
	m.emit(m.allocateInstr().asFmvToInt(by, ry, true))
	diff := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asALU(aluOpXor, diff, operandNR(bx), operandNR(by), true))
	m.emit(m.allocateInstr().asALU(aluOpAnd, diff, operandNR(diff), operandNR(mask), true))
	m.emit(m.allocateInstr().asALU(aluOpXor, diff, operandNR(by), operandNR(diff), true))
	// Move back as a full 64-bit pattern: for an f32 both inputs were already
	// NaN-boxed, and blending two boxed values bit-for-bit keeps the box.
	m.emit(m.allocateInstr().asFmvFromInt(rd, operandNR(diff), true))
}

// lowerExitWithCode emits an unconditional exit back to Go.
//
// It used to clear pendingInstructions first, which no other backend does and
// which dropped the source-offset marker LowerInstr had just put there: the
// trap's recorded address then resolved against the *previous* marker, and a
// DWARF backtrace named an earlier line than the one that trapped.
func (m *machine) lowerExitWithCode(execCtx regalloc.VReg, code nativeapi.ExitCode) {
	tmp := m.compiler.AllocateVReg(ssa.TypeI64)
	m.lowerConstantI64(tmp, int64(code))
	m.storeExecCtxField(execCtx, tmp, nativeapi.ExecutionContextOffsetExitCodeOffset.I64(), 32)

	sp := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asMove64(sp, spVReg))
	m.storeExecCtxField(execCtx, sp, nativeapi.ExecutionContextOffsetStackPointerBeforeGoCall.I64(), 64)

	// The address of *this* exit, with no displacement: a trap never resumes,
	// and what reads this field back is the backtracer, which needs an address
	// inside the function that trapped. Displacing it past the exit sequence
	// lands one byte past the end of a function whose body ends in the trap --
	// outside the executable entirely, where the frame is dropped rather than
	// misattributed. See abi_go_call.go for the exit that really does resume.
	ra := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asAdrPCRel(ra, 0))
	m.storeExecCtxField(execCtx, ra, nativeapi.ExecutionContextOffsetGoCallReturnAddress.I64(), 64)

	m.insert(m.allocateInstr().asExitSequence(execCtx))
}

func (m *machine) storeExecCtxField(execCtx, src regalloc.VReg, off int64, bits byte) {
	amode := m.amodePool.Allocate()
	*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: execCtx, imm: off}
	st := m.allocateInstr()
	st.asStore(src, amode, bits, true)
	m.insert(st)
}

// lowerExitIfTrueWithCode branches to the shared trap island for `code` when
// the condition holds. One island per exit code per function keeps the check
// site to a single branch on the hot path.
// lowerExitIfTrueWithCodeInline lowers a conditional trap with its exit
// sequence in place, reached by falling through a branch on the inverted
// condition. Costlier in code size than the shared island, and the only form
// whose recorded address identifies the trap site rather than the island.
//
// The sequence it emits touches only the reserved registers, which is not a
// style choice. The branch over it sits inside a basic block, and the register
// allocator treats a basic block as straight-line code: it will place a reload
// in the region the branch skips, leaving the value garbage on the path that
// skipped it. That is not theoretical -- it cost a SIGSEGV in TinyGo's GC,
// where the reload of a spilled execution context landed inside the skipped
// trap and the in-bounds path then stored through a bounds-check temporary.
// With no allocated register in the region there is nothing to place.
func (m *machine) lowerExitIfTrueWithCodeInline(execCtx regalloc.VReg, cond ssa.Value, code nativeapi.ExitCode) {
	flag, rx, ry := m.lowerTrapBranchOperands(cond)

	// Last thing before the branch, to keep the window in which a spill could
	// land on tmpReg as short as it can be.
	m.insert(m.allocateInstr().asMove64(tmpRegVReg, execCtx))

	// The branch is patched once the target label exists, which is only after
	// the sequence it skips has been emitted.
	cbr := m.allocateInstr()
	m.insert(cbr)
	m.emitTrapExitInline(code)
	nop, l := m.allocateBrTarget()
	m.insert(nop)
	cbr.asCondBr(flag.invert(), rx, ry, l)
}

// emitTrapExitInline emits the body of a trap island in place, reading the
// execution context out of tmpReg. It is the same sequence emitTrapIslands
// writes after register allocation, and for the same reason: reserved registers
// only.
func (m *machine) emitTrapExitInline(code nativeapi.ExitCode) {
	m.lowerConstantI64(tmpReg2VReg, int64(code))
	m.storeExecCtxField(tmpRegVReg, tmpReg2VReg, nativeapi.ExecutionContextOffsetExitCodeOffset.I64(), 32)

	m.emit(m.allocateInstr().asMove64(tmpReg2VReg, spVReg))
	m.storeExecCtxField(tmpRegVReg, tmpReg2VReg, nativeapi.ExecutionContextOffsetStackPointerBeforeGoCall.I64(), 64)

	// The address of this exit, undisplaced: see lowerExitWithCode.
	m.emit(m.allocateInstr().asAdrPCRel(tmpReg2VReg, 0))
	m.storeExecCtxField(tmpRegVReg, tmpReg2VReg, nativeapi.ExecutionContextOffsetGoCallReturnAddress.I64(), 64)

	m.insert(m.allocateInstr().asExitSequence(tmpRegVReg))
}

// lowerTrapBranchOperands reduces a trap's condition to the three operands a
// RISC-V conditional branch takes. A comparison feeding the trap folds into the
// branch; anything else is tested against zero.
func (m *machine) lowerTrapBranchOperands(cond ssa.Value) (cond_ condFlag, rx, ry operand) {
	condDef := m.compiler.ValueDefinition(cond)
	if m.compiler.MatchInstr(condDef, ssa.OpcodeIcmp) {
		x, y, c := condDef.Instr.IcmpData()
		flag, swap := condFlagFromSSAIntegerCmpCond(c)
		rx = m.getOperand_NR(m.compiler.ValueDefinition(x))
		ry = m.getOperand_NR(m.compiler.ValueDefinition(y))
		if swap {
			rx, ry = ry, rx
		}
		condDef.Instr.MarkLowered()
		return flag, rx, ry
	}
	return condNE, m.getOperand_NR(condDef), operandNR(zeroVReg)
}

func (m *machine) lowerExitIfTrueWithCode(execCtx regalloc.VReg, cond ssa.Value, code nativeapi.ExitCode) {
	island := m.getOrCreateTrapIsland(code)

	// The island reads the execution context out of the reserved tmpReg.
	mv := m.allocateInstr()
	mv.asMove64(tmpRegVReg, execCtx)
	m.insert(mv)

	flag, rx, ry := m.lowerTrapBranchOperands(cond)
	br := m.allocateInstr()
	br.asCondBr(flag, rx, ry, island)
	m.insert(br)
}

package riscv64

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/moremath"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// sewForLane maps a wasm lane shape to the RVV element width that covers it.
func sewForLane(lane ssa.VecLane) uint32 {
	switch lane {
	case ssa.VecLaneI8x16:
		return vsew8
	case ssa.VecLaneI16x8:
		return vsew16
	case ssa.VecLaneI32x4, ssa.VecLaneF32x4:
		return vsew32
	case ssa.VecLaneI64x2, ssa.VecLaneF64x2:
		return vsew64
	}
	panic(fmt.Sprintf("BUG: unsupported vector lane %s", lane))
}

// lowerVecRRR lowers a lane-wise binary operation.
func (m *machine) lowerVecRRR(funct6, form uint32, instr *ssa.Instruction) {
	x, y, lane := instr.Arg2WithLane()
	rd := m.compiler.VRegOf(instr.Return())
	vs2 := m.getOperand_NR(m.compiler.ValueDefinition(x))
	vs1 := m.getOperand_NR(m.compiler.ValueDefinition(y))
	i := m.allocateInstr()
	i.asVecRRR(funct6, form, rd, vs2, vs1, sewForLane(lane))
	m.insert(i)
}

// lowerVecRR lowers a lane-wise unary operation.
func (m *machine) lowerVecRR(funct6, variant, form uint32, instr *ssa.Instruction) {
	x, lane := instr.ArgWithLane()
	rd := m.compiler.VRegOf(instr.Return())
	vs2 := m.getOperand_NR(m.compiler.ValueDefinition(x))
	i := m.allocateInstr()
	i.asVecRR(funct6, variant, form, rd, vs2, sewForLane(lane))
	m.insert(i)
}

// lowerVecShift lowers the lane-wise shifts, whose amount is a scalar.
//
// wasm takes the shift amount modulo the lane width; RVV does the same, using
// only as many bits of the scalar as the element needs, so no masking is
// required.
func (m *machine) lowerVecShift(funct6 uint32, instr *ssa.Instruction) {
	x, y, lane := instr.Arg2WithLane()
	rd := m.compiler.VRegOf(instr.Return())
	vs2 := m.getOperand_NR(m.compiler.ValueDefinition(x))
	rs1 := m.getOperand_NR(m.compiler.ValueDefinition(y))
	i := m.allocateInstr()
	i.asVecRX(funct6, rd, vs2, rs1, sewForLane(lane))
	m.insert(i)
}

// lowerVecCmp lowers a lane-wise comparison to all-ones/all-zeros lanes.
func (m *machine) lowerVecCmp(funct6, form uint32, rd regalloc.VReg, x, y ssa.Value, sew uint32, swap bool) {
	vs2 := m.getOperand_NR(m.compiler.ValueDefinition(x))
	vs1 := m.getOperand_NR(m.compiler.ValueDefinition(y))
	if swap {
		vs2, vs1 = vs1, vs2
	}
	i := m.allocateInstr()
	i.asVecCmp(funct6, form, rd, vs2, vs1, sew)
	m.insert(i)
}

// lowerVIcmp lowers the integer lane-wise comparisons.
//
// RVV offers eq, ne, lt, le and their unsigned forms; the "greater" directions
// come from swapping the operands, exactly as in the scalar case.
func (m *machine) lowerVIcmp(instr *ssa.Instruction) {
	x, y, c, lane := instr.VIcmpData()
	rd := m.compiler.VRegOf(instr.Return())
	sew := sewForLane(lane)

	var funct6 uint32
	var swap bool
	switch c {
	case ssa.IntegerCmpCondEqual:
		funct6 = vfunctMseq
	case ssa.IntegerCmpCondNotEqual:
		funct6 = vfunctMsne
	case ssa.IntegerCmpCondSignedLessThan:
		funct6 = vfunctMslt
	case ssa.IntegerCmpCondSignedGreaterThan:
		funct6, swap = vfunctMslt, true
	case ssa.IntegerCmpCondSignedLessThanOrEqual:
		funct6 = vfunctMsle
	case ssa.IntegerCmpCondSignedGreaterThanOrEqual:
		funct6, swap = vfunctMsle, true
	case ssa.IntegerCmpCondUnsignedLessThan:
		funct6 = vfunctMsltu
	case ssa.IntegerCmpCondUnsignedGreaterThan:
		funct6, swap = vfunctMsltu, true
	case ssa.IntegerCmpCondUnsignedLessThanOrEqual:
		funct6 = vfunctMsleu
	case ssa.IntegerCmpCondUnsignedGreaterThanOrEqual:
		funct6, swap = vfunctMsleu, true
	default:
		panic(fmt.Sprintf("BUG: unhandled vector integer comparison %s", c))
	}
	m.lowerVecCmp(funct6, opivv, rd, x, y, sew, swap)
}

// lowerVFcmp lowers the float lane-wise comparisons.
//
// As in the scalar case the three hardware comparisons already answer false
// for unordered lanes, which is what wasm wants, and the two missing
// directions come from swapping the operands.
func (m *machine) lowerVFcmp(instr *ssa.Instruction) {
	x, y, c, lane := instr.VFcmpData()
	rd := m.compiler.VRegOf(instr.Return())
	sew := sewForLane(lane)

	var funct6 uint32
	var swap bool
	switch c {
	case ssa.FloatCmpCondEqual:
		funct6 = vfunctMfeq
	case ssa.FloatCmpCondNotEqual:
		funct6 = vfunctMfne
	case ssa.FloatCmpCondLessThan:
		funct6 = vfunctMflt
	case ssa.FloatCmpCondLessThanOrEqual:
		funct6 = vfunctMfle
	case ssa.FloatCmpCondGreaterThan:
		funct6, swap = vfunctMflt, true
	case ssa.FloatCmpCondGreaterThanOrEqual:
		funct6, swap = vfunctMfle, true
	default:
		panic(fmt.Sprintf("BUG: unhandled vector float comparison %s", c))
	}
	m.lowerVecCmp(funct6, opfvv, rd, x, y, sew, swap)
}

// lowerVIneg negates each lane, which RVV spells as a reverse subtract from
// the zero register rather than a negate instruction.
func (m *machine) lowerVIneg(instr *ssa.Instruction) {
	x, lane := instr.ArgWithLane()
	rd := m.compiler.VRegOf(instr.Return())
	vs2 := m.getOperand_NR(m.compiler.ValueDefinition(x))
	i := m.allocateInstr()
	i.asVecRX(vfunctRsub, rd, vs2, operandNR(zeroVReg), sewForLane(lane))
	m.insert(i)
}

// lowerVbnot inverts every bit, which RVV spells as xor against all-ones.
func (m *machine) lowerVbnot(instr *ssa.Instruction) {
	x := instr.Arg()
	rd := m.compiler.VRegOf(instr.Return())
	vs2 := m.getOperand_NR(m.compiler.ValueDefinition(x))
	ones := m.compiler.AllocateVReg(ssa.TypeI64)
	m.lowerConstantI64(ones, -1)
	i := m.allocateInstr()
	i.asVecRX(vfunctXor, rd, vs2, operandNR(ones), vsew64)
	m.insert(i)
}

// vecUn is the shared shape of the unary lowerings whose operand and result
// are both v128 and whose lane width comes from the instruction.
func (m *machine) vecUn(instr *ssa.Instruction) (vs2 operand, rd regalloc.VReg, sew uint32) {
	x, lane := instr.ArgWithLane()
	return m.getOperand_NR(m.compiler.ValueDefinition(x)), m.compiler.VRegOf(instr.Return()), sewForLane(lane)
}

// lowerVFneg and lowerVFabs are sign manipulations, not arithmetic: RVV has no
// vector negate or absolute-value instruction any more than the scalar side
// does, and both are fsgnj forms with the source used twice.
func (m *machine) lowerVFneg(instr *ssa.Instruction) {
	vs2, rd, sew := m.vecUn(instr)
	m.insert(m.allocateInstr().asVecRRR(vfunctFsgnjn, opfvv, rd, vs2, vs2, sew))
}

func (m *machine) lowerVFabs(instr *ssa.Instruction) {
	vs2, rd, sew := m.vecUn(instr)
	m.insert(m.allocateInstr().asVecRRR(vfunctFsgnjx, opfvv, rd, vs2, vs2, sew))
}

// lowerVIabs computes max(x, -x) per lane.
//
// That is exact for every value except the most negative one, where the
// negation overflows back to itself -- and wasm specifies exactly that
// wrapping result, so no saturation is wanted here.
func (m *machine) lowerVIabs(instr *ssa.Instruction) {
	vs2, rd, sew := m.vecUn(instr)
	neg := m.compiler.AllocateVReg(ssa.TypeV128)
	m.insert(m.allocateInstr().asVecRX(vfunctRsub, neg, vs2, operandNR(zeroVReg), sew))
	m.insert(m.allocateInstr().asVecRRR(vfunctMax, opivv, rd, vs2, operandNR(neg), sew))
}

// lowerVbandnot computes `x & ~y`. The base vector extension has no andn, so
// the complement is an explicit xor against all-ones.
func (m *machine) lowerVbandnot(instr *ssa.Instruction) {
	x, y := instr.Arg2()
	rd := m.compiler.VRegOf(instr.Return())
	vx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	vy := m.getOperand_NR(m.compiler.ValueDefinition(y))
	ones := m.compiler.AllocateVReg(ssa.TypeI64)
	m.lowerConstantI64(ones, -1)
	notY := m.compiler.AllocateVReg(ssa.TypeV128)
	m.insert(m.allocateInstr().asVecRX(vfunctXor, notY, vy, operandNR(ones), vsew64))
	m.insert(m.allocateInstr().asVecRRR(vfunctAnd, opivv, rd, vx, operandNR(notY), vsew64))
}

// lowerVbitselect computes `(x & c) | (y & ~c)`, bit for bit rather than lane
// by lane, which is why it is not a merge.
//
// Written as `y ^ ((x ^ y) & c)`: three operations instead of five, and no
// complement of c is needed.
func (m *machine) lowerVbitselect(instr *ssa.Instruction) {
	c, x, y := instr.SelectData()
	rd := m.compiler.VRegOf(instr.Return())
	vc := m.getOperand_NR(m.compiler.ValueDefinition(c))
	vx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	vy := m.getOperand_NR(m.compiler.ValueDefinition(y))

	t := m.compiler.AllocateVReg(ssa.TypeV128)
	m.insert(m.allocateInstr().asVecRRR(vfunctXor, opivv, t, vx, vy, vsew64))
	m.insert(m.allocateInstr().asVecRRR(vfunctAnd, opivv, t, operandNR(t), vc, vsew64))
	m.insert(m.allocateInstr().asVecRRR(vfunctXor, opivv, rd, vy, operandNR(t), vsew64))
}

// lowerVanyTrue and lowerVallTrue both reduce to counting lanes.
//
// vecMaskPop compares against zero and counts the mask, so any_true is "the
// count of zero lanes is less than the lane count" and all_true is "the count
// of zero lanes is zero". Testing zero lanes rather than non-zero ones means a
// single comparison serves both.
func (m *machine) lowerVanyTrue(instr *ssa.Instruction) {
	x := instr.Arg()
	rd := m.compiler.VRegOf(instr.Return())
	vs2 := m.getOperand_NR(m.compiler.ValueDefinition(x))
	// any_true is defined over the whole 128 bits, not a lane shape, so the
	// widest element is used: a non-zero vector has a non-zero 64-bit lane.
	count := m.compiler.AllocateVReg(ssa.TypeI64)
	m.insert(m.allocateInstr().asVecMaskPop(count, vs2, vsew64, true /* count zero lanes */))
	// rd = (count != 2), i.e. at least one lane was non-zero.
	m.insert(m.allocateInstr().asALU(aluOpXor, rd, operandNR(count), operandImm(2), true))
	m.insert(m.allocateInstr().asALU(aluOpSltu, rd, operandNR(zeroVReg), operandNR(rd), true))
}

func (m *machine) lowerVallTrue(instr *ssa.Instruction) {
	x, lane := instr.ArgWithLane()
	rd := m.compiler.VRegOf(instr.Return())
	vs2 := m.getOperand_NR(m.compiler.ValueDefinition(x))
	count := m.compiler.AllocateVReg(ssa.TypeI64)
	m.insert(m.allocateInstr().asVecMaskPop(count, vs2, sewForLane(lane), true))
	// rd = (count == 0): no lane was zero.
	m.insert(m.allocateInstr().asALU(aluOpSltu, rd, operandNR(count), operandImm(1), true))
}

// lowerSplat broadcasts a scalar across every lane.
func (m *machine) lowerSplat(instr *ssa.Instruction) {
	x, lane := instr.ArgWithLane()
	rd := m.compiler.VRegOf(instr.Return())
	rs := m.getOperand_NR(m.compiler.ValueDefinition(x))
	sew := sewForLane(lane)
	if lane == ssa.VecLaneF32x4 || lane == ssa.VecLaneF64x2 {
		// The value is in an f-register; cross to the integer file first,
		// since vmv.v.x reads an integer register.
		bits := m.compiler.AllocateVReg(ssa.TypeI64)
		m.insert(m.allocateInstr().asFmvToInt(bits, rs, lane == ssa.VecLaneF64x2))
		rs = operandNR(bits)
	}
	m.insert(m.allocateInstr().asVecSplat(rd, rs, sew))
}

// lowerVFcvtFromInt converts integer lanes to float lanes.
func (m *machine) lowerVFcvtFromInt(instr *ssa.Instruction, signed bool) {
	vs2, rd, sew := m.vecUn(instr)
	variant := uint32(vsubCvtFXu)
	if signed {
		variant = vsubCvtFX
	}
	m.insert(m.allocateInstr().asVecRR(vfunctFunary0, variant, opfvv, rd, vs2, sew))
}

// lowerVFcvtToIntSat converts float lanes to saturating integer lanes.
//
// RVV's conversion already saturates the infinities and out-of-range values,
// but maps NaN to the maximum where wasm requires zero -- the same divergence
// as the scalar case, and corrected the same way, by masking the lanes that
// compare unordered against themselves.
func (m *machine) lowerVFcvtToIntSat(instr *ssa.Instruction, signed bool) {
	vs2, rd, sew := m.vecUn(instr)
	variant := uint32(vsubCvtRtzXuF)
	if signed {
		variant = vsubCvtRtzXF
	}
	m.insert(m.allocateInstr().asVecRR(vfunctFunary0, variant, opfvv, rd, vs2, sew))

	// Zero the lanes whose input was NaN: vmfeq of a value with itself is
	// false exactly there, so merging against a zero vector under that mask
	// leaves everything else alone.
	zeros := m.compiler.AllocateVReg(ssa.TypeV128)
	m.insert(m.allocateInstr().asVecSplat(zeros, operandNR(zeroVReg), sew))
	m.insert(m.allocateInstr().asVecNaNZero(rd, vs2, operandNR(zeros), sew))
}

// lowerFvdemote and lowerFvpromoteLow narrow and widen float lanes.
func (m *machine) lowerFvdemote(instr *ssa.Instruction) {
	vs2, rd, _ := m.vecUn(instr)
	// The source is f64x2 and the result f32x4, so the conversion runs at the
	// *narrow* width, which is what the .w suffix denotes.
	m.insert(m.allocateInstr().asVecWiden(vfunctFunary0, vsubFncvtFF, opfvv, rd, vs2, vsew32))
}

func (m *machine) lowerFvpromoteLow(instr *ssa.Instruction) {
	vs2, rd, _ := m.vecUn(instr)
	m.insert(m.allocateInstr().asVecWiden(vfunctFunary0, vsubFwcvtFF, opfvv, rd, vs2, vsew32))
}

// lowerVwiden widens the low or high half of a vector, signed or not.
//
// RVV's extensions always read the *low* half, so the high-half forms slide
// the wanted lanes down first.
func (m *machine) lowerVwiden(instr *ssa.Instruction, signed, high bool) {
	x, lane := instr.ArgWithLane()
	rd := m.compiler.VRegOf(instr.Return())
	vs2 := m.getOperand_NR(m.compiler.ValueDefinition(x))
	srcSew := sewForLane(lane)

	if high {
		half := vecAVLFor(srcSew) / 2
		slid := m.compiler.AllocateVReg(ssa.TypeV128)
		m.insert(m.allocateInstr().asVecSlide(slid, vs2, half, srcSew, false /* down */))
		vs2 = operandNR(slid)
	}
	variant := uint32(vsubZext2)
	if signed {
		variant = vsubSext2
	}
	// The extension is expressed at the *destination* width, which is one
	// step wider: the sew constants are an index, so that is simply +1.
	m.insert(m.allocateInstr().asVecExtend(vfunctXunary, variant, opmvv, rd, vs2, srcSew+1))
}

// lowerSwizzle permutes bytes by a runtime index vector.
//
// vrgather already matches wasm here without a range check: an index at or
// beyond the vector length reads as zero, and with the length pinned to 16
// bytes that is exactly i8x16.swizzle's rule for out-of-range indices.
func (m *machine) lowerSwizzle(instr *ssa.Instruction) {
	x, y, _ := instr.Arg2WithLane()
	rd := m.compiler.VRegOf(instr.Return())
	vx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	vy := m.getOperand_NR(m.compiler.ValueDefinition(y))
	m.insert(m.allocateInstr().asVecRRR(vfunctRgather, opivv, rd, vx, vy, vsew8).asVecNoOverlap())
}

// lowerSqmulRoundSat is the saturating rounding doubling multiply, which RVV
// provides directly as vsmul.
func (m *machine) lowerSqmulRoundSat(instr *ssa.Instruction) {
	x, y, lane := instr.Arg2WithLane()
	rd := m.compiler.VRegOf(instr.Return())
	vx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	vy := m.getOperand_NR(m.compiler.ValueDefinition(y))
	m.insert(m.allocateInstr().asVecRRR(vfunctSmul, opivv, rd, vx, vy, sewForLane(lane)))
}

// lowerVFminFmax lowers the lane-wise min and max.
//
// RVV's vfmin and vfmax return the non-NaN operand when exactly one lane is
// NaN, exactly as the scalar instructions do, while wasm propagates the NaN.
// So the hardware result is corrected on the unordered lanes: vmfeq of each
// operand with itself is false precisely there, and merging a canonical NaN
// in under that mask fixes them without touching the rest.
func (m *machine) lowerVFminFmax(instr *ssa.Instruction, isMax bool) {
	x, y, lane := instr.Arg2WithLane()
	rd := m.compiler.VRegOf(instr.Return())
	vx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	vy := m.getOperand_NR(m.compiler.ValueDefinition(y))
	sew := sewForLane(lane)

	funct6 := uint32(vfunctFmin)
	if isMax {
		funct6 = vfunctFmax
	}
	m.insert(m.allocateInstr().asVecRRR(funct6, opfvv, rd, vx, vy, sew))

	// A NaN in *either* operand must show up in the result, so both are
	// checked; the first correction makes the result NaN, which the second
	// then leaves alone.
	nanVec := m.vecCanonicalNaN(lane)
	m.insert(m.allocateInstr().asVecNaNZero(rd, vx, operandNR(nanVec), sew))
	m.insert(m.allocateInstr().asVecNaNZero(rd, vy, operandNR(nanVec), sew))
}

// vecCanonicalNaN builds a vector whose every lane is the canonical NaN of the
// lane's width.
func (m *machine) vecCanonicalNaN(lane ssa.VecLane) regalloc.VReg {
	bits := m.compiler.AllocateVReg(ssa.TypeI64)
	if lane == ssa.VecLaneF64x2 {
		m.lowerConstantI64(bits, int64(moremath.F64CanonicalNaNBits))
	} else {
		m.lowerConstantI64(bits, int64(int32(moremath.F32CanonicalNaNBits)))
	}
	v := m.compiler.AllocateVReg(ssa.TypeV128)
	m.insert(m.allocateInstr().asVecSplat(v, operandNR(bits), sewForLane(lane)))
	return v
}

// lowerVMinMaxPseudo lowers wasm's pmin and pmax.
//
// These are deliberately *not* min and max: pmin(x, y) is "y < x ? y : x",
// which returns x whenever the comparison is false -- including when either
// operand is NaN, and including for equal zeros of opposite sign. Expressing
// them as a comparison and a merge, rather than reaching for vfmin, is what
// preserves that.
func (m *machine) lowerVMinMaxPseudo(instr *ssa.Instruction, isMax bool) {
	x, y, lane := instr.Arg2WithLane()
	rd := m.compiler.VRegOf(instr.Return())
	vx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	vy := m.getOperand_NR(m.compiler.ValueDefinition(y))
	sew := sewForLane(lane)

	// pmin: mask = y < x, result = mask ? y : x.
	// pmax: mask = x < y, result = mask ? y : x.
	a, b := vy, vx
	if isMax {
		a, b = vx, vy
	}
	m.insert(m.allocateInstr().asVecSelectLt(rd, a, b, vy, vx, sew))
}

// lowerVhighBits gathers the sign bit of each lane into an integer.
//
// The comparison already produces exactly this, as a mask register holding one
// bit per lane; all that remains is to read those bits out as an integer,
// which needs a second vtype wide enough to cover the lane count.
func (m *machine) lowerVhighBits(instr *ssa.Instruction) {
	x, lane := instr.ArgWithLane()
	rd := m.compiler.VRegOf(instr.Return())
	vs2 := m.getOperand_NR(m.compiler.ValueDefinition(x))
	m.insert(m.allocateInstr().asVecHighBits(rd, vs2, sewForLane(lane)))
}

// lowerExtractlane reads one lane out into a scalar register.
//
// vmv.x.s only ever reads lane 0, so the wanted lane is slid down to it first.
func (m *machine) lowerExtractlane(instr *ssa.Instruction) {
	x, index, signed, lane := instr.ExtractlaneData()
	rd := m.compiler.VRegOf(instr.Return())
	vs2 := m.getOperand_NR(m.compiler.ValueDefinition(x))
	sew := sewForLane(lane)

	src := vs2
	if index != 0 {
		slid := m.compiler.AllocateVReg(ssa.TypeV128)
		m.insert(m.allocateInstr().asVecSlide(slid, vs2, uint32(index), sew, false))
		src = operandNR(slid)
	}
	m.insert(m.allocateInstr().asVecExtract(rd, src, sew, signed, lane))
}

// lowerInsertlane writes a scalar into one lane, leaving the rest alone.
//
// The lane is selected by comparing each lane's index against the target,
// which vid.v would give directly; without it the same mask is built by
// splatting the index and comparing.
func (m *machine) lowerInsertlane(instr *ssa.Instruction) {
	x, y, index, lane := instr.InsertlaneData()
	rd := m.compiler.VRegOf(instr.Return())
	vx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	vy := m.getOperand_NR(m.compiler.ValueDefinition(y))
	sew := sewForLane(lane)

	scalar := vy
	if lane == ssa.VecLaneF32x4 || lane == ssa.VecLaneF64x2 {
		bits := m.compiler.AllocateVReg(ssa.TypeI64)
		m.insert(m.allocateInstr().asFmvToInt(bits, vy, lane == ssa.VecLaneF64x2))
		scalar = operandNR(bits)
	}
	m.insert(m.allocateInstr().asVecInsert(rd, vx, scalar, uint32(index), sew))
}

// lowerNarrow lowers the saturating narrowing operations.
//
// vnclip narrows with saturation in one instruction, shifting by zero: the two
// source vectors are narrowed separately and the results concatenated, since
// RVV narrows one register at a time.
func (m *machine) lowerNarrow(instr *ssa.Instruction, signed bool) {
	x, y, lane := instr.Arg2WithLane()
	rd := m.compiler.VRegOf(instr.Return())
	vx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	vy := m.getOperand_NR(m.compiler.ValueDefinition(y))
	// lane names the *source* shape; the result lanes are half as wide.
	dstSew := sewForLane(lane) - 1

	funct6 := uint32(vfunctNclipu)
	if signed {
		funct6 = vfunctNclip
	}
	lo := m.compiler.AllocateVReg(ssa.TypeV128)
	hi := m.compiler.AllocateVReg(ssa.TypeV128)
	m.insert(m.allocateInstr().asVecNarrow(funct6, lo, vx, dstSew))
	m.insert(m.allocateInstr().asVecNarrow(funct6, hi, vy, dstSew))
	// Place the second half above the first.
	m.insert(m.allocateInstr().asVecMov(rd, lo))
	m.insert(m.allocateInstr().asVecSlide(rd, operandNR(hi), vecAVLFor(dstSew)/2, dstSew, true))
}

// lowerVconst materializes a 128-bit constant.
//
// vmv.s.x writes lane 0 only, so the two halves go in one at a time: the high
// half is placed in lane 0 and slid up into lane 1, then the low half is
// written into the lane 0 it vacated.
func (m *machine) lowerVconst(instr *ssa.Instruction) {
	rd := m.compiler.VRegOf(instr.Return())
	lo, hi := instr.VconstData()

	loReg := m.compiler.AllocateVReg(ssa.TypeI64)
	hiReg := m.compiler.AllocateVReg(ssa.TypeI64)
	m.lowerConstantI64(loReg, int64(lo))
	m.lowerConstantI64(hiReg, int64(hi))
	m.insert(m.allocateInstr().asVecConst(rd, operandNR(loReg), operandNR(hiReg)))
}

// lowerExtIaddPairwise widens adjacent lanes and adds each pair.
//
// The pairs are already adjacent inside a destination-width element, so no
// gather or slide is needed: shifting that element left then right isolates
// the low half, and shifting right alone isolates the high half. Signedness
// picks between the arithmetic and logical right shift.
func (m *machine) lowerExtIaddPairwise(instr *ssa.Instruction) {
	v, lane, signed := instr.ExtIaddPairwiseData()
	rd := m.compiler.VRegOf(instr.Return())
	vs := m.getOperand_NR(m.compiler.ValueDefinition(v))

	srcSew := sewForLane(lane)
	dstSew := srcSew + 1
	half := uint32(8 << srcSew) // bits in a source lane

	right := uint32(vfunctSrl)
	if signed {
		right = vfunctSra
	}

	lo := m.compiler.AllocateVReg(ssa.TypeV128)
	hi := m.compiler.AllocateVReg(ssa.TypeV128)
	m.insert(m.allocateInstr().asVecShiftImm(vfunctSll, lo, vs, half, dstSew))
	m.insert(m.allocateInstr().asVecShiftImm(right, lo, operandNR(lo), half, dstSew))
	m.insert(m.allocateInstr().asVecShiftImm(right, hi, vs, half, dstSew))
	m.insert(m.allocateInstr().asVecRRR(vfunctAdd, opivv, rd, operandNR(lo), operandNR(hi), dstSew))
}

// lowerWideningPairwiseDotProduct is i32x4.dot_i16x8_s: multiply adjacent
// signed 16-bit pairs and add each pair's products.
//
// Same idea as the pairwise add -- the two halves of each 32-bit element are
// isolated with shifts rather than gathers -- with a multiply in between.
func (m *machine) lowerWideningPairwiseDotProduct(instr *ssa.Instruction) {
	x, y := instr.Arg2()
	rd := m.compiler.VRegOf(instr.Return())
	vx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	vy := m.getOperand_NR(m.compiler.ValueDefinition(y))

	const dstSew, half = vsew32, 16
	xlo := m.compiler.AllocateVReg(ssa.TypeV128)
	ylo := m.compiler.AllocateVReg(ssa.TypeV128)
	xhi := m.compiler.AllocateVReg(ssa.TypeV128)
	yhi := m.compiler.AllocateVReg(ssa.TypeV128)

	for _, p := range []struct {
		dst regalloc.VReg
		src operand
	}{{xlo, vx}, {ylo, vy}} {
		m.insert(m.allocateInstr().asVecShiftImm(vfunctSll, p.dst, p.src, half, dstSew))
		m.insert(m.allocateInstr().asVecShiftImm(vfunctSra, p.dst, operandNR(p.dst), half, dstSew))
	}
	m.insert(m.allocateInstr().asVecShiftImm(vfunctSra, xhi, vx, half, dstSew))
	m.insert(m.allocateInstr().asVecShiftImm(vfunctSra, yhi, vy, half, dstSew))

	m.insert(m.allocateInstr().asVecRRR(vfunctMul, opmvv, xlo, operandNR(xlo), operandNR(ylo), dstSew))
	m.insert(m.allocateInstr().asVecRRR(vfunctMul, opmvv, xhi, operandNR(xhi), operandNR(yhi), dstSew))
	m.insert(m.allocateInstr().asVecRRR(vfunctAdd, opivv, rd, operandNR(xlo), operandNR(xhi), dstSew))
}

// lowerVIpopcnt counts the set bits of each byte lane.
//
// The base vector extension has no per-lane population count -- that is Zvbb --
// so this is the byte-wide SWAR fold, the same shape as the scalar sequence
// but without the multiply step, which is unnecessary at eight bits.
func (m *machine) lowerVIpopcnt(instr *ssa.Instruction) {
	vs2, rd, _ := m.vecUn(instr)
	const sew = vsew8

	c55 := m.vecSplatConst(0x55, sew)
	c33 := m.vecSplatConst(0x33, sew)
	c0f := m.vecSplatConst(0x0f, sew)

	t := m.compiler.AllocateVReg(ssa.TypeV128)
	acc := m.compiler.AllocateVReg(ssa.TypeV128)

	// acc = x - ((x >> 1) & 0x55)
	m.insert(m.allocateInstr().asVecShiftImm(vfunctSrl, t, vs2, 1, sew))
	m.insert(m.allocateInstr().asVecRRR(vfunctAnd, opivv, t, operandNR(t), operandNR(c55), sew))
	m.insert(m.allocateInstr().asVecRRR(vfunctSub, opivv, acc, vs2, operandNR(t), sew))

	// acc = (acc & 0x33) + ((acc >> 2) & 0x33)
	m.insert(m.allocateInstr().asVecShiftImm(vfunctSrl, t, operandNR(acc), 2, sew))
	m.insert(m.allocateInstr().asVecRRR(vfunctAnd, opivv, t, operandNR(t), operandNR(c33), sew))
	m.insert(m.allocateInstr().asVecRRR(vfunctAnd, opivv, acc, operandNR(acc), operandNR(c33), sew))
	m.insert(m.allocateInstr().asVecRRR(vfunctAdd, opivv, acc, operandNR(acc), operandNR(t), sew))

	// rd = (acc + (acc >> 4)) & 0x0f
	m.insert(m.allocateInstr().asVecShiftImm(vfunctSrl, t, operandNR(acc), 4, sew))
	m.insert(m.allocateInstr().asVecRRR(vfunctAdd, opivv, acc, operandNR(acc), operandNR(t), sew))
	m.insert(m.allocateInstr().asVecRRR(vfunctAnd, opivv, rd, operandNR(acc), operandNR(c0f), sew))
}

// vecSplatConst builds a vector whose every lane holds the same small constant.
func (m *machine) vecSplatConst(v int64, sew uint32) regalloc.VReg {
	r := m.compiler.AllocateVReg(ssa.TypeI64)
	m.lowerConstantI64(r, v)
	vec := m.compiler.AllocateVReg(ssa.TypeV128)
	m.insert(m.allocateInstr().asVecSplat(vec, operandNR(r), sew))
	return vec
}

// lowerVecRound lowers the lane-wise ceil, floor, trunc and nearest.
//
// The scalar versions name their rounding mode in the instruction; RVV's
// vfcvt has no such field and always follows the fcsr, so every mode but
// truncation has to set frm around the conversion and put it back. The rest
// mirrors the scalar lowering: convert out and back, restore the sign so that
// ceil(-0.3) is -0.0, and leave anything already integral -- or NaN, or
// infinite -- untouched.
func (m *machine) lowerVecRound(instr *ssa.Instruction, mode roundMode) {
	vs2, rd, sew := m.vecUn(instr)
	m.insert(m.allocateInstr().asVecRound(rd, vs2, sew, mode))
}

// lowerVZeroExtLoad loads a narrow value into lane 0 and zeroes the rest.
func (m *machine) lowerVZeroExtLoad(instr *ssa.Instruction) {
	ptr, offset, typ := instr.VZeroExtLoadData()
	rd := m.compiler.VRegOf(instr.Return())
	amode := m.lowerToAddressMode(ptr, offset)

	scalar := m.compiler.AllocateVReg(ssa.TypeI64)
	load := m.allocateInstr()
	load.asLoad(scalar, amode, typ.Bits(), false /* zero-extended */)
	m.insert(load)

	// vmv.s.x writes lane 0 and, with the tail agnostic, leaves the rest
	// undefined -- so the register is zeroed first.
	m.insert(m.allocateInstr().asVecSplat(rd, operandNR(zeroVReg), vsew64))
	m.insert(m.allocateInstr().asVecInsertLane0(rd, operandNR(scalar), vsew64))
}

// lowerLoadSplat loads one element and broadcasts it.
func (m *machine) lowerLoadSplat(instr *ssa.Instruction) {
	ptr, offset, lane := instr.LoadSplatData()
	rd := m.compiler.VRegOf(instr.Return())
	amode := m.lowerToAddressMode(ptr, offset)
	sew := sewForLane(lane)

	scalar := m.compiler.AllocateVReg(ssa.TypeI64)
	load := m.allocateInstr()
	load.asLoad(scalar, amode, byte(8<<sew), false)
	m.insert(load)
	m.insert(m.allocateInstr().asVecSplat(rd, operandNR(scalar), sew))
}

// lowerShuffle permutes bytes of two vectors by a compile-time index vector.
//
// vrgather reads one register, and an index at or beyond the vector length
// gives zero, so each source is gathered with its own index vector -- the
// second biased down by 16 -- and the two results combined. Indices below 16
// select from the first source and read zero from the second, and vice versa,
// so a plain or is enough to merge them.
func (m *machine) lowerShuffle(instr *ssa.Instruction) {
	x, y, lo, hi := instr.ShuffleData()
	rd := m.compiler.VRegOf(instr.Return())
	vx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	vy := m.getOperand_NR(m.compiler.ValueDefinition(y))

	idxLo := m.compiler.AllocateVReg(ssa.TypeV128)
	idxHi := m.compiler.AllocateVReg(ssa.TypeV128)
	m.lowerVconstInto(idxLo, lo, hi)
	// The second gather's indices are the same minus 16, so that 16..31 map to
	// 0..15 and everything below 16 falls out of range and reads zero.
	bias := m.vecSplatConst(16, vsew8)
	m.insert(m.allocateInstr().asVecRRR(vfunctSub, opivv, idxHi, operandNR(idxLo), operandNR(bias), vsew8))

	a := m.compiler.AllocateVReg(ssa.TypeV128)
	b := m.compiler.AllocateVReg(ssa.TypeV128)
	m.insert(m.allocateInstr().asVecRRR(vfunctRgather, opivv, a, vx, operandNR(idxLo), vsew8).asVecNoOverlap())
	m.insert(m.allocateInstr().asVecRRR(vfunctRgather, opivv, b, vy, operandNR(idxHi), vsew8).asVecNoOverlap())
	m.insert(m.allocateInstr().asVecRRR(vfunctOr, opivv, rd, operandNR(a), operandNR(b), vsew8))
}

// lowerVconstInto materializes a 128-bit constant into an existing register.
func (m *machine) lowerVconstInto(rd regalloc.VReg, lo, hi uint64) {
	loReg := m.compiler.AllocateVReg(ssa.TypeI64)
	hiReg := m.compiler.AllocateVReg(ssa.TypeI64)
	m.lowerConstantI64(loReg, int64(lo))
	m.lowerConstantI64(hiReg, int64(hi))
	m.insert(m.allocateInstr().asVecConst(rd, operandNR(loReg), operandNR(hiReg)))
}

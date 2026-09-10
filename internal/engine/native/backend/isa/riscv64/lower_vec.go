package riscv64

import (
	"fmt"

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

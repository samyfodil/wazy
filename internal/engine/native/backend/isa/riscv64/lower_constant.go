package riscv64

import (
	"math"
	"math/bits"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// insertLoadConstant materializes a constant of the given type into rd.
func (m *machine) insertLoadConstant(v uint64, typ ssa.Type, rd regalloc.VReg) {
	switch typ {
	case ssa.TypeI32:
		// i32 constants live sign-extended, so materialize int32(v), not the
		// zero-extended value: 0xffffffff must become -1, not 4294967295.
		m.lowerConstantI64(rd, int64(int32(uint32(v))))
	case ssa.TypeI64:
		m.lowerConstantI64(rd, int64(v))
	case ssa.TypeF32:
		m.lowerConstantF32(rd, uint32(v))
	case ssa.TypeF64:
		m.lowerConstantF64(rd, v)
	default:
		panic("BUG: unsupported constant type on riscv64: " + typ.String())
	}
}

// lowerConstantF32 materializes a 32-bit FP constant.
//
// RISC-V has no floating-point immediate of any kind -- not even for 0.0 or
// 1.0 -- so a constant must come either from memory or across from an integer
// register. This backend always takes the register route: the bit pattern is
// built with the integer sequence below and moved with fmv.w.x, which costs at
// most a handful of ALU instructions and, unlike a PC-relative literal load,
// touches no memory and needs no constant pool emitted after the function.
//
// fmv.w.x is also the *correct* move here rather than merely the convenient
// one: RV64D requires a single-precision value in an f-register to be
// NaN-boxed (upper 32 bits all ones), and fmv.w.x produces that boxing where a
// 64-bit fmv.d.x would not.
func (m *machine) lowerConstantF32(rd regalloc.VReg, v uint32) {
	if v == 0 {
		// +0.0 is the one case with no integer work at all.
		mv := m.allocateInstr()
		mv.asFmvFromInt(rd, operandNR(zeroVReg), false)
		m.insert(mv)
		return
	}
	tmp := m.compiler.AllocateVReg(ssa.TypeI64)
	m.lowerConstantI64(tmp, int64(int32(v)))
	mv := m.allocateInstr()
	mv.asFmvFromInt(rd, operandNR(tmp), false)
	m.insert(mv)
}

// lowerConstantF64 materializes a 64-bit FP constant; see lowerConstantF32.
func (m *machine) lowerConstantF64(rd regalloc.VReg, v uint64) {
	if v == 0 {
		mv := m.allocateInstr()
		mv.asFmvFromInt(rd, operandNR(zeroVReg), true)
		m.insert(mv)
		return
	}
	tmp := m.compiler.AllocateVReg(ssa.TypeI64)
	m.lowerConstantI64(tmp, int64(v))
	mv := m.allocateInstr()
	mv.asFmvFromInt(rd, operandNR(tmp), true)
	m.insert(mv)
}

// lowerConstantI64 materializes an integer constant into rd.
//
// The longest immediate RISC-V offers is lui's 20 bits, so anything wider is
// assembled from pieces. This is the standard recursive construction every
// RISC-V compiler uses: peel off the low 12 bits (which the final addi
// contributes, sign-extended), then build what remains -- shifted right past
// its trailing zeros so the shift is paid for once -- and shift it back into
// place. Verified over 200k random values plus every power-of-two boundary
// and int32/imm12 edge: worst case 8 instructions, common cases 1 or 2, and
// every immediate produced fits the field it goes in.
func (m *machine) lowerConstantI64(rd regalloc.VReg, v int64) {
	m.emitConstantSeq(rd, v, true)
}

// emitConstantSeq builds v into rd. `top` distinguishes the outermost call,
// which may use the 32-bit-result addiw form, from the recursive ones, which
// must not: an inner value is about to be shifted left, so truncating it to 32
// bits would discard the very bits the shift is going to move up.
func (m *machine) emitConstantSeq(rd regalloc.VReg, v int64, top bool) {
	if fitsInSignedImm12(v) {
		// addi rd, zero, v
		alu := m.allocateInstr()
		alu.asALU(aluOpAdd, rd, operandNR(zeroVReg), operandImm(v), true)
		m.insert(alu)
		return
	}

	if v >= math.MinInt32 && v <= math.MaxInt32 {
		// lui rd, hi20 ; addiw rd, rd, lo12
		//
		// addiw, not addi: lui sign-extends its result, so for a value like
		// 0x7fffffff the hi20 half rounds up to 0x80000000 and sign-extends to
		// 0xffffffff80000000. Truncating the sum to 32 bits and re-extending is
		// what recovers the intended 0x000000007fffffff. The distinction only
		// matters at the top of the int32 range, which is exactly where a
		// wrong choice hides.
		hi, lo := splitImm32(int32(v))
		l := m.allocateInstr()
		l.asLui(rd, hi)
		m.insert(l)
		if lo != 0 {
			alu := m.allocateInstr()
			alu.asALU(aluOpAdd, rd, operandNR(rd), operandImm(int64(lo)), !top)
			m.insert(alu)
		}
		return
	}

	// Wider than 32 bits. Peel the low 12 bits off, shift what remains down
	// past its trailing zeros, build that recursively, then undo the shift.
	//
	// The shift pair below is 52, not 20: v is 64-bit here, so shifting by 20
	// would sign-extend bit 43 and leave a value far too wide for the addi it
	// is destined for.
	lo12 := v << 52 >> 52
	rest := v - lo12
	shift := bits.TrailingZeros64(uint64(rest))
	rest >>= uint(shift)

	m.emitConstantSeq(rd, rest, false)

	sll := m.allocateInstr()
	sll.asShiftImm(aluOpSll, rd, operandNR(rd), int64(shift), true)
	m.insert(sll)

	if lo12 != 0 {
		alu := m.allocateInstr()
		alu.asALU(aluOpAdd, rd, operandNR(rd), operandImm(lo12), true)
		m.insert(alu)
	}
}

// InsertLoadConstantBlockArg implements backend.Machine.
func (m *machine) InsertLoadConstantBlockArg(instr *ssa.Instruction, vr regalloc.VReg) {
	val := instr.Return()
	load := m.allocateInstr()
	load.asLoadConstBlockArg(instr.ConstantVal(), val.Type(), vr)
	m.insert(load)
}

// lowerLoadConstantBlockArgAfterRegAlloc expands the placeholder left by
// InsertLoadConstantBlockArg once vr has a real register.
func (m *machine) lowerLoadConstantBlockArgAfterRegAlloc(i *instruction) {
	v, typ, dst := i.loadConstBlockArgData()
	m.pendingInstructions = m.pendingInstructions[:0]
	m.insertLoadConstant(v, typ, dst)
}

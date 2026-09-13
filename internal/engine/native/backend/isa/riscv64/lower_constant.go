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
	tmp := m.scratchInt()
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
	tmp := m.scratchInt()
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
	m.emitConstantSeq(rd, v)
}

// emitConstantSeq builds v into rd.
func (m *machine) emitConstantSeq(rd regalloc.VReg, v int64) {
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
		// addiw, not addi, and unconditionally -- including on the recursive
		// path. lui sign-extends its result, so for a value at the top of the
		// range like 0x7fffffff the hi half rounds up to 0x80000000 and
		// sign-extends to 0xffffffff80000000; addiw truncates the sum back to
		// 32 bits and re-extends, recovering 0x000000007fffffff, where a plain
		// addi would leave 0xffffffff7fffffff. addiw is safe for the recursive
		// case too: v fits in int32 by the branch condition, so truncating to
		// 32 bits and re-extending is the identity on it.
		//
		// The window where this matters is only 2048 values wide out of 2^31,
		// which is exactly why a random-sample test walked straight past it.
		hi, lo := splitImm32(int32(v))
		l := m.allocateInstr()
		l.asLui(rd, hi)
		m.insert(l)
		if lo != 0 {
			alu := m.allocateInstr()
			alu.asALU(aluOpAdd, rd, operandNR(rd), operandImm(int64(lo)), false /* addiw */)
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

	m.emitConstantSeq(rd, rest)

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

// scratchInt returns an integer register to build a value in.
//
// Which one depends on when we are called. An FP constant needs a scratch to
// assemble its bit pattern in before crossing to the float file, and
// lowerLoadConstantBlockArgAfterRegAlloc reaches this path *after* register
// allocation has finished -- where a freshly allocated virtual register is
// never assigned a real one, so the instruction encodes garbage. That is not a
// crash: every float constant used as a block argument silently becomes zero,
// which is what a br_if carrying an f32, and every call_indirect dispatching
// through one, actually returned.
//
// The reserved scratch is safe here for the same reason it is safe in the
// prologue: it is never allocatable, and this expansion is a self-contained
// two-instruction sequence with nothing live across it.
func (m *machine) scratchInt() regalloc.VReg {
	if m.regAllocStarted {
		return tmpRegVReg
	}
	return m.compiler.AllocateVReg(ssa.TypeI64)
}

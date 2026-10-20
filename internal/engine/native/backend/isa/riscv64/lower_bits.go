package riscv64

import (
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// i32.clz, i32.ctz and i32.popcnt have no RV64G instruction. The Zbb extension
// provides clz/ctz/cpop, but it is optional and this backend does not require
// it, so each is built from the classic branch-free bit-twiddling sequences.
//
// These are the only wasm integer operators that cost more than a couple of
// instructions here, and the cost is real: popcount is 12 instructions where
// arm64 spends 3 and amd64 one. If Zbb detection is added later these become
// single instructions; the shape of the lowering does not otherwise change.

// lowerPopcnt emits the SWAR population count.
//
//	x -= (x >> 1) & 0x5555...      pairs
//	x  = (x & 0x3333...) + ((x >> 2) & 0x3333...)
//	x  = (x + (x >> 4)) & 0x0f0f...
//	x  = (x * 0x0101...) >> (bits - 8)
//
// The final multiply-and-shift sums the bytes in one step, which is why this
// is 12 instructions rather than the 20-odd a naive fold takes. RV64M gives us
// the multiply for free.
func (m *machine) lowerPopcnt(rn operand, rd regalloc.VReg, _64bit bool) {
	tmp := m.compiler.AllocateVReg(ssa.TypeI64)
	acc := m.compiler.AllocateVReg(ssa.TypeI64)

	src := rn
	if !_64bit {
		// The i32 form must not count the replicated sign bits.
		src = m.zeroExtend32(rn)
	}

	c1, c2, c3, c4 := uint64(0x5555555555555555), uint64(0x3333333333333333),
		uint64(0x0f0f0f0f0f0f0f0f), uint64(0x0101010101010101)
	shift := int64(56)
	if !_64bit {
		c1, c2, c3, c4 = 0x55555555, 0x33333333, 0x0f0f0f0f, 0x01010101
		shift = 24
	}

	k := m.compiler.AllocateVReg(ssa.TypeI64)

	// acc = src - ((src >> 1) & c1)
	m.emit(m.allocateInstr().asShiftImm(aluOpSrl, tmp, src, 1, true))
	m.lowerConstantI64(k, int64(c1))
	m.emit(m.allocateInstr().asALU(aluOpAnd, tmp, operandNR(tmp), operandNR(k), true))
	m.emit(m.allocateInstr().asALU(aluOpSub, acc, src, operandNR(tmp), true))

	// acc = (acc & c2) + ((acc >> 2) & c2)
	m.lowerConstantI64(k, int64(c2))
	m.emit(m.allocateInstr().asShiftImm(aluOpSrl, tmp, operandNR(acc), 2, true))
	m.emit(m.allocateInstr().asALU(aluOpAnd, tmp, operandNR(tmp), operandNR(k), true))
	m.emit(m.allocateInstr().asALU(aluOpAnd, acc, operandNR(acc), operandNR(k), true))
	m.emit(m.allocateInstr().asALU(aluOpAdd, acc, operandNR(acc), operandNR(tmp), true))

	// acc = (acc + (acc >> 4)) & c3
	m.emit(m.allocateInstr().asShiftImm(aluOpSrl, tmp, operandNR(acc), 4, true))
	m.emit(m.allocateInstr().asALU(aluOpAdd, acc, operandNR(acc), operandNR(tmp), true))
	m.lowerConstantI64(k, int64(c3))
	m.emit(m.allocateInstr().asALU(aluOpAnd, acc, operandNR(acc), operandNR(k), true))

	// rd = ((acc * c4) >> shift) & 0x7f
	//
	// The mask is not optional on RV64. The classic sequence assumes the
	// multiply is the same width as the value, so that the byte sums above the
	// one being extracted fall off the top; here `mul` is always 64-bit, so in
	// the 32-bit case shifting right by 24 leaves the partial sums from bytes
	// 4..7 sitting above the answer. Masking to 7 bits keeps only the count,
	// which cannot exceed 64. Without it, popcount(0xffffffff) returns
	// 135272480 rather than 32.
	m.lowerConstantI64(k, int64(c4))
	m.emit(m.allocateInstr().asALU(aluOpMul, acc, operandNR(acc), operandNR(k), true))
	m.emit(m.allocateInstr().asShiftImm(aluOpSrl, acc, operandNR(acc), shift, true))
	m.emit(m.allocateInstr().asALU(aluOpAnd, rd, operandNR(acc), operandImm(0x7f), true))
}

// lowerCtz counts trailing zeros as popcount(^x & (x-1)).
//
// The identity holds for zero too, where ^0 & -1 is all ones and the count is
// the full width -- which is what wasm's i32.ctz(0) = 32 requires, with no
// special case.
func (m *machine) lowerCtz(rn operand, rd regalloc.VReg, _64bit bool) {
	src := rn
	if !_64bit {
		src = m.zeroExtend32(rn)
	}
	t := m.compiler.AllocateVReg(ssa.TypeI64)
	dec := m.compiler.AllocateVReg(ssa.TypeI64)

	// t = ^x, dec = x - 1
	m.emit(m.allocateInstr().asALU(aluOpXor, t, src, operandImm(-1), true))
	m.emit(m.allocateInstr().asALU(aluOpAdd, dec, src, operandImm(-1), true))
	m.emit(m.allocateInstr().asALU(aluOpAnd, t, operandNR(t), operandNR(dec), true))

	if !_64bit {
		// The i32 form: x-1 for x==0 sets the whole upper half, which would
		// make popcount return 64 rather than 32. Mask it back to 32 bits.
		masked := m.zeroExtend32(operandNR(t))
		m.lowerPopcnt(masked, rd, true)
		return
	}
	m.lowerPopcnt(operandNR(t), rd, true)
}

// lowerClz counts leading zeros by smearing every set bit downwards -- so the
// value becomes all ones below its highest set bit -- and counting the zeros
// that remain.
//
// clz(0) is the full width here as well, since nothing is smeared and the
// complement is all ones, which is what wasm requires.
func (m *machine) lowerClz(rn operand, rd regalloc.VReg, _64bit bool) {
	src := rn
	width := int64(64)
	if !_64bit {
		src = m.zeroExtend32(rn)
		width = 32
	}
	x := m.compiler.AllocateVReg(ssa.TypeI64)
	t := m.compiler.AllocateVReg(ssa.TypeI64)
	m.emit(m.allocateInstr().asALU(aluOpAdd, x, src, operandImm(0), true))

	for sh := int64(1); sh < width; sh <<= 1 {
		m.emit(m.allocateInstr().asShiftImm(aluOpSrl, t, operandNR(x), sh, true))
		m.emit(m.allocateInstr().asALU(aluOpOr, x, operandNR(x), operandNR(t), true))
	}
	// rd = popcount(^x), counting the bits above the highest set one.
	m.emit(m.allocateInstr().asALU(aluOpXor, x, operandNR(x), operandImm(-1), true))
	if !_64bit {
		masked := m.zeroExtend32(operandNR(x))
		m.lowerPopcnt(masked, rd, true)
		return
	}
	m.lowerPopcnt(operandNR(x), rd, true)
}

// emit is a small convenience for the sequence builders above.
func (m *machine) emit(i *instruction) { m.insert(i) }

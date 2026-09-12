package riscv64

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// The threads proposal on RV64A.
//
// Two gaps in the baseline shape everything here. There is no sub-word AMO
// (that is Zabha) and no compare-exchange at any width (that is Zacas), so the
// byte and halfword read-modify-writes and every compare-exchange go through an
// LR/SC retry loop on the containing word. And the loop must be a single
// backend instruction rather than a few blocks, because the register allocator
// reasons in basic blocks and would not see a branch inside one.
//
// The loop needs three scratch registers, which is exactly what the backend
// reserves (tmpReg, tmpReg2, tmpReg3). Everything else the sequence needs --
// the word-aligned address and the shift that places the field inside it -- is
// computed here, ahead of register allocation, as ordinary instructions.

// atomicWordAndShift pulls apart an atomic instruction's address argument into the
// pair the sub-word sequences want: the containing word's address, and the bit
// position of the field within it.
//
// The frontend has already trapped a misaligned access, so a halfword's offset
// is 0 or 2 and a byte's is 0 to 3 -- the shift is always a whole number of
// field widths.
func (m *machine) atomicWordAndShift(addr operand) (word, shift operand) {
	aligned := m.compiler.AllocateVReg(ssa.TypeI64)
	m.insert(m.allocateInstr().asALU(aluOpAnd, aligned, addr, operandImm(-4), true))
	sh := m.compiler.AllocateVReg(ssa.TypeI64)
	m.insert(m.allocateInstr().asALU(aluOpAnd, sh, addr, operandImm(3), true))
	m.insert(m.allocateInstr().asALU(aluOpSll, sh, operandNR(sh), operandImm(3), true))
	return operandNR(aligned), operandNR(sh)
}

// keepAliveAcross extends the operands' live ranges past the instruction that
// was just inserted.
//
// The LR/SC sequences re-read their operands on every retry, so the result
// register must not be one of them -- and without this the allocator is free to
// reuse a dying operand's register for the definition, which is the ordinary
// and usually correct thing for it to do.
func (m *machine) keepAliveAcross(ops ...operand) {
	for _, op := range ops {
		m.insert(m.allocateInstr().asNopUseReg(op.nr()))
	}
}

// truncateTo narrows a value to the access width, zero-extended.
//
// wasm compares a cmpxchg's expected value wrapped to the width of the access,
// and the loaded field is zero-extended, so the two only match if the expected
// value is wrapped the same way.
func (m *machine) truncateTo(v operand, size uint64) operand {
	if size == 8 {
		return v
	}
	rd := m.compiler.AllocateVReg(ssa.TypeI64)
	shift := int64(64 - size*8)
	m.insert(m.allocateInstr().asALU(aluOpSll, rd, v, operandImm(shift), true))
	m.insert(m.allocateInstr().asALU(aluOpSrl, rd, operandNR(rd), operandImm(shift), true))
	return operandNR(rd)
}

func (m *machine) lowerAtomicRmw(instr *ssa.Instruction) {
	op, size := instr.AtomicRmwData()
	addrV, valV := instr.Arg2()
	addr := m.getOperand_NR(m.compiler.ValueDefinition(addrV))
	val := m.getOperand_NR(m.compiler.ValueDefinition(valV))
	rd := m.compiler.VRegOf(instr.Return())
	wide := instr.Return().Type().Bits() == 64

	if size < 4 {
		word, shift := m.atomicWordAndShift(addr)
		m.insert(m.allocateInstr().asAtomicRmwSeq(op, rd, word, val, shift, size))
		m.keepAliveAcross(word, val, shift)
		return
	}

	// RV64A has no atomic subtract, so it is an add of the negation. The
	// negation is a plain instruction: only the memory update has to be atomic.
	funct5 := uint32(amoFunctAdd)
	switch op {
	case ssa.AtomicRmwOpAdd:
	case ssa.AtomicRmwOpSub:
		neg := m.compiler.AllocateVReg(ssa.TypeI64)
		m.insert(m.allocateInstr().asALU(aluOpSub, neg, operandNR(zeroVReg), val, true))
		val = operandNR(neg)
	case ssa.AtomicRmwOpAnd:
		funct5 = amoFunctAnd
	case ssa.AtomicRmwOpOr:
		funct5 = amoFunctOr
	case ssa.AtomicRmwOpXor:
		funct5 = amoFunctXor
	case ssa.AtomicRmwOpXchg:
		funct5 = amoFunctSwap
	default:
		panic(fmt.Sprintf("BUG: unknown atomic rmw op %s", op))
	}
	m.insert(m.allocateInstr().asAtomicRmw(funct5, rd, addr, val, size, wide))
}

func (m *machine) lowerAtomicCas(instr *ssa.Instruction) {
	size := instr.AtomicTargetSize()
	addrV, expV, replV := instr.Arg3()
	addr := m.getOperand_NR(m.compiler.ValueDefinition(addrV))
	exp := m.getOperand_NR(m.compiler.ValueDefinition(expV))
	repl := m.getOperand_NR(m.compiler.ValueDefinition(replV))
	rd := m.compiler.VRegOf(instr.Return())
	wide := instr.Return().Type().Bits() == 64

	if size < 4 {
		word, shift := m.atomicWordAndShift(addr)
		exp = m.truncateTo(exp, size)
		m.insert(m.allocateInstr().asAtomicCasSeq(rd, word, exp, repl, shift, size))
		m.keepAliveAcross(word, exp, repl, shift)
		return
	}
	if size == 4 {
		// lr.w sign-extends what it loads, so the expected value has to be
		// sign-extended too -- which for an i32 it already is, and for an i64
		// (i64.atomic.rmw32.cmpxchg_u) it is not.
		if wide {
			exp = m.signExtend32(exp)
		}
	}
	m.insert(m.allocateInstr().asAtomicCas(rd, addr, exp, repl, size, wide))
	m.keepAliveAcross(addr, exp, repl)
}

func (m *machine) lowerAtomicLoad(instr *ssa.Instruction) {
	size := instr.AtomicTargetSize()
	addr := m.getOperand_NR(m.compiler.ValueDefinition(instr.Arg()))
	rd := m.compiler.VRegOf(instr.Return())
	m.insert(m.allocateInstr().asAtomicLoad(rd, addr, size, instr.Return().Type().Bits() == 64))
}

func (m *machine) lowerAtomicStore(instr *ssa.Instruction) {
	size := instr.AtomicTargetSize()
	addrV, valV := instr.Arg2()
	addr := m.getOperand_NR(m.compiler.ValueDefinition(addrV))
	val := m.getOperand_NR(m.compiler.ValueDefinition(valV))
	m.insert(m.allocateInstr().asAtomicStore(addr, val, size))
}

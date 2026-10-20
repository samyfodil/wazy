package riscv64

import (
	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// getOperand_NR returns the value as a register operand, materializing a
// constant inline rather than letting it occupy a register across its whole
// live range.
//
// Unlike arm64's equivalent there is no extension mode here: an i32 already
// lives sign-extended (see the note at the top of instr.go), so no widening is
// needed to feed it to a 64-bit instruction. What *is* sometimes needed is the
// opposite -- zero extension, for the operators that read an i32 as unsigned --
// and that is explicit at those sites via zeroExtend32.
func (m *machine) getOperand_NR(def backend.SSAValueDefinition) operand {
	if def.IsFromInstr() && def.Instr.Constant() {
		v := m.lowerConstant(def.Instr)
		def.Instr.MarkLowered()
		return operandNR(v)
	}
	return operandNR(m.compiler.VRegOf(def.V))
}

// getOperand_Imm12_NR is getOperand_NR, except that a constant small enough to
// ride along in an I-type instruction's 12-bit field is returned as an
// immediate rather than materialized into a register.
func (m *machine) getOperand_Imm12_NR(def backend.SSAValueDefinition) operand {
	if def.IsFromInstr() && def.Instr.Constant() {
		if v, ok := asImm12Constant(def.Instr); ok {
			def.Instr.MarkLowered()
			return operandImm(v)
		}
	}
	return m.getOperand_NR(def)
}

// asImm12Constant reports whether a constant instruction's value fits the
// signed 12-bit immediate field, in the width the value is typed at.
func asImm12Constant(instr *ssa.Instruction) (int64, bool) {
	c := instr.ConstantVal()
	var v int64
	switch instr.Return().Type() {
	case ssa.TypeI32:
		v = int64(int32(uint32(c)))
	case ssa.TypeI64:
		v = int64(c)
	default:
		return 0, false
	}
	if !fitsInSignedImm12(v) {
		return 0, false
	}
	return v, true
}

// lowerConstant materializes a constant instruction into a fresh register.
func (m *machine) lowerConstant(instr *ssa.Instruction) regalloc.VReg {
	val := instr.Return()
	vr := m.compiler.AllocateVReg(val.Type())
	m.insertLoadConstant(instr.ConstantVal(), val.Type(), vr)
	return vr
}

// zeroExtend32 returns a register holding the unsigned 32-bit value of rn.
//
// This is the mirror hazard of the sign-extended i32 representation: every
// consumer that reads an i32 as an unsigned quantity -- a memory or table
// index, a length, an unsigned widening -- must clear the replicated sign
// bits first, or a value like 0x80000000 subtracts 2GiB from a base address
// instead of adding it. RV64 has no single zero-extend-word instruction
// without Zba, so it is the canonical shift pair.
func (m *machine) zeroExtend32(rn operand) operand {
	dst := m.compiler.AllocateVReg(ssa.TypeI64)
	sll := m.allocateInstr()
	sll.asShiftImm(aluOpSll, dst, rn, 32, true)
	m.insert(sll)
	srl := m.allocateInstr()
	srl.asShiftImm(aluOpSrl, dst, operandNR(dst), 32, true)
	m.insert(srl)
	return operandNR(dst)
}

// signExtend32 re-establishes the i32 invariant on a value that may have
// stale high bits, using addiw's sign-extending 32-bit result.
func (m *machine) signExtend32(rn operand) operand {
	dst := m.compiler.AllocateVReg(ssa.TypeI64)
	i := m.allocateInstr()
	i.asALU(aluOpAdd, dst, rn, operandImm(0), false /* addiw */)
	m.insert(i)
	return operandNR(dst)
}

// addJmpTableTarget records a br_table's target list and returns its index.
func (m *machine) addJmpTableTarget(targets ssa.Values) uint32 {
	if m.jmpTableTargetsNext == len(m.jmpTableTargets) {
		m.jmpTableTargets = append(m.jmpTableTargets, make([]uint32, 0, len(targets.View())))
	}
	index := m.jmpTableTargetsNext
	m.jmpTableTargetsNext++
	m.jmpTableTargets[index] = m.jmpTableTargets[index][:0]
	for _, targetBlockID := range targets.View() {
		target := m.compiler.SSABuilder().BasicBlock(ssa.BasicBlockID(targetBlockID))
		m.jmpTableTargets[index] = append(m.jmpTableTargets[index], uint32(ssaBlockLabel(target)))
	}
	return uint32(index)
}

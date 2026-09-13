package riscv64

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// LowerSingleBranch implements backend.Machine.
func (m *machine) LowerSingleBranch(br *ssa.Instruction) {
	switch br.Opcode() {
	case ssa.OpcodeJump:
		_, _, targetBlkID := br.BranchData()
		if br.IsFallthroughJump() {
			return
		}
		b := m.allocateInstr()
		targetBlk := m.compiler.SSABuilder().BasicBlock(targetBlkID)
		if targetBlk.ReturnBlock() {
			b.asRet()
		} else {
			b.asBr(ssaBlockLabel(targetBlk))
		}
		m.insert(b)
	case ssa.OpcodeBrTable:
		m.lowerBrTable(br)
	default:
		panic("BUG: unexpected branch opcode " + br.Opcode().String())
	}
}

// lowerBrTable lowers br_table: clamp the index to the last entry (which the
// frontend places as the default target), then dispatch through the jump table.
func (m *machine) lowerBrTable(i *ssa.Instruction) {
	index, targetBlockIDs := i.BrTableData()
	targetBlockCount := len(targetBlockIDs.View())
	indexOperand := m.getOperand_NR(m.compiler.ValueDefinition(index))

	// The index is an i32 read as unsigned, so clear the replicated sign bits
	// before comparing or scaling it: a negative i32 would otherwise compare
	// below the bound and then index backwards out of the table.
	idx := m.zeroExtend32(indexOperand)

	maxIndex := m.compiler.AllocateVReg(ssa.TypeI64)
	m.lowerConstantI64(maxIndex, int64(targetBlockCount-1))

	// adjusted = idx < maxIndex ? idx : maxIndex, branch-free:
	//
	//	sltu  cond, idx, maxIndex     ; 1 when in range
	//	sub   cond, zero, cond        ; 0 or all-ones
	//	xor   diff, idx, maxIndex
	//	and   diff, diff, cond
	//	xor   adjusted, maxIndex, diff
	//
	// which selects idx when cond is all-ones and maxIndex when it is zero.
	// RISC-V has no conditional move without Zicond, and a branch here would
	// add an unpredictable edge to every br_table.
	cond := m.compiler.AllocateVReg(ssa.TypeI64)
	slt := m.allocateInstr()
	slt.asALU(aluOpSltu, cond, idx, operandNR(maxIndex), true)
	m.insert(slt)

	mask := m.allocateInstr()
	mask.asALU(aluOpSub, cond, operandNR(zeroVReg), operandNR(cond), true)
	m.insert(mask)

	diff := m.compiler.AllocateVReg(ssa.TypeI64)
	x1 := m.allocateInstr()
	x1.asALU(aluOpXor, diff, idx, operandNR(maxIndex), true)
	m.insert(x1)

	a1 := m.allocateInstr()
	a1.asALU(aluOpAnd, diff, operandNR(diff), operandNR(cond), true)
	m.insert(a1)

	adjusted := m.compiler.AllocateVReg(ssa.TypeI64)
	x2 := m.allocateInstr()
	x2.asALU(aluOpXor, adjusted, operandNR(maxIndex), operandNR(diff), true)
	m.insert(x2)

	brSequence := m.allocateInstr()
	tableIndex := m.addJmpTableTarget(targetBlockIDs)
	brSequence.asBrTableSequence(adjusted, tableIndex, targetBlockCount)
	m.insert(brSequence)
}

// LowerConditionalBranch implements backend.Machine.
//
// RISC-V's B-type instructions compare two registers and branch in one
// instruction, so a comparison feeding a branch never needs to be materialized
// into a value at all -- which is the common case by far, and why this fuses
// icmp directly rather than going through lowerIcmpToReg.
func (m *machine) LowerConditionalBranch(b *ssa.Instruction) {
	cval, args, targetBlkID := b.BranchData()
	if len(args) > 0 {
		panic(fmt.Sprintf(
			"conditional branch shouldn't have args; likely a bug in critical edge splitting: from %s to %s",
			m.currentLabelPos.sb, targetBlkID,
		))
	}

	target := ssaBlockLabel(m.compiler.SSABuilder().BasicBlock(targetBlkID))
	cvalDef := m.compiler.ValueDefinition(cval)
	brz := b.Opcode() == ssa.OpcodeBrz

	if m.compiler.MatchInstr(cvalDef, ssa.OpcodeIcmp) {
		cvalInstr := cvalDef.Instr
		x, y, c := cvalInstr.IcmpData()

		flag, swap := condFlagFromSSAIntegerCmpCond(c)
		if brz {
			// Branch when the comparison is false.
			flag = flag.invert()
		}
		rx := m.getOperand_NR(m.compiler.ValueDefinition(x))
		ry := m.getOperand_NR(m.compiler.ValueDefinition(y))
		if swap {
			rx, ry = ry, rx
		}
		cbr := m.allocateInstr()
		cbr.asCondBr(flag, rx, ry, target)
		m.insert(cbr)
		cvalInstr.MarkLowered()
		return
	}

	if m.compiler.MatchInstr(cvalDef, ssa.OpcodeFcmp) {
		// An FP comparison has to become a 0/1 in a register first -- there is
		// no FP branch -- and then the branch tests that against zero.
		cvalInstr := cvalDef.Instr
		x, y, c := cvalInstr.FcmpData()
		rd := m.compiler.AllocateVReg(ssa.TypeI64)
		m.lowerFcmpToReg(x, y, c, rd)
		flag := condNE
		if brz {
			flag = condEQ
		}
		cbr := m.allocateInstr()
		cbr.asCondBr(flag, operandNR(rd), operandNR(zeroVReg), target)
		m.insert(cbr)
		cvalInstr.MarkLowered()
		return
	}

	rn := m.getOperand_NR(cvalDef)
	flag := condNE
	if brz {
		flag = condEQ
	}
	cbr := m.allocateInstr()
	cbr.asCondBr(flag, rn, operandNR(zeroVReg), target)
	m.insert(cbr)
}

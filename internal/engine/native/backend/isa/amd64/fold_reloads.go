package amd64

import "github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"

// foldReloads turns a reload from a spill slot into a register that the next instruction reads once and
// nothing reads after, into that instruction reading the slot itself: `imul $k, 32(%rsp), %r15d` in place
// of `movl 32(%rsp), %r15d; imul $k, %r15d, %r15d`. Register allocation places a reload right before the
// use it is for.
func (m *machine) foldReloads() {
	for cur := m.rootInstr; cur != nil; cur = cur.next {
		width, ok := reloadWidth(cur)
		if !ok {
			continue
		}
		use := cur.next
		if use == nil {
			continue
		}
		r := cur.op2.reg().RealReg()
		mem := cur.op1
		if !m.foldReloadInto(use, r, mem, width) {
			continue
		}
		cur.prev.next, use.prev = use, cur.prev
		cur = use
	}
}

// reloadWidth reports whether instr loads a scalar from the stack into a register, and how many bytes.
func reloadWidth(instr *instruction) (byte, bool) {
	if instr.op1.kind != operandKindMem {
		return 0, false
	}
	if a := instr.op1.addressMode(); a.kind() != amodeImmReg || a.base != rspVReg {
		return 0, false
	}
	switch instr.kind {
	case movzxRmR:
		if extMode(instr.u1) == extModeLQ {
			return 4, true
		}
	case mov64MR:
		return 8, true
	case xmmUnaryRmR:
		switch sseOpcode(instr.u1) {
		case sseOpcodeMovss:
			return 4, true
		case sseOpcodeMovsd:
			return 8, true
		}
	}
	return 0, false
}

// foldReloadInto makes use read mem, width bytes, where it reads r, if it can and r is dead after it.
func (m *machine) foldReloadInto(use *instruction, r regalloc.RealReg, mem operand, width byte) bool {
	if use.op1.kind != operandKindReg || use.op1.reg().RealReg() != r {
		return false
	}
	dst := use.op2.reg().RealReg()
	switch use.kind {
	case aluRmiR, cmpRmiR:
		if dst == r || use.b1 && width < 8 {
			return false
		}
	case imulImm:
		if use.b1 && width < 8 {
			return false
		}
	case xmmRmR:
		if dst == r || scalarWidth(sseOpcode(use.u1)) != width {
			return false
		}
	case xmmRmRVex:
		if regalloc.VReg(use.u1).RealReg() == r || scalarWidth(sseOpcode(use.u2)) != width {
			return false
		}
	default:
		return false
	}
	if dst != r && !m.deadAfter(use, r) {
		return false
	}
	use.op1 = mem
	return true
}

func scalarWidth(op sseOpcode) byte {
	switch op {
	case sseOpcodeAddss, sseOpcodeSubss, sseOpcodeMulss, sseOpcodeDivss:
		return 4
	case sseOpcodeAddsd, sseOpcodeSubsd, sseOpcodeMulsd, sseOpcodeDivsd:
		return 8
	}
	return 0
}

// deadAfter reports whether the instructions after instr overwrite r before anything reads it, without
// leaving the block. It looks only through instructions whose Uses and Defs name every register they
// touch, and gives up at anything else, a label, a branch or a call say.
func (m *machine) deadAfter(instr *instruction, r regalloc.RealReg) bool {
	const lookahead = 24
	cur := instr.next
	for n := 0; n < lookahead && cur != nil; n, cur = n+1, cur.next {
		if !explicitOperands(cur.kind) {
			return false
		}
		for _, u := range cur.Uses(&m.foldRegs) {
			if u.RealReg() == r {
				return false
			}
		}
		for _, d := range cur.Defs(&m.foldRegs) {
			if d.RealReg() == r {
				return overwrites(cur)
			}
		}
	}
	return false
}

// explicitOperands reports whether every register an instruction of kind k reads or writes is in its
// Uses or Defs.
func explicitOperands(k instructionKind) bool {
	switch k {
	case aluRmiR, cmpRmiR, imulImm, imm, movRR, movzxRmR, movsxRmR, mov64MR, lea, movRM, xmmMovRM, shiftR,
		xmmRmR, xmmRmRVex, xmmUnaryRmR, xmmUnaryRmRImm, xmmRmRImm, xmmRmiReg, xmmCmpRmR, gprToXmm, xmmToGpr,
		cmove, zeros, sourceOffsetInfo:
		return true
	}
	return false
}

// overwrites reports whether instr, which defines a register, writes all of it, so nothing of what the
// register held before survives. A bsf whose source is zero leaves its destination as it was, and a
// scalar SSE operation keeps the upper lanes of its destination.
func overwrites(instr *instruction) bool {
	switch instr.kind {
	case imm, movRR, movzxRmR, movsxRmR, mov64MR, lea, imulImm, zeros, xmmToGpr, xmmRmRVex:
		return true
	case gprToXmm:
		op := sseOpcode(instr.u1)
		return op == sseOpcodeMovd || op == sseOpcodeMovq
	case xmmUnaryRmR:
		switch sseOpcode(instr.u1) {
		case sseOpcodeMovaps, sseOpcodeMovapd, sseOpcodeMovdqa, sseOpcodeMovdqu, sseOpcodeMovups:
			return true
		case sseOpcodeMovss, sseOpcodeMovsd:
			return instr.op1.kind == operandKindMem
		}
	}
	return false
}

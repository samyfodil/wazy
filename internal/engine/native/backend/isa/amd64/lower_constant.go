package amd64

import (
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// lowerConstant allocates a new VReg and inserts the instruction to load the constant value.
func (m *machine) lowerConstant(instr *ssa.Instruction) (vr regalloc.VReg) {
	val := instr.Return()
	valType := val.Type()

	vr = m.c.AllocateVReg(valType)
	m.insertLoadConstant(instr, vr)
	return
}

// InsertLoadConstantBlockArg implements backend.Machine.
func (m *machine) InsertLoadConstantBlockArg(instr *ssa.Instruction, vr regalloc.VReg) {
	m.insertLoadConstant(instr, vr)
}

func (m *machine) insertLoadConstant(instr *ssa.Instruction, vr regalloc.VReg) {
	val := instr.Return()
	valType := val.Type()
	v := instr.ConstantVal()

	bits := valType.Bits()
	if bits < 64 { // Clear the redundant bits just in case it's unexpectedly sign-extended, etc.
		v = v & ((1 << valType.Bits()) - 1)
	}

	switch valType {
	case ssa.TypeF32, ssa.TypeF64:
		m.lowerFconst(vr, v, bits == 64)
	case ssa.TypeI32, ssa.TypeI64:
		m.lowerIconst(vr, v, bits == 64)
	default:
		panic("BUG")
	}
}

// lowerFconst loads a nonzero scalar float constant from the function's constant pool: one load, where
// materializing it takes an immediate move to a general register and a move across.
func (m *machine) lowerFconst(dst regalloc.VReg, c uint64, _64 bool) {
	if c == 0 {
		xor := m.allocateInstr().asZeros(dst)
		m.insert(xor)
		return
	}
	op := sseOpcodeMovss
	if _64 {
		op = sseOpcodeMovsd
	}
	m.insert(m.allocateInstr().asXmmUnaryRmR(op, newOperandMem(m.newAmodeRipRel(m.fconstLabel(c))), dst))
}

// fconstLabel returns the constant pool slot holding c, adding one the first time c is asked for.
func (m *machine) fconstLabel(c uint64) label {
	for i := range m.consts {
		if vc := &m.consts[i]; vc._var == nil && vc.lo == c && vc.hi == 0 {
			return vc.label
		}
	}
	l, pos := m.allocateLabel()
	m.consts = append(m.consts, _const{label: l, labelPos: pos, lo: c})
	return l
}

func (m *machine) lowerIconst(dst regalloc.VReg, c uint64, _64 bool) {
	i := m.allocateInstr()
	if c == 0 {
		i.asZeros(dst)
	} else {
		i.asImm(dst, c, _64)
	}
	m.insert(i)
}

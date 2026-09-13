package riscv64

import (
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// lowerToAddressMode turns a pointer plus a constant byte offset into the one
// addressing mode RISC-V has: [base + imm12].
//
// arm64 spends a whole pass collecting addends so it can fold a shifted index
// and an extension into the instruction. There is nothing to fold into here --
// no register+register form, no scaled index, no extension -- so anything the
// 12-bit displacement cannot express becomes an explicit add, and the mode is
// always the same shape.
func (m *machine) lowerToAddressMode(ptr ssa.Value, offsetBase uint32) *addressMode {
	base := m.getOperand_NR(m.compiler.ValueDefinition(ptr)).nr()
	offset := int64(offsetBase)

	amode := m.amodePool.Allocate()
	if fitsInSignedImm12(offset) {
		*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: base, imm: offset}
		return amode
	}

	// Fold the offset into a fresh register. A virtual one, not the reserved
	// scratch: this runs before register allocation, and the address stays
	// live across whatever the allocator decides to put in between.
	sum := m.compiler.AllocateVReg(ssa.TypeI64)
	m.lowerConstantI64(sum, offset)
	add := m.allocateInstr()
	add.asALU(aluOpAdd, sum, operandNR(sum), operandNR(base), true)
	m.insert(add)
	*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: sum, imm: 0}
	return amode
}

// lowerLoad lowers a full-width load of the value's own type.
func (m *machine) lowerLoad(ptr ssa.Value, offset uint32, typ ssa.Type, ret ssa.Value) {
	amode := m.lowerToAddressMode(ptr, offset)
	dst := m.compiler.VRegOf(ret)
	load := m.allocateInstr()
	switch typ {
	case ssa.TypeI32:
		// lw, i.e. signed: an i32 in a register is kept sign-extended, and
		// lwu would leave a value that compares wrong against every other i32.
		load.asLoad(dst, amode, 32, true)
	case ssa.TypeI64:
		load.asLoad(dst, amode, 64, false)
	case ssa.TypeF32, ssa.TypeF64:
		// flw, not fld, for an f32: the single-precision form is what produces
		// the NaN boxing RV64D requires of a value in an f-register.
		load.asFpuLoad(dst, amode, typ.Bits())
	case ssa.TypeV128:
		load.asVecLoad(dst, amode)
	default:
		panic("BUG: unsupported load type on riscv64: " + typ.String())
	}
	m.insert(load)
}

// lowerExtLoad lowers the narrow loads: 8, 16 or 32 bits widened to the
// destination's width, signed or not.
func (m *machine) lowerExtLoad(ptr ssa.Value, offset uint32, bits byte, signed bool, ret regalloc.VReg) {
	amode := m.lowerToAddressMode(ptr, offset)
	load := m.allocateInstr()
	load.asLoad(ret, amode, bits, signed)
	m.insert(load)
}

// lowerStore lowers a store of any width.
func (m *machine) lowerStore(si *ssa.Instruction) {
	value, ptr, offset, storeSizeInBits := si.StoreData()
	amode := m.lowerToAddressMode(ptr, offset)
	src := m.getOperand_NR(m.compiler.ValueDefinition(value))
	store := m.allocateInstr()
	if value.Type() == ssa.TypeV128 {
		store.asVecStore(src.nr(), amode)
	} else {
		store.asStore(src.nr(), amode, storeSizeInBits, value.Type().IsInt())
	}
	m.insert(store)
}

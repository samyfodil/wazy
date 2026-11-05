package amd64

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/platform"
)

// An OpcodeVecMap expands in place into a loop over its elements. Its operands arrive in fixed registers:
// the pointers in vecMapPtrRegs, the element count in rcx, the invariants in xmm0 and up, which is also
// where the expansion broadcasts them, and the rest of xmm0-xmm7 hold its temporaries. All of those are
// caller-saved, so the register allocator, which sees a call here, keeps nothing in them across it. The
// expansion advances the pointers and counts rcx down, and on AVX ends with vzeroupper, so code after it,
// and Go code it may lead to, pays no penalty for the dirty upper halves.
var vecMapPtrRegs = [...]regalloc.RealReg{rax, rbx, rsi, rdi, r8, r9, r10, r11}

func (m *machine) lowerVecMap(instr *ssa.Instruction) {
	n, args := instr.VecMapData()
	prog := m.c.SSABuilder().VecProgram(n)
	if prog.Ptrs > len(vecMapPtrRegs) || prog.Invs > ssa.VecMapRegs {
		panic("BUG: vector map program does not fit its registers")
	}
	var mask uint64
	for i := 0; i < prog.Ptrs; i++ {
		r := vecMapPtrRegs[i]
		m.InsertMove(regInfo.RealRegToVReg[r], m.vecMapOperand(args[i]), ssa.TypeI64)
		mask |= 1 << r
	}
	m.InsertMove(rcxVReg, m.vecMapOperand(args[prog.Ptrs]), ssa.TypeI64)
	mask |= 1 << rcx
	typ := ssa.TypeF32
	if prog.F64 {
		typ = ssa.TypeF64
	}
	for i := 0; i < prog.Invs; i++ {
		r := xmm0 + regalloc.RealReg(i)
		m.InsertMove(regInfo.RealRegToVReg[r], m.vecMapOperand(args[prog.Ptrs+1+i]), typ)
		mask |= 1 << r
	}
	vm := m.allocateInstr()
	vm.kind = vecMap
	vm.u2 = mask
	vm.b1 = m.cpuFeatures.Has(platform.CpuFeatureAmd64AVX)
	vm.vecProg = prog
	m.insert(vm)
}

// vecMapOperand returns the register holding v, materializing a constant, which is otherwise lowered
// only where it is used.
func (m *machine) vecMapOperand(v ssa.Value) regalloc.VReg {
	op := m.getOperand_Reg(m.c.ValueDefinition(v))
	return op.reg()
}

// vecMapUses appends the registers a vecMap instruction reads.
func (i *instruction) vecMapUses(regs *[]regalloc.VReg) {
	for r := regalloc.RealReg(0); r < 64; r++ {
		if i.u2&(1<<r) != 0 {
			*regs = append(*regs, regInfo.RealRegToVReg[r])
		}
	}
}

func (i *instruction) vecMapString() string {
	isa := "sse"
	if i.b1 {
		isa = "avx"
	}
	return fmt.Sprintf("vecmap.%s %d ops", isa, len(i.vecProg.Ops))
}

// encodeVecMap emits the expansion.
func (i *instruction) encodeVecMap(c backend.Compiler) {
	p := i.vecProg
	avx := i.b1
	regs, ok := p.Assign()
	if !ok {
		panic("BUG: vector map program does not fit its registers")
	}
	e := 4
	if p.F64 {
		e = 8
	}
	width := 16
	if avx {
		width = 32
	}
	lanes := width / e
	// pd forms carry the 66 prefix (pp=1), ps forms none.
	pp := byte(0)
	if p.F64 {
		pp = 1
	}

	// Broadcast each invariant across its register.
	for k := 0; k < p.Invs; k++ {
		x := byte(k)
		if p.F64 {
			// movddup x, x: F2 0F 12 /r
			if avx {
				vex(c, x, x, 0, 1, false, 3, 0x12)
			} else {
				c.EmitByte(0xf2)
				c.EmitByte(0x0f)
				c.EmitByte(0x12)
				c.EmitByte(modRMRegReg(x, x))
			}
		} else {
			// shufps x, x, 0: 0F C6 /r ib
			if avx {
				vex(c, x, x, x, 1, false, 0, 0xc6)
			} else {
				c.EmitByte(0x0f)
				c.EmitByte(0xc6)
				c.EmitByte(modRMRegReg(x, x))
			}
			c.EmitByte(0)
		}
		if avx {
			// vinsertf128 ymm_x, ymm_x, xmm_x, 1: VEX.256.66.0F3A 18 /r ib
			vex(c, x, x, x, 3, true, 1, 0x18)
			c.EmitByte(1)
		}
	}

	loopStart := len(*c.BufPtr())
	for idx, op := range p.Ops {
		r := regs[idx]
		switch op.Kind {
		case ssa.VecOpInv:
		case ssa.VecOpLoad:
			base := vecMapPtrRegs[op.Arg]
			// movupd/movups r, [base]: (66) 0F 10 /r
			if avx {
				vexMem(c, r, base, pp, 0x10)
			} else {
				legacyMem(c, r, base, pp, 0x10)
			}
		case ssa.VecOpAdd, ssa.VecOpSub, ssa.VecOpMul, ssa.VecOpDiv:
			opc := map[uint8]byte{ssa.VecOpAdd: 0x58, ssa.VecOpSub: 0x5c, ssa.VecOpMul: 0x59, ssa.VecOpDiv: 0x5e}[op.Kind]
			vecBinary(c, avx, pp, opc, r, regs[op.A], regs[op.B])
		case ssa.VecOpXor, ssa.VecOpAnd:
			opc := byte(0x57)
			if op.Kind == ssa.VecOpAnd {
				opc = 0x54
			}
			vecBinary(c, avx, pp, opc, r, regs[op.A], op.Arg)
		case ssa.VecOpSqrt:
			// sqrtpd/sqrtps r, a: (66) 0F 51 /r
			if avx {
				vex(c, r, regs[op.A], 0, 1, true, pp, 0x51)
			} else {
				legacyRegReg(c, r, regs[op.A], pp, 0x51)
			}
		}
	}
	// movupd/movups [store], value: (66) 0F 11 /r
	value := regs[len(p.Ops)-1]
	if avx {
		vexMem(c, value, vecMapPtrRegs[p.Store], pp, 0x11)
	} else {
		legacyMem(c, value, vecMapPtrRegs[p.Store], pp, 0x11)
	}
	for k := 0; k < p.Ptrs; k++ {
		// add reg, width: REX.W 83 /0 ib
		emitRexW(c, vecMapPtrRegs[k])
		c.EmitByte(0x83)
		c.EmitByte(0xc0 | regEncodings[vecMapPtrRegs[k]].encoding())
		c.EmitByte(byte(width))
	}
	// sub rcx, lanes: REX.W 83 /5 ib
	c.EmitByte(0x48)
	c.EmitByte(0x83)
	c.EmitByte(0xe9)
	c.EmitByte(byte(lanes))
	// jnz loopStart
	rel := loopStart - (len(*c.BufPtr()) + 2)
	if rel >= -128 {
		c.EmitByte(0x75)
		c.EmitByte(byte(int8(rel)))
	} else {
		rel = loopStart - (len(*c.BufPtr()) + 6)
		c.EmitByte(0x0f)
		c.EmitByte(0x85)
		c.Emit4Bytes(uint32(int32(rel)))
	}
	if avx {
		// vzeroupper: C5 F8 77
		c.EmitByte(0xc5)
		c.EmitByte(0xf8)
		c.EmitByte(0x77)
	}
}

func modRMRegReg(reg, rm byte) byte { return 0xc0 | (reg&7)<<3 | rm&7 }

func emitRexW(c backend.Compiler, r regalloc.RealReg) {
	rex := byte(0x48)
	if regEncodings[r].rexBit() > 0 {
		rex |= 1
	}
	c.EmitByte(rex)
}

// vex emits a three-byte VEX prefix, the opcode and a register-register ModRM: dst in ModRM.reg, src in
// ModRM.rm, src1 in VEX.vvvv (0 when the instruction has no such operand, which encodes as 1111). m is the
// opcode map (1 = 0F, 3 = 0F3A), pp the implied prefix (1 = 66, 3 = F2).
func vex(c backend.Compiler, dst, src, src1 byte, m byte, l256 bool, pp byte, opcode byte) {
	b1 := byte(0xe0) | m // R̄, X̄, B̄ set: no extension
	if dst&8 != 0 {
		b1 &^= 0x80
	}
	if src&8 != 0 {
		b1 &^= 0x20
	}
	b2 := (^src1&0xf)<<3 | pp
	if l256 {
		b2 |= 4
	}
	c.EmitByte(0xc4)
	c.EmitByte(b1)
	c.EmitByte(b2)
	c.EmitByte(opcode)
	c.EmitByte(modRMRegReg(dst, src))
}

// vexMem emits VEX.256.pp.0F opcode with ModRM addressing [base], vvvv unused.
func vexMem(c backend.Compiler, reg byte, base regalloc.RealReg, pp byte, opcode byte) {
	enc := regEncodings[base]
	b1 := byte(0xe0) | 1
	if reg&8 != 0 {
		b1 &^= 0x80
	}
	if enc.rexBit() > 0 {
		b1 &^= 0x20
	}
	b2 := byte(0xf<<3) | 4 | pp // vvvv = 1111: unused; L = 256
	c.EmitByte(0xc4)
	c.EmitByte(b1)
	c.EmitByte(b2)
	c.EmitByte(opcode)
	c.EmitByte((reg&7)<<3 | enc.encoding())
}

// legacyMem emits (66) 0F opcode with ModRM addressing [base].
func legacyMem(c backend.Compiler, reg byte, base regalloc.RealReg, pp byte, opcode byte) {
	enc := regEncodings[base]
	if pp == 1 {
		c.EmitByte(0x66)
	}
	if rex := byte(0x40); reg&8 != 0 || enc.rexBit() > 0 {
		if reg&8 != 0 {
			rex |= 4
		}
		if enc.rexBit() > 0 {
			rex |= 1
		}
		c.EmitByte(rex)
	}
	c.EmitByte(0x0f)
	c.EmitByte(opcode)
	c.EmitByte((reg&7)<<3 | enc.encoding())
}

// legacyRegReg emits (66) 0F opcode with a register-register ModRM; the registers are below xmm8.
func legacyRegReg(c backend.Compiler, reg, rm byte, pp byte, opcode byte) {
	if pp == 1 {
		c.EmitByte(0x66)
	}
	c.EmitByte(0x0f)
	c.EmitByte(opcode)
	c.EmitByte(modRMRegReg(reg, rm))
}

// vecBinary emits dst = a op b: one three-operand instruction on AVX, a copy of a into dst when they
// differ and the two-operand instruction otherwise. Assign never gives dst b's register, unless a and b
// are the same op.
func vecBinary(c backend.Compiler, avx bool, pp, opcode, dst, a, b byte) {
	if avx {
		vex(c, dst, b, a, 1, true, pp, opcode)
		return
	}
	if dst != a {
		// movapd/movaps dst, a: (66) 0F 28 /r
		legacyRegReg(c, dst, a, pp, 0x28)
	}
	legacyRegReg(c, dst, b, pp, opcode)
}

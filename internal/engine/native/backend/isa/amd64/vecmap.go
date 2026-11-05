package amd64

import (
	"fmt"
	"math/bits"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/platform"
)

// An OpcodeVecMap expands in place into a loop over its elements. Its operands arrive in fixed registers:
// the pointers in vecMapPtrRegs, the element count in rcx, the invariants in xmm0 and up, which is also
// where the expansion broadcasts them, and the next of xmm0-xmm15 hold its accumulators and temporaries.
// The instruction uses every register it overwrites, so the register allocator keeps nothing it needs
// afterwards in them, and nothing else; it defines those of xmm8-xmm15 it overwrites first, so the
// prologue saves them. The expansion advances the pointers and counts rcx down, leaves its first total in
// rcx and the others in the pointer registers past its pointers, and after 256-bit instructions ends with
// vzeroupper, so code after it, and Go code it may lead to, pays no penalty for the dirty upper halves.
var vecMapPtrRegs = [...]regalloc.RealReg{rax, rbx, rsi, rdi, r8, r9, r10, r11}

// vecMapResultReg returns the register total k of p is left in.
func vecMapResultReg(p *ssa.VecProgram, k int) regalloc.RealReg {
	if k == 0 {
		return rcx
	}
	return vecMapPtrRegs[p.Ptrs+k-1]
}

func (m *machine) lowerVecMap(instr *ssa.Instruction) {
	n, args := instr.VecMapData()
	prog := m.c.SSABuilder().VecProgram(n)
	if prog.Ptrs > len(vecMapPtrRegs) || prog.Invs > ssa.VecMapRegs || prog.Ptrs+len(prog.Reduces)-1 > len(vecMapPtrRegs) {
		panic("BUG: vector map program does not fit its registers")
	}
	for _, red := range prog.Reduces {
		if red.Widen {
			panic("BUG: a widening total is for arm64's uadalp only")
		}
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
	if prog.Elem.Size() == 8 {
		typ = ssa.TypeF64
	}
	for i := 0; i < prog.Invs; i++ {
		r := xmm0 + regalloc.RealReg(i)
		m.InsertMove(regInfo.RealRegToVReg[r], m.vecMapOperand(args[prog.Ptrs+1+i]), typ)
		mask |= 1 << r
	}
	// The temporaries, in whichever assignment the encoding ends up with.
	for _, fold := range []bool{false, true} {
		regs, ok := prog.Assign(fold)
		if !ok {
			continue
		}
		for k, r := range regs {
			if !(fold && prog.Folded(k)) {
				mask |= 1 << (xmm0 + regalloc.RealReg(r))
			}
		}
	}
	for k := range prog.Reduces {
		mask |= 1<<(xmm0+regalloc.RealReg(prog.Invs+k)) | 1<<vecMapResultReg(prog, k)
	}
	if prog.NeedsScratch() {
		mask |= 1 << (xmm0 + regalloc.RealReg(prog.Scratch()))
	}
	for r := max(xmm8, xmm0+regalloc.RealReg(prog.Invs)); r <= xmm15; r++ {
		if mask&(1<<r) != 0 {
			m.insert(m.allocateInstr().asDefineUninitializedReg(regInfo.RealRegToVReg[r]))
		}
	}
	vm := m.allocateInstr()
	vm.kind = vecMap
	vm.u2 = mask
	// 256-bit vectors: AVX has them for floats, AVX2 for integers.
	if prog.Elem.IsInt() {
		vm.b1 = m.cpuFeatures.Has(platform.CpuFeatureAmd64AVX2)
	} else {
		vm.b1 = m.cpuFeatures.Has(platform.CpuFeatureAmd64AVX)
	}
	vm.vecProg = prog
	m.insert(vm)
	first, rest := instr.Returns()
	for k := range prog.Reduces {
		v := first
		if k > 0 {
			v = rest[k-1]
		}
		m.InsertMove(m.c.VRegOf(v), regInfo.RealRegToVReg[vecMapResultReg(prog, k)], prog.ReduceType(k))
	}
}

// vecMapOperand returns the register holding v, materializing a constant, which is otherwise lowered
// only where it is used.
func (m *machine) vecMapOperand(v ssa.Value) regalloc.VReg {
	op := m.getOperand_Reg(m.c.ValueDefinition(v))
	return op.reg()
}

// vecMapUses appends the registers a vecMap instruction reads or overwrites.
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

// vecEnc is how one VecOp kind encodes for one lane type: the opcode map (1 = 0F, 2 = 0F38), the implied
// prefix (0 none, 1 = 66), the opcode, and for a shift by an immediate the ModRM.reg extension.
type vecEnc struct {
	m, pp, opcode, ext byte
}

// vecEncOf returns how an op of kind encodes in a program on lanes of elem, an integer one on lanes as
// wide as its value, w bytes.
func vecEncOf(kind, w uint8, elem ssa.Type) vecEnc {
	if !elem.IsInt() {
		pp := byte(0)
		if elem == ssa.TypeF64 {
			pp = 1
		}
		opcode := map[uint8]byte{
			ssa.VecOpAdd: 0x58, ssa.VecOpSub: 0x5c, ssa.VecOpMul: 0x59, ssa.VecOpDiv: 0x5e,
			ssa.VecOpXor: 0x57, ssa.VecOpAnd: 0x54, ssa.VecOpSqrt: 0x51,
		}[kind]
		return vecEnc{m: 1, pp: pp, opcode: opcode}
	}
	q := w == 8
	switch kind {
	case ssa.VecOpAdd:
		if q {
			return vecEnc{1, 1, 0xd4, 0}
		}
		return vecEnc{1, 1, 0xfe, 0}
	case ssa.VecOpSub:
		if q {
			return vecEnc{1, 1, 0xfb, 0}
		}
		return vecEnc{1, 1, 0xfa, 0}
	case ssa.VecOpMul: // pmulld; there is no 64-bit lane multiply before AVX-512.
		return vecEnc{2, 1, 0x40, 0}
	case ssa.VecOpMulU: // pmuludq
		return vecEnc{1, 1, 0xf4, 0}
	case ssa.VecOpMinS: // pminsd
		return vecEnc{2, 1, 0x39, 0}
	case ssa.VecOpMinU: // pminud
		return vecEnc{2, 1, 0x3b, 0}
	case ssa.VecOpMaxS: // pmaxsd
		return vecEnc{2, 1, 0x3d, 0}
	case ssa.VecOpMaxU: // pmaxud
		return vecEnc{2, 1, 0x3f, 0}
	case ssa.VecOpBand:
		return vecEnc{1, 1, 0xdb, 0}
	case ssa.VecOpBor:
		return vecEnc{1, 1, 0xeb, 0}
	case ssa.VecOpBxor:
		return vecEnc{1, 1, 0xef, 0}
	}
	opcode := byte(0x72)
	if q {
		opcode = 0x73
	}
	ext := map[uint8]byte{ssa.VecOpShl: 6, ssa.VecOpShrU: 2, ssa.VecOpShrS: 4}[kind]
	return vecEnc{1, 1, opcode, ext}
}

// Shifts of a whole vector by an immediate, 66 0F 73 /ext ib: pslldq by bytes, and psllq and psrlq.
var (
	vecEncPslldq = vecEnc{1, 1, 0x73, 7}
	vecEncPsllq  = vecEnc{1, 1, 0x73, 6}
	vecEncPsrlq  = vecEnc{1, 1, 0x73, 2}
	vecEncPslld  = vecEnc{1, 1, 0x72, 6}
	vecEncPaddd  = vecEnc{1, 1, 0xfe, 0}
)

// encodeVecMap emits the expansion.
func (i *instruction) encodeVecMap(c backend.Compiler) {
	p := i.vecProg
	avx := i.b1
	fold := avx // Legacy SSE would need its memory operands aligned.
	regs, ok := p.Assign(fold)
	if !ok {
		fold = false
		if regs, ok = p.Assign(false); !ok {
			panic("BUG: vector map program does not fit its registers")
		}
	}
	width := 16
	if avx {
		width = 32
	}
	e := p.Elem.Size()
	isInt := p.Elem.IsInt()
	// Loads and stores: movups/movupd for floats, movdqu for integers.
	movPP, movLoad, movStore := byte(0), byte(0x10), byte(0x11)
	if isInt {
		movPP, movLoad, movStore = 2, 0x6f, 0x7f
	} else if e == 8 {
		movPP = 1
	}

	// Broadcast each invariant across its register.
	for k := 0; k < p.Invs; k++ {
		x := byte(k)
		if e == 8 {
			// movddup x, x: F2 0F 12 /r
			if avx {
				vex(c, x, x, 0, 1, false, 3, 0x12)
			} else {
				legacyRR(c, 3, 1, 0x12, x, x)
			}
		} else {
			// shufps x, x, 0: 0F C6 /r ib
			if avx {
				vex(c, x, x, x, 1, false, 0, 0xc6)
			} else {
				legacyRR(c, 0, 1, 0xc6, x, x)
			}
			c.EmitByte(0)
		}
		if avx {
			// vinsertf128 ymm_x, ymm_x, xmm_x, 1: VEX.256.66.0F3A 18 /r ib
			vex(c, x, x, x, 3, true, 1, 0x18)
			c.EmitByte(1)
		}
	}
	// An iota's register starts at the value of its invariant in every lane; lane j gets j steps more,
	// added within each 128 bits from the step shifted a lane's width at a time, and onto the upper 128
	// bits of a 256-bit vector as many steps as the lower holds lanes. Its step's register then holds the
	// steps of a whole vector, which each copy of the program adds after it.
	scratch := p.Scratch()
	lanes := width / int(e)
	for _, op := range p.Ops {
		if op.Kind != ssa.VecOpIota {
			continue
		}
		iota, step := op.Arg, op.B
		for sh := e; sh < 16; sh += e {
			vecShift(c, avx, vecEncPslldq, scratch, step, byte(sh))
			vecBinary(c, avx, vecEncPaddd, iota, iota, scratch)
		}
		if avx {
			vecShift(c, avx, vecEncPslld, scratch, step, byte(bits.TrailingZeros(uint(lanes/2))))
			// vperm2i128 scratch, scratch, scratch, 0x08: VEX.256.66.0F3A 46 /r ib, the lower 128 bits zero
			// and the upper the lower's.
			vex(c, scratch, scratch, scratch, 3, true, 1, 0x46)
			c.EmitByte(0x08)
			vecBinary(c, avx, vecEncPaddd, iota, iota, scratch)
		}
		vecShift(c, avx, vecEncPslld, step, step, byte(bits.TrailingZeros(uint(lanes))))
	}
	// Each accumulator starts at the identity: all ones for and, zero otherwise.
	for k, red := range p.Reduces {
		acc := byte(p.Invs + k)
		opcode := byte(0xef) // pxor
		if red.Op == ssa.VecOpBand {
			opcode = 0x76 // pcmpeqd
		}
		if avx {
			vex(c, acc, acc, acc, 1, true, 1, opcode)
		} else {
			legacyRR(c, 1, 1, opcode, acc, acc)
		}
	}

	// rcx counts elements; turn it into a byte index running from -bytes up to zero, with every pointer
	// at its stream's end, so the loop addresses [ptr + rcx] and only rcx moves.
	shift := byte(2)
	if e == 8 {
		shift = 3
	}
	// shl rcx, shift: REX.W C1 /4 ib
	c.EmitByte(0x48)
	c.EmitByte(0xc1)
	c.EmitByte(0xe1)
	c.EmitByte(shift)
	for k := 0; k < p.Ptrs; k++ {
		// add ptr, rcx: REX.W 01 /r, rcx in ModRM.reg
		enc := regEncodings[vecMapPtrRegs[k]]
		rex := byte(0x48)
		if enc.rexBit() > 0 {
			rex |= 1
		}
		c.EmitByte(rex)
		c.EmitByte(0x01)
		c.EmitByte(0xc0 | 1<<3 | enc.encoding())
	}
	// neg rcx: REX.W F7 /3
	c.EmitByte(0x48)
	c.EmitByte(0xf7)
	c.EmitByte(0xd9)

	last := len(p.Ops) - 1
	// A reduction of a load alone reads it straight into the accumulating instruction.
	foldRoot := fold && len(p.Reduces) == 1 && int(p.Reduces[0].Root) == last && p.Ops[last].Kind == ssa.VecOpLoad
	loopStart := len(*c.BufPtr())
	// Two vectors' worth per iteration, the second width bytes further on.
	for copyN := 0; copyN < 2; copyN++ {
		disp := byte(copyN * width)
		for idx, op := range p.Ops {
			r := regs[idx]
			switch op.Kind {
			case ssa.VecOpInv, ssa.VecOpIota:
			case ssa.VecOpLoad:
				if fold && p.Folded(idx) || foldRoot && idx == last {
					continue
				}
				vecMem(c, avx, r, 0, vecMapPtrRegs[op.Arg], disp, 1, movPP, movLoad)
			case ssa.VecOpSqrt:
				enc := vecEncOf(op.Kind, op.W, p.Elem)
				if avx {
					vex(c, r, regs[op.A], 0, 1, true, enc.pp, enc.opcode)
				} else {
					legacyRR(c, enc.pp, 1, enc.opcode, r, regs[op.A])
				}
			case ssa.VecOpShl, ssa.VecOpShrU, ssa.VecOpShrS:
				vecShift(c, avx, vecEncOf(op.Kind, op.W, p.Elem), r, regs[op.A], op.Arg)
			case ssa.VecOpZext:
				vecShift(c, avx, vecEncPsllq, r, regs[op.A], 32)
				vecShift(c, avx, vecEncPsrlq, r, r, 32)
			case ssa.VecOpXor, ssa.VecOpAnd:
				enc := vecEncOf(op.Kind, op.W, p.Elem)
				vecBinary(c, avx, enc, r, regs[op.A], op.Arg)
			default:
				enc := vecEncOf(op.Kind, op.W, p.Elem)
				if fold && p.Folded(int(op.B)) {
					vecMem(c, true, r, regs[op.A], vecMapPtrRegs[p.Ops[op.B].Arg], disp, enc.m, enc.pp, enc.opcode)
				} else {
					vecBinary(c, avx, enc, r, regs[op.A], regs[op.B])
				}
			}
		}
		if p.Store >= 0 {
			vecMem(c, avx, regs[last], 0, vecMapPtrRegs[p.Store], disp, 1, movPP, movStore)
		}
		for k, red := range p.Reduces {
			acc := byte(p.Invs + k)
			enc := vecEncOf(red.Op, p.Ops[red.Root].W, p.Elem)
			if foldRoot {
				vecMem(c, true, acc, acc, vecMapPtrRegs[p.Ops[last].Arg], disp, enc.m, enc.pp, enc.opcode)
			} else {
				vecBinary(c, avx, enc, acc, acc, regs[red.Root])
			}
		}
		for _, op := range p.Ops {
			if op.Kind == ssa.VecOpIota {
				vecBinary(c, avx, vecEncPaddd, op.Arg, op.Arg, op.B)
			}
		}
	}
	// add rcx, 2*width: REX.W 83 /0 ib
	c.EmitByte(0x48)
	c.EmitByte(0x83)
	c.EmitByte(0xc1)
	c.EmitByte(byte(2 * width))
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

	// Fold each accumulator's lanes into its lowest and move that to its total's register: the upper 128
	// bits onto the lower, the upper half of those onto the lower, and for packed 32-bit lanes the upper
	// lane of that onto the lowest.
	t := scratch
	if avx {
		for k, red := range p.Reduces {
			acc := byte(p.Invs + k)
			enc := vecEncOf(red.Op, p.Ops[red.Root].W, p.Elem)
			// vextractf128 xmm_t, ymm_acc, 1: VEX.256.66.0F3A 19 /r ib, the source in ModRM.reg.
			vex(c, acc, t, 0, 3, true, 1, 0x19)
			c.EmitByte(1)
			vex(c, acc, t, acc, 1, false, 1, enc.opcode)
		}
		vzeroupper(c)
	}
	imms := []byte{0x4e}
	if e == 4 {
		imms = append(imms, 0xb1)
	}
	for k, red := range p.Reduces {
		acc := byte(p.Invs + k)
		enc := vecEncOf(red.Op, p.Ops[red.Root].W, p.Elem)
		// pshufd t, acc, imm: 66 0F 70 /r ib, then acc op= t.
		for _, imm := range imms {
			legacyRR(c, 1, 1, 0x70, t, acc)
			c.EmitByte(imm)
			legacyRR(c, 1, enc.m, enc.opcode, acc, t)
		}
		// movd/movq dst, acc: 66 REX 0F 7E /r, the vector register in ModRM.reg.
		dst := regEncodings[vecMapResultReg(p, k)]
		c.EmitByte(0x66)
		rex := byte(0x40)
		if p.ReduceType(k) == ssa.TypeI64 {
			rex |= 8
		}
		if acc&8 != 0 {
			rex |= 4
		}
		if dst.rexBit() > 0 {
			rex |= 1
		}
		if rex != 0x40 {
			c.EmitByte(rex)
		}
		c.EmitByte(0x0f)
		c.EmitByte(0x7e)
		c.EmitByte(modRMRegReg(acc, byte(dst.encoding())))
	}
	if len(p.Reduces) == 0 && avx {
		vzeroupper(c)
	}
}

// vecShift emits dst = src shifted by imm, enc a shift by an immediate.
func vecShift(c backend.Compiler, avx bool, enc vecEnc, dst, src, imm byte) {
	if avx {
		// The destination is in VEX.vvvv, the source in ModRM.rm.
		vex(c, enc.ext, src, dst, 1, true, 1, enc.opcode)
	} else {
		if dst != src {
			legacyRR(c, 0, 1, 0x28, dst, src) // movaps
		}
		legacyRR(c, 1, 1, enc.opcode, enc.ext, dst)
	}
	c.EmitByte(imm)
}

// vzeroupper: C5 F8 77
func vzeroupper(c backend.Compiler) {
	c.EmitByte(0xc5)
	c.EmitByte(0xf8)
	c.EmitByte(0x77)
}

func modRMRegReg(reg, rm byte) byte { return 0xc0 | (reg&7)<<3 | rm&7 }

// vex emits a three-byte VEX prefix, the opcode and a register-register ModRM: dst in ModRM.reg, src in
// ModRM.rm, src1 in VEX.vvvv (0 when the instruction has no such operand, which encodes as 1111). m is the
// opcode map (1 = 0F, 2 = 0F38, 3 = 0F3A), pp the implied prefix (1 = 66, 2 = F3, 3 = F2).
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

// vecMem emits an instruction with register reg and memory at [base + rcx + disp]: VEX.256 on AVX, with
// src1 in VEX.vvvv, and legacy SSE otherwise, where it has no src1. m and pp are as for vex.
func vecMem(c backend.Compiler, avx bool, reg, src1 byte, base regalloc.RealReg, disp byte, m, pp, opcode byte) {
	enc := regEncodings[base]
	if avx {
		b1 := byte(0xe0) | m // R̄, X̄, B̄ set
		if reg&8 != 0 {
			b1 &^= 0x80
		}
		if enc.rexBit() > 0 {
			b1 &^= 0x20
		}
		c.EmitByte(0xc4)
		c.EmitByte(b1)
		c.EmitByte((^src1&0xf)<<3 | 4 | pp) // L = 256
	} else {
		if pfx := ppPrefix(pp); pfx != 0 {
			c.EmitByte(pfx)
		}
		if enc.rexBit() > 0 || reg&8 != 0 {
			rex := byte(0x40)
			if reg&8 != 0 {
				rex |= 4
			}
			if enc.rexBit() > 0 {
				rex |= 1
			}
			c.EmitByte(rex)
		}
		emitEscape(c, m)
	}
	c.EmitByte(opcode)
	// ModRM: mod = 00 or 01 (disp8), rm = 100 (SIB); SIB: scale 1, index rcx, base.
	mod := byte(0)
	if disp != 0 {
		mod = 1
	}
	c.EmitByte(mod<<6 | (reg&7)<<3 | 4)
	c.EmitByte(1<<3 | enc.encoding())
	if disp != 0 {
		c.EmitByte(disp)
	}
}

func ppPrefix(pp byte) byte {
	return [...]byte{0, 0x66, 0xf3, 0xf2}[pp]
}

func emitEscape(c backend.Compiler, m byte) {
	c.EmitByte(0x0f)
	switch m {
	case 2:
		c.EmitByte(0x38)
	case 3:
		c.EmitByte(0x3a)
	}
}

// legacyRR emits a legacy SSE instruction with a register-register ModRM, with a REX prefix for xmm8 and up.
func legacyRR(c backend.Compiler, pp, m, opcode, reg, rm byte) {
	if pfx := ppPrefix(pp); pfx != 0 {
		c.EmitByte(pfx)
	}
	if reg&8 != 0 || rm&8 != 0 {
		c.EmitByte(0x40 | reg>>3<<2 | rm>>3)
	}
	emitEscape(c, m)
	c.EmitByte(opcode)
	c.EmitByte(modRMRegReg(reg, rm))
}

// vecBinary emits dst = a op b: one three-operand instruction on AVX, a copy of a into dst when they
// differ and the two-operand instruction otherwise. Assign never gives dst b's register, unless a and b
// are the same op.
func vecBinary(c backend.Compiler, avx bool, enc vecEnc, dst, a, b byte) {
	if avx {
		vex(c, dst, b, a, enc.m, true, enc.pp, enc.opcode)
		return
	}
	if dst != a {
		legacyRR(c, 0, 1, 0x28, dst, a) // movaps
	}
	legacyRR(c, enc.pp, enc.m, enc.opcode, dst, b)
}

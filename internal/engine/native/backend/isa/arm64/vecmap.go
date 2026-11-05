package arm64

import (
	"fmt"
	"math/bits"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// An OpcodeVecMap becomes, after register allocation, a loop over its elements in 128-bit vectors. Its
// operands arrive in fixed registers: the pointers in vecMapPtrRegs, the element count in
// vecMapCountReg, the invariants in v0 and up, which is also where the loop broadcasts them, and the next
// of v0-v15 hold its accumulators and temporaries. The instruction uses every register it overwrites, so
// the register allocator keeps nothing it needs afterwards in them, and nothing else: those are all
// caller-saved. The loop advances the pointers, counts the count register down to zero, and leaves its
// first total in it and the others in the pointer registers past its pointers.
var vecMapPtrRegs = [...]regalloc.RealReg{x0, x1, x2, x3, x4, x5, x6}

const vecMapCountReg = x7

// vecMapResultReg returns the register total k of a program with ptrs pointers is left in.
func vecMapResultReg(ptrs, k int) regalloc.RealReg {
	if k == 0 {
		return vecMapCountReg
	}
	return vecMapPtrRegs[ptrs+k-1]
}

func (m *machine) lowerVecMap(instr *ssa.Instruction) {
	n, args := instr.VecMapData()
	prog := m.compiler.SSABuilder().VecProgram(n)
	if prog.Ptrs > len(vecMapPtrRegs) || prog.Invs > ssa.VecMapRegs || prog.Ptrs+len(prog.Reduces)-1 > len(vecMapPtrRegs) {
		panic("BUG: vector map program does not fit its registers")
	}
	var mask uint64
	for i := 0; i < prog.Ptrs; i++ {
		r := vecMapPtrRegs[i]
		m.InsertMove(regInfo.RealRegToVReg[r], m.vecMapOperand(args[i]), ssa.TypeI64)
		mask |= 1 << r
	}
	m.InsertMove(regInfo.RealRegToVReg[vecMapCountReg], m.vecMapOperand(args[prog.Ptrs]), ssa.TypeI64)
	mask |= 1 << vecMapCountReg
	typ := ssa.TypeF32
	if prog.Elem.Size() == 8 {
		typ = ssa.TypeF64
	}
	for i := 0; i < prog.Invs; i++ {
		r := v0 + regalloc.RealReg(i)
		m.InsertMove(regInfo.RealRegToVReg[r], m.vecMapOperand(args[prog.Ptrs+1+i]), typ)
		mask |= 1 << r
	}
	regs, ok := prog.Assign(false)
	if !ok {
		panic("BUG: vector map program does not fit its registers")
	}
	for _, r := range regs {
		mask |= 1 << (v0 + regalloc.RealReg(r))
	}
	for k := range prog.Reduces {
		mask |= 1<<(v0+regalloc.RealReg(prog.Invs+k)) | 1<<vecMapResultReg(prog.Ptrs, k)
	}
	if prog.NeedsScratch() {
		mask |= 1 << (v0 + regalloc.RealReg(prog.Scratch()))
	}
	vm := m.allocateInstr()
	vm.kind = vecMap
	vm.u1 = uint64(n) | uint64(len(prog.Reduces))<<32 | uint64(prog.Ptrs)<<40
	vm.u2 = mask
	m.insert(vm)
	first, rest := instr.Returns()
	for k := range prog.Reduces {
		v := first
		if k > 0 {
			v = rest[k-1]
		}
		m.InsertMove(m.compiler.VRegOf(v), regInfo.RealRegToVReg[vecMapResultReg(prog.Ptrs, k)], prog.ReduceType(k))
	}
}

// vecMapOperand returns the register holding v, materializing a constant, which is otherwise lowered
// only where it is used.
func (m *machine) vecMapOperand(v ssa.Value) regalloc.VReg {
	return m.getOperand_NR(m.compiler.ValueDefinition(v), extModeNone).nr()
}

// vecMapResults appends the registers a vecMap instruction leaves its totals in.
func (i *instruction) vecMapResults(regs *[]regalloc.VReg) {
	ptrs, n := int(uint8(i.u1>>40)), int(uint8(i.u1>>32))
	for k := 0; k < n; k++ {
		*regs = append(*regs, regInfo.RealRegToVReg[vecMapResultReg(ptrs, k)])
	}
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
	return fmt.Sprintf("vecmap #%d", uint32(i.u1))
}

// vecArrangementOf returns the arrangement of a 128-bit vector of lanes w bytes wide.
func vecArrangementOf(w uint8) vecArrangement {
	if w == 8 {
		return vecArrangement2D
	}
	return vecArrangement4S
}

// expandVecMaps replaces each vecMap with its loop.
func (m *machine) expandVecMaps() {
	for cur := m.rootInstr; cur != nil; cur = cur.next {
		if cur.kind == vecMap {
			cur = m.expandVecMap(cur)
		}
	}
}

// expandVecMap replaces vm with its loop and returns the last instruction of it:
//
//		dup   vI, vI[0]           ;; each invariant, across its lanes
//		lsl   x7, x7, #log2(size) ;; elements to bytes
//	loop:
//		ldr   q, [xP], #16        ;; twice over: the program on two vectors
//		...
//		str   q, [xS], #16
//		subs  x7, x7, #32
//		b.ne  loop
//
// and, for a reduction, the folding of each accumulator's lanes into its total's register.
func (m *machine) expandVecMap(vm *instruction) *instruction {
	p := m.compiler.SSABuilder().VecProgram(int(uint32(vm.u1)))
	regs, _ := p.Assign(false)
	vreg := func(k uint8) regalloc.VReg { return regInfo.RealRegToVReg[v0+regalloc.RealReg(k)] }
	xreg := func(r regalloc.RealReg) regalloc.VReg { return regInfo.RealRegToVReg[r] }
	count := xreg(vecMapCountReg)
	e := p.Elem.Size()
	arr := vecArrangementOf(uint8(e))
	isInt := p.Elem.IsInt()
	scratch := vreg(p.Scratch())

	cur, next := vm.prev, vm.next
	add := func(i *instruction) { cur = linkInstr(cur, i) }
	rrr := func(op vecOp, rd, rn, rm regalloc.VReg, a vecArrangement) {
		i := m.allocateInstr()
		i.asVecRRR(op, rd, operandNR(rn), operandNR(rm), a)
		add(i)
	}
	shiftImm := func(op vecOp, rd, rn regalloc.VReg, sh byte, a vecArrangement) {
		i := m.allocateInstr()
		i.asVecShiftImm(op, rd, operandNR(rn), operandShiftImm(sh), a)
		add(i)
	}

	laneArr := vecArrangementS
	if e == 8 {
		laneArr = vecArrangementD
	}
	for k := 0; k < p.Invs; k++ {
		dup := m.allocateInstr()
		dup.asVecDupElement(vreg(uint8(k)), operandNR(vreg(uint8(k))), laneArr, vecIndex(0))
		add(dup)
	}
	// An iota's register starts at the value of its invariant in every lane; lane j gets j steps more,
	// added from the step with its lanes below j cleared one at a time. Its step's register then holds
	// the steps of a whole vector, which each copy of the program adds after it.
	lanes := uint32(16 / e)
	for _, op := range p.Ops {
		if op.Kind != ssa.VecOpIota {
			continue
		}
		iota, step := vreg(op.Arg), vreg(op.B)
		rrr(vecOpOrr, scratch, step, step, vecArrangement16B)
		for j := uint32(0); j < lanes-1; j++ {
			ins := m.allocateInstr()
			ins.asMovToVec(scratch, operandNR(xzrVReg), laneArr, vecIndex(j))
			add(ins)
			rrr(vecOpAdd, iota, iota, scratch, vecArrangement4S)
		}
		shiftImm(vecOpShl, step, step, byte(bits.TrailingZeros32(lanes)), vecArrangement4S)
	}
	for k, red := range p.Reduces {
		acc := vreg(uint8(p.Invs + k))
		if red.Op == ssa.VecOpBand { // All ones: every lane equals itself.
			rrr(vecOpCmeq, acc, acc, acc, vecArrangement4S)
		} else {
			rrr(vecOpEOR, acc, acc, acc, vecArrangement16B)
		}
	}
	shift := byte(2)
	if e == 8 {
		shift = 3
	}
	lsl := m.allocateInstr()
	lsl.asALUShift(aluOpLsl, count, operandNR(count), operandShiftImm(shift), true)
	add(lsl)

	loop, loopLabel := m.allocateBrTarget()
	add(loop)
	// The last access through each pointer in a copy of the program advances it past the vector; any
	// before, a stream loaded twice or loaded and stored in place, reads where it is.
	last := len(p.Ops) - 1
	var lastAccess [len(vecMapPtrRegs)]int
	for k := range lastAccess {
		lastAccess[k] = -1
	}
	for idx, op := range p.Ops {
		if op.Kind == ssa.VecOpLoad {
			lastAccess[op.Arg] = idx
		}
	}
	if p.Store >= 0 {
		lastAccess[p.Store] = len(p.Ops)
	}
	access := func(ptr uint8, idx int) *addressMode {
		mode := m.amodePool.Allocate()
		if lastAccess[ptr] == idx {
			*mode = addressMode{kind: addressModeKindPostIndex, rn: xreg(vecMapPtrRegs[ptr]), imm: 16}
		} else {
			*mode = addressMode{kind: addressModeKindRegUnsignedImm12, rn: xreg(vecMapPtrRegs[ptr])}
		}
		return mode
	}
	for copyN := 0; copyN < 2; copyN++ {
		for idx, op := range p.Ops {
			r := vreg(regs[idx])
			opArr := arr
			if isInt {
				opArr = vecArrangementOf(op.W)
			}
			switch op.Kind {
			case ssa.VecOpInv, ssa.VecOpIota:
			case ssa.VecOpLoad:
				i := m.allocateInstr()
				i.asFpuLoad(r, access(op.Arg, idx), 128)
				add(i)
			case ssa.VecOpSqrt:
				i := m.allocateInstr()
				i.asVecMisc(vecOpFsqrt, r, operandNR(vreg(regs[op.A])), arr)
				add(i)
			case ssa.VecOpShl, ssa.VecOpShrU, ssa.VecOpShrS:
				if op.Arg == 0 {
					rrr(vecOpOrr, r, vreg(regs[op.A]), vreg(regs[op.A]), vecArrangement16B)
					break
				}
				vop := map[uint8]vecOp{ssa.VecOpShl: vecOpShl, ssa.VecOpShrU: vecOpUshr, ssa.VecOpShrS: vecOpSshr}[op.Kind]
				shiftImm(vop, r, vreg(regs[op.A]), op.Arg, opArr)
			case ssa.VecOpZext:
				shiftImm(vecOpShl, r, vreg(regs[op.A]), 32, vecArrangement2D)
				shiftImm(vecOpUshr, r, r, 32, vecArrangement2D)
			case ssa.VecOpMulU:
				// umull multiplies the two 32-bit lanes of the low halves: narrow each operand's lanes into one.
				a, b := vreg(regs[op.A]), vreg(regs[op.B])
				xtn := m.allocateInstr()
				xtn.asVecMisc(vecOpXtn, scratch, operandNR(a), vecArrangement2S)
				add(xtn)
				nb := scratch
				if op.B != op.A {
					nb = r
					xtn := m.allocateInstr()
					xtn.asVecMisc(vecOpXtn, nb, operandNR(b), vecArrangement2S)
					add(xtn)
				}
				rrr(vecOpUmull, r, scratch, nb, vecArrangement4S)
			case ssa.VecOpXor, ssa.VecOpAnd:
				vop := vecOpEOR
				if op.Kind == ssa.VecOpAnd {
					vop = vecOpAnd
				}
				rrr(vop, r, vreg(regs[op.A]), vreg(op.Arg), vecArrangement16B)
			default:
				rrr(vecMapBinaryOp(op.Kind, isInt), r, vreg(regs[op.A]), vreg(regs[op.B]), vecMapBinaryArrangement(op.Kind, opArr))
			}
		}
		if p.Store >= 0 {
			st := m.allocateInstr()
			st.asStore(operandNR(vreg(regs[last])), access(uint8(p.Store), len(p.Ops)), 128)
			add(st)
		}
		for k, red := range p.Reduces {
			acc := vreg(uint8(p.Invs + k))
			rrr(vecMapBinaryOp(red.Op, true), acc, acc, vreg(regs[red.Root]), vecMapBinaryArrangement(red.Op, vecArrangementOf(p.Ops[red.Root].W)))
		}
		for _, op := range p.Ops {
			if op.Kind == ssa.VecOpIota {
				rrr(vecOpAdd, vreg(op.Arg), vreg(op.Arg), vreg(op.B), vecArrangement4S)
			}
		}
	}
	subs := m.allocateInstr()
	subs.asALU(aluOpSubS, count, operandNR(count), operandImm12(32, 0), true)
	add(subs)
	bne := m.allocateInstr()
	bne.asCondBr(ne.asCond(), loopLabel, false)
	add(bne)

	// Fold each accumulator's lanes into its lowest: the upper half onto the lower, and for packed 32-bit
	// lanes the second lane onto the first. Then move it to its total's register.
	ptrs := int(uint8(vm.u1 >> 40))
	for k, red := range p.Reduces {
		acc := vreg(uint8(p.Invs + k))
		op, opArr := vecMapBinaryOp(red.Op, true), vecMapBinaryArrangement(red.Op, vecArrangementOf(p.Ops[red.Root].W))
		fold := func(dupArr vecArrangement, index vecIndex) {
			dup := m.allocateInstr()
			dup.asVecDupElement(scratch, operandNR(acc), dupArr, index)
			add(dup)
			rrr(op, acc, acc, scratch, opArr)
		}
		fold(vecArrangementD, 1)
		if e == 4 {
			fold(vecArrangementS, 1)
		}
		resArr := vecArrangementS
		if p.ReduceType(k) == ssa.TypeI64 {
			resArr = vecArrangementD
		}
		mov := m.allocateInstr()
		mov.asMovFromVec(xreg(vecMapResultReg(ptrs, k)), operandNR(acc), resArr, vecIndex(0), false)
		add(mov)
	}
	return linkInstr(cur, next).prev
}

// vecMapBinaryOp returns the vector instruction for a two-operand VecOp kind.
func vecMapBinaryOp(kind uint8, isInt bool) vecOp {
	switch kind {
	case ssa.VecOpAdd:
		if isInt {
			return vecOpAdd
		}
		return vecOpFadd
	case ssa.VecOpSub:
		if isInt {
			return vecOpSub
		}
		return vecOpFsub
	case ssa.VecOpMul:
		if isInt {
			return vecOpMul
		}
		return vecOpFmul
	case ssa.VecOpDiv:
		return vecOpFdiv
	case ssa.VecOpBand:
		return vecOpAnd
	case ssa.VecOpBor:
		return vecOpOrr
	case ssa.VecOpBxor:
		return vecOpEOR
	}
	panic("BUG: not a binary vector map op")
}

// vecMapBinaryArrangement is arr for arithmetic and 16B for the bitwise ops, which have no lanes.
func vecMapBinaryArrangement(kind uint8, arr vecArrangement) vecArrangement {
	switch kind {
	case ssa.VecOpBand, ssa.VecOpBor, ssa.VecOpBxor:
		return vecArrangement16B
	}
	return arr
}

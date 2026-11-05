package arm64

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// An OpcodeVecMap becomes, after register allocation, a loop over its elements in 128-bit vectors. Its
// operands arrive in fixed registers: the pointers in vecMapPtrRegs, the element count in
// vecMapCountReg, the invariants in v0 and up, which is also where the loop broadcasts them, and the next
// of v0-v7 hold its accumulator and temporaries. The instruction uses every register it overwrites, so
// the register allocator keeps nothing it needs afterwards in them, and nothing else: those are all
// caller-saved. The loop advances the pointers, counts the count register down to zero, and leaves a
// reduction's total in it.
var vecMapPtrRegs = [...]regalloc.RealReg{x0, x1, x2, x3, x4, x5, x6}

const vecMapCountReg = x7

func (m *machine) lowerVecMap(instr *ssa.Instruction) {
	n, args := instr.VecMapData()
	prog := m.compiler.SSABuilder().VecProgram(n)
	if prog.Ptrs > len(vecMapPtrRegs) || prog.Invs > ssa.VecMapRegs {
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
	if prog.Reduce != 0 {
		mask |= 1<<(v0+regalloc.RealReg(prog.Invs)) | 1<<v0 | 1<<v1
	}
	vm := m.allocateInstr()
	vm.kind = vecMap
	vm.u1 = uint64(n)
	if prog.Reduce != 0 {
		vm.u1 |= 1 << 32
	}
	vm.u2 = mask
	m.insert(vm)
	if prog.Reduce != 0 {
		m.InsertMove(m.compiler.VRegOf(instr.Return()), regInfo.RealRegToVReg[vecMapCountReg], prog.Elem)
	}
}

// vecMapOperand returns the register holding v, materializing a constant, which is otherwise lowered
// only where it is used.
func (m *machine) vecMapOperand(v ssa.Value) regalloc.VReg {
	return m.getOperand_NR(m.compiler.ValueDefinition(v), extModeNone).nr()
}

func (i *instruction) vecMapReduces() bool { return i.u1>>32 != 0 }

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

// vecArrangementOf returns the arrangement of a 128-bit vector of lanes of t.
func vecArrangementOf(t ssa.Type) vecArrangement {
	if t.Size() == 8 {
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
// and, for a reduction, the folding of the accumulator's lanes into x7.
func (m *machine) expandVecMap(vm *instruction) *instruction {
	p := m.compiler.SSABuilder().VecProgram(int(uint32(vm.u1)))
	regs, _ := p.Assign(false)
	vreg := func(k uint8) regalloc.VReg { return regInfo.RealRegToVReg[v0+regalloc.RealReg(k)] }
	xreg := func(r regalloc.RealReg) regalloc.VReg { return regInfo.RealRegToVReg[r] }
	count := xreg(vecMapCountReg)
	arr := vecArrangementOf(p.Elem)
	isInt := p.Elem.IsInt()

	cur, next := vm.prev, vm.next
	add := func(i *instruction) { cur = linkInstr(cur, i) }

	laneArr := vecArrangementS
	if arr == vecArrangement2D {
		laneArr = vecArrangementD
	}
	for k := 0; k < p.Invs; k++ {
		dup := m.allocateInstr()
		dup.asVecDupElement(vreg(uint8(k)), operandNR(vreg(uint8(k))), laneArr, vecIndex(0))
		add(dup)
	}
	acc := uint8(p.Invs)
	if p.Reduce != 0 {
		init := m.allocateInstr()
		if p.Reduce == ssa.VecOpBand { // All ones: every lane equals itself.
			init.asVecRRR(vecOpCmeq, vreg(acc), operandNR(vreg(acc)), operandNR(vreg(acc)), vecArrangement4S)
		} else {
			init.asVecRRR(vecOpEOR, vreg(acc), operandNR(vreg(acc)), operandNR(vreg(acc)), vecArrangement16B)
		}
		add(init)
	}
	shift := byte(2)
	if p.Elem.Size() == 8 {
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
			i := m.allocateInstr()
			switch op.Kind {
			case ssa.VecOpInv:
				continue
			case ssa.VecOpLoad:
				i.asFpuLoad(r, access(op.Arg, idx), 128)
			case ssa.VecOpSqrt:
				i.asVecMisc(vecOpFsqrt, r, operandNR(vreg(regs[op.A])), arr)
			case ssa.VecOpShl, ssa.VecOpShrU, ssa.VecOpShrS:
				if op.Arg == 0 {
					i.asVecRRR(vecOpOrr, r, operandNR(vreg(regs[op.A])), operandNR(vreg(regs[op.A])), vecArrangement16B)
					break
				}
				vop := map[uint8]vecOp{ssa.VecOpShl: vecOpShl, ssa.VecOpShrU: vecOpUshr, ssa.VecOpShrS: vecOpSshr}[op.Kind]
				i.asVecShiftImm(vop, r, operandNR(vreg(regs[op.A])), operandShiftImm(op.Arg), arr)
			case ssa.VecOpXor, ssa.VecOpAnd:
				vop := vecOpEOR
				if op.Kind == ssa.VecOpAnd {
					vop = vecOpAnd
				}
				i.asVecRRR(vop, r, operandNR(vreg(regs[op.A])), operandNR(vreg(op.Arg)), vecArrangement16B)
			default:
				i.asVecRRR(vecMapBinaryOp(op.Kind, isInt), r, operandNR(vreg(regs[op.A])), operandNR(vreg(regs[op.B])), vecMapBinaryArrangement(op.Kind, arr))
			}
			add(i)
		}
		switch {
		case p.Store >= 0:
			st := m.allocateInstr()
			st.asStore(operandNR(vreg(regs[last])), access(uint8(p.Store), len(p.Ops)), 128)
			add(st)
		default:
			red := m.allocateInstr()
			red.asVecRRR(vecMapBinaryOp(p.Reduce, true), vreg(acc), operandNR(vreg(acc)), operandNR(vreg(regs[last])), vecMapBinaryArrangement(p.Reduce, arr))
			add(red)
		}
	}
	subs := m.allocateInstr()
	subs.asALU(aluOpSubS, count, operandNR(count), operandImm12(32, 0), true)
	add(subs)
	bne := m.allocateInstr()
	bne.asCondBr(ne.asCond(), loopLabel, false)
	add(bne)

	if p.Reduce != 0 {
		// Fold the accumulator's lanes into its lowest: the upper half onto the lower, and for 32-bit lanes
		// the second lane onto the first.
		t := uint8(0)
		if acc == 0 {
			t = 1
		}
		op, opArr := vecMapBinaryOp(p.Reduce, true), vecMapBinaryArrangement(p.Reduce, arr)
		fold := func(dupArr vecArrangement, index vecIndex) {
			dup := m.allocateInstr()
			dup.asVecDupElement(vreg(t), operandNR(vreg(acc)), dupArr, index)
			add(dup)
			o := m.allocateInstr()
			o.asVecRRR(op, vreg(acc), operandNR(vreg(acc)), operandNR(vreg(t)), opArr)
			add(o)
		}
		fold(vecArrangementD, 1)
		if arr == vecArrangement4S {
			fold(vecArrangementS, 1)
		}
		mov := m.allocateInstr()
		mov.asMovFromVec(count, operandNR(vreg(acc)), laneArr, vecIndex(0), false)
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

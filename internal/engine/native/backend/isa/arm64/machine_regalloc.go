package arm64

// This file implements the interfaces required for register allocations. See backend.RegAllocFunctionMachine.

import (
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// regAllocFn implements regalloc.Function.
type regAllocFn struct {
	ssaB                   ssa.Builder
	m                      *machine
	loopNestingForestRoots []ssa.BasicBlock
	blockIter              int
}

// PostOrderBlockIteratorBegin implements regalloc.Function.
func (f *regAllocFn) PostOrderBlockIteratorBegin() *labelPosition {
	f.blockIter = len(f.m.orderedSSABlockLabelPos) - 1
	return f.PostOrderBlockIteratorNext()
}

// PostOrderBlockIteratorNext implements regalloc.Function.
func (f *regAllocFn) PostOrderBlockIteratorNext() *labelPosition {
	if f.blockIter < 0 {
		return nil
	}
	b := f.m.orderedSSABlockLabelPos[f.blockIter]
	f.blockIter--
	return b
}

// ReversePostOrderBlockIteratorBegin implements regalloc.Function.
func (f *regAllocFn) ReversePostOrderBlockIteratorBegin() *labelPosition {
	f.blockIter = 0
	return f.ReversePostOrderBlockIteratorNext()
}

// ReversePostOrderBlockIteratorNext implements regalloc.Function.
func (f *regAllocFn) ReversePostOrderBlockIteratorNext() *labelPosition {
	if f.blockIter >= len(f.m.orderedSSABlockLabelPos) {
		return nil
	}
	b := f.m.orderedSSABlockLabelPos[f.blockIter]
	f.blockIter++
	return b
}

// ClobberedRegisters implements regalloc.Function.
func (f *regAllocFn) ClobberedRegisters(regs []regalloc.VReg) {
	f.m.clobberedRegs = append(f.m.clobberedRegs[:0], regs...)
}

// LoopNestingForestRoots implements regalloc.Function.
func (f *regAllocFn) LoopNestingForestRoots() int {
	f.loopNestingForestRoots = f.ssaB.LoopNestingForestRoots()
	return len(f.loopNestingForestRoots)
}

// LoopNestingForestRoot implements regalloc.Function.
func (f *regAllocFn) LoopNestingForestRoot(i int) *labelPosition {
	root := f.loopNestingForestRoots[i]
	pos := f.m.getOrAllocateSSABlockLabelPosition(root)
	return pos
}

// LowestCommonAncestor implements regalloc.Function.
func (f *regAllocFn) LowestCommonAncestor(blk1, blk2 *labelPosition) *labelPosition {
	sb := f.ssaB.LowestCommonAncestor(blk1.sb, blk2.sb)
	pos := f.m.getOrAllocateSSABlockLabelPosition(sb)
	return pos
}

// Idom implements regalloc.Function.
func (f *regAllocFn) Idom(blk *labelPosition) *labelPosition {
	sb := f.ssaB.Idom(blk.sb)
	pos := f.m.getOrAllocateSSABlockLabelPosition(sb)
	return pos
}

// SwapBefore implements regalloc.Function.
func (f *regAllocFn) SwapBefore(x1, x2, tmp regalloc.VReg, instr *instruction) {
	f.m.swap(instr.prev, x1, x2, tmp)
}

// StoreRegisterBefore implements regalloc.Function.
func (f *regAllocFn) StoreRegisterBefore(v regalloc.VReg, instr *instruction) {
	m := f.m
	m.insertStoreRegisterAt(v, instr, false)
}

// StoreRegisterAfter implements regalloc.Function.
func (f *regAllocFn) StoreRegisterAfter(v regalloc.VReg, instr *instruction) {
	m := f.m
	m.insertStoreRegisterAt(v, instr, true)
}

// ReloadRegisterBefore implements regalloc.Function.
func (f *regAllocFn) ReloadRegisterBefore(v regalloc.VReg, instr *instruction) {
	m := f.m
	m.insertReloadRegisterAt(v, instr, false)
}

// ReloadRegisterAfter implements regalloc.Function.
func (f *regAllocFn) ReloadRegisterAfter(v regalloc.VReg, instr *instruction) {
	m := f.m
	m.insertReloadRegisterAt(v, instr, true)
}

// InsertMoveBefore implements regalloc.Function.
func (f *regAllocFn) InsertMoveBefore(dst, src regalloc.VReg, instr *instruction) {
	f.m.insertMoveBefore(dst, src, instr)
}

// LoopNestingForestChild implements regalloc.Function.
func (f *regAllocFn) LoopNestingForestChild(pos *labelPosition, i int) *labelPosition {
	childSB := pos.sb.LoopNestingForestChildren()[i]
	return f.m.getOrAllocateSSABlockLabelPosition(childSB)
}

// Succ implements regalloc.Block.
func (f *regAllocFn) Succ(pos *labelPosition, i int) *labelPosition {
	succSB := pos.sb.Succ(i)
	if succSB.ReturnBlock() {
		return nil
	}
	return f.m.getOrAllocateSSABlockLabelPosition(succSB)
}

// Pred implements regalloc.Block.
func (f *regAllocFn) Pred(pos *labelPosition, i int) *labelPosition {
	predSB := pos.sb.Pred(i)
	return f.m.getOrAllocateSSABlockLabelPosition(predSB)
}

// BlockParams implements regalloc.Function.
func (f *regAllocFn) BlockParams(pos *labelPosition, regs *[]regalloc.VReg) []regalloc.VReg {
	c := f.m.compiler
	*regs = (*regs)[:0]
	for i := 0; i < pos.sb.Params(); i++ {
		v := c.VRegOf(pos.sb.Param(i))
		*regs = append(*regs, v)
	}
	return *regs
}

// ID implements regalloc.Block.
func (pos *labelPosition) ID() int32 {
	return int32(pos.sb.ID())
}

// InstrIteratorBegin implements regalloc.Block.
func (pos *labelPosition) InstrIteratorBegin() *instruction {
	ret := pos.begin
	pos.cur = ret
	return ret
}

// InstrIteratorNext implements regalloc.Block.
func (pos *labelPosition) InstrIteratorNext() *instruction {
	for {
		if pos.cur == pos.end {
			return nil
		}
		instr := pos.cur.next
		pos.cur = instr
		if instr == nil {
			return nil
		} else if instr.addedBeforeRegAlloc {
			// Only concerned about the instruction added before regalloc.
			return instr
		}
	}
}

// InstrRevIteratorBegin implements regalloc.Block.
func (pos *labelPosition) InstrRevIteratorBegin() *instruction {
	pos.cur = pos.end
	return pos.cur
}

// InstrRevIteratorNext implements regalloc.Block.
func (pos *labelPosition) InstrRevIteratorNext() *instruction {
	for {
		if pos.cur == pos.begin {
			return nil
		}
		instr := pos.cur.prev
		pos.cur = instr
		if instr == nil {
			return nil
		} else if instr.addedBeforeRegAlloc {
			// Only concerned about the instruction added before regalloc.
			return instr
		}
	}
}

// FirstInstr implements regalloc.Block.
func (pos *labelPosition) FirstInstr() *instruction { return pos.begin }

// LastInstrForInsertion implements regalloc.Block.
func (pos *labelPosition) LastInstrForInsertion() *instruction {
	return lastInstrForInsertion(pos.begin, pos.end)
}

// Preds implements regalloc.Block.
func (pos *labelPosition) Preds() int { return pos.sb.Preds() }

// Entry implements regalloc.Block.
func (pos *labelPosition) Entry() bool { return pos.sb.EntryBlock() }

// Succs implements regalloc.Block.
func (pos *labelPosition) Succs() int { return pos.sb.Succs() }

// LoopHeader implements regalloc.Block.
func (pos *labelPosition) LoopHeader() bool { return pos.sb.LoopHeader() }

// LoopNestingForestChildren implements regalloc.Block.
func (pos *labelPosition) LoopNestingForestChildren() int {
	return len(pos.sb.LoopNestingForestChildren())
}

func (m *machine) swap(cur *instruction, x1, x2, tmp regalloc.VReg) {
	prevNext := cur.next
	var mov1, mov2, mov3 *instruction
	if x1.RegType() == regalloc.RegTypeInt {
		if !tmp.Valid() {
			tmp = tmpRegVReg
		}
		mov1 = m.allocateInstr().asMove64(tmp, x1)
		mov2 = m.allocateInstr().asMove64(x1, x2)
		mov3 = m.allocateInstr().asMove64(x2, tmp)
		cur = linkInstr(cur, mov1)
		cur = linkInstr(cur, mov2)
		cur = linkInstr(cur, mov3)
		linkInstr(cur, prevNext)
	} else {
		if !tmp.Valid() {
			r2 := x2.RealReg()
			// Temporarily spill x1 to stack.
			cur = m.insertStoreRegisterAt(x1, cur, true).prev
			// Then move x2 to x1.
			cur = linkInstr(cur, m.allocateInstr().asFpuMov128(x1, x2))
			linkInstr(cur, prevNext)
			// Then reload the original value on x1 from stack to r2.
			m.insertReloadRegisterAt(x1.SetRealReg(r2), cur, true)
		} else {
			mov1 = m.allocateInstr().asFpuMov128(tmp, x1)
			mov2 = m.allocateInstr().asFpuMov128(x1, x2)
			mov3 = m.allocateInstr().asFpuMov128(x2, tmp)
			cur = linkInstr(cur, mov1)
			cur = linkInstr(cur, mov2)
			cur = linkInstr(cur, mov3)
			linkInstr(cur, prevNext)
		}
	}
}

func (m *machine) insertMoveBefore(dst, src regalloc.VReg, instr *instruction) {
	typ := src.RegType()
	if typ != dst.RegType() {
		panic("BUG: src and dst must have the same type")
	}

	mov := m.allocateInstr()
	if typ == regalloc.RegTypeInt {
		mov.asMove64(dst, src)
	} else {
		mov.asFpuMov128(dst, src)
	}

	cur := instr.prev
	prevNext := cur.next
	cur = linkInstr(cur, mov)
	linkInstr(cur, prevNext)
}

// spillSlotBitsOf returns the width, in bits, of the spill slot backing v, whose recorded SSA type
// is typ. For a VReg standing for an SSA value, that is simply the width of its type. A VReg that
// names a real register directly (regalloc.FromRealReg) has no SSA type at all, though:
// compiler.TypeOf indexes ssaTypeOfVRegID by VRegID and the reserved real-register IDs are never
// written, so it returns typeInvalid. Nothing tells us which part of such a register is live, so
// the whole architectural register is saved and restored: 64 bits for an X register and 128 bits
// for a Q register. That is the rule setupPrologue and setupEpilogueAfter already apply to the
// clobbered registers, which are untyped for exactly the same reason.
func spillSlotBitsOf(v regalloc.VReg, typ ssa.Type) byte {
	switch typ {
	case ssa.TypeI32, ssa.TypeI64, ssa.TypeF32, ssa.TypeF64, ssa.TypeV128:
		return typ.Bits()
	default:
		return regTypeToRegisterSizeInBits(v.RegType())
	}
}

func (m *machine) insertStoreRegisterAt(v regalloc.VReg, instr *instruction, after bool) *instruction {
	if !v.IsRealReg() {
		panic("BUG: VReg must be backed by real reg to be stored")
	}

	bits := spillSlotBitsOf(v, m.compiler.TypeOf(v))

	var prevNext, cur *instruction
	if after {
		cur, prevNext = instr, instr.next
	} else {
		cur, prevNext = instr.prev, instr
	}

	offsetFromSP := m.getVRegSpillSlotOffsetFromSP(v.ID(), bits/8)
	var amode *addressMode
	cur, amode = m.resolveAddressModeForOffsetAndInsert(cur, offsetFromSP, bits, spVReg, true)
	store := m.allocateInstr()
	store.asStore(operandNR(v), amode, bits)

	cur = linkInstr(cur, store)
	return linkInstr(cur, prevNext)
}

func (m *machine) insertReloadRegisterAt(v regalloc.VReg, instr *instruction, after bool) *instruction {
	if !v.IsRealReg() {
		panic("BUG: VReg must be backed by real reg to be stored")
	}

	typ := m.compiler.TypeOf(v)
	bits := spillSlotBitsOf(v, typ)

	var prevNext, cur *instruction
	if after {
		cur, prevNext = instr, instr.next
	} else {
		cur, prevNext = instr.prev, instr
	}

	offsetFromSP := m.getVRegSpillSlotOffsetFromSP(v.ID(), bits/8)
	var amode *addressMode
	cur, amode = m.resolveAddressModeForOffsetAndInsert(cur, offsetFromSP, bits, spVReg, true)
	load := m.allocateInstr()
	switch typ {
	case ssa.TypeI32, ssa.TypeI64:
		load.asULoad(v, amode, typ.Bits())
	case ssa.TypeF32, ssa.TypeF64:
		load.asFpuLoad(v, amode, typ.Bits())
	case ssa.TypeV128:
		load.asFpuLoad(v, amode, 128)
	default:
		// No SSA type: v names a real register directly, so insertStoreRegisterAt saved the whole
		// register and we reload the whole register. spillSlotBitsOf already picked the
		// width from the register class, so all that is left is the instruction form:
		// LDR (immediate, unsigned offset), 64-bit, bits 31:22 = 1111100001 (0xf9400000) for an
		// X register, and LDR (immediate, SIMD&FP), 128-bit, bits 31:22 = 0011110011 (0x3dc00000)
		// for a Q register. setupEpilogueAfter restores the clobbered registers with this same
		// pair of forms.
		if v.RegType() == regalloc.RegTypeInt {
			load.asULoad(v, amode, bits)
		} else {
			load.asFpuLoad(v, amode, bits)
		}
	}

	cur = linkInstr(cur, load)
	return linkInstr(cur, prevNext)
}

// pairSpillAccesses merges neighbouring 64-bit spill stores/reloads into a single
// STP/LDP, extending to the spill region what C9 already does for the clobbered
// registers. Two such accesses adjacent in the list have nothing between them and
// address disjoint slots, so one pair instruction does the same work with half the
// memory operations. Run after postRegAlloc, when every address mode is final.
//
// Nothing but a store64/uLoad64 is ever unlinked, and every labelPosition begins and
// ends on a nop0 (see StartBlock/EndBlock), so no block bound is disturbed -- and a
// block bound between two accesses is exactly what stops them from being paired,
// which is what keeps a spill from moving across a control-flow edge.
func (m *machine) pairSpillAccesses() {
	var prev *instruction
	for cur := m.rootInstr; cur != nil; cur = cur.next {
		if prev != nil && m.tryPairSpillAccess(prev, cur) {
			prev = nil
			continue
		}
		if cur.kind == store64 || cur.kind == uLoad64 {
			prev = cur
		} else {
			prev = nil
		}
	}
}

// tryPairSpillAccess rewrites `a` into the STP/LDP covering both `a` and `b` and
// unlinks `b`, reporting whether it did.
func (m *machine) tryPairSpillAccess(a, b *instruction) bool {
	if a.kind != b.kind {
		return false
	}
	load := a.kind == uLoad64
	am, bm := a.getAmode(), b.getAmode()
	if am.rn != spVReg || bm.rn != spVReg || !immOffsetAmode(am) || !immOffsetAmode(bm) {
		return false
	}
	var r1, r2 regalloc.VReg
	if load {
		r1, r2 = a.rd, b.rd
	} else {
		if a.rn.kind != operandKindNR || b.rn.kind != operandKindNR {
			return false
		}
		r1, r2 = a.rn.nr(), b.rn.nr()
	}
	// LDP with two identical destinations is CONSTRAINED UNPREDICTABLE, and a
	// duplicate store is not worth pairing either.
	if r1 == r2 {
		return false
	}
	lo, hi := am.imm, bm.imm
	if lo > hi {
		lo, hi = hi, lo
		r1, r2 = r2, r1
	}
	// "Load/store register pair (signed offset)": the offset is the low slot's, in
	// units of 8 bytes, and imm7 is signed.
	if hi-lo != 8 || lo%8 != 0 || lo < -64*8 || lo > 63*8 {
		return false
	}

	amode := m.amodePool.Allocate()
	*amode = addressMode{kind: addressModeKindRegSignedImm7, rn: spVReg, imm: lo}
	if load {
		a.asLoadPair64(r1, r2, amode)
	} else {
		a.asStorePair64(r1, r2, amode)
	}
	a.next = b.next
	if b.next != nil {
		b.next.prev = a
	}
	return true
}

// immOffsetAmode reports whether the address mode is a plain base+immediate one,
// i.e. its offset is already known and does not need an index register.
func immOffsetAmode(a *addressMode) bool {
	return a.kind == addressModeKindRegUnsignedImm12 || a.kind == addressModeKindRegSignedImm9
}

func lastInstrForInsertion(begin, end *instruction) *instruction {
	cur := end
	for cur.kind == nop0 {
		cur = cur.prev
		if cur == begin {
			return end
		}
	}
	switch cur.kind {
	case br:
		return cur
	default:
		return end
	}
}

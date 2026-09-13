package riscv64

// This file implements the interfaces required for register allocation. See
// backend.RegAllocFunctionMachine.

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
	return f.m.getOrAllocateSSABlockLabelPosition(f.loopNestingForestRoots[i])
}

// LowestCommonAncestor implements regalloc.Function.
func (f *regAllocFn) LowestCommonAncestor(blk1, blk2 *labelPosition) *labelPosition {
	return f.m.getOrAllocateSSABlockLabelPosition(f.ssaB.LowestCommonAncestor(blk1.sb, blk2.sb))
}

// Idom implements regalloc.Function.
func (f *regAllocFn) Idom(blk *labelPosition) *labelPosition {
	return f.m.getOrAllocateSSABlockLabelPosition(f.ssaB.Idom(blk.sb))
}

// SwapBefore implements regalloc.Function.
func (f *regAllocFn) SwapBefore(x1, x2, tmp regalloc.VReg, instr *instruction) {
	f.m.swap(instr.prev, x1, x2, tmp)
}

// StoreRegisterBefore implements regalloc.Function.
func (f *regAllocFn) StoreRegisterBefore(v regalloc.VReg, instr *instruction) {
	f.m.insertStoreRegisterAt(v, instr, false)
}

// StoreRegisterAfter implements regalloc.Function.
func (f *regAllocFn) StoreRegisterAfter(v regalloc.VReg, instr *instruction) {
	f.m.insertStoreRegisterAt(v, instr, true)
}

// ReloadRegisterBefore implements regalloc.Function.
func (f *regAllocFn) ReloadRegisterBefore(v regalloc.VReg, instr *instruction) {
	f.m.insertReloadRegisterAt(v, instr, false)
}

// ReloadRegisterAfter implements regalloc.Function.
func (f *regAllocFn) ReloadRegisterAfter(v regalloc.VReg, instr *instruction) {
	f.m.insertReloadRegisterAt(v, instr, true)
}

// InsertMoveBefore implements regalloc.Function.
func (f *regAllocFn) InsertMoveBefore(dst, src regalloc.VReg, instr *instruction) {
	f.m.insertMoveBefore(dst, src, instr)
}

// LoopNestingForestChild implements regalloc.Function.
func (f *regAllocFn) LoopNestingForestChild(pos *labelPosition, i int) *labelPosition {
	return f.m.getOrAllocateSSABlockLabelPosition(pos.sb.LoopNestingForestChildren()[i])
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
	return f.m.getOrAllocateSSABlockLabelPosition(pos.sb.Pred(i))
}

// BlockParams implements regalloc.Function.
func (f *regAllocFn) BlockParams(pos *labelPosition, regs *[]regalloc.VReg) []regalloc.VReg {
	c := f.m.compiler
	*regs = (*regs)[:0]
	for i := 0; i < pos.sb.Params(); i++ {
		*regs = append(*regs, c.VRegOf(pos.sb.Param(i)))
	}
	return *regs
}

// ID implements regalloc.Block.
func (pos *labelPosition) ID() int32 { return int32(pos.sb.ID()) }

// InstrIteratorBegin implements regalloc.Block.
func (pos *labelPosition) InstrIteratorBegin() *instruction {
	pos.cur = pos.begin
	return pos.cur
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
			// Only concerned with instructions added before regalloc.
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

// swap exchanges the contents of x1 and x2, using tmp when the allocator could
// spare one and the stack when it could not.
func (m *machine) swap(cur *instruction, x1, x2, tmp regalloc.VReg) {
	prevNext := cur.next
	typ := x1.RegType()

	if tmp.Valid() {
		mov := func(dst, src regalloc.VReg) *instruction { return m.allocateMoveFor(typ, dst, src) }
		cur = linkInstr(cur, mov(tmp, x1))
		cur = linkInstr(cur, mov(x1, x2))
		cur = linkInstr(cur, mov(x2, tmp))
		linkInstr(cur, prevNext)
		return
	}

	// No spare register of this class. Integers always have one -- tmpReg is
	// reserved and never allocated -- so this only arises for the FP and
	// vector files, which have no reserved scratch. Park x1 in its spill slot,
	// move x2 into x1, then reload the parked value into x2.
	if typ == regalloc.RegTypeInt {
		cur = linkInstr(cur, m.allocateInstr().asMove64(tmpRegVReg, x1))
		cur = linkInstr(cur, m.allocateInstr().asMove64(x1, x2))
		cur = linkInstr(cur, m.allocateInstr().asMove64(x2, tmpRegVReg))
		linkInstr(cur, prevNext)
		return
	}

	r2 := x2.RealReg()
	cur = m.insertStoreRegisterAt(x1, cur, true).prev
	cur = linkInstr(cur, m.allocateMoveFor(typ, x1, x2))
	linkInstr(cur, prevNext)
	m.insertReloadRegisterAt(x1.SetRealReg(r2), cur, true)
}

// allocateMoveFor builds the register-to-register copy appropriate to a
// register class.
func (m *machine) allocateMoveFor(typ regalloc.RegType, dst, src regalloc.VReg) *instruction {
	i := m.allocateInstr()
	switch typ {
	case regalloc.RegTypeInt:
		return i.asMove64(dst, src)
	case regalloc.RegTypeFloat:
		// fsgnj.d, i.e. all 64 bits: a single-precision value in an f-register
		// carries a NaN box in its upper half that a narrower copy would drop.
		return i.asFpuMov(dst, src)
	case regalloc.RegTypeVec:
		return i.asVecMov(dst, src)
	default:
		panic("BUG: unknown register type")
	}
}

func (m *machine) insertMoveBefore(dst, src regalloc.VReg, instr *instruction) {
	typ := src.RegType()
	if typ != dst.RegType() {
		panic("BUG: src and dst must have the same register type")
	}
	cur := instr.prev
	prevNext := cur.next
	cur = linkInstr(cur, m.allocateMoveFor(typ, dst, src))
	linkInstr(cur, prevNext)
}

// spillSlotSizeOf returns the size in bytes of the spill slot backing v.
//
// Every RV64G register is 64 bits, so a scalar slot is 8 bytes regardless of
// whether the value in it is an i32, an f32 or an untyped real register. Only
// a vector register needs more, and it needs exactly the 16 bytes of a v128 --
// not VLEN, which is unknown here and irrelevant: the backend only ever keeps
// 16 architectural bytes in a v-register.
func spillSlotSizeOf(v regalloc.VReg, typ ssa.Type) int64 {
	if typ == ssa.TypeV128 || v.RegType() == regalloc.RegTypeVec {
		return 16
	}
	return 8
}

func (m *machine) insertStoreRegisterAt(v regalloc.VReg, instr *instruction, after bool) *instruction {
	if !v.IsRealReg() {
		panic("BUG: VReg must be backed by a real register to be stored")
	}
	typ := m.compiler.TypeOf(v)
	size := spillSlotSizeOf(v, typ)

	var prevNext, cur *instruction
	if after {
		cur, prevNext = instr, instr.next
	} else {
		cur, prevNext = instr.prev, instr
	}

	offsetFromSP := m.getVRegSpillSlotOffsetFromSP(v.ID(), size)

	if v.RegType() == regalloc.RegTypeVec {
		var amode *addressMode
		cur, amode = m.resolveAddressModeForOffsetAndInsert(cur, offsetFromSP, spVReg, true)
		cur = linkInstr(cur, m.allocateInstr().asVecStore(v, amode))
		return linkInstr(cur, prevNext)
	}

	var amode *addressMode
	cur, amode = m.resolveAddressModeForOffsetAndInsert(cur, offsetFromSP, spVReg, true)
	store := m.allocateInstr()
	// Always the full 64 bits, whatever the value's declared width: an i32 is
	// stored sign-extended and must reload that way, and an f32 carries a NaN
	// box in its upper half.
	store.asStore(v, amode, 64, v.RegType() == regalloc.RegTypeInt)
	cur = linkInstr(cur, store)
	return linkInstr(cur, prevNext)
}

func (m *machine) insertReloadRegisterAt(v regalloc.VReg, instr *instruction, after bool) *instruction {
	if !v.IsRealReg() {
		panic("BUG: VReg must be backed by a real register to be reloaded")
	}
	typ := m.compiler.TypeOf(v)
	size := spillSlotSizeOf(v, typ)

	var prevNext, cur *instruction
	if after {
		cur, prevNext = instr, instr.next
	} else {
		cur, prevNext = instr.prev, instr
	}

	offsetFromSP := m.getVRegSpillSlotOffsetFromSP(v.ID(), size)

	if v.RegType() == regalloc.RegTypeVec {
		var amode *addressMode
		cur, amode = m.resolveAddressModeForOffsetAndInsert(cur, offsetFromSP, spVReg, true)
		cur = linkInstr(cur, m.allocateInstr().asVecLoad(v, amode))
		return linkInstr(cur, prevNext)
	}

	var amode *addressMode
	cur, amode = m.resolveAddressModeForOffsetAndInsert(cur, offsetFromSP, spVReg, true)
	load := m.allocateInstr()
	if v.RegType() == regalloc.RegTypeInt {
		load.asLoad(v, amode, 64, false)
	} else {
		load.asFpuLoad(v, amode, 64)
	}
	cur = linkInstr(cur, load)
	return linkInstr(cur, prevNext)
}

// getVRegSpillSlotOffsetFromSP returns the SP-relative offset of v's spill
// slot, allocating one on first use. Offsets skip the 16-byte frame-size slot
// that sits at the very bottom of the frame.
func (m *machine) getVRegSpillSlotOffsetFromSP(id regalloc.VRegID, size int64) int64 {
	offset, ok := m.spillSlots[id]
	if !ok {
		offset = m.spillSlotSize
		m.spillSlots[id] = offset
		m.spillSlotSize += size
	}
	return offset + 16
}

// lastInstrForInsertion returns the instruction spill/reload adjustments may be
// inserted before: the block's trailing nop, unless the block ends in an
// unconditional branch, which must stay last.
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

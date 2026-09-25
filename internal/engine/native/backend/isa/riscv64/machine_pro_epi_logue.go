package riscv64

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
)

// PostRegAlloc implements backend.Machine.
func (m *machine) PostRegAlloc() {
	m.setupPrologue()
	m.postRegAlloc()
	m.emitTrapIslands()
}

// setupPrologue builds the function's frame.
//
//	         (high address)                    (high address)
//	SP----> +-----------------+               +------------------+ <----+
//	        |     .......     |               |     .......      |      |
//	        |      ret Y      |               |      ret Y       |      |
//	        |     .......     |               |     .......      |      |
//	        |      ret 0      |               |      ret 0       |      |
//	        |      arg X      |               |      arg X       |      |  size_of_arg_ret
//	        |     .......     |     ====>     |     .......      |      |
//	        |      arg 1      |               |      arg 1       |      |
//	        |      arg 0      |               |      arg 0       | <----+
//	        |-----------------|               |  size_of_arg_ret |
//	                                          |  return address  |
//	                                          +------------------+ <---- SP
//	          (low address)                     (low address)
//
// then, below the return address, the clobbered registers, the spill slots and
// finally the frame-size word the unwinder chases (see unwind_stack.go).
func (m *machine) setupPrologue() {
	cur := m.rootInstr
	prevInitInst := cur.next

	// Save the return address (ra) and size_of_arg_ret below SP. The latter is
	// what lets the unwinder step over this frame's arguments.
	cur = m.createReturnAddrAndSizeOfArgRetSlot(cur)

	if !m.stackBoundsCheckDisabled && !m.canSkipStackBoundsCheck() {
		cur = m.insertStackBoundsCheck(m.requiredStackSize(), cur)
	}

	if m.spillSlotSize == 0 && len(m.spillSlots) != 0 {
		panic(fmt.Sprintf("BUG: spillSlotSize=%d, spillSlots=%v", m.spillSlotSize, m.spillSlots))
	}

	// Reserve the whole clobber region with one SP adjustment, then save each
	// register at a positive offset. RV64 has no store-pair instruction, so
	// each save is its own 8-byte store -- every register in either file is 64
	// bits wide, unlike arm64 where a v-register forces 16.
	if regs := m.clobberedRegs; len(regs) > 0 {
		total := m.clobberedRegSlotSize()
		cur = m.addsAddOrSubStackPointer(cur, total, false /* decrement */)
		for i, r := range regs {
			cur = m.storeRegToSPOffset(cur, r, int64(i)*8)
		}
	}

	if size := m.spillSlotSize; size > 0 {
		if size&0xf != 0 {
			panic(fmt.Errorf("BUG: spill slot size %d is not 16-byte aligned", size))
		}
		cur = m.addsAddOrSubStackPointer(cur, size, false)
	}

	// Push the frame size so the stack can be unwound.
	cur = m.createFrameSizeSlot(cur, m.frameSize())

	// For a function with an EH context, stash the entry-ABI context registers
	// into the reserved slots at the bottom of the spill region. a0 always
	// holds the execution context whenever native code is entered from Go, and
	// a1 the module context.
	if m.hasEHContext {
		execOff, modOff := m.EhCtxSlotOffsets()
		cur = m.storeRegToSPOffset(cur, x10VReg, execOff)
		cur = m.storeRegToSPOffset(cur, x11VReg, modOff)
	}

	linkInstr(cur, prevInitInst)
}

// storeRegToSPOffset emits `sd r, off(sp)` (or the FP/vector equivalent).
func (m *machine) storeRegToSPOffset(cur *instruction, r regalloc.VReg, off int64) *instruction {
	if r.RegType() == regalloc.RegTypeVec {
		var vm *addressMode
		cur, vm = m.resolveAddressModeForOffsetAndInsert(cur, off, spVReg, true)
		return linkInstr(cur, m.allocateInstr().asVecStore(r, vm))
	}
	var amode *addressMode
	cur, amode = m.resolveAddressModeForOffsetAndInsert(cur, off, spVReg, true)
	store := m.allocateInstr()
	store.asStore(r, amode, 64, r.RegType() == regalloc.RegTypeInt)
	return linkInstr(cur, store)
}

// loadRegFromSPOffset is storeRegToSPOffset's mirror.
func (m *machine) loadRegFromSPOffset(cur *instruction, r regalloc.VReg, off int64) *instruction {
	if r.RegType() == regalloc.RegTypeVec {
		var vm *addressMode
		cur, vm = m.resolveAddressModeForOffsetAndInsert(cur, off, spVReg, true)
		return linkInstr(cur, m.allocateInstr().asVecLoad(r, vm))
	}
	var amode *addressMode
	cur, amode = m.resolveAddressModeForOffsetAndInsert(cur, off, spVReg, true)
	load := m.allocateInstr()
	if r.RegType() == regalloc.RegTypeInt {
		load.asLoad(r, amode, 64, false)
	} else {
		load.asFpuLoad(r, amode, 64)
	}
	return linkInstr(cur, load)
}

// addsAddOrSubStackPointer adjusts SP by diff. Unlike arm64, SP is an ordinary
// register on RISC-V, so this is a plain add with no special encoding.
func (m *machine) addsAddOrSubStackPointer(cur *instruction, diff int64, increment bool) *instruction {
	if !increment {
		diff = -diff
	}
	if fitsInSignedImm12(diff) {
		alu := m.allocateInstr()
		alu.asALU(aluOpAdd, spVReg, operandNR(spVReg), operandImm(diff), true)
		return linkInstr(cur, alu)
	}
	cur = m.lowerConstantI64AndInsert(cur, tmpRegVReg, diff)
	alu := m.allocateInstr()
	alu.asALU(aluOpAdd, spVReg, operandNR(spVReg), operandNR(tmpRegVReg), true)
	return linkInstr(cur, alu)
}

// lowerConstantI64AndInsert materializes v into rd in the post-regalloc world,
// where instructions are linked rather than queued.
func (m *machine) lowerConstantI64AndInsert(cur *instruction, rd regalloc.VReg, v int64) *instruction {
	m.pendingInstructions = m.pendingInstructions[:0]
	m.lowerConstantI64(rd, v)
	for _, instr := range m.pendingInstructions {
		cur = linkInstr(cur, instr)
	}
	m.pendingInstructions = m.pendingInstructions[:0]
	return cur
}

func (m *machine) createReturnAddrAndSizeOfArgRetSlot(cur *instruction) *instruction {
	// Step SP down over the argument area first, so the return address lands
	// immediately below arg 0.
	s := int64(m.currentABI.AlignedArgResultStackSlotSize())
	sizeOfArgRetReg := zeroVReg
	if s > 0 {
		cur = m.lowerConstantI64AndInsert(cur, tmpRegVReg, s)
		sizeOfArgRetReg = tmpRegVReg
		sub := m.allocateInstr()
		sub.asALU(aluOpSub, spVReg, operandNR(spVReg), operandNR(sizeOfArgRetReg), true)
		cur = linkInstr(cur, sub)
	}

	// addi sp, sp, -16 ; sd ra, 0(sp) ; sd size_of_arg_ret, 8(sp)
	cur = m.addsAddOrSubStackPointer(cur, 16, false)
	cur = m.storeRegToSPOffset(cur, raVReg, 0)
	return m.storeRegToSPOffset(cur, sizeOfArgRetReg, 8)
}

func (m *machine) createFrameSizeSlot(cur *instruction, s int64) *instruction {
	frameSizeReg := zeroVReg
	if s > 0 {
		cur = m.lowerConstantI64AndInsert(cur, tmpRegVReg, s)
		frameSizeReg = tmpRegVReg
	}
	// 16 rather than 8 to keep SP 16-byte aligned.
	cur = m.addsAddOrSubStackPointer(cur, 16, false)
	return m.storeRegToSPOffset(cur, frameSizeReg, 0)
}

// postRegAlloc walks the instruction list to insert epilogues, expand the
// block-argument constant placeholders, and drop copies that became no-ops
// once registers were assigned.
func (m *machine) postRegAlloc() {
	for cur := m.rootInstr; cur != nil; cur = cur.next {
		switch cur.kind {
		case ret:
			m.setupEpilogueAfter(cur.prev)
		case tailCall, tailCallInd:
			m.setupEpilogueAfter(cur.prev)
			// A proper tail call never returns here, so the trailing
			// instructions are dead. See internal/engine/RATIONALE.md.
			m.removeUntilRet(cur.next)
		case loadConstBlockArg:
			lc := cur
			next := lc.next
			m.pendingInstructions = m.pendingInstructions[:0]
			m.lowerLoadConstantBlockArgAfterRegAlloc(lc)
			for _, instr := range m.pendingInstructions {
				cur = linkInstr(cur, instr)
			}
			linkInstr(cur, next)
			m.pendingInstructions = m.pendingInstructions[:0]
		default:
			if cur.IsCopy() && cur.rs1.realReg() == cur.rd.RealReg() {
				prev, next := cur.prev, cur.next
				prev.next = next
				if next != nil {
					next.prev = prev
				}
			}
		}
	}
}

func (m *machine) setupEpilogueAfter(cur *instruction) {
	prevNext := cur.next

	// Drop the frame-size word.
	cur = m.addsAddOrSubStackPointer(cur, 16, true)

	if s := m.spillSlotSize; s > 0 {
		cur = m.addsAddOrSubStackPointer(cur, s, true)
	}

	if regs := m.clobberedRegs; len(regs) > 0 {
		for i, r := range regs {
			cur = m.loadRegFromSPOffset(cur, r, int64(i)*8)
		}
		cur = m.addsAddOrSubStackPointer(cur, m.clobberedRegSlotSize(), true)
	}

	// Reload the return address and drop its slot.
	cur = m.loadRegFromSPOffset(cur, raVReg, 0)
	cur = m.addsAddOrSubStackPointer(cur, 16, true)

	if s := int64(m.currentABI.AlignedArgResultStackSlotSize()); s > 0 {
		cur = m.addsAddOrSubStackPointer(cur, s, true)
	}

	linkInstr(cur, prevNext)
}

// removeUntilRet unlinks instructions from cur up to and including the next ret.
func (m *machine) removeUntilRet(cur *instruction) {
	for ; cur != nil; cur = cur.next {
		prev, next := cur.prev, cur.next
		prev.next = next
		if next != nil {
			next.prev = prev
		}
		if cur.kind == ret {
			return
		}
	}
}

// canSkipStackBoundsCheck reports whether this function's prologue may omit the
// stack-overflow check.
//
// A leaf -- one that makes no call of any kind, so maxRequiredStackSizeForCalls
// stays zero -- whose frame fits inside the margin its caller already reserved
// below it cannot overflow, and never re-establishes that margin for a callee
// of its own because it has none. See backend/go_call.go for the invariant.
func (m *machine) canSkipStackBoundsCheck() bool {
	return m.maxRequiredStackSizeForCalls == 0 &&
		m.frameSize()+backend.LeafStackCheckHeadroomBytes <= backend.StackBoundsCheckMarginBytes
}

// insertStackBoundsCheck emits the prologue's overflow check.
//
// RISC-V has no condition flags, so where arm64 does a flag-setting subtract
// and branches on the result, this compares the two pointers directly with a
// single unsigned branch.
func (m *machine) insertStackBoundsCheck(requiredStackSize int64, cur *instruction) *instruction {
	if requiredStackSize%16 != 0 {
		panic("BUG: required stack size must be 16-byte aligned")
	}

	// tmp = sp - requiredStackSize
	if fitsInSignedImm12(-requiredStackSize) {
		sub := m.allocateInstr()
		sub.asALU(aluOpAdd, tmpRegVReg, operandNR(spVReg), operandImm(-requiredStackSize), true)
		cur = linkInstr(cur, sub)
	} else {
		cur = m.lowerConstantI64AndInsert(cur, tmpRegVReg, requiredStackSize)
		sub := m.allocateInstr()
		sub.asALU(aluOpSub, tmpRegVReg, operandNR(spVReg), operandNR(tmpRegVReg), true)
		cur = linkInstr(cur, sub)
	}

	// tmp2 = execCtx.stackBottomPtr. tmpReg2 is reserved, so unlike arm64 this
	// needs no borrowed caller-saved register.
	ldr := m.allocateInstr()
	amode := m.amodePool.Allocate()
	*amode = addressMode{
		kind: addressModeKindRegSignedImm12,
		rn:   x10VReg, // the execution context is always the first argument.
		imm:  nativeapi.ExecutionContextOffsetStackBottomPtr.I64(),
	}
	ldr.asLoad(tmpReg2VReg, amode, 64, false)
	cur = linkInstr(cur, ldr)

	// Enough room if tmp >= tmp2, compared as unsigned addresses.
	afterGrow, afterGrowLabel := m.allocateBrTarget()
	brOK := m.allocateInstr()
	brOK.asCondBr(condGEU, operandNR(tmpRegVReg), operandNR(tmpReg2VReg), afterGrowLabel)
	cur = linkInstr(cur, brOK)

	// Record how much is needed and hand over to the stack-grow builtin.
	cur = m.lowerConstantI64AndInsert(cur, tmpRegVReg, requiredStackSize)
	setSize := m.allocateInstr()
	amode2 := m.amodePool.Allocate()
	*amode2 = addressMode{
		kind: addressModeKindRegSignedImm12,
		rn:   x10VReg, imm: nativeapi.ExecutionContextOffsetStackGrowRequiredSize.I64(),
	}
	setSize.asStore(tmpRegVReg, amode2, 64, true)
	cur = linkInstr(cur, setSize)

	// Hand over to the shared stack-grow sequence rather than exiting to Go
	// from here.
	//
	// The difference is not stylistic. CompileStackGrowCallSequence saves and
	// restores every register the allocator can hand out; exiting inline saves
	// nothing, so the grow returns with the incoming arguments destroyed --
	// including a1, the module context. Nothing fails at that point either:
	// execution continues with a garbage module context, and the next thing to
	// read through it reports something unrelated. Here that was a
	// call_indirect whose table length came back wrong, failing an infinite
	// recursion with "invalid table access" where "stack overflow" belonged.
	ldrTrampoline := m.allocateInstr()
	amode3 := m.amodePool.Allocate()
	*amode3 = addressMode{
		kind: addressModeKindRegSignedImm12,
		rn:   x10VReg, // the execution context is always the first argument.
		imm:  nativeapi.ExecutionContextOffsetStackGrowCallTrampolineAddress.I64(),
	}
	ldrTrampoline.asLoad(tmpRegVReg, amode3, 64, false)
	cur = linkInstr(cur, ldrTrampoline)

	call := m.allocateInstr()
	call.asCallIndirect(tmpRegVReg, nil)
	cur = linkInstr(cur, call)

	cur = linkInstr(cur, afterGrow)
	// Measure the skip here rather than leaving it to resolveRelativeAddresses.
	// A function prologue does go through that pass, but
	// CompileGoFunctionTrampoline does not -- and an unresolved conditional
	// branch encodes a displacement of zero, which on RISC-V is a branch to
	// itself. The trampoline for a signature large enough to need this check
	// spun forever, and only a signature that large reaches it. Where the pass
	// does run it recomputes the same distance from the label.
	resolveCondBrToAfter(brOK, cur)
	return cur
}

// resolveCondBrToAfter writes into br the displacement that lands just past
// last.
//
// It iterates because growing a branch to reach a distant target also moves the
// target: each pass can only raise the expansion level, which is capped, so it
// settles.
func resolveCondBrToAfter(br, last *instruction) {
	for {
		var off int64
		for cur := br; ; cur = cur.next {
			off += cur.size()
			if cur == last {
				break
			}
		}
		if want := condBrExpansionFor(off, br.condBrExpansion()); want != br.condBrExpansion() {
			br.setCondBrExpansion(want)
			continue
		}
		br.condBrOffsetResolve(off)
		return
	}
}

// insertExitSequence emits the "set the exit code, record SP and the resume
// address, then hand control back to Go" sequence.
func (m *machine) insertExitSequence(cur *instruction, execCtx regalloc.VReg, code nativeapi.ExitCode) *instruction {
	cur = m.lowerConstantI64AndInsert(cur, tmpRegVReg, int64(code))
	cur = m.storeToExecCtx(cur, execCtx, tmpRegVReg, nativeapi.ExecutionContextOffsetExitCodeOffset.I64(), 32)

	// The stack pointer, so the frame can be unwound from Go.
	mv := m.allocateInstr()
	mv.asMove64(tmpRegVReg, spVReg)
	cur = linkInstr(cur, mv)
	cur = m.storeToExecCtx(cur, execCtx, tmpRegVReg, nativeapi.ExecutionContextOffsetStackPointerBeforeGoCall.I64(), 64)

	// The address to resume at, which is the instruction just past the exit
	// sequence: adr covers the auipc+addi pair and the exit sequence itself.
	adr := m.allocateInstr()
	adr.asAdrPCRel(tmpRegVReg, goExitResumeOffsetFromAdr)
	cur = linkInstr(cur, adr)
	cur = m.storeToExecCtx(cur, execCtx, tmpRegVReg, nativeapi.ExecutionContextOffsetGoCallReturnAddress.I64(), 64)

	exit := m.allocateInstr()
	exit.asExitSequence(execCtx)
	return linkInstr(cur, exit)
}

// storeToExecCtx writes one register into the execution context.
//
// The store follows src's register class. Hardcoding the integer form here --
// which it did -- makes a float register's *number* name an integer register
// instead, so `sd f8` stored x8. That is how saveRegistersInExecutionContext
// came to save none of the twelve callee-saved float registers while
// restoreRegistersInExecutionContext dutifully reloaded all twelve, handing
// the compiled code an integer bit pattern where an f64 had been. It took a
// function with enough live floats across a Go call to notice: twenty f64 and
// twenty f32 results live across a listener.
func (m *machine) storeToExecCtx(cur *instruction, execCtx, src regalloc.VReg, off int64, bits byte) *instruction {
	amode := m.amodePool.Allocate()
	*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: execCtx, imm: off}
	store := m.allocateInstr()
	store.asStore(src, amode, bits, src.RegType() == regalloc.RegTypeInt)
	return linkInstr(cur, store)
}

// emitTrapIslands materializes the shared trap islands allocated during
// lowering, after the function body.
//
// An island is only ever branched to and never returns, so it can be emitted
// after register allocation using real registers alone: the execution context
// arrives in the reserved tmpReg, and tmpReg2 serves as scratch.
func (m *machine) emitTrapIslands() {
	if len(m.trapIslands) == 0 {
		return
	}

	lastPos := m.orderedSSABlockLabelPos[len(m.orderedSSABlockLabelPos)-1]
	cur := lastPos.end
	for cur.next != nil {
		cur = cur.next
	}

	for _, ti := range m.trapIslands {
		nop := m.allocateInstr()
		nop.asNop0WithLabel(ti.l)
		pos := m.labelPositionPool.GetOrAllocate(int(ti.l))
		pos.begin, pos.end = nop, nop
		cur = linkInstr(cur, nop)

		cur = m.lowerConstantI64AndInsert(cur, tmpReg2VReg, int64(ti.code))
		cur = m.storeToExecCtx(cur, tmpRegVReg, tmpReg2VReg, nativeapi.ExecutionContextOffsetExitCodeOffset.I64(), 32)

		mv := m.allocateInstr()
		mv.asMove64(tmpReg2VReg, spVReg)
		cur = linkInstr(cur, mv)
		cur = m.storeToExecCtx(cur, tmpRegVReg, tmpReg2VReg, nativeapi.ExecutionContextOffsetStackPointerBeforeGoCall.I64(), 64)

		// The island's own address: inside the function that trapped, which is
		// all the backtracer needs. See lowerExitWithCode.
		adr := m.allocateInstr()
		adr.asAdrPCRel(tmpReg2VReg, 0)
		cur = linkInstr(cur, adr)
		cur = m.storeToExecCtx(cur, tmpRegVReg, tmpReg2VReg, nativeapi.ExecutionContextOffsetGoCallReturnAddress.I64(), 64)

		exit := m.allocateInstr()
		exit.asExitSequence(tmpRegVReg)
		cur = linkInstr(cur, exit)
	}

	lastPos.end = cur
}

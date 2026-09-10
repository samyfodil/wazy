package riscv64

import (
	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// calleeSavedRegistersSorted is the set compiled code must preserve across a
// call back into Go, in the order their slots are laid out in
// native.executionContext.savedRegisters.
//
// This order is load-bearing: afterThrowTransferEntrypoint in
// abi_entry_riscv64.s restores the same registers with hard-coded offsets, so
// the two must agree exactly. Slots are 8 bytes because every RV64G register
// is 64 bits -- there is no wider vector file to pad for, unlike arm64.
//
// x27 is deliberately absent. It is Go's g, and this backend never allocates
// it, so there is nothing to preserve.
var calleeSavedRegistersSorted = []regalloc.VReg{
	x8VReg, x9VReg, x18VReg, x19VReg, x20VReg, x21VReg, x22VReg, x23VReg, x24VReg, x25VReg, x26VReg,
	f8VReg, f9VReg, f18VReg, f19VReg, f20VReg, f21VReg, f22VReg, f23VReg, f24VReg, f25VReg, f26VReg, f27VReg,
}

// savedRegisterOffset returns the execution-context offset of the i-th entry
// of calleeSavedRegistersSorted. Kept as one function so the save, the restore
// and the assembly's hard-coded offsets cannot drift apart silently.
func savedRegisterOffset(i int) int64 {
	return nativeapi.ExecutionContextOffsetSavedRegistersBegin.I64() + int64(i)*8
}

func init() {
	// The saved area must actually hold what we intend to put in it.
	if last := savedRegisterOffset(len(calleeSavedRegistersSorted) - 1); last+8 > nativeapi.ExecutionContextOffsetSavedRegistersEnd.I64() {
		panic("BUG: riscv64 callee-saved registers overflow executionContext.savedRegisters")
	}
}

// CompileGoFunctionTrampoline implements backend.Machine.
//
// The stack it builds matches the other backends:
//
//	              (high address)
//	SP ------> +-----------------+  <----+
//	           |     .......     |       |
//	           |      ret 0      |       |
//	           |      arg X      |       |  size_of_arg_ret
//	           |      arg 0      |  <----+ <-- originalArg0Reg
//	           | size_of_arg_ret |
//	           |  ReturnAddress  |
//	           +-----------------+ <----+
//	      +--->|  arg[N]/ret[M]  |      |
//	 slice|    |   ............  |      | goCallStackSize
//	      +--->|  arg[0]/ret[0]  | <----+ <-- arg0ret0AddrReg
//	           |    sliceSize    |
//	           |   frame_size    |
//	           +-----------------+
//	              (low address)
//
// The arg[0]..ret[M] region is what the Go side reads as a []uint64.
func (m *machine) CompileGoFunctionTrampoline(exitCode nativeapi.ExitCode, sig *ssa.Signature, needModuleContextPtr bool) []byte {
	argBegin := 1 // Skip the execution context.
	if needModuleContextPtr {
		argBegin++
	}

	abi := &backend.FunctionABI{}
	abi.Init(sig, intParamResultRegs, floatParamResultRegs, regalloc.RegTypeVec)
	m.currentABI = abi

	cur := m.allocateInstr()
	cur.asNop0()
	m.rootInstr = cur

	execCtrPtr := x10VReg // the execution context is always the first argument.

	cur = m.createReturnAddrAndSizeOfArgRetSlot(cur)

	const frameInfoSize = 16 // frame_size + sliceSize.

	goCallStackSize, sliceSizeInBytes := backend.GoFunctionCallRequiredStackSize(sig, argBegin)
	// Every wasm frame's prologue check reserves StackBoundsCheckMarginBytes of
	// slack below its own frame base at each call site, so when this
	// trampoline's own usage fits inside that margin it cannot underflow the
	// buffer and the check can be skipped. Only huge-arity signatures pay it.
	if requiredStackSizeForGoCall := goCallStackSize + frameInfoSize; requiredStackSizeForGoCall > backend.StackBoundsCheckMarginBytes {
		cur = m.insertStackBoundsCheck(requiredStackSizeForGoCall, cur)
	}

	// Caller-saved registers, free for the trampoline's own bookkeeping.
	originalArg0Reg := x7VReg  // t2
	arg0ret0AddrReg := x28VReg // t3
	intTmp := x29VReg          // t4
	floatTmp := f1VReg         // ft1

	if m.currentABI.AlignedArgResultStackSlotSize() > 0 {
		// SP points at ReturnAddress, so arg 0 is frameInfoSize above it.
		cur = m.addRegImm(cur, originalArg0Reg, spVReg, frameInfoSize)
	}

	cur = m.saveRegistersInExecutionContext(cur, calleeSavedRegistersSorted)

	if needModuleContextPtr {
		// The module context is always the second argument.
		cur = m.storeToExecCtx(cur, execCtrPtr, x11VReg,
			nativeapi.ExecutionContextOffsetGoFunctionCallCalleeModuleContextOpaque.I64(), 64)
	}

	cur = m.addsAddOrSubStackPointer(cur, goCallStackSize, false)

	copySp := m.allocateInstr()
	copySp.asMove64(arg0ret0AddrReg, spVReg)
	cur = linkInstr(cur, copySp)

	// Write the arguments out in the flat []uint64 layout Go expects.
	for i := range abi.Args[argBegin:] {
		arg := &abi.Args[argBegin+i]
		var v regalloc.VReg
		if arg.Kind == backend.ABIArgKindReg {
			v = arg.Reg
		} else {
			cur, v = m.goFunctionCallLoadStackArg(cur, originalArg0Reg, arg, intTmp, floatTmp)
		}
		store := m.allocateInstr()
		amode := m.amodePool.Allocate()
		*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: arg0ret0AddrReg, imm: 0}
		store.asStore(v, amode, 64, arg.Type.IsInt())
		cur = linkInstr(cur, store)
		// No post-index addressing on RISC-V; advance explicitly.
		cur = m.addRegImm(cur, arg0ret0AddrReg, arg0ret0AddrReg, goCallSlotSize(arg.Type))
	}

	// Push frame_size and sliceSize below arg[0].
	frameSizeReg, sliceSizeReg := zeroVReg, zeroVReg
	if goCallStackSize > 0 {
		cur = m.lowerConstantI64AndInsert(cur, tmpRegVReg, goCallStackSize)
		frameSizeReg = tmpRegVReg
		cur = m.lowerConstantI64AndInsert(cur, tmpReg2VReg, sliceSizeInBytes/8)
		sliceSizeReg = tmpReg2VReg
	}
	cur = m.addsAddOrSubStackPointer(cur, 16, false)
	cur = m.storeRegToSPOffset(cur, frameSizeReg, 0)
	cur = m.storeRegToSPOffset(cur, sliceSizeReg, 8)

	// Hand control back to Go.
	cur = m.insertExitSequence(cur, execCtrPtr, exitCode)

	// --- resumed here after the Go call returns ---

	cur = m.restoreRegistersInExecutionContext(cur, calleeSavedRegistersSorted)

	if len(abi.Rets) > 0 {
		cur = m.addRegImm(cur, arg0ret0AddrReg, spVReg, frameInfoSize)
	}

	// Step SP back up to ReturnAddress and reload it.
	cur = m.addsAddOrSubStackPointer(cur, frameInfoSize+goCallStackSize, true)
	cur = m.loadRegFromSPOffset(cur, raVReg, 0)
	cur = m.addsAddOrSubStackPointer(cur, 16, true)

	originalRet0Reg := x7VReg // t2, reused now that the args are consumed.
	if m.currentABI.RetStackSize > 0 {
		cur = m.addRegImm(cur, originalRet0Reg, spVReg, m.currentABI.ArgStackSize)
	}

	if s := int64(m.currentABI.AlignedArgResultStackSlotSize()); s > 0 {
		cur = m.addsAddOrSubStackPointer(cur, s, true)
	}

	// Read the results back out of the flat slice.
	for i := range abi.Rets {
		r := &abi.Rets[i]
		amode := m.amodePool.Allocate()
		*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: arg0ret0AddrReg, imm: 0}
		load := m.allocateInstr()
		dst := r.Reg
		if r.Kind != backend.ABIArgKindReg {
			if r.Type.IsInt() {
				dst = intTmp
			} else {
				dst = floatTmp
			}
		}
		if r.Type.IsInt() {
			// An i32 result must arrive sign-extended like every other i32.
			load.asLoad(dst, amode, r.Type.Bits(), r.Type == ssa.TypeI32)
		} else {
			load.asFpuLoad(dst, amode, r.Type.Bits())
		}
		cur = linkInstr(cur, load)
		cur = m.addRegImm(cur, arg0ret0AddrReg, arg0ret0AddrReg, goCallSlotSize(r.Type))

		if r.Kind != backend.ABIArgKindReg {
			cur = m.goFunctionCallStoreStackResult(cur, originalRet0Reg, r, dst)
		}
	}

	ret := m.allocateInstr()
	ret.asRet()
	linkInstr(cur, ret)

	m.encode(m.rootInstr)
	return m.compiler.Buf()
}

// goCallSlotSize is the stride of one entry in the Go-visible []uint64.
func goCallSlotSize(typ ssa.Type) int64 {
	if typ == ssa.TypeV128 {
		return 16
	}
	return 8
}

func (m *machine) addRegImm(cur *instruction, rd, rn regalloc.VReg, imm int64) *instruction {
	if fitsInSignedImm12(imm) {
		alu := m.allocateInstr()
		alu.asALU(aluOpAdd, rd, operandNR(rn), operandImm(imm), true)
		return linkInstr(cur, alu)
	}
	cur = m.lowerConstantI64AndInsert(cur, rd, imm)
	alu := m.allocateInstr()
	alu.asALU(aluOpAdd, rd, operandNR(rd), operandNR(rn), true)
	return linkInstr(cur, alu)
}

func (m *machine) goFunctionCallLoadStackArg(cur *instruction, originalArg0Reg regalloc.VReg, arg *backend.ABIArg, intTmp, floatTmp regalloc.VReg) (*instruction, regalloc.VReg) {
	dst := intTmp
	load := m.allocateInstr()
	amode := m.amodePool.Allocate()
	*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: originalArg0Reg, imm: 0}
	if arg.Type.IsInt() {
		load.asLoad(dst, amode, arg.Type.Bits(), arg.Type == ssa.TypeI32)
	} else {
		dst = floatTmp
		load.asFpuLoad(dst, amode, arg.Type.Bits())
	}
	cur = linkInstr(cur, load)
	cur = m.addRegImm(cur, originalArg0Reg, originalArg0Reg, goCallSlotSize(arg.Type))
	return cur, dst
}

func (m *machine) goFunctionCallStoreStackResult(cur *instruction, originalRet0Reg regalloc.VReg, result *backend.ABIArg, resultVReg regalloc.VReg) *instruction {
	store := m.allocateInstr()
	amode := m.amodePool.Allocate()
	*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: originalRet0Reg, imm: 0}
	store.asStore(resultVReg, amode, result.Type.Bits(), result.Type.IsInt())
	cur = linkInstr(cur, store)
	return m.addRegImm(cur, originalRet0Reg, originalRet0Reg, goCallSlotSize(result.Type))
}

func (m *machine) saveRegistersInExecutionContext(cur *instruction, regs []regalloc.VReg) *instruction {
	for i, r := range regs {
		cur = m.storeToExecCtx(cur, x10VReg, r, savedRegisterOffset(i), 64)
	}
	return cur
}

func (m *machine) restoreRegistersInExecutionContext(cur *instruction, regs []regalloc.VReg) *instruction {
	for i, r := range regs {
		amode := m.amodePool.Allocate()
		*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: x10VReg, imm: savedRegisterOffset(i)}
		load := m.allocateInstr()
		if r.RegType() == regalloc.RegTypeInt {
			load.asLoad(r, amode, 64, false)
		} else {
			load.asFpuLoad(r, amode, 64)
		}
		cur = linkInstr(cur, load)
	}
	return cur
}

// saveRequiredRegs is what must survive a stack grow: the callee-saved set plus
// the argument registers, excluding a0, which always holds the execution
// context whenever native code is entered from Go.
var saveRequiredRegs = append([]regalloc.VReg{
	x1VReg, x5VReg, x6VReg, x7VReg,
	x11VReg, x12VReg, x13VReg, x14VReg, x15VReg, x16VReg, x17VReg,
	x28VReg, x29VReg,
	f0VReg, f1VReg, f2VReg, f3VReg, f4VReg, f5VReg, f6VReg, f7VReg,
	f10VReg, f11VReg, f12VReg, f13VReg, f14VReg, f15VReg, f16VReg, f17VReg,
	f28VReg, f29VReg, f30VReg,
}, calleeSavedRegistersSorted...)

func init() {
	// Everything the allocator can hand out must be preserved across a grow,
	// bar the execution context itself. A register missing here is not a crash
	// but a silently wrong value after the stack moves.
	saved := make(map[regalloc.VReg]bool, len(saveRequiredRegs))
	for _, r := range saveRequiredRegs {
		saved[r] = true
	}
	for class, regs := range regInfo.AllocatableRegisters {
		if regalloc.RegType(class) == regalloc.RegTypeVec {
			// Not yet, and not by oversight. A v128 cannot reach the backend
			// while the platform gate withholds SIMD, so no vector register is
			// ever allocated today. It also could not be saved here if it
			// were: executionContext.savedRegisters has 512 bytes, of which
			// the integer and float sets already use most, and 30 vector
			// registers need 480 more. Enabling RVV means enlarging that area
			// or spilling vectors elsewhere -- a decision for that work, which
			// this check exists to force rather than let slip.
			continue
		}
		for _, r := range regs {
			v := regInfo.RealRegToVReg[r]
			if v != x10VReg && !saved[v] {
				panic("BUG: allocatable register " + regNames[r] + " is not preserved across a stack grow")
			}
		}
	}
}

// CompileStackGrowCallSequence implements backend.Machine.
func (m *machine) CompileStackGrowCallSequence() []byte {
	cur := m.allocateInstr()
	cur.asNop0()
	m.rootInstr = cur

	// Save every register the Go side may clobber, then exit with the
	// grow-stack code; the engine re-enters through
	// afterGoFunctionCallEntrypoint once the new stack is in place.
	cur = m.saveRegistersInExecutionContext(cur, saveRequiredRegs)
	cur = m.insertExitSequence(cur, x10VReg, nativeapi.ExitCodeGrowStack)
	cur = m.restoreRegistersInExecutionContext(cur, saveRequiredRegs)

	ret := m.allocateInstr()
	ret.asRet()
	linkInstr(cur, ret)

	m.encode(m.rootInstr)
	return m.compiler.Buf()
}

// CompileThrowTransferRegisterRestore implements backend.Machine.
//
// riscv64 does not use this blob: afterThrowTransferEntrypoint inlines the
// restore in assembly instead, for the reason spelled out there -- calling out
// to a blob would leave that SP-writing NOSPLIT entrypoint on the stack as a
// non-innermost frame, which the Go runtime's traceback refuses to unwind
// through if a signal lands while the blob is running. The method is still
// implemented, and correctly, so the Machine interface stays uniform and so
// that anything which does call it gets working code rather than a stub.
func (m *machine) CompileThrowTransferRegisterRestore() []byte {
	cur := m.allocateInstr()
	cur.asNop0()
	m.rootInstr = cur

	cur = m.restoreRegistersInExecutionContext(cur, calleeSavedRegistersSorted)

	ret := m.allocateInstr()
	ret.asRet()
	linkInstr(cur, ret)

	m.encode(m.rootInstr)
	return m.compiler.Buf()
}

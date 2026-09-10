package riscv64

import (
	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// CompileEntryPreamble implements backend.Machine.
//
// This assumes the `entrypoint` function in abi_entry_riscv64.s has already
// placed:
//
//  1. the execution context pointer in a0 and the module context pointer in a1,
//     which are also where the compiled function expects its first two
//     arguments;
//  2. the param/result slice pointer in s2 -- the []uint64 arguments arrive in
//     and results are written back to;
//  3. the Go-allocated wasm stack pointer in s4;
//  4. the compiled function's address in s6.
//
// s2/s4/s6 are callee-saved so they survive the call this preamble makes. SP
// and s0 hold Go's own values on entry and ra the return address into Go.
func (m *machine) CompileEntryPreamble(signature *ssa.Signature) []byte {
	root := m.constructEntryPreamble(signature)
	m.encode(root)
	return m.compiler.Buf()
}

var (
	executionContextPtrReg = x10VReg // a0
	// Callee-saved, so that they survive into the epilogue.
	paramResultSlicePtr      = x18VReg // s2
	savedExecutionContextPtr = x19VReg // s3
	goAllocatedStackPtr      = x20VReg // s4
	paramResultSliceCopied   = x21VReg // s5
	functionExecutable       = x22VReg // s6
	// Go's frame pointer on riscv64, which this backend allocates and so must
	// save and restore across the boundary.
	goFramePointer = x8VReg // s0
	// Caller-saved staging registers for stack-passed arguments and results.
	entryIntScratch   = x6VReg // t1
	entryFloatScratch = f0VReg // ft0
)

func (m *machine) constructEntryPreamble(sig *ssa.Signature) (root *instruction) {
	abi := backend.FunctionABI{}
	abi.Init(sig, intParamResultRegs, floatParamResultRegs, regalloc.RegTypeVec)

	root = m.allocateNop()

	// ---------------------------- prologue ----------------------------

	// Keep the execution context somewhere callee-saved: a0 is about to become
	// the callee's first argument register.
	cur := m.move64(savedExecutionContextPtr, executionContextPtrReg, root)

	// Hand Go's frame pointer, stack pointer and return address to the
	// execution context, so the exit sequence can put them back. SP is an
	// ordinary register here, so unlike arm64 it is stored directly.
	cur = m.storeAtExecutionContext(goFramePointer, nativeapi.ExecutionContextOffsetOriginalFramePointer, cur)
	cur = m.storeAtExecutionContext(spVReg, nativeapi.ExecutionContextOffsetOriginalStackPointer, cur)
	cur = m.storeAtExecutionContext(raVReg, nativeapi.ExecutionContextOffsetGoReturnAddress, cur)

	// Switch to the Go-allocated wasm stack.
	cur = m.move64(spVReg, goAllocatedStackPtr, cur)

	prReg := paramResultSlicePtr
	if len(abi.Args) > 2 && len(abi.Rets) > 0 {
		// passArg walks the slice pointer forward, so keep the original for
		// the epilogue to write results back through.
		cur = m.move64(paramResultSliceCopied, paramResultSlicePtr, cur)
		prReg = paramResultSliceCopied
	}

	stackSlotSize := int64(abi.AlignedArgResultStackSlotSize())
	for i := range abi.Args {
		if i < 2 {
			// The execution and module context pointers are already in a0/a1.
			continue
		}
		cur = m.goEntryPreamblePassArg(cur, prReg, &abi.Args[i], -stackSlotSize)
	}

	call := m.allocateInstr()
	call.asCallIndirect(functionExecutable, &abi)
	cur = linkInstr(cur, call)

	// ---------------------------- epilogue ----------------------------

	for i := range abi.Rets {
		cur = m.goEntryPreamblePassResult(cur, paramResultSlicePtr, &abi.Rets[i], abi.ArgStackSize-stackSlotSize)
	}

	cur = m.loadFromExecutionContext(goFramePointer, nativeapi.ExecutionContextOffsetOriginalFramePointer, cur)
	cur = m.loadFromExecutionContext(raVReg, nativeapi.ExecutionContextOffsetGoReturnAddress, cur)
	// SP last: the loads above address memory through the saved context
	// register, not through SP, but restoring it earlier would still leave the
	// remaining loads reading from Go's stack rather than the wasm one.
	cur = m.loadFromExecutionContext(spVReg, nativeapi.ExecutionContextOffsetOriginalStackPointer, cur)

	retInst := m.allocateInstr()
	retInst.asRet()
	linkInstr(cur, retInst)
	return
}

// goEntryPreamblePassArg loads one argument out of the []uint64 the Go caller
// supplied and puts it where the ABI says the callee will look.
func (m *machine) goEntryPreamblePassArg(cur *instruction, paramSlicePtr regalloc.VReg, arg *backend.ABIArg, argStartOffsetFromSP int64) *instruction {
	typ := arg.Type
	isStackArg := arg.Kind == backend.ABIArgKindStack

	var dst regalloc.VReg
	if !isStackArg {
		dst = arg.Reg
	} else if typ.IsInt() {
		dst = entryIntScratch
	} else {
		dst = entryFloatScratch
	}

	// Load from the slice. There is no post-index addressing on RISC-V, so the
	// pointer is advanced explicitly afterwards.
	amode := m.amodePool.Allocate()
	*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: paramSlicePtr, imm: 0}
	load := m.allocateInstr()
	switch typ {
	case ssa.TypeI32:
		// The slice element is a full uint64 but only its low half is the
		// value; load it as a word so it arrives sign-extended, which is the
		// form every other i32 in this backend has.
		load.asLoad(dst, amode, 32, true)
	case ssa.TypeI64:
		load.asLoad(dst, amode, 64, false)
	case ssa.TypeF32:
		load.asFpuLoad(dst, amode, 32)
	case ssa.TypeF64:
		load.asFpuLoad(dst, amode, 64)
	default:
		panic("BUG: unsupported argument type in the entry preamble: " + typ.String())
	}
	cur = linkInstr(cur, load)

	step := int64(8)
	if typ == ssa.TypeV128 {
		step = 16
	}
	adv := m.allocateInstr()
	adv.asALU(aluOpAdd, paramSlicePtr, operandNR(paramSlicePtr), operandImm(step), true)
	cur = linkInstr(cur, adv)

	if !isStackArg {
		return cur
	}

	// Stack-passed: write the staged value into the callee's argument area.
	var st *addressMode
	cur, st = m.resolveAddressModeForOffsetAndInsert(cur, argStartOffsetFromSP+arg.Offset, spVReg, true)
	store := m.allocateInstr()
	store.asStore(dst, st, typ.Bits(), typ.IsInt())
	return linkInstr(cur, store)
}

// goEntryPreamblePassResult is the mirror: take each result from where the ABI
// left it and write it back into the caller's []uint64.
func (m *machine) goEntryPreamblePassResult(cur *instruction, resultSlicePtr regalloc.VReg, result *backend.ABIArg, resultStartOffsetFromSP int64) *instruction {
	typ := result.Type
	isStackArg := result.Kind == backend.ABIArgKindStack

	var src regalloc.VReg
	if !isStackArg {
		src = result.Reg
	} else {
		if typ.IsInt() {
			src = entryIntScratch
		} else {
			src = entryFloatScratch
		}
		var ld *addressMode
		cur, ld = m.resolveAddressModeForOffsetAndInsert(cur, resultStartOffsetFromSP+result.Offset, spVReg, true)
		load := m.allocateInstr()
		if typ.IsInt() {
			load.asLoad(src, ld, typ.Bits(), typ == ssa.TypeI32)
		} else {
			load.asFpuLoad(src, ld, typ.Bits())
		}
		cur = linkInstr(cur, load)
	}

	amode := m.amodePool.Allocate()
	*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: resultSlicePtr, imm: 0}
	store := m.allocateInstr()
	store.asStore(src, amode, 64, typ.IsInt())
	cur = linkInstr(cur, store)

	step := int64(8)
	if typ == ssa.TypeV128 {
		step = 16
	}
	adv := m.allocateInstr()
	adv.asALU(aluOpAdd, resultSlicePtr, operandNR(resultSlicePtr), operandImm(step), true)
	return linkInstr(cur, adv)
}

func (m *machine) move64(dst, src regalloc.VReg, prev *instruction) *instruction {
	instr := m.allocateInstr()
	instr.asMove64(dst, src)
	return linkInstr(prev, instr)
}

func (m *machine) storeAtExecutionContext(d regalloc.VReg, offset nativeapi.Offset, prev *instruction) *instruction {
	mode := m.amodePool.Allocate()
	*mode = addressMode{kind: addressModeKindRegSignedImm12, rn: savedExecutionContextPtr, imm: offset.I64()}
	instr := m.allocateInstr()
	instr.asStore(d, mode, 64, true)
	return linkInstr(prev, instr)
}

func (m *machine) loadFromExecutionContext(d regalloc.VReg, offset nativeapi.Offset, prev *instruction) *instruction {
	mode := m.amodePool.Allocate()
	*mode = addressMode{kind: addressModeKindRegSignedImm12, rn: savedExecutionContextPtr, imm: offset.I64()}
	instr := m.allocateInstr()
	instr.asLoad(d, mode, 64, false)
	return linkInstr(prev, instr)
}

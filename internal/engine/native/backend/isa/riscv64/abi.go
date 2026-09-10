package riscv64

import (
	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// References:
// * https://github.com/riscv-non-isa/riscv-elf-psabi-doc/blob/master/riscv-cc.adoc
// * https://github.com/golang/go/blob/master/src/cmd/internal/obj/riscv/cpu.go (Go's reservations)

var (
	intParamResultRegs   = []regalloc.RealReg{x10, x11, x12, x13, x14, x15, x16, x17} // a0-a7
	floatParamResultRegs = []regalloc.RealReg{f10, f11, f12, f13, f14, f15, f16, f17} // fa0-fa7
)

var regInfo = &regalloc.RegisterInfo{
	AllocatableRegisters: [regalloc.NumRegType][]regalloc.RealReg{
		// We don't allocate:
		//  - x0 (zero): hardwired, not a storage location.
		//  - x2 (sp): the stack pointer.
		//  - x3 (gp), x4 (tp): reserved by the psABI; the Go runtime and the
		//    dynamic loader assume they are untouched.
		//  - x27 (s11): Go's goroutine pointer (g). Clobbering it corrupts the
		//    Go runtime the moment we return through the entry trampoline --
		//    the same reason arm64 leaves x28 alone.
		//  - x30, x31 (t5, t6): the two reserved scratch registers, see tmpReg.
		regalloc.RegTypeInt: {
			x5, x6, x7, // t0-t2
			x8, x9, // s0-s1
			x18, x19, x20, x21, x22, x23, x24, x25, x26, // s2-s10
			x28, // t3 (t4 is tmpReg3, see reg.go)
			// Clobbered by every call (jal writes it), so least preferred of
			// the non-argument registers.
			x1, // ra
			// These are the argument/return registers. Less preferred in the allocation.
			x17, x16, x15, x14, x13, x12, x11, x10, // a7..a0
		},
		regalloc.RegTypeFloat: {
			f0, f1, f2, f3, f4, f5, f6, f7, // ft0-ft7
			f8, f9, // fs0-fs1
			f18, f19, f20, f21, f22, f23, f24, f25, f26, f27, // fs2-fs11
			f28, f29, f30, // ft8-ft10 (ft11 = fpTmpReg)
			// These are the argument/return registers. Less preferred in the allocation.
			f17, f16, f15, f14, f13, f12, f11, f10, // fa7..fa0
		},
		// RVV. v0 is the architecturally fixed mask register and v31 is the
		// reserved scratch; the rest are free. v128 never travels in a
		// register across a call (FunctionABI puts it on the stack when the
		// ISA has a vector file), so there is no argument-register ordering
		// to respect here.
		regalloc.RegTypeVec: {
			v1, v2, v3, v4, v5, v6, v7, v8, v9, v10, v11, v12, v13, v14, v15,
			v16, v17, v18, v19, v20, v21, v22, v23, v24, v25, v26, v27, v28, v29, v30,
		},
	},
	CalleeSavedRegisters: regalloc.NewRegSet(
		x8, x9, x18, x19, x20, x21, x22, x23, x24, x25, x26, x27,
		f8, f9, f18, f19, f20, f21, f22, f23, f24, f25, f26, f27,
	),
	CallerSavedRegisters: regalloc.NewRegSet(
		x1, x5, x6, x7, x10, x11, x12, x13, x14, x15, x16, x17, x28, x29, x30, x31,
		f0, f1, f2, f3, f4, f5, f6, f7, f10, f11, f12, f13, f14, f15, f16, f17,
		f28, f29, f30, f31,
		// The RVV psABI makes the whole vector file caller-saved.
		v0, v1, v2, v3, v4, v5, v6, v7, v8, v9, v10, v11, v12, v13, v14, v15,
		v16, v17, v18, v19, v20, v21, v22, v23, v24, v25, v26, v27, v28, v29, v30, v31,
	),
	RealRegToVReg: []regalloc.VReg{
		x0: x0VReg, x1: x1VReg, x2: x2VReg, x3: x3VReg, x4: x4VReg, x5: x5VReg, x6: x6VReg, x7: x7VReg,
		x8: x8VReg, x9: x9VReg, x10: x10VReg, x11: x11VReg, x12: x12VReg, x13: x13VReg, x14: x14VReg,
		x15: x15VReg, x16: x16VReg, x17: x17VReg, x18: x18VReg, x19: x19VReg, x20: x20VReg, x21: x21VReg,
		x22: x22VReg, x23: x23VReg, x24: x24VReg, x25: x25VReg, x26: x26VReg, x27: x27VReg, x28: x28VReg,
		x29: x29VReg, x30: x30VReg, x31: x31VReg,
		f0: f0VReg, f1: f1VReg, f2: f2VReg, f3: f3VReg, f4: f4VReg, f5: f5VReg, f6: f6VReg, f7: f7VReg,
		f8: f8VReg, f9: f9VReg, f10: f10VReg, f11: f11VReg, f12: f12VReg, f13: f13VReg, f14: f14VReg,
		f15: f15VReg, f16: f16VReg, f17: f17VReg, f18: f18VReg, f19: f19VReg, f20: f20VReg, f21: f21VReg,
		f22: f22VReg, f23: f23VReg, f24: f24VReg, f25: f25VReg, f26: f26VReg, f27: f27VReg, f28: f28VReg,
		f29: f29VReg, f30: f30VReg, f31: f31VReg,
		v0: v0VReg, v1: v1VReg, v2: v2VReg, v3: v3VReg, v4: v4VReg, v5: v5VReg, v6: v6VReg, v7: v7VReg,
		v8: v8VReg, v9: v9VReg, v10: v10VReg, v11: v11VReg, v12: v12VReg, v13: v13VReg, v14: v14VReg, v15: v15VReg,
		v16: v16VReg, v17: v17VReg, v18: v18VReg, v19: v19VReg, v20: v20VReg, v21: v21VReg, v22: v22VReg, v23: v23VReg,
		v24: v24VReg, v25: v25VReg, v26: v26VReg, v27: v27VReg, v28: v28VReg, v29: v29VReg, v30: v30VReg, v31: v31VReg,
	},
	RealRegName: func(r regalloc.RealReg) string { return regNames[r] },
	RealRegType: func(r regalloc.RealReg) regalloc.RegType {
		switch {
		case r < f0:
			return regalloc.RegTypeInt
		case r < v0:
			return regalloc.RegTypeFloat
		default:
			return regalloc.RegTypeVec
		}
	},
}

// RV64's f-registers are 64 bits wide and cannot hold a v128, and RVV's
// v-registers are a separate file. So vectors get their own allocation class
// here, unlike every other backend, and v128 call arguments travel on the
// stack (see backend.FunctionABI.setABIArgs).
func (m *machine) V128RegType() regalloc.RegType { return regalloc.RegTypeVec }

// ArgsResultsRegs implements backend.Machine.
func (m *machine) ArgsResultsRegs() (argResultInts, argResultFloats []regalloc.RealReg) {
	return intParamResultRegs, floatParamResultRegs
}

// LowerParams implements backend.Machine.
func (m *machine) LowerParams(args []ssa.Value) {
	a := m.currentABI

	for i, ssaArg := range args {
		if !ssaArg.Valid() {
			continue
		}
		reg := m.compiler.VRegOf(ssaArg)
		arg := &a.Args[i]
		if arg.Kind == backend.ABIArgKindReg {
			m.InsertMove(reg, arg.Reg, arg.Type)
			continue
		}
		// Stack-passed. The distance from SP down to the argument area is not
		// known until the frame is laid out, so the address mode records the
		// argument-area offset and is fixed up in resolveAddressModes.
		amode := m.amodePool.Allocate()
		*amode = addressMode{kind: addressModeKindArgStackSpace, rn: spVReg, imm: arg.Offset}
		load := m.allocateInstr()
		switch arg.Type {
		case ssa.TypeI32:
			load.asLoad(reg, amode, 32, true /* signed: i32 values are kept sign-extended */)
		case ssa.TypeI64:
			load.asLoad(reg, amode, 64, false)
		case ssa.TypeF32, ssa.TypeF64:
			load.asFpuLoad(reg, amode, arg.Type.Bits())
		case ssa.TypeV128:
			// A v128 always arrives on the stack (setABIArgs puts it there
			// for a vector-file ISA), and RVV's load takes a bare base
			// register, so the address is materialized first.
			m.insert(m.allocateInstr().asALU(aluOpAdd, tmpRegVReg, operandNR(spVReg), operandImm(0), true))
			load.asVecLoad(reg, tmpRegVReg)
			m.insert(load)
			m.unresolvedAddressModes = append(m.unresolvedAddressModes, load)
			continue
		default:
			panic("BUG: unsupported param type on riscv64: " + arg.Type.String())
		}
		m.insert(load)
		m.unresolvedAddressModes = append(m.unresolvedAddressModes, load)
	}
}

// LowerReturns implements backend.Machine.
func (m *machine) LowerReturns(rets []ssa.Value) {
	a := m.currentABI

	l := len(rets) - 1
	for i := range rets {
		// Reverse order so a stack return does not overwrite a value still
		// sitting in a return register.
		ret := rets[l-i]
		r := &a.Rets[l-i]
		reg := m.compiler.VRegOf(ret)
		if def := m.compiler.ValueDefinition(ret); def.IsFromInstr() {
			// Constant instructions are inlined.
			if inst := def.Instr; inst.Constant() {
				m.insertLoadConstant(inst.ConstantVal(), inst.Return().Type(), reg)
			}
		}
		if r.Kind == backend.ABIArgKindReg {
			m.InsertMove(r.Reg, reg, r.Type)
			continue
		}
		amode := m.amodePool.Allocate()
		*amode = addressMode{kind: addressModeKindResultStackSpace, rn: spVReg, imm: r.Offset}
		store := m.allocateInstr()
		store.asStore(reg, amode, r.Type.Bits(), r.Type.IsInt())
		m.insert(store)
		m.unresolvedAddressModes = append(m.unresolvedAddressModes, store)
	}
}

// callerGenVRegToFunctionArg is the caller-side counterpart of LowerParams.
func (m *machine) callerGenVRegToFunctionArg(a *backend.FunctionABI, argIndex int, reg regalloc.VReg, def backend.SSAValueDefinition, slotBegin int64) {
	arg := &a.Args[argIndex]
	if def.IsFromInstr() {
		if inst := def.Instr; inst.Constant() {
			m.insertLoadConstant(inst.ConstantVal(), inst.Return().Type(), reg)
		}
	}
	if arg.Kind == backend.ABIArgKindReg {
		m.InsertMove(arg.Reg, reg, arg.Type)
		return
	}
	// SP is already adjusted at this point.
	amode := m.resolveAddressModeForOffset(arg.Offset-slotBegin, spVReg, false)
	store := m.allocateInstr()
	store.asStore(reg, amode, arg.Type.Bits(), arg.Type.IsInt())
	m.insert(store)
}

func (m *machine) callerGenFunctionReturnVReg(a *backend.FunctionABI, retIndex int, reg regalloc.VReg, slotBegin int64) {
	r := &a.Rets[retIndex]
	if r.Kind == backend.ABIArgKindReg {
		m.InsertMove(reg, r.Reg, r.Type)
		return
	}
	amode := m.resolveAddressModeForOffset(a.ArgStackSize+r.Offset-slotBegin, spVReg, false)
	ldr := m.allocateInstr()
	switch r.Type {
	case ssa.TypeI32:
		ldr.asLoad(reg, amode, 32, true)
	case ssa.TypeI64:
		ldr.asLoad(reg, amode, 64, false)
	case ssa.TypeF32, ssa.TypeF64:
		ldr.asFpuLoad(reg, amode, r.Type.Bits())
	default:
		panic("BUG: unsupported return type on riscv64: " + r.Type.String())
	}
	m.insert(ldr)
}

// resolveAddressModeForOffsetAndInsert is resolveAddressModeForOffset for the
// post-regalloc phase, where instructions are linked rather than inserted.
func (m *machine) resolveAddressModeForOffsetAndInsert(cur *instruction, offset int64, rn regalloc.VReg, allowTmpRegUse bool) (*instruction, *addressMode) {
	m.pendingInstructions = m.pendingInstructions[:0]
	mode := m.resolveAddressModeForOffset(offset, rn, allowTmpRegUse)
	for _, instr := range m.pendingInstructions {
		cur = linkInstr(cur, instr)
	}
	return cur, mode
}

// resolveAddressModeForOffset builds `[rn + offset]`. RISC-V load/store take a
// single signed 12-bit displacement and nothing else -- there is no
// register+register or scaled-index form -- so an out-of-range offset must be
// folded into a register with an explicit add.
func (m *machine) resolveAddressModeForOffset(offset int64, rn regalloc.VReg, allowTmpRegUse bool) *addressMode {
	if rn.RegType() != regalloc.RegTypeInt {
		panic("BUG: rn should be a pointer: " + formatVReg(rn))
	}
	amode := m.amodePool.Allocate()
	if fitsInSignedImm12(offset) {
		*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: rn, imm: offset}
		return amode
	}
	// The scratch we build the address in must not be the base register
	// itself: materializing the offset into it would destroy the base before
	// the add reads it (`add tmp, tmp, tmp` computes 2*offset). tmpReg2 exists
	// precisely so the post-regalloc paths have a second choice here.
	var base regalloc.VReg
	if allowTmpRegUse {
		base = tmpRegVReg
		if rn == tmpRegVReg {
			base = tmpReg2VReg
		}
	} else {
		base = m.compiler.AllocateVReg(ssa.TypeI64)
	}
	m.lowerConstantI64(base, offset)
	add := m.allocateInstr()
	add.asALU(aluOpAdd, base, operandNR(base), operandNR(rn), true)
	m.insert(add)
	*amode = addressMode{kind: addressModeKindRegSignedImm12, rn: base, imm: 0}
	return amode
}

func (m *machine) lowerCall(si *ssa.Instruction) {
	isDirectCall := si.Opcode() == ssa.OpcodeCall
	indirectCalleePtr, directCallee, calleeABI, stackSlotSize := m.prepareCall(si, isDirectCall)

	if isDirectCall {
		call := m.allocateInstr()
		call.asCall(directCallee, calleeABI)
		m.insert(call)
	} else {
		ptr := m.compiler.VRegOf(indirectCalleePtr)
		callInd := m.allocateInstr()
		callInd.asCallIndirect(ptr, calleeABI)
		m.insert(callInd)
	}

	m.insertReturns(si, calleeABI, stackSlotSize)
}

func (m *machine) prepareCall(si *ssa.Instruction, isDirectCall bool) (ssa.Value, ssa.FuncRef, *backend.FunctionABI, int64) {
	var indirectCalleePtr ssa.Value
	var directCallee ssa.FuncRef
	var sigID ssa.SignatureID
	var args []ssa.Value
	if isDirectCall {
		directCallee, sigID, args = si.CallData()
	} else {
		indirectCalleePtr, sigID, args, _ = si.CallIndirectData()
	}
	calleeABI := m.compiler.GetFunctionABI(m.compiler.SSABuilder().ResolveSignature(sigID))

	stackSlotSize := int64(calleeABI.AlignedArgResultStackSlotSize())
	if m.maxRequiredStackSizeForCalls < stackSlotSize+16 {
		m.maxRequiredStackSizeForCalls = stackSlotSize + 16 // return address frame.
	}

	for i, arg := range args {
		reg := m.compiler.VRegOf(arg)
		def := m.compiler.ValueDefinition(arg)
		m.callerGenVRegToFunctionArg(calleeABI, i, reg, def, stackSlotSize)
	}
	return indirectCalleePtr, directCallee, calleeABI, stackSlotSize
}

func (m *machine) insertReturns(si *ssa.Instruction, calleeABI *backend.FunctionABI, stackSlotSize int64) {
	var index int
	r1, rs := si.Returns()
	if r1.Valid() {
		m.callerGenFunctionReturnVReg(calleeABI, 0, m.compiler.VRegOf(r1), stackSlotSize)
		index++
	}
	for _, r := range rs {
		m.callerGenFunctionReturnVReg(calleeABI, index, m.compiler.VRegOf(r), stackSlotSize)
		index++
	}
}

func (m *machine) lowerTailCall(si *ssa.Instruction) {
	isDirectCall := si.Opcode() == ssa.OpcodeTailCallReturnCall
	indirectCalleePtr, directCallee, calleeABI, stackSlotSize := m.prepareCall(si, isDirectCall)

	// Tail calls are only proper tail calls when every argument travels in a
	// register; otherwise we fall back to a plain call. See internal/engine/RATIONALE.md.
	isAllRegs := stackSlotSize == 0

	instr := m.allocateInstr()
	switch {
	case isDirectCall && isAllRegs:
		instr.asTailCall(directCallee, calleeABI)
	case !isDirectCall && isAllRegs:
		instr.asTailCallIndirect(m.compiler.VRegOf(indirectCalleePtr), calleeABI)
	case isDirectCall:
		instr.asCall(directCallee, calleeABI)
	default:
		instr.asCallIndirect(m.compiler.VRegOf(indirectCalleePtr), calleeABI)
	}
	m.insert(instr)

	// If this is a proper tail call, returns are cleared in the postRegAlloc phase.
	m.insertReturns(si, calleeABI, stackSlotSize)
}

// insertAddOrSubStackPointer emits `rd = sp +/- diff`.
func (m *machine) insertAddOrSubStackPointer(rd regalloc.VReg, diff int64, add bool) {
	if !add {
		diff = -diff
	}
	if fitsInSignedImm12(diff) {
		alu := m.allocateInstr()
		alu.asALU(aluOpAdd, rd, operandNR(spVReg), operandImm(diff), true)
		m.insert(alu)
		return
	}
	m.lowerConstantI64(tmpRegVReg, diff)
	alu := m.allocateInstr()
	alu.asALU(aluOpAdd, rd, operandNR(spVReg), operandNR(tmpRegVReg), true)
	m.insert(alu)
}

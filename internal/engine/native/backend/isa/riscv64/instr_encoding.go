package riscv64

import (
	"context"
	"fmt"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
)

// compilerBuf is the slice of backend.Compiler the encoders here need. Keeping
// it narrow lets the multi-instruction sequences be unit tested against a plain
// buffer instead of a whole compiler.
type compilerBuf interface {
	Emit4Bytes(uint32)
	Emit8Bytes(uint64)
}

// intRd returns the encoding number of an integer destination register,
// refusing the ones compiled code must never write.
//
// x27 is Go's g. Clobbering it does not fail where it happens: the program
// runs on until the next signal arrives, and the runtime then reports "fatal:
// bad g in signal handler" from somewhere entirely unrelated, with no
// traceback, because it has no goroutine to attribute the fault to. x3 (gp)
// and x4 (tp) are reserved by the psABI and fail just as remotely. Catching
// the write at encode time turns all three into a normal Go panic naming the
// instruction that did it.
func intRd(v regalloc.VReg) uint32 {
	switch r := v.RealReg(); r {
	case x27:
		panic("BUG: compiled code must not write x27, which is Go's g")
	case x3, x4:
		panic("BUG: compiled code must not write " + regNames[r] + ", reserved by the psABI")
	default:
		return regNumberInEncoding[r]
	}
}

// size returns the number of bytes this instruction occupies once encoded.
// Every real RISC-V instruction here is 4 bytes -- the backend never emits the
// compressed RVC forms -- so only meta instructions and the multi-instruction
// expansions differ.
func (i *instruction) size() int64 {
	switch i.kind {
	case nop0, sourceOffsetInfo, loadConstBlockArg:
		return 0
	case fpuConstPoolData:
		return int64(i.u2) / 8 // 4 or 8 bytes of raw data.
	case adr:
		return 8 // auipc + addi
	case call, tailCall:
		return callSequenceSize // auipc + jalr
	case exitSequence:
		return exitSequenceSize
	case condBr:
		switch i.condBrExpansion() {
		case 0:
			return 4
		case 1:
			return 8
		default:
			return 12
		}
	case br:
		if i.brExpansion() == 0 {
			return 4
		}
		return 8
	case brTableSequence:
		return brTableSequenceOffsetTableBegin + int64(i.u2)*4
	case vecRRR, vecRR, vecRX, vecSplat:
		return 8 // vsetivli + the operation
	case vecCmp:
		return 20 // vsetivli + all-ones + zeros + compare + merge
	case vecMaskPop:
		return 12 // vsetivli + compare + vcpop
	case vecMov:
		return 4 // vmv1r.v needs no vtype
	case vecLoad, vecStore:
		return 8 // vsetivli + the access
	case load, store, fpuLoad, fpuStore:
		if i.bigOffset() {
			return 12 // lui + add + the access itself.
		}
		return 4
	default:
		return 4
	}
}

// emitAccessBase materializes the address base a load/store should use,
// handling the grown out-of-range-displacement form. It returns the base
// register number and the displacement to put in the access itself.
func emitAccessBase(c compilerBuf, i *instruction) (base uint32, disp int32) {
	a := i.getAmode()
	rn := regNumberInEncoding[a.rn.RealReg()]
	if !i.bigOffset() {
		return rn, int32(a.imm)
	}
	tmp := regNumberInEncoding[tmpReg]
	// lui+add has no truncating tail either: the displacement reaches the load
	// through a register, not through a 32-bit-result instruction.
	hi, lo := splitImm32PCRel(a.imm, "oversized stack displacement")
	c.Emit4Bytes(encodeLui(tmp, hi))
	c.Emit4Bytes(encodeAluRRR(aluOpAdd, tmp, tmp, rn, true))
	return tmp, lo
}

// encode appends this instruction's machine code to the compiler's buffer.
func (i *instruction) encode(m *machine) {
	c := m.compiler
	switch i.kind {
	case nop0, loadConstBlockArg, sourceOffsetInfo:
	case aluRRR:
		op := aluOp(i.u1)
		_64bit := i.u2 == 1
		rd := intRd(i.rd)
		rs1 := regNumberInEncoding[i.rs1.realReg()]
		if i.rs2.kind == operandKindImm {
			c.Emit4Bytes(encodeAluRRImm(op, rd, rs1, int32(i.rs2.imm), _64bit))
		} else {
			c.Emit4Bytes(encodeAluRRR(op, rd, rs1, regNumberInEncoding[i.rs2.realReg()], _64bit))
		}
	case shiftImm:
		c.Emit4Bytes(encodeAluRRImm(aluOp(i.u1), intRd(i.rd),
			regNumberInEncoding[i.rs1.realReg()], int32(i.rs2.imm), i.u2 == 1))
	case lui:
		c.Emit4Bytes(encodeLui(intRd(i.rd), int32(uint32(i.u1))))
	case adr:
		// resolveRelativeAddresses put the PC-relative displacement in u2.
		rd := intRd(i.rd)
		hi, lo := splitImm32PCRel(int64(int32(uint32(i.u2))), "adr")
		c.Emit4Bytes(encodeAuipc(rd, hi))
		c.Emit4Bytes(encodeAluRRImm(aluOpAdd, rd, rd, lo, true))
	case mov:
		// `mv rd, rs` is `addi rd, rs, 0`.
		c.Emit4Bytes(encodeAluRRImm(aluOpAdd, intRd(i.rd),
			regNumberInEncoding[i.rs1.realReg()], 0, true))
	case fpuMov:
		// `fmv.d rd, rs` is `fsgnj.d rd, rs, rs`.
		r := regNumberInEncoding[i.rs1.realReg()]
		c.Emit4Bytes(encodeFpuRRR(fpuBinOpSgnj, regNumberInEncoding[i.rd.RealReg()], r, r, true))
	case load:
		base, disp := emitAccessBase(c, i)
		c.Emit4Bytes(encodeLoad(intRd(i.rd), base, disp, byte(i.u1), i.loadIsSigned()))
	case store:
		base, disp := emitAccessBase(c, i)
		c.Emit4Bytes(encodeStore(regNumberInEncoding[i.rd.RealReg()], base, disp, byte(i.u1)))
	case fpuLoad:
		base, disp := emitAccessBase(c, i)
		c.Emit4Bytes(encodeFpuLoad(regNumberInEncoding[i.rd.RealReg()], base, disp, byte(i.u1)))
	case fpuStore:
		base, disp := emitAccessBase(c, i)
		c.Emit4Bytes(encodeFpuStore(regNumberInEncoding[i.rd.RealReg()], base, disp, byte(i.u1)))
	case condBr:
		encodeCondBr(c, i)
	case br:
		encodeBr(c, i)
	case call:
		// The auipc+jalr pair is patched by ResolveRelocations once the
		// callee's address is known; emit a correctly sized placeholder.
		c.AddRelocationInfo(i.callFuncRef(), false)
		c.Emit4Bytes(encodeAuipc(regNumberInEncoding[raReg], 0))
		c.Emit4Bytes(encodeJalr(regNumberInEncoding[raReg], regNumberInEncoding[raReg], 0))
	case tailCall:
		c.AddRelocationInfo(i.callFuncRef(), true)
		c.Emit4Bytes(encodeAuipc(regNumberInEncoding[tmpReg], 0))
		c.Emit4Bytes(encodeJalr(regNumberInEncoding[zeroReg], regNumberInEncoding[tmpReg], 0))
	case callInd:
		c.Emit4Bytes(encodeJalr(regNumberInEncoding[raReg], regNumberInEncoding[i.rs1.realReg()], 0))
	case tailCallInd:
		c.Emit4Bytes(encodeJalr(regNumberInEncoding[zeroReg], regNumberInEncoding[i.rs1.realReg()], 0))
	case ret:
		c.Emit4Bytes(encodeRet())
	case udf:
		c.Emit4Bytes(encodeEbreak())
	case exitSequence:
		encodeExitSequence(c, i.rs1.nr())
	case fpuRRR:
		c.Emit4Bytes(encodeFpuRRR(fpuBinOp(i.u1), regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], regNumberInEncoding[i.rs2.realReg()], i.u2 == 1))
	case fpuRR:
		encodeFpuRR(c, i)
	case fpuCmp:
		c.Emit4Bytes(encodeFpuCmp(fpuCmpOp(i.u1), intRd(i.rd),
			regNumberInEncoding[i.rs1.realReg()], regNumberInEncoding[i.rs2.realReg()], i.u2 == 1))
	case fcvtToInt:
		dst64, src64, signed := i.u1&1 == 1, i.u1>>1&1 == 1, i.u1>>2&1 == 1
		c.Emit4Bytes(encodeFcvtToIntRM(intRd(i.rd),
			regNumberInEncoding[i.rs1.realReg()], dst64, src64, signed, uint32(i.u2)))
	case fcvtFromInt:
		dst64, src64, signed := i.u1&1 == 1, i.u1>>1&1 == 1, i.u1>>2&1 == 1
		c.Emit4Bytes(encodeFcvtFromInt(regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], dst64, src64, signed))
	case fcvtSD:
		c.Emit4Bytes(encodeFcvtSD(regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], i.u1 == 1))
	case fmvToInt:
		c.Emit4Bytes(encodeFmvToInt(intRd(i.rd),
			regNumberInEncoding[i.rs1.realReg()], i.u1 == 1))
	case fmvFromInt:
		c.Emit4Bytes(encodeFmvFromInt(regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], i.u1 == 1))
	case fclass:
		c.Emit4Bytes(encodeFclass(intRd(i.rd),
			regNumberInEncoding[i.rs1.realReg()], i.u1 == 1))
	case fence:
		c.Emit4Bytes(encodeFence(0b0011, 0b0011))
	case fpuConstPoolData:
		if i.u2 == 32 {
			c.Emit4Bytes(uint32(i.u1))
		} else {
			c.Emit8Bytes(i.u1)
		}
	case vecRRR:
		funct6, form := uint32(i.u1&0xff), uint32(i.u1>>8&0xff)
		emitVsetivli(c, uint32(i.u2))
		c.Emit4Bytes(encodeVecVV(funct6, vecReg(i.rd), vecReg(i.rs1.nr()), vecReg(i.rs2.nr()), form))
	case vecRR:
		funct6, form, variant := uint32(i.u1&0xff), uint32(i.u1>>8&0xff), uint32(i.u1>>16&0xff)
		emitVsetivli(c, uint32(i.u2))
		c.Emit4Bytes(encodeVecUnary(funct6, vecReg(i.rd), vecReg(i.rs1.nr()), variant, form))
	case vecRX:
		emitVsetivli(c, uint32(i.u2))
		c.Emit4Bytes(encodeVecVX(uint32(i.u1), vecReg(i.rd), vecReg(i.rs1.nr()),
			regNumberInEncoding[i.rs2.realReg()]))
	case vecSplat:
		emitVsetivli(c, uint32(i.u2))
		c.Emit4Bytes(encodeVmvVX(vecReg(i.rd), regNumberInEncoding[i.rs1.realReg()]))
	case vecCmp:
		encodeVecCmp(c, i)
	case vecMaskPop:
		encodeVecMaskPop(c, i)
	case vecMov:
		c.Emit4Bytes(encodeVmv1r(vecReg(i.rd), vecReg(i.rs1.nr())))
	case vecLoad:
		emitVsetivli(c, vsew64)
		c.Emit4Bytes(encodeVectorLoad(vecReg(i.rd), regNumberInEncoding[i.rs1.realReg()], vsew64))
	case vecStore:
		emitVsetivli(c, vsew64)
		c.Emit4Bytes(encodeVectorStore(vecReg(i.rd), regNumberInEncoding[i.rs1.realReg()], vsew64))
	case brTableSequence:
		encodeBrTableSequence(c, m, i)
	default:
		panic(fmt.Sprintf("BUG: unhandled instruction kind %d in encode: %s", i.kind, i))
	}
}

// encodeCondBr emits a conditional branch at whichever expansion level
// resolveRelativeAddresses settled on.
func encodeCondBr(c compilerBuf, i *instruction) {
	cond := i.condBrCond()
	rs1 := regNumberInEncoding[i.rs1.realReg()]
	rs2 := regNumberInEncoding[i.rs2.realReg()]
	offset := i.condBrOffset()

	switch i.condBrExpansion() {
	case 0:
		c.Emit4Bytes(encodeBranch(cond, rs1, rs2, int32(offset)))
	case 1:
		// Branch *over* the jal when the condition does not hold.
		c.Emit4Bytes(encodeBranch(cond.invert(), rs1, rs2, 8))
		c.Emit4Bytes(encodeJal(regNumberInEncoding[zeroReg], int32(offset-4)))
	default:
		c.Emit4Bytes(encodeBranch(cond.invert(), rs1, rs2, 12))
		hi, lo := splitImm32PCRel(offset-4, "long conditional branch")
		c.Emit4Bytes(encodeAuipc(regNumberInEncoding[tmpReg], hi))
		c.Emit4Bytes(encodeJalr(regNumberInEncoding[zeroReg], regNumberInEncoding[tmpReg], lo))
	}
}

func encodeBr(c compilerBuf, i *instruction) {
	offset := i.brOffset()
	if i.brExpansion() == 0 {
		c.Emit4Bytes(encodeJal(regNumberInEncoding[zeroReg], int32(offset)))
		return
	}
	hi, lo := splitImm32PCRel(offset, "long unconditional branch")
	c.Emit4Bytes(encodeAuipc(regNumberInEncoding[tmpReg], hi))
	c.Emit4Bytes(encodeJalr(regNumberInEncoding[zeroReg], regNumberInEncoding[tmpReg], lo))
}

// encodeFpuRR emits the one-operand FP instructions. RISC-V has no fneg or
// fabs opcode: both are sign-injection aliases of fsgnj with one source used
// twice.
func encodeFpuRR(c compilerBuf, i *instruction) {
	rd := regNumberInEncoding[i.rd.RealReg()]
	rs := regNumberInEncoding[i.rs1.realReg()]
	_64bit := i.u2 == 1
	switch fpuUniOp(i.u1) {
	case fpuUniOpSqrt:
		c.Emit4Bytes(encodeFsqrt(rd, rs, _64bit))
	case fpuUniOpNeg:
		c.Emit4Bytes(encodeFpuRRR(fpuBinOpSgnjn, rd, rs, rs, _64bit))
	case fpuUniOpAbs:
		c.Emit4Bytes(encodeFpuRRR(fpuBinOpSgnjx, rd, rs, rs, _64bit))
	default:
		panic(fmt.Sprintf("BUG: unknown fpuUniOp %d", i.u1))
	}
}

// exitSequenceSize is the byte length of encodeExitSequence's output. It is
// fixed: the context-eviction move is emitted unconditionally so that callers
// recording a resume address (insertExitSequence, emitTrapIslands) can add a
// constant rather than depend on which register the context landed in.
const exitSequenceSize = 5 * 4

// adrSequenceSize is the size of the auipc+addi pair an `adr` expands to.
const adrSequenceSize = 8

// goExitResumeOffsetFromAdr is how far past its own `adr` the resume address
// of a Go exit lies: the adr itself, the store that puts its result into the
// execution context, and then the exit sequence.
//
// It is spelled out rather than written as a number because the obvious number
// is arm64's. There the adr is a single instruction, so the same distance is
// 8+exitSequenceSize; here the adr is a pair, and copying that constant across
// leaves the resume address four bytes short -- pointing into the middle of
// the exit sequence rather than past it. Nothing fails at that point: the
// program runs on until a signal arrives and the runtime reports "fatal: bad g
// in signal handler" from somewhere unrelated.
const goExitResumeOffsetFromAdr = adrSequenceSize + 4 + exitSequenceSize

// encodeExitSequence restores Go's ra, frame pointer and sp from the execution
// context and returns, handing control back to the Go side of the call.
//
// SP is an ordinary register on RISC-V, so it is reloaded directly -- arm64
// needs a scratch because it cannot load into SP. s0 is Go's frame pointer on
// riscv64 and this backend allocates it, so it has to be restored here for
// Go's traceback to work on the other side.
func encodeExitSequence(c compilerBuf, ctxReg regalloc.VReg) {
	// Move the context into the reserved scratch first. This is unconditional
	// so the sequence is a constant size, and it is necessary whenever the
	// context happens to live in ra or s0, which the loads below overwrite.
	ctx := regNumberInEncoding[ctxReg.RealReg()]
	tmp := regNumberInEncoding[tmpReg]
	c.Emit4Bytes(encodeAluRRImm(aluOpAdd, tmp, ctx, 0, true)) // mv tmp, ctx

	c.Emit4Bytes(encodeLoad(regNumberInEncoding[raReg], tmp,
		int32(nativeapi.ExecutionContextOffsetGoReturnAddress.I64()), 64, false))
	c.Emit4Bytes(encodeLoad(regNumberInEncoding[x8], tmp,
		int32(nativeapi.ExecutionContextOffsetOriginalFramePointer.I64()), 64, false))
	c.Emit4Bytes(encodeLoad(regNumberInEncoding[spReg], tmp,
		int32(nativeapi.ExecutionContextOffsetOriginalStackPointer.I64()), 64, false))
	c.Emit4Bytes(encodeRet())
}

// brTableSequenceOffsetTableBegin is the byte offset, from the start of the
// br_table sequence, at which the jump table itself begins -- i.e. the size of
// the seven instructions encodeBrTableSequence emits ahead of the data.
const brTableSequenceOffsetTableBegin = 7 * 4

// encodeBrTableSequence emits the br_table dispatch and the table it reads.
//
// Each table entry is a 32-bit displacement from the table's own address (see
// resolveRelativeAddresses), which keeps the table position-independent -- the
// executable is mmap'd at an address unknown at compile time, so an absolute
// target could not be baked in.
//
//	auipc tmp,  0            ; tmp  = address of this instruction
//	addi  tmp,  tmp, 28      ; tmp  = address of the table
//	slli  tmp2, idx, 2       ; tmp2 = idx * 4
//	add   tmp2, tmp, tmp2    ; tmp2 = &table[idx]
//	lw    tmp2, 0(tmp2)      ; tmp2 = table[idx], sign-extended
//	add   tmp,  tmp, tmp2    ; tmp  = table address + displacement
//	jalr  zero, 0(tmp)
//	<table>
//
// The index has already been bounds-checked by the lowering, so no negative or
// out-of-range entry can be reached here.
func encodeBrTableSequence(c compilerBuf, m *machine, i *instruction) {
	tmp := regNumberInEncoding[tmpReg]
	tmp2 := regNumberInEncoding[tmpReg2]
	idx := regNumberInEncoding[i.rs1.realReg()]

	c.Emit4Bytes(encodeAuipc(tmp, 0))
	c.Emit4Bytes(encodeAluRRImm(aluOpAdd, tmp, tmp, brTableSequenceOffsetTableBegin, true))
	c.Emit4Bytes(encodeAluRRImm(aluOpSll, tmp2, idx, 2, true))
	c.Emit4Bytes(encodeAluRRR(aluOpAdd, tmp2, tmp, tmp2, true))
	c.Emit4Bytes(encodeLoad(tmp2, tmp2, 0, 32, true))
	c.Emit4Bytes(encodeAluRRR(aluOpAdd, tmp, tmp, tmp2, true))
	c.Emit4Bytes(encodeJalr(regNumberInEncoding[zeroReg], tmp, 0))

	for _, off := range m.jmpTableTargets[i.u1] {
		c.Emit4Bytes(off)
	}
}

// resolveRelativeAddresses assigns every label its binary offset, grows the
// branches whose displacement does not fit, and writes the resolved
// displacements back into the instructions.
//
// The expansion loop runs to a fixed point. A branch's level only ever
// increases (condBrExpansionFor never returns below the current level), so
// each pass can only grow the function and there are finitely many branches
// to grow -- the loop terminates.
func (m *machine) resolveRelativeAddresses(ctx context.Context) {
	m.resolveAddressModes()
	m.unresolvedAddressModes = m.unresolvedAddressModes[:0]

	for {
		var fn string
		var fnIndex int
		var labelPosToLabel map[*labelPosition]label
		if nativeapi.PerfMapEnabled {
			labelPosToLabel = make(map[*labelPosition]label)
			for i := 0; i <= m.labelPositionPool.MaxIDEncountered(); i++ {
				labelPosToLabel[m.labelPositionPool.Get(i)] = label(i)
			}
			fn = nativeapi.GetCurrentFunctionName(ctx)
			fnIndex = nativeapi.GetCurrentFunctionIndex(ctx)
		}

		// Lay out every block to learn each label's offset.
		var offset int64
		for _, pos := range m.orderedSSABlockLabelPos {
			pos.binaryOffset = offset
			var size int64
			for cur := pos.begin; ; cur = cur.next {
				if cur.kind == nop0 {
					if l, ok := cur.nop0Label(); ok {
						if lp := m.labelPositionPool.Get(int(l)); lp != nil {
							lp.binaryOffset = offset + size
						}
					}
				}
				size += cur.size()
				if cur == pos.end {
					break
				}
			}
			if nativeapi.PerfMapEnabled && size > 0 {
				nativeapi.PerfMap.AddModuleEntry(fnIndex, offset, uint64(size),
					fmt.Sprintf("%s:::::%s", fn, labelPosToLabel[pos]))
			}
			offset += size
		}

		// Grow any branch whose displacement no longer fits.
		needRerun := false
		var currentOffset int64
		for cur := m.rootInstr; cur != nil; cur = cur.next {
			switch cur.kind {
			case condBr:
				target := m.labelPositionPool.Get(int(cur.condBrLabel())).binaryOffset
				if want := condBrExpansionFor(target-currentOffset, cur.condBrExpansion()); want != cur.condBrExpansion() {
					cur.setCondBrExpansion(want)
					needRerun = true
				}
			case br:
				target := m.labelPositionPool.Get(int(cur.brLabel())).binaryOffset
				if cur.brExpansion() == 0 && !fitsInSignedImm21(target-currentOffset) {
					cur.setBrExpansion(1)
					needRerun = true
				}
			}
			currentOffset += cur.size()
		}

		if !needRerun {
			break
		}
		if nativeapi.PerfMapEnabled {
			nativeapi.PerfMap.Clear()
		}
	}

	// Write the resolved displacements back.
	var currentOffset int64
	for cur := m.rootInstr; cur != nil; cur = cur.next {
		switch cur.kind {
		case br:
			cur.brOffsetResolve(m.labelOffset(cur.brLabel(), "br") - currentOffset)
		case condBr:
			cur.condBrOffsetResolve(m.labelOffset(cur.condBrLabel(), "condBr") - currentOffset)
		case adr:
			// asAdrPCRel carries its displacement directly and marks u1 with
			// the invalid sentinel, precisely so it is not looked up here.
			if l := label(cur.u1); l != labelInvalid && l != labelReturn {
				cur.u2 = uint64(uint32(int32(m.labelOffset(l, "adr") - currentOffset)))
			}
		case brTableSequence:
			targets := m.jmpTableTargets[cur.u1]
			for i := range targets {
				t := m.labelOffset(label(targets[i]), "br_table entry")
				targets[i] = uint32(int32(t - (currentOffset + brTableSequenceOffsetTableBegin)))
			}
		case sourceOffsetInfo:
			m.compiler.AddSourceOffsetInfo(currentOffset, cur.sourceOffset())
		}
		currentOffset += cur.size()
	}
}

// condBrExpansionFor returns the smallest expansion level that can encode the
// given displacement, never below the level already reached.
func condBrExpansionFor(diff int64, current byte) byte {
	var want byte
	switch {
	case fitsInSignedImm13(diff):
		want = 0
	case fitsInSignedImm21(diff - 4):
		want = 1
	default:
		want = 2
	}
	if want < current {
		return current
	}
	return want
}

// labelOffset returns a label's resolved offset, failing loudly rather than
// dereferencing nil when a branch names a label that was never placed -- which
// is a lowering bug, and one that is otherwise reported as an opaque nil
// dereference deep inside encoding.
func (m *machine) labelOffset(l label, what string) int64 {
	pos := m.labelPositionPool.Get(int(l))
	if pos == nil {
		panic(fmt.Sprintf("BUG: %s targets %s, which has no position", what, l))
	}
	return pos.binaryOffset
}

// vecReg is the encoding number of a vector register.
func vecReg(v regalloc.VReg) uint32 { return regNumberInEncoding[v.RealReg()] }

// emitVsetivli configures the vector unit for exactly 16 bytes at the given
// element width.
//
// It is emitted before every vector operation rather than tracked across them.
// A vtype is a piece of machine state, and getting it wrong is silent -- the
// operation simply reads the wrong number of lanes at the wrong width -- so
// the correct-by-construction version comes first. Collapsing runs of
// identical vsetivli is a straightforward peephole over the final instruction
// list, and worth doing, but it is an optimization and belongs after the
// semantics are pinned by the spec suite.
func emitVsetivli(c compilerBuf, sew uint32) {
	c.Emit4Bytes(encodeVsetivli(vecAVLFor(sew), sew))
}

// encodeVecCmp emits a lane-wise comparison as all-ones/all-zeros lanes.
//
// RVV comparisons produce a *mask* -- one bit per lane -- where wasm wants a
// full-width vector of all-ones or all-zeros. The mask therefore has to be
// materialized: build the two candidate vectors, run the comparison into v0
// (the architecturally fixed mask register, which is why it is held out of
// allocation), and merge.
func encodeVecCmp(c compilerBuf, i *instruction) {
	funct6, form := uint32(i.u1&0xff), uint32(i.u1>>8&0xff)
	sew := uint32(i.u2)
	vd, vs2, vs1 := vecReg(i.rd), vecReg(i.rs1.nr()), vecReg(i.rs2.nr())
	tmp := regNumberInEncoding[vecTmpReg]

	emitVsetivli(c, sew)
	c.Emit4Bytes(encodeVmvVI(tmp, -1)) // all-ones lanes
	c.Emit4Bytes(encodeVmvVI(vd, 0))   // all-zeros lanes
	c.Emit4Bytes(encodeVec(funct6, 1, vs2, vs1, form, regNumberInEncoding[vecMaskReg]))
	c.Emit4Bytes(encodeVmerge(vd, vd, tmp))
}

// encodeVecMaskPop answers any_true and all_true.
//
// Both reduce to counting lanes: any_true is "not every lane is zero" and
// all_true is "no lane is zero", so one comparison against zero and a
// population count of the resulting mask serves both, with the caller
// comparing the count afterwards.
func encodeVecMaskPop(c compilerBuf, i *instruction) {
	sew := uint32(i.u2)
	rd := intRd(i.rd)
	vs2 := vecReg(i.rs1.nr())
	mask := regNumberInEncoding[vecMaskReg]

	emitVsetivli(c, sew)
	// vmseq.vi v0, vs2, 0 -- lanes that are zero.
	c.Emit4Bytes(encodeVec(vfunctMseq, 1, vs2, 0, opivi, mask))
	c.Emit4Bytes(encodeVcpopM(rd, mask))
}

package riscv64

import (
	"context"
	"fmt"

	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
)

// compilerBuf is the slice of backend.Compiler the encoders here need. Keeping
// it narrow lets the multi-instruction sequences be unit tested against a plain
// buffer instead of a whole compiler.
type compilerBuf interface {
	Emit4Bytes(uint32)
	Emit8Bytes(uint64)
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
	hi, lo := splitImm32(int32(a.imm))
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
		rd := regNumberInEncoding[i.rd.RealReg()]
		rs1 := regNumberInEncoding[i.rs1.realReg()]
		if i.rs2.kind == operandKindImm {
			c.Emit4Bytes(encodeAluRRImm(op, rd, rs1, int32(i.rs2.imm), _64bit))
		} else {
			c.Emit4Bytes(encodeAluRRR(op, rd, rs1, regNumberInEncoding[i.rs2.realReg()], _64bit))
		}
	case shiftImm:
		c.Emit4Bytes(encodeAluRRImm(aluOp(i.u1), regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], int32(i.rs2.imm), i.u2 == 1))
	case lui:
		c.Emit4Bytes(encodeLui(regNumberInEncoding[i.rd.RealReg()], int32(uint32(i.u1))))
	case adr:
		// resolveRelativeAddresses put the PC-relative displacement in u2.
		rd := regNumberInEncoding[i.rd.RealReg()]
		hi, lo := splitImm32(int32(uint32(i.u2)))
		c.Emit4Bytes(encodeAuipc(rd, hi))
		c.Emit4Bytes(encodeAluRRImm(aluOpAdd, rd, rd, lo, true))
	case mov:
		// `mv rd, rs` is `addi rd, rs, 0`.
		c.Emit4Bytes(encodeAluRRImm(aluOpAdd, regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], 0, true))
	case fpuMov:
		// `fmv.d rd, rs` is `fsgnj.d rd, rs, rs`.
		r := regNumberInEncoding[i.rs1.realReg()]
		c.Emit4Bytes(encodeFpuRRR(fpuBinOpSgnj, regNumberInEncoding[i.rd.RealReg()], r, r, true))
	case load:
		base, disp := emitAccessBase(c, i)
		c.Emit4Bytes(encodeLoad(regNumberInEncoding[i.rd.RealReg()], base, disp, byte(i.u1), i.loadIsSigned()))
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
		c.Emit4Bytes(encodeFpuCmp(fpuCmpOp(i.u1), regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], regNumberInEncoding[i.rs2.realReg()], i.u2 == 1))
	case fcvtToInt:
		dst64, src64, signed := i.u1&1 == 1, i.u1>>1&1 == 1, i.u1>>2&1 == 1
		c.Emit4Bytes(encodeFcvtToInt(regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], dst64, src64, signed))
	case fcvtFromInt:
		dst64, src64, signed := i.u1&1 == 1, i.u1>>1&1 == 1, i.u1>>2&1 == 1
		c.Emit4Bytes(encodeFcvtFromInt(regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], dst64, src64, signed))
	case fcvtSD:
		c.Emit4Bytes(encodeFcvtSD(regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], i.u1 == 1))
	case fmvToInt:
		c.Emit4Bytes(encodeFmvToInt(regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], i.u1 == 1))
	case fmvFromInt:
		c.Emit4Bytes(encodeFmvFromInt(regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], i.u1 == 1))
	case fclass:
		c.Emit4Bytes(encodeFclass(regNumberInEncoding[i.rd.RealReg()],
			regNumberInEncoding[i.rs1.realReg()], i.u1 == 1))
	case fence:
		c.Emit4Bytes(encodeFence(0b0011, 0b0011))
	case fpuConstPoolData:
		if i.u2 == 32 {
			c.Emit4Bytes(uint32(i.u1))
		} else {
			c.Emit8Bytes(i.u1)
		}
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
		hi, lo := splitImm32(int32(offset - 4))
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
	hi, lo := splitImm32(int32(offset))
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
			target := m.labelPositionPool.Get(int(cur.brLabel())).binaryOffset
			cur.brOffsetResolve(target - currentOffset)
		case condBr:
			target := m.labelPositionPool.Get(int(cur.condBrLabel())).binaryOffset
			cur.condBrOffsetResolve(target - currentOffset)
		case adr:
			target := m.labelPositionPool.Get(int(cur.u1)).binaryOffset
			cur.u2 = uint64(uint32(int32(target - currentOffset)))
		case brTableSequence:
			targets := m.jmpTableTargets[cur.u1]
			for i := range targets {
				t := m.labelPositionPool.Get(int(label(targets[i]))).binaryOffset
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

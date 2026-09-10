package riscv64

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// ---------------------------------------------------------------------------
// i32 representation
// ---------------------------------------------------------------------------
//
// Every 32-bit wasm value lives in a 64-bit register *sign-extended*, which is
// the RV64 convention and effectively forced by the ISA: the .w arithmetic
// forms, lw, and the word shifts all sign-extend their result, and the branch
// instructions compare full 64-bit registers with no narrow form.
//
// This is not merely convenient, it is also correct for unsigned comparison.
// Sign-extension is order-preserving under *unsigned* 64-bit comparison of
// sign-extended 32-bit values: every value with bit 31 set maps into
// 0xFFFFFFFF_xxxxxxxx and every value without it into 0x00000000_xxxxxxxx, so
// the two groups stay in the same relative order that u32 comparison puts them
// in, and order within each group is preserved outright. So `sltu`/`bltu` on
// sign-extended operands implement i32 unsigned comparison directly, with no
// masking.
//
// The invariant is a *precondition* of that argument, so every producer of an
// i32 has to maintain it. These are the ones that do not maintain it for free,
// and what each needs -- this list is the lowering's checklist, not background:
//
//	i32.const               materialize int32(v), not uint32(v). A LUI-based
//	                        constant needs ADDIW, not ADDI, or 0x7fffffff comes
//	                        out as 0xffffffff7fffffff.
//	i32.wrap_i64            addiw dst, src, 0. A plain copy leaves
//	                        0x0000000080000000 looking positive.
//	i32 add/sub/mul/shl     the .w forms. Wide 0x7fffffff+1 yields a
//	                        non-canonical 0x0000000080000000.
//	i32 shr_u/shr_s/rotl/r  the .w shifts: wide shifts take six count bits, so
//	                        i32.shr_s(1, 32) would return 0 instead of 1.
//	                        Rotations need the count reduced modulo 32 and both
//	                        halves canonical.
//	i32.extend8_s/16_s      word shifts (or XLEN-correct distances); the naive
//	                        (x<<24)>>s24 on 64 bits leaves 0x80 positive.
//	i32.load, globals,      lw, not lwu/ld. A stored 0x80000000 reloaded with
//	  spills, host results   lwu is non-canonical, and -1 from a helper that
//	                        zero-extends compares unequal to canonical -1.
//	i32.clz/ctz/popcnt      the software sequences must be width-aware:
//	                        popcnt of sign-extended -1 is 64, not 32, and
//	                        ctz(0) must be 32.
//
// And the mirror-image hazard: every *unsigned consumer* of an i32 must
// zero-extend first. Adding a sign-extended 0x80000000 to a memory base
// subtracts 2GiB rather than adding 2GiB, so effective-address arithmetic,
// memory/table indices and lengths, and host marshaling all need the
// slli/srli 32 pair. i64.extend_i32_u is only the most visible case.
//
// What is already canonical and needs nothing: the bitwise ops (canonical in,
// canonical out), FCVT.W/WU and FMV.X.W, the word AMO and LR/SC results, and
// i32.load8_u/load16_u.

type (
	// instruction is one machine instruction or a meta-instruction convenient
	// for code generation (a label anchor, a constant-pool datum, ...). Fields
	// are interpreted according to kind.
	instruction struct {
		prev, next          *instruction
		rd                  regalloc.VReg
		rs1, rs2, rs3       operand
		amode               *addressMode
		u1, u2              uint64
		kind                instructionKind
		addedBeforeRegAlloc bool
	}

	instructionKind byte
)

const (
	instrInvalid instructionKind = iota
	// nop0 is a zero-width meta instruction used to anchor a label.
	nop0
	// sourceOffsetInfo is a zero-width marker recording a wasm source offset.
	sourceOffsetInfo
	// loadConstBlockArg is a placeholder for a block-argument constant, expanded
	// after register allocation.
	loadConstBlockArg
	// aluRRR is `rd = rs1 <op> rs2`, where rs2 is a register or a 12-bit
	// immediate. u1 is the aluOp, u2 is 1 for a 64-bit operation.
	aluRRR
	// shiftImm is `rd = rs1 <shift> imm`. u1 is the aluOp, u2 is 1 for 64-bit.
	shiftImm
	// lui is `rd = imm << 12`. u1 holds the already-shifted 32-bit value.
	lui
	// adr materializes the address of a label into rd via auipc+addi. u1 is the
	// label; the pair is resolved in resolveRelativeAddresses.
	adr
	// mov is a register-to-register copy, `addi rd, rs1, 0`.
	mov
	// fpuMov is an FP register copy, `fsgnj.d rd, rs1, rs1`.
	fpuMov
	// load is `rd = [rs1 + imm]`. u1 is the width in bits, u2 is 1 when signed.
	load
	// store is `[rs1 + imm] = rd`. u1 is the width in bits.
	store
	// fpuLoad / fpuStore are the FP equivalents. u1 is the width in bits.
	fpuLoad
	fpuStore
	// condBr is `b<cond> rs1, rs2, target`. u1 is the condFlag, u2 the target label.
	condBr
	// br is `jal zero, target`. u1 is the target label.
	br
	// brTableSequence is the jump-table dispatch for br_table.
	brTableSequence
	// call / callInd / tailCall / tailCallInd.
	call
	callInd
	tailCall
	tailCallInd
	// ret returns from the current function.
	ret
	// exitSequence transfers control back to Go. rs1 holds the execution context.
	exitSequence
	// udf is an always-trapping instruction, used for unreachable code.
	udf
	// fpuRRR is a two-operand FP instruction. u1 is the fpuBinOp, u2 is 1 for f64.
	fpuRRR
	// fpuRR is a one-operand FP instruction (sqrt/neg/abs). u1 is the fpuUniOp.
	fpuRR
	// fpuCmp writes an integer 0/1 from an FP comparison. u1 is the fpuCmpOp.
	fpuCmp
	// fpuRound implements the f32/f64 rounding operators, which RV64D lacks.
	fpuRound
	// fcvtToInt / fcvtFromInt / fcvtSD are the conversion instructions.
	fcvtToInt
	fcvtFromInt
	fcvtSD
	// fmvToInt / fmvFromInt are raw bit reinterpretations between the files.
	fmvToInt
	fmvFromInt
	// fclass writes the IEEE class mask of an FP register into an integer register.
	fclass
	// fpuConstPoolData is a labeled 4- or 8-byte FP literal emitted after the body.
	fpuConstPoolData
	// atomicRmw / atomicCas / atomicLoad / atomicStore / fence implement the
	// threads proposal on top of the A extension.
	atomicRmw
	// atomicCas is compare-exchange. Unlike every other instruction here it
	// has *three* sources -- address, expected, replacement -- so it carries
	// the third in rs3 and is the sole user of useKindRS1RS2RS3. Modelling
	// only two would let the allocator overwrite the expected value, which
	// the LR/SC retry loop must keep live across iterations.
	atomicCas
	atomicLoad
	atomicStore
	fence
	// clzCtzPopcnt expands to the software bit-counting sequence.
	clzCtzPopcnt
	// vecMov is a whole-register vector copy (vmv1r.v), which needs no
	// preceding vsetivli because it is defined on the register rather than on
	// the current vtype.
	vecMov
	// vecLoad / vecStore move exactly 16 bytes to or from the address in rs1.
	// RVV load/store have no displacement field at all -- the address must
	// already be in a register -- so a spill computes it first.
	vecLoad
	vecStore

	numInstructionKinds
)

// ---------------------------------------------------------------------------
// operand
// ---------------------------------------------------------------------------

type operandKind byte

const (
	// operandKindInvalid marks an unused operand slot.
	operandKindInvalid operandKind = iota
	// operandKindNR is a plain register.
	operandKindNR
	// operandKindImm is a signed 12-bit immediate.
	operandKindImm
)

type operand struct {
	kind operandKind
	r    regalloc.VReg
	imm  int64
}

func operandNR(r regalloc.VReg) operand { return operand{kind: operandKindNR, r: r} }

func operandImm(v int64) operand {
	if !fitsInSignedImm12(v) {
		panic(fmt.Sprintf("BUG: %d does not fit in a signed 12-bit immediate", v))
	}
	return operand{kind: operandKindImm, imm: v}
}

func (o operand) nr() regalloc.VReg {
	if o.kind != operandKindNR {
		panic("BUG: not a register operand")
	}
	return o.r
}

func (o operand) realReg() regalloc.RealReg { return o.nr().RealReg() }

func (o operand) isReg() bool { return o.kind == operandKindNR }

func (o operand) format() string {
	switch o.kind {
	case operandKindNR:
		return formatVReg(o.r)
	case operandKindImm:
		return fmt.Sprintf("%d", o.imm)
	default:
		return "<invalid>"
	}
}

// ---------------------------------------------------------------------------
// addressMode
// ---------------------------------------------------------------------------

type addressModeKind byte

const (
	addressModeKindInvalid addressModeKind = iota
	// addressModeKindRegSignedImm12 is the only addressing mode RISC-V has:
	// [rn + imm12]. Everything else must be materialized into a register.
	addressModeKindRegSignedImm12
	// addressModeKindArgStackSpace is [sp + imm + argStackOffset], where the
	// offset is not known until the frame is laid out.
	addressModeKindArgStackSpace
	// addressModeKindResultStackSpace is the same for the result area.
	addressModeKindResultStackSpace
)

type addressMode struct {
	kind addressModeKind
	rn   regalloc.VReg
	imm  int64
}

func resetAddressMode(a *addressMode) { *a = addressMode{} }

func (a *addressMode) format() string {
	return fmt.Sprintf("%d(%s)", a.imm, formatVReg(a.rn))
}

// ---------------------------------------------------------------------------
// unary FP ops
// ---------------------------------------------------------------------------

type fpuUniOp byte

const (
	fpuUniOpSqrt fpuUniOp = iota
	fpuUniOpNeg
	fpuUniOpAbs
)

func (o fpuUniOp) String() string {
	switch o {
	case fpuUniOpSqrt:
		return "fsqrt"
	case fpuUniOpNeg:
		return "fneg"
	case fpuUniOpAbs:
		return "fabs"
	}
	panic(fmt.Sprintf("BUG: unknown fpuUniOp %d", o))
}

// ---------------------------------------------------------------------------
// regalloc.Instr
// ---------------------------------------------------------------------------

// IsCall implements regalloc.Instr.
func (i *instruction) IsCall() bool { return i.kind == call }

// IsIndirectCall implements regalloc.Instr.
func (i *instruction) IsIndirectCall() bool { return i.kind == callInd }

// IsReturn implements regalloc.Instr.
func (i *instruction) IsReturn() bool { return i.kind == ret }

// IsCopy implements regalloc.Instr.
func (i *instruction) IsCopy() bool { return i.kind == mov || i.kind == fpuMov }

type defKind byte

const (
	defKindNone defKind = iota + 1
	defKindRD
	defKindCall
)

var defKinds = [numInstructionKinds]defKind{
	nop0:              defKindNone,
	sourceOffsetInfo:  defKindNone,
	loadConstBlockArg: defKindRD,
	aluRRR:            defKindRD,
	shiftImm:          defKindRD,
	lui:               defKindRD,
	adr:               defKindRD,
	mov:               defKindRD,
	fpuMov:            defKindRD,
	load:              defKindRD,
	store:             defKindNone,
	fpuLoad:           defKindRD,
	fpuStore:          defKindNone,
	condBr:            defKindNone,
	br:                defKindNone,
	brTableSequence:   defKindNone,
	call:              defKindCall,
	callInd:           defKindCall,
	tailCall:          defKindCall,
	tailCallInd:       defKindCall,
	ret:               defKindNone,
	exitSequence:      defKindNone,
	udf:               defKindNone,
	fpuRRR:            defKindRD,
	fpuRR:             defKindRD,
	fpuCmp:            defKindRD,
	fpuRound:          defKindRD,
	fcvtToInt:         defKindRD,
	fcvtFromInt:       defKindRD,
	fcvtSD:            defKindRD,
	fmvToInt:          defKindRD,
	fmvFromInt:        defKindRD,
	fclass:            defKindRD,
	fpuConstPoolData:  defKindNone,
	atomicRmw:         defKindRD,
	atomicCas:         defKindRD,
	atomicLoad:        defKindRD,
	atomicStore:       defKindNone,
	fence:             defKindNone,
	clzCtzPopcnt:      defKindRD,
	vecMov:            defKindRD,
	vecLoad:           defKindRD,
	vecStore:          defKindNone,
}

// Defs implements regalloc.Instr.
func (i *instruction) Defs(regs *[]regalloc.VReg) []regalloc.VReg {
	*regs = (*regs)[:0]
	switch defKinds[i.kind] {
	case defKindNone:
	case defKindRD:
		*regs = append(*regs, i.rd)
	case defKindCall:
		_, _, retIntRealRegs, retFloatRealRegs, _ := backend.ABIInfoFromUint64(i.u2)
		for i := byte(0); i < retIntRealRegs; i++ {
			*regs = append(*regs, regInfo.RealRegToVReg[intParamResultRegs[i]])
		}
		for i := byte(0); i < retFloatRealRegs; i++ {
			*regs = append(*regs, regInfo.RealRegToVReg[floatParamResultRegs[i]])
		}
	default:
		panic(fmt.Sprintf("BUG: invalid defKind \"%d\" for %s", defKinds[i.kind], i))
	}
	return *regs
}

// AssignDef implements regalloc.Instr.
func (i *instruction) AssignDef(reg regalloc.VReg) {
	switch defKinds[i.kind] {
	case defKindNone:
	case defKindRD:
		i.rd = reg
	default:
		panic(fmt.Sprintf("BUG: invalid defKind \"%d\" for %s", defKinds[i.kind], i))
	}
}

type useKind byte

const (
	useKindNone       useKind = iota + 1
	useKindRS1                // rs1 only
	useKindRS1RS2             // rs1 and rs2 (rs2 may be an immediate, which is skipped)
	useKindRS1RS2RS3          // rs1, rs2 and rs3, all registers (compare-exchange)
	useKindRDRS1              // rd is a stored *source*, rs1 the address register
	useKindRS1Amode           // rs1 is the address base
	useKindRDRS1Amode         // rd is a stored *source*, rs1 the address base
	useKindCall
	useKindCallInd
)

var useKinds = [numInstructionKinds]useKind{
	nop0:              useKindNone,
	sourceOffsetInfo:  useKindNone,
	loadConstBlockArg: useKindNone,
	aluRRR:            useKindRS1RS2,
	shiftImm:          useKindRS1,
	lui:               useKindNone,
	adr:               useKindNone,
	mov:               useKindRS1,
	fpuMov:            useKindRS1,
	load:              useKindRS1Amode,
	store:             useKindRDRS1Amode,
	fpuLoad:           useKindRS1Amode,
	fpuStore:          useKindRDRS1Amode,
	condBr:            useKindRS1RS2,
	br:                useKindNone,
	brTableSequence:   useKindRS1,
	call:              useKindCall,
	callInd:           useKindCallInd,
	tailCall:          useKindCall,
	tailCallInd:       useKindCallInd,
	ret:               useKindNone,
	exitSequence:      useKindRS1,
	udf:               useKindNone,
	fpuRRR:            useKindRS1RS2,
	fpuRR:             useKindRS1,
	fpuCmp:            useKindRS1RS2,
	fpuRound:          useKindRS1,
	fcvtToInt:         useKindRS1,
	fcvtFromInt:       useKindRS1,
	fcvtSD:            useKindRS1,
	fmvToInt:          useKindRS1,
	fmvFromInt:        useKindRS1,
	fclass:            useKindRS1,
	fpuConstPoolData:  useKindNone,
	atomicRmw:         useKindRS1RS2,
	atomicCas:         useKindRS1RS2RS3,
	atomicLoad:        useKindRS1,
	atomicStore:       useKindRS1RS2,
	fence:             useKindNone,
	clzCtzPopcnt:      useKindRS1,
	vecMov:            useKindRS1,
	vecLoad:           useKindRS1,
	// A vector store reads the value in rd and the address in rs1, the same
	// shape as the scalar store.
	vecStore: useKindRDRS1,
}

// Uses implements regalloc.Instr.
func (i *instruction) Uses(regs *[]regalloc.VReg) []regalloc.VReg {
	*regs = (*regs)[:0]
	switch useKinds[i.kind] {
	case useKindNone:
	case useKindRS1:
		if i.rs1.isReg() {
			*regs = append(*regs, i.rs1.nr())
		}
	case useKindRS1RS2:
		if i.rs1.isReg() {
			*regs = append(*regs, i.rs1.nr())
		}
		if i.rs2.isReg() {
			*regs = append(*regs, i.rs2.nr())
		}
	case useKindRS1RS2RS3:
		*regs = append(*regs, i.rs1.nr(), i.rs2.nr(), i.rs3.nr())
	case useKindRDRS1:
		*regs = append(*regs, i.rd, i.rs1.nr())
	case useKindRS1Amode:
		*regs = append(*regs, i.getAmode().rn)
	case useKindRDRS1Amode:
		// A store reads the value in rd and the base in the address mode.
		*regs = append(*regs, i.rd, i.getAmode().rn)
	case useKindCall:
		argIntRealRegs, argFloatRealRegs, _, _, _ := backend.ABIInfoFromUint64(i.u2)
		for j := byte(0); j < argIntRealRegs; j++ {
			*regs = append(*regs, regInfo.RealRegToVReg[intParamResultRegs[j]])
		}
		for j := byte(0); j < argFloatRealRegs; j++ {
			*regs = append(*regs, regInfo.RealRegToVReg[floatParamResultRegs[j]])
		}
	case useKindCallInd:
		*regs = append(*regs, i.rs1.nr())
		argIntRealRegs, argFloatRealRegs, _, _, _ := backend.ABIInfoFromUint64(i.u2)
		for j := byte(0); j < argIntRealRegs; j++ {
			*regs = append(*regs, regInfo.RealRegToVReg[intParamResultRegs[j]])
		}
		for j := byte(0); j < argFloatRealRegs; j++ {
			*regs = append(*regs, regInfo.RealRegToVReg[floatParamResultRegs[j]])
		}
	default:
		panic(fmt.Sprintf("BUG: invalid useKind %d for %s", useKinds[i.kind], i))
	}
	return *regs
}

// AssignUse implements regalloc.Instr.
func (i *instruction) AssignUse(index int, reg regalloc.VReg) {
	switch useKinds[i.kind] {
	case useKindNone:
	case useKindRS1:
		if i.rs1.isReg() {
			i.rs1 = operandNR(reg)
		}
	case useKindRS1RS2:
		if index == 0 {
			if i.rs1.isReg() {
				i.rs1 = operandNR(reg)
			} else {
				i.rs2 = operandNR(reg)
			}
		} else {
			i.rs2 = operandNR(reg)
		}
	case useKindRS1RS2RS3:
		switch index {
		case 0:
			i.rs1 = operandNR(reg)
		case 1:
			i.rs2 = operandNR(reg)
		default:
			i.rs3 = operandNR(reg)
		}
	case useKindRDRS1:
		if index == 0 {
			i.rd = reg
		} else {
			i.rs1 = operandNR(reg)
		}
	case useKindRS1Amode:
		i.getAmode().rn = reg
	case useKindRDRS1Amode:
		if index == 0 {
			i.rd = reg
		} else {
			i.getAmode().rn = reg
		}
	case useKindCallInd:
		if index == 0 {
			i.rs1 = operandNR(reg)
		}
	case useKindCall:
	default:
		panic(fmt.Sprintf("BUG: invalid useKind %d for %s", useKinds[i.kind], i))
	}
}

// ---------------------------------------------------------------------------
// constructors
// ---------------------------------------------------------------------------

// asNop0 builds an *unlabeled* nop0. u1 must be the invalid sentinel rather
// than the zero value: label 0 is a perfectly good label (SSA block 0, the
// entry block), so a zero u1 would make every block-boundary nop claim to
// anchor L0 and clobber the entry block's resolved offset during layout.
func (i *instruction) asNop0() *instruction {
	i.kind = nop0
	i.u1 = uint64(labelInvalid)
	return i
}

func (i *instruction) asNop0WithLabel(l label) *instruction {
	i.kind = nop0
	i.u1 = uint64(l)
	return i
}

// nop0Label returns the label anchored by this nop0, if any. labelReturn is
// rejected alongside labelInvalid: the return block's labelPosition lives in
// machine.returnLabelPos, not in the pool, so it must never be looked up there.
func (i *instruction) nop0Label() (label, bool) {
	l := label(i.u1)
	if l == labelInvalid || l == labelReturn {
		return labelInvalid, false
	}
	return l, true
}

func (i *instruction) asALU(op aluOp, rd regalloc.VReg, rs1, rs2 operand, _64bit bool) *instruction {
	i.kind = aluRRR
	i.rd = rd
	i.rs1 = rs1
	i.rs2 = rs2
	i.u1 = uint64(op)
	i.u2 = b2u64(_64bit)
	return i
}

func (i *instruction) asShiftImm(op aluOp, rd regalloc.VReg, rs1 operand, shamt int64, _64bit bool) *instruction {
	i.kind = shiftImm
	i.rd = rd
	i.rs1 = rs1
	i.rs2 = operand{kind: operandKindImm, imm: shamt}
	i.u1 = uint64(op)
	i.u2 = b2u64(_64bit)
	return i
}

func (i *instruction) asLui(rd regalloc.VReg, v int32) *instruction {
	i.kind = lui
	i.rd = rd
	i.u1 = uint64(uint32(v))
	return i
}

func (i *instruction) asAdr(rd regalloc.VReg, l label) *instruction {
	i.kind = adr
	i.rd = rd
	i.u1 = uint64(l)
	return i
}

// asAdrPCRel is asAdr for a displacement already known in bytes, rather than
// one resolved from a label. u1 is set to labelInvalid so
// resolveRelativeAddresses leaves the offset alone.
func (i *instruction) asAdrPCRel(rd regalloc.VReg, offset int64) *instruction {
	i.kind = adr
	i.rd = rd
	i.u1 = uint64(labelInvalid)
	i.u2 = uint64(uint32(int32(offset)))
	return i
}

func (i *instruction) asMove64(rd, rs regalloc.VReg) *instruction {
	i.kind = mov
	i.rd = rd
	i.rs1 = operandNR(rs)
	return i
}

func (i *instruction) asFpuMov(rd, rs regalloc.VReg) *instruction {
	i.kind = fpuMov
	i.rd = rd
	i.rs1 = operandNR(rs)
	return i
}

func (i *instruction) asLoad(rd regalloc.VReg, amode *addressMode, bits byte, signed bool) *instruction {
	i.kind = load
	i.rd = rd
	i.setAmode(amode)
	i.u1 = uint64(bits)
	i.u2 = b2u64(signed)
	return i
}

// A load or store whose displacement does not fit in the 12-bit field grows
// into `lui tmp, hi; add tmp, tmp, base; l/s rd, lo(tmp)`. The flag lives in
// bit 1 of u2 (bit 0 is the load's signedness) so size() and encode() agree
// without a separate field on every instruction.
func (i *instruction) setBigOffset(v bool) { i.u2 = i.u2&^2 | b2u64(v)<<1 }

func (i *instruction) bigOffset() bool { return i.u2&2 != 0 }

func (i *instruction) loadIsSigned() bool { return i.u2&1 != 0 }

func (i *instruction) asFpuLoad(rd regalloc.VReg, amode *addressMode, bits byte) *instruction {
	i.kind = fpuLoad
	i.rd = rd
	i.setAmode(amode)
	i.u1 = uint64(bits)
	return i
}

// asStore stores `src`. `isInt` selects the integer or FP store; the source
// register lives in rd because that is the field the use-table reads.
func (i *instruction) asStore(src regalloc.VReg, amode *addressMode, bits byte, isInt bool) *instruction {
	if isInt {
		i.kind = store
	} else {
		i.kind = fpuStore
	}
	i.rd = src
	i.setAmode(amode)
	i.u1 = uint64(bits)
	return i
}

// Branch encoding, and why both branch kinds carry an "expansion level".
//
// RISC-V conditional branches reach only +/-4KiB and jal only +/-1MiB, both far
// short of what a large compiled function needs. Rather than always emitting
// the worst case, each branch records how far it has had to be expanded, and
// resolveRelativeAddresses grows it only when the resolved displacement does
// not fit:
//
//	condBr level 0 (4B):  b<cond>  rs1, rs2, target
//	condBr level 1 (8B):  b<!cond> rs1, rs2, .+8 ; jal zero, target
//	condBr level 2 (12B): b<!cond> rs1, rs2, .+12; auipc tmp, hi; jalr zero, lo(tmp)
//	br     level 0 (4B):  jal zero, target
//	br     level 1 (8B):  auipc tmp, hi; jalr zero, lo(tmp)
//
// Expansion is monotonic -- a level never decreases -- so the fixed-point loop
// in resolveRelativeAddresses converges: each pass can only grow the function,
// and there are finitely many branches to grow.
//
// Field packing for condBr: u1 holds the condition in bits [7:0], the
// expansion level in [15:8] and the resolved displacement in [63:32];
// u2 holds the target label. For br: u1 is the label, u2 packs the level in
// bit 0 and the displacement in [63:32].

func (i *instruction) asCondBr(c condFlag, rs1, rs2 operand, target label) *instruction {
	i.kind = condBr
	i.rs1 = rs1
	i.rs2 = rs2
	i.u1 = uint64(c)
	i.u2 = uint64(target)
	return i
}

func (i *instruction) condBrCond() condFlag  { return condFlag(i.u1 & 0xff) }
func (i *instruction) condBrLabel() label    { return label(i.u2) }
func (i *instruction) condBrExpansion() byte { return byte(i.u1 >> 8 & 0xff) }

func (i *instruction) setCondBrExpansion(level byte) {
	i.u1 = i.u1&^0xff00 | uint64(level)<<8
}

func (i *instruction) condBrOffset() int64 { return int64(int32(i.u1 >> 32)) }

func (i *instruction) condBrOffsetResolve(offset int64) {
	i.u1 = i.u1&0xffffffff | uint64(uint32(int32(offset)))<<32
}

func (i *instruction) asBr(target label) *instruction {
	i.kind = br
	i.u1 = uint64(target)
	return i
}

func (i *instruction) brLabel() label    { return label(i.u1) }
func (i *instruction) brExpansion() byte { return byte(i.u2 & 1) }
func (i *instruction) setBrExpansion(level byte) {
	i.u2 = i.u2&^1 | uint64(level&1)
}
func (i *instruction) brOffset() int64 { return int64(int32(i.u2 >> 32)) }

func (i *instruction) brOffsetResolve(offset int64) {
	i.u2 = i.u2&0xffffffff | uint64(uint32(int32(offset)))<<32
}

// asBrTableSequence records a br_table dispatch: rs1 is the already-clamped
// index, u1 the entry in machine.jmpTableTargets and u2 the table length.
func (i *instruction) asBrTableSequence(index regalloc.VReg, tableIndex uint32, targetCount int) *instruction {
	i.kind = brTableSequence
	i.rs1 = operandNR(index)
	i.u1 = uint64(tableIndex)
	i.u2 = uint64(targetCount)
	return i
}

func (i *instruction) asRet() *instruction {
	i.kind = ret
	return i
}

func (i *instruction) asUDF() *instruction {
	i.kind = udf
	return i
}

func (i *instruction) asCall(ref ssa.FuncRef, abi *backend.FunctionABI) *instruction {
	i.kind = call
	i.u1 = uint64(ref)
	if abi != nil {
		i.u2 = abi.ABIInfoAsUint64()
	}
	return i
}

func (i *instruction) asCallIndirect(ptr regalloc.VReg, abi *backend.FunctionABI) *instruction {
	i.kind = callInd
	i.rs1 = operandNR(ptr)
	if abi != nil {
		i.u2 = abi.ABIInfoAsUint64()
	}
	return i
}

func (i *instruction) asTailCall(ref ssa.FuncRef, abi *backend.FunctionABI) *instruction {
	i.asCall(ref, abi)
	i.kind = tailCall
	return i
}

func (i *instruction) asTailCallIndirect(ptr regalloc.VReg, abi *backend.FunctionABI) *instruction {
	i.asCallIndirect(ptr, abi)
	i.kind = tailCallInd
	return i
}

func (i *instruction) callFuncRef() ssa.FuncRef { return ssa.FuncRef(i.u1) }

func (i *instruction) asExitSequence(execCtx regalloc.VReg) *instruction {
	i.kind = exitSequence
	i.rs1 = operandNR(execCtx)
	return i
}

func (i *instruction) asFpuRRR(op fpuBinOp, rd regalloc.VReg, rs1, rs2 operand, _64bit bool) *instruction {
	i.kind = fpuRRR
	i.rd = rd
	i.rs1 = rs1
	i.rs2 = rs2
	i.u1 = uint64(op)
	i.u2 = b2u64(_64bit)
	return i
}

func (i *instruction) asFpuRR(op fpuUniOp, rd regalloc.VReg, rs1 operand, _64bit bool) *instruction {
	i.kind = fpuRR
	i.rd = rd
	i.rs1 = rs1
	i.u1 = uint64(op)
	i.u2 = b2u64(_64bit)
	return i
}

func (i *instruction) asFpuCmp(op fpuCmpOp, rd regalloc.VReg, rs1, rs2 operand, _64bit bool) *instruction {
	i.kind = fpuCmp
	i.rd = rd
	i.rs1 = rs1
	i.rs2 = rs2
	i.u1 = uint64(op)
	i.u2 = b2u64(_64bit)
	return i
}

// asFcvtToInt is the fcvt.{w,wu,l,lu}.{s,d} family. u1 packs the three flags
// and u2 the rounding mode, which defaults to round-toward-zero because that
// is what wasm's truncating conversions want.
func (i *instruction) asFcvtToInt(rd regalloc.VReg, rs1 operand, dst64, src64, signed bool) *instruction {
	i.kind = fcvtToInt
	i.rd = rd
	i.rs1 = rs1
	i.u1 = b2u64(dst64) | b2u64(src64)<<1 | b2u64(signed)<<2
	i.u2 = rmRTZ
	return i
}

// asFcvtToIntRounded is asFcvtToInt under an explicit rounding mode, used by
// the ceil/floor/trunc/nearest lowering.
func (i *instruction) asFcvtToIntRounded(rd regalloc.VReg, rs1 operand, dst64, src64, signed bool, mode roundMode) *instruction {
	i.asFcvtToInt(rd, rs1, dst64, src64, signed)
	switch mode {
	case roundModeNearest:
		i.u2 = rmRNE
	case roundModeTrunc:
		i.u2 = rmRTZ
	case roundModeFloor:
		i.u2 = rmRDN
	case roundModeCeil:
		i.u2 = rmRUP
	default:
		panic(fmt.Sprintf("BUG: unknown round mode %d", mode))
	}
	return i
}

func (i *instruction) asFcvtFromInt(rd regalloc.VReg, rs1 operand, dst64, src64, signed bool) *instruction {
	i.kind = fcvtFromInt
	i.rd = rd
	i.rs1 = rs1
	i.u1 = b2u64(dst64) | b2u64(src64)<<1 | b2u64(signed)<<2
	return i
}

func (i *instruction) asFcvtSD(rd regalloc.VReg, rs1 operand, toDouble bool) *instruction {
	i.kind = fcvtSD
	i.rd = rd
	i.rs1 = rs1
	i.u1 = b2u64(toDouble)
	return i
}

func (i *instruction) asFmvToInt(rd regalloc.VReg, rs1 operand, _64bit bool) *instruction {
	i.kind = fmvToInt
	i.rd = rd
	i.rs1 = rs1
	i.u1 = b2u64(_64bit)
	return i
}

func (i *instruction) asFmvFromInt(rd regalloc.VReg, rs1 operand, _64bit bool) *instruction {
	i.kind = fmvFromInt
	i.rd = rd
	i.rs1 = rs1
	i.u1 = b2u64(_64bit)
	return i
}

func (i *instruction) asFclass(rd regalloc.VReg, rs1 operand, _64bit bool) *instruction {
	i.kind = fclass
	i.rd = rd
	i.rs1 = rs1
	i.u1 = b2u64(_64bit)
	return i
}

func (i *instruction) asVecMov(rd, rs regalloc.VReg) *instruction {
	i.kind = vecMov
	i.rd = rd
	i.rs1 = operandNR(rs)
	return i
}

// asVecLoad / asVecStore move the 16 bytes of a v128 to or from the address
// already materialized in addr.
func (i *instruction) asVecLoad(rd, addr regalloc.VReg) *instruction {
	i.kind = vecLoad
	i.rd = rd
	i.rs1 = operandNR(addr)
	return i
}

func (i *instruction) asVecStore(src, addr regalloc.VReg) *instruction {
	i.kind = vecStore
	i.rd = src
	i.rs1 = operandNR(addr)
	return i
}

func (i *instruction) asFence() *instruction {
	i.kind = fence
	return i
}

func (i *instruction) asSourceOffsetInfo(l ssa.SourceOffset) *instruction {
	i.kind = sourceOffsetInfo
	i.u1 = uint64(l)
	return i
}

func (i *instruction) sourceOffset() ssa.SourceOffset { return ssa.SourceOffset(i.u1) }

func (i *instruction) asLoadConstBlockArg(v uint64, typ ssa.Type, dst regalloc.VReg) *instruction {
	i.kind = loadConstBlockArg
	i.u1 = v
	i.u2 = uint64(typ)
	i.rd = dst
	return i
}

func (i *instruction) loadConstBlockArgData() (v uint64, typ ssa.Type, dst regalloc.VReg) {
	return i.u1, ssa.Type(i.u2), i.rd
}

// ---------------------------------------------------------------------------
// address mode plumbing
//
// The address mode lives in a pool rather than inline so that an instruction
// stays small; rs2 carries the pointer, mirroring arm64.
// ---------------------------------------------------------------------------

func (i *instruction) setAmode(a *addressMode) { i.amode = a }

func (i *instruction) getAmode() *addressMode { return i.amode }

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

func b2u64(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func resetInstruction(i *instruction) {
	*i = instruction{}
}

func linkInstr(prev, next *instruction) *instruction {
	prev.next = next
	next.prev = prev
	return next
}

// String implements fmt.Stringer, used by the register allocator's debug output
// and by machine.Format().
func (i *instruction) String() string {
	switch i.kind {
	case nop0:
		return "nop"
	case sourceOffsetInfo:
		return fmt.Sprintf("source_offset_info %d", i.sourceOffset())
	case loadConstBlockArg:
		v, typ, dst := i.loadConstBlockArgData()
		return fmt.Sprintf("load_const_block_arg %s, %d, %s", formatVReg(dst), v, typ)
	case aluRRR:
		op := aluOp(i.u1)
		suffix := ""
		if i.u2 == 0 {
			suffix = "w"
		}
		if i.rs2.kind == operandKindImm {
			return fmt.Sprintf("%si%s %s, %s, %s", op, suffix, formatVReg(i.rd), i.rs1.format(), i.rs2.format())
		}
		return fmt.Sprintf("%s%s %s, %s, %s", op, suffix, formatVReg(i.rd), i.rs1.format(), i.rs2.format())
	case shiftImm:
		op := aluOp(i.u1)
		suffix := ""
		if i.u2 == 0 {
			suffix = "w"
		}
		return fmt.Sprintf("%si%s %s, %s, %d", op, suffix, formatVReg(i.rd), i.rs1.format(), i.rs2.imm)
	case lui:
		return fmt.Sprintf("lui %s, %#x", formatVReg(i.rd), int32(uint32(i.u1))>>12)
	case adr:
		return fmt.Sprintf("adr %s, %s", formatVReg(i.rd), label(i.u1))
	case mov:
		return fmt.Sprintf("mv %s, %s", formatVReg(i.rd), i.rs1.format())
	case fpuMov:
		return fmt.Sprintf("fmv.d %s, %s", formatVReg(i.rd), i.rs1.format())
	case load:
		return fmt.Sprintf("l%s %s, %s", loadSuffix(byte(i.u1), i.u2 == 1), formatVReg(i.rd), i.getAmode().format())
	case store:
		return fmt.Sprintf("s%s %s, %s", widthSuffix(byte(i.u1)), formatVReg(i.rd), i.getAmode().format())
	case fpuLoad:
		return fmt.Sprintf("f%s %s, %s", fpWidthSuffix(byte(i.u1), true), formatVReg(i.rd), i.getAmode().format())
	case fpuStore:
		return fmt.Sprintf("f%s %s, %s", fpWidthSuffix(byte(i.u1), false), formatVReg(i.rd), i.getAmode().format())
	case condBr:
		return fmt.Sprintf("%s %s, %s, %s", i.condBrCond(), i.rs1.format(), i.rs2.format(), i.condBrLabel())
	case br:
		return fmt.Sprintf("j %s", i.brLabel())
	case brTableSequence:
		return fmt.Sprintf("br_table_sequence %s", i.rs1.format())
	case call:
		return fmt.Sprintf("call %d", i.u1)
	case callInd:
		return fmt.Sprintf("call_ind %s", i.rs1.format())
	case tailCall:
		return fmt.Sprintf("tail_call %d", i.u1)
	case tailCallInd:
		return fmt.Sprintf("tail_call_ind %s", i.rs1.format())
	case ret:
		return "ret"
	case exitSequence:
		return fmt.Sprintf("exit_sequence %s", i.rs1.format())
	case udf:
		return "udf"
	case fpuRRR:
		return fmt.Sprintf("%s.%s %s, %s, %s", fpuBinOp(i.u1), fpSuffix(i.u2 == 1),
			formatVReg(i.rd), i.rs1.format(), i.rs2.format())
	case fpuRR:
		return fmt.Sprintf("%s.%s %s, %s", fpuUniOp(i.u1), fpSuffix(i.u2 == 1), formatVReg(i.rd), i.rs1.format())
	case fpuCmp:
		return fmt.Sprintf("%s.%s %s, %s, %s", fpuCmpOp(i.u1), fpSuffix(i.u2 == 1),
			formatVReg(i.rd), i.rs1.format(), i.rs2.format())
	case fpuRound:
		return fmt.Sprintf("fround %s, %s", formatVReg(i.rd), i.rs1.format())
	case fcvtToInt:
		return fmt.Sprintf("fcvt_to_int %s, %s", formatVReg(i.rd), i.rs1.format())
	case fcvtFromInt:
		return fmt.Sprintf("fcvt_from_int %s, %s", formatVReg(i.rd), i.rs1.format())
	case fcvtSD:
		return fmt.Sprintf("fcvt_sd %s, %s", formatVReg(i.rd), i.rs1.format())
	case fmvToInt:
		return fmt.Sprintf("fmv.x %s, %s", formatVReg(i.rd), i.rs1.format())
	case fmvFromInt:
		return fmt.Sprintf("fmv.f %s, %s", formatVReg(i.rd), i.rs1.format())
	case fclass:
		return fmt.Sprintf("fclass %s, %s", formatVReg(i.rd), i.rs1.format())
	case fpuConstPoolData:
		return fmt.Sprintf("fp_const_pool_data %#x", i.u1)
	case atomicRmw:
		return fmt.Sprintf("atomic_rmw %s, %s, %s", formatVReg(i.rd), i.rs1.format(), i.rs2.format())
	case atomicCas:
		return fmt.Sprintf("atomic_cas %s, %s, %s", formatVReg(i.rd), i.rs1.format(), i.rs2.format())
	case atomicLoad:
		return fmt.Sprintf("atomic_load %s, %s", formatVReg(i.rd), i.rs1.format())
	case atomicStore:
		return fmt.Sprintf("atomic_store %s, %s", i.rs1.format(), i.rs2.format())
	case fence:
		return "fence rw, rw"
	case clzCtzPopcnt:
		return fmt.Sprintf("bitcount %s, %s", formatVReg(i.rd), i.rs1.format())
	case vecMov:
		return fmt.Sprintf("vmv1r.v %s, %s", formatVReg(i.rd), i.rs1.format())
	case vecLoad:
		return fmt.Sprintf("vle64.v %s, (%s)", formatVReg(i.rd), i.rs1.format())
	case vecStore:
		return fmt.Sprintf("vse64.v %s, (%s)", formatVReg(i.rd), i.rs1.format())
	}
	panic(fmt.Sprintf("BUG: unknown instruction kind %d", i.kind))
}

func widthSuffix(bits byte) string {
	switch bits {
	case 8:
		return "b"
	case 16:
		return "h"
	case 32:
		return "w"
	case 64:
		return "d"
	}
	panic(fmt.Sprintf("BUG: invalid width %d", bits))
}

func loadSuffix(bits byte, signed bool) string {
	s := widthSuffix(bits)
	if !signed && bits != 64 {
		s += "u"
	}
	return s
}

func fpWidthSuffix(bits byte, isLoad bool) string {
	verb := "s"
	if isLoad {
		verb = "l"
	}
	if bits == 32 {
		return verb + "w"
	}
	return verb + "d"
}

func fpSuffix(_64bit bool) string {
	if _64bit {
		return "d"
	}
	return "s"
}

package riscv64

import "fmt"

// Raw RV64G instruction encoders.
//
// Everything here is a pure function from fields to a 32-bit word, so it can be
// (and is, in encode_test.go) checked instruction-for-instruction against a
// real assembler. The backend never emits compressed (RVC) instructions: every
// instruction is exactly four bytes, which keeps branch-offset resolution and
// the trampoline islands trivial.
//
// Field layout, from the RISC-V Unprivileged ISA spec, Chapter 2.2:
//
//	R-type  funct7[31:25] rs2[24:20] rs1[19:15] funct3[14:12] rd[11:7] opcode[6:0]
//	I-type  imm[11:0][31:20]         rs1        funct3        rd       opcode
//	S-type  imm[11:5]     rs2        rs1        funct3        imm[4:0] opcode
//	B-type  imm[12|10:5]  rs2        rs1        funct3        imm[4:1|11] opcode
//	U-type  imm[31:12][31:12]                                 rd       opcode
//	J-type  imm[20|10:1|11|19:12][31:12]                      rd       opcode

// Opcodes (inst[6:0]).
const (
	opLoad    = 0b0000011
	opLoadFP  = 0b0000111
	opMiscMem = 0b0001111
	opOpImm   = 0b0010011
	opAuipc   = 0b0010111
	opOpImm32 = 0b0011011
	opStore   = 0b0100011
	opStoreFP = 0b0100111
	opAmo     = 0b0101111
	opOp      = 0b0110011
	opLui     = 0b0110111
	opOp32    = 0b0111011
	opMadd    = 0b1000011
	opMsub    = 0b1000111
	opNmsub   = 0b1001011
	opNmadd   = 0b1001111
	opOpFP    = 0b1010011
	opBranch  = 0b1100011
	opJalr    = 0b1100111
	opJal     = 0b1101111
	opSystem  = 0b1110011
)

// Rounding modes (the funct3 field of an OP-FP arithmetic instruction).
const (
	rmRNE = 0b000 // round to nearest, ties to even -- what every wasm FP arithmetic op wants.
	rmRTZ = 0b001 // round towards zero -- what every wasm float->int truncation wants.
	rmDYN = 0b111 // take the mode from the fcsr.
)

func encodeR(funct7, rs2, rs1, funct3, rd, opcode uint32) uint32 {
	return funct7<<25 | rs2<<20 | rs1<<15 | funct3<<12 | rd<<7 | opcode
}

func encodeI(imm12 int32, rs1, funct3, rd, opcode uint32) uint32 {
	return uint32(imm12&0xfff)<<20 | rs1<<15 | funct3<<12 | rd<<7 | opcode
}

func encodeS(imm12 int32, rs2, rs1, funct3, opcode uint32) uint32 {
	u := uint32(imm12) & 0xfff
	return (u>>5)<<25 | rs2<<20 | rs1<<15 | funct3<<12 | (u&0x1f)<<7 | opcode
}

func encodeB(imm13 int32, rs2, rs1, funct3, opcode uint32) uint32 {
	u := uint32(imm13)
	return ((u>>12)&1)<<31 | ((u>>5)&0x3f)<<25 | rs2<<20 | rs1<<15 | funct3<<12 |
		((u>>1)&0xf)<<8 | ((u>>11)&1)<<7 | opcode
}

func encodeU(imm int32, rd, opcode uint32) uint32 {
	return uint32(imm)&0xfffff000 | rd<<7 | opcode
}

func encodeJ(imm21 int32, rd, opcode uint32) uint32 {
	u := uint32(imm21)
	return ((u>>20)&1)<<31 | ((u>>1)&0x3ff)<<21 | ((u>>11)&1)<<20 | ((u>>12)&0xff)<<12 |
		rd<<7 | opcode
}

// fitsInSignedImm12 reports whether v is representable in the 12-bit signed
// immediate every I-type and S-type instruction carries.
func fitsInSignedImm12(v int64) bool { return v >= -2048 && v <= 2047 }

// fitsInSignedImm13 reports whether v is a reachable conditional-branch
// displacement (13-bit signed, always even).
func fitsInSignedImm13(v int64) bool { return v >= -4096 && v <= 4095 && v%2 == 0 }

// fitsInSignedImm21 reports whether v is a reachable jal displacement (21-bit
// signed, always even).
func fitsInSignedImm21(v int64) bool { return v >= -1048576 && v <= 1048575 && v%2 == 0 }

// splitImm32 splits v into the (hi, lo) pair that `lui hi; addi lo` (or
// `auipc hi; jalr lo`) materializes. The low half is the sign-extended low 12
// bits, and because addi and jalr sign-extend that immediate, a set bit 11 is
// compensated by rounding the high half up.
//
// Above maxAuipcPairOffset the `v - lo` carry pushes hi past INT32_MAX and it
// wraps. Whether that matters depends entirely on what closes the pair:
//
//   - `lui hi; addiw lo` is correct for the *whole* int32 range. addiw
//     truncates the sum to 32 bits and re-extends, which undoes the wrap
//     exactly. Constant materialization uses this form and needs no gate.
//   - `auipc hi; jalr lo`, `auipc hi; addi lo` and `lui hi; add rN` have no
//     truncating tail, so a wrapped hi is fatal -- and silently so, because
//     auipc sign-extends its 20-bit upper immediate on RV64, landing 4GiB
//     below the intended address rather than failing. Those callers must gate
//     on fitsInAuipcPair first.
//
// splitImm32 itself is pure arithmetic and deliberately does not panic: it
// cannot tell which of the two shapes its caller is building.
func splitImm32(v int32) (hi, lo int32) {
	lo = int32(v) << 20 >> 20 // sign-extend the low 12 bits
	hi = v - lo
	return
}

// The reachable range of an auipc/lui + addi(/jalr) pair. It is asymmetric:
// the low half contributes [-2048, 2047] on top of a high half that is a
// multiple of 4096 capped at INT32_MAX-4095.
const (
	maxAuipcPairOffset = 0x7ffff7ff
	minAuipcPairOffset = -0x80000800
)

func fitsInAuipcPair(v int64) bool {
	return v >= minAuipcPairOffset && v <= maxAuipcPairOffset
}

// splitImm32PCRel is splitImm32 for the pair shapes that have no truncating
// tail -- auipc+jalr, auipc+addi, lui+add -- where a wrapped high half is
// silently wrong rather than merely surprising. Every such displacement here
// is an intra-function or intra-executable offset that cannot legitimately
// approach 2GiB, so this is an assertion rather than a condition to handle.
func splitImm32PCRel(v int64, what string) (hi, lo int32) {
	if !fitsInAuipcPair(v) {
		panic(fmt.Sprintf("BUG: %s displacement %#x is out of auipc-pair range", what, v))
	}
	return splitImm32(int32(v))
}

// ---------------------------------------------------------------------------
// Integer register-register (RV64I + RV64M)
// ---------------------------------------------------------------------------

type aluOp byte

const (
	aluOpAdd aluOp = iota
	aluOpSub
	aluOpSll
	aluOpSlt
	aluOpSltu
	aluOpXor
	aluOpSrl
	aluOpSra
	aluOpOr
	aluOpAnd
	aluOpMul
	aluOpMulhu
	aluOpMulh
	aluOpDiv
	aluOpDivu
	aluOpRem
	aluOpRemu
)

func (a aluOp) String() string {
	switch a {
	case aluOpAdd:
		return "add"
	case aluOpSub:
		return "sub"
	case aluOpSll:
		return "sll"
	case aluOpSlt:
		return "slt"
	case aluOpSltu:
		return "sltu"
	case aluOpXor:
		return "xor"
	case aluOpSrl:
		return "srl"
	case aluOpSra:
		return "sra"
	case aluOpOr:
		return "or"
	case aluOpAnd:
		return "and"
	case aluOpMul:
		return "mul"
	case aluOpMulhu:
		return "mulhu"
	case aluOpMulh:
		return "mulh"
	case aluOpDiv:
		return "div"
	case aluOpDivu:
		return "divu"
	case aluOpRem:
		return "rem"
	case aluOpRemu:
		return "remu"
	}
	panic(fmt.Sprintf("BUG: unknown aluOp %d", a))
}

// aluRRRFields returns the (funct7, funct3) pair for a register-register ALU
// op. `_64bit` selects between OP/OP-32 (handled by the caller via aluOpcode).
func aluRRRFields(a aluOp) (funct7, funct3 uint32) {
	switch a {
	case aluOpAdd:
		return 0b0000000, 0b000
	case aluOpSub:
		return 0b0100000, 0b000
	case aluOpSll:
		return 0b0000000, 0b001
	case aluOpSlt:
		return 0b0000000, 0b010
	case aluOpSltu:
		return 0b0000000, 0b011
	case aluOpXor:
		return 0b0000000, 0b100
	case aluOpSrl:
		return 0b0000000, 0b101
	case aluOpSra:
		return 0b0100000, 0b101
	case aluOpOr:
		return 0b0000000, 0b110
	case aluOpAnd:
		return 0b0000000, 0b111
	case aluOpMul:
		return 0b0000001, 0b000
	case aluOpMulh:
		return 0b0000001, 0b001
	case aluOpMulhu:
		return 0b0000001, 0b011
	case aluOpDiv:
		return 0b0000001, 0b100
	case aluOpDivu:
		return 0b0000001, 0b101
	case aluOpRem:
		return 0b0000001, 0b110
	case aluOpRemu:
		return 0b0000001, 0b111
	}
	panic(fmt.Sprintf("BUG: unknown aluOp %d", a))
}

// encodeAluRRR encodes `op rd, rs1, rs2`. When _64bit is false the "word"
// (.w) form is emitted, which computes on the low 32 bits and sign-extends the
// result into the full 64-bit destination -- exactly wasm's i32 semantics
// given the sign-extended representation this backend maintains.
func encodeAluRRR(a aluOp, rd, rs1, rs2 uint32, _64bit bool) uint32 {
	funct7, funct3 := aluRRRFields(a)
	opcode := uint32(opOp)
	if !_64bit {
		if !aluOpHasWordForm(a) {
			panic("BUG: " + a.String() + " has no 32-bit word form")
		}
		opcode = opOp32
	}
	return encodeR(funct7, rs2, rs1, funct3, rd, opcode)
}

// aluOpHasWordForm reports whether the op has an OP-32 (.w) encoding. RV64
// only defines word forms where the narrow result differs from truncating the
// wide one: the bitwise ops and the set-less-thans have none.
func aluOpHasWordForm(a aluOp) bool {
	switch a {
	case aluOpAdd, aluOpSub, aluOpSll, aluOpSrl, aluOpSra,
		aluOpMul, aluOpDiv, aluOpDivu, aluOpRem, aluOpRemu:
		return true
	}
	return false
}

// encodeAluRRImm encodes the I-type form `op rd, rs1, imm`. Only the ops with
// an OP-IMM encoding are valid here; sub-immediate does not exist (negate the
// immediate and use addi).
func encodeAluRRImm(a aluOp, rd, rs1 uint32, imm int32, _64bit bool) uint32 {
	opcode := uint32(opOpImm)
	if !_64bit {
		opcode = opOpImm32
	}
	switch a {
	case aluOpAdd:
		return encodeI(imm, rs1, 0b000, rd, opcode)
	case aluOpSlt:
		return encodeI(imm, rs1, 0b010, rd, opOpImm)
	case aluOpSltu:
		return encodeI(imm, rs1, 0b011, rd, opOpImm)
	case aluOpXor:
		return encodeI(imm, rs1, 0b100, rd, opOpImm)
	case aluOpOr:
		return encodeI(imm, rs1, 0b110, rd, opOpImm)
	case aluOpAnd:
		return encodeI(imm, rs1, 0b111, rd, opOpImm)
	case aluOpSll:
		return encodeShiftImm(0b0000000, imm, rs1, 0b001, rd, _64bit)
	case aluOpSrl:
		return encodeShiftImm(0b0000000, imm, rs1, 0b101, rd, _64bit)
	case aluOpSra:
		return encodeShiftImm(0b0100000, imm, rs1, 0b101, rd, _64bit)
	}
	panic("BUG: " + a.String() + " has no immediate form")
}

// encodeShiftImm encodes slli/srli/srai and their .w forms. The shift amount
// is 6 bits on RV64 and 5 bits in the word forms, with the discriminating
// funct7-ish bits above it.
func encodeShiftImm(upper uint32, shamt int32, rs1, funct3, rd uint32, _64bit bool) uint32 {
	if _64bit {
		if shamt < 0 || shamt > 63 {
			panic(fmt.Sprintf("BUG: shift amount out of range: %d", shamt))
		}
		return encodeR(upper|uint32(shamt)>>5, uint32(shamt)&0x1f, rs1, funct3, rd, opOpImm)
	}
	if shamt < 0 || shamt > 31 {
		panic(fmt.Sprintf("BUG: word shift amount out of range: %d", shamt))
	}
	return encodeR(upper, uint32(shamt), rs1, funct3, rd, opOpImm32)
}

func encodeLui(rd uint32, imm int32) uint32   { return encodeU(imm, rd, opLui) }
func encodeAuipc(rd uint32, imm int32) uint32 { return encodeU(imm, rd, opAuipc) }

// ---------------------------------------------------------------------------
// Loads and stores
// ---------------------------------------------------------------------------

// encodeLoad encodes `l{b,h,w,d}[u] rd, imm(rs1)`.
func encodeLoad(rd, rs1 uint32, imm int32, bits byte, signed bool) uint32 {
	var funct3 uint32
	switch bits {
	case 8:
		funct3 = 0b000
	case 16:
		funct3 = 0b001
	case 32:
		funct3 = 0b010
	case 64:
		funct3 = 0b011
	default:
		panic(fmt.Sprintf("BUG: invalid load width %d", bits))
	}
	if !signed && bits != 64 {
		funct3 |= 0b100 // lbu/lhu/lwu
	}
	return encodeI(imm, rs1, funct3, rd, opLoad)
}

// encodeStore encodes `s{b,h,w,d} rs2, imm(rs1)`.
func encodeStore(rs2, rs1 uint32, imm int32, bits byte) uint32 {
	var funct3 uint32
	switch bits {
	case 8:
		funct3 = 0b000
	case 16:
		funct3 = 0b001
	case 32:
		funct3 = 0b010
	case 64:
		funct3 = 0b011
	default:
		panic(fmt.Sprintf("BUG: invalid store width %d", bits))
	}
	return encodeS(imm, rs2, rs1, funct3, opStore)
}

// encodeFpuLoad encodes `f{lw,ld} rd, imm(rs1)`.
func encodeFpuLoad(rd, rs1 uint32, imm int32, bits byte) uint32 {
	funct3 := uint32(0b010) // flw
	if bits == 64 {
		funct3 = 0b011 // fld
	}
	return encodeI(imm, rs1, funct3, rd, opLoadFP)
}

// encodeFpuStore encodes `f{sw,sd} rs2, imm(rs1)`.
func encodeFpuStore(rs2, rs1 uint32, imm int32, bits byte) uint32 {
	funct3 := uint32(0b010) // fsw
	if bits == 64 {
		funct3 = 0b011 // fsd
	}
	return encodeS(imm, rs2, rs1, funct3, opStoreFP)
}

// ---------------------------------------------------------------------------
// Control flow
// ---------------------------------------------------------------------------

type condFlag byte

const (
	condEQ condFlag = iota
	condNE
	condLT  // signed
	condGE  // signed
	condLTU // unsigned
	condGEU // unsigned
)

func (c condFlag) String() string {
	switch c {
	case condEQ:
		return "beq"
	case condNE:
		return "bne"
	case condLT:
		return "blt"
	case condGE:
		return "bge"
	case condLTU:
		return "bltu"
	case condGEU:
		return "bgeu"
	}
	panic(fmt.Sprintf("BUG: unknown condFlag %d", c))
}

// invert returns the condition that is true exactly when c is false.
func (c condFlag) invert() condFlag {
	switch c {
	case condEQ:
		return condNE
	case condNE:
		return condEQ
	case condLT:
		return condGE
	case condGE:
		return condLT
	case condLTU:
		return condGEU
	case condGEU:
		return condLTU
	}
	panic(fmt.Sprintf("BUG: unknown condFlag %d", c))
}

func (c condFlag) funct3() uint32 {
	switch c {
	case condEQ:
		return 0b000
	case condNE:
		return 0b001
	case condLT:
		return 0b100
	case condGE:
		return 0b101
	case condLTU:
		return 0b110
	case condGEU:
		return 0b111
	}
	panic(fmt.Sprintf("BUG: unknown condFlag %d", c))
}

// encodeBranch encodes `b<cond> rs1, rs2, offset`.
func encodeBranch(c condFlag, rs1, rs2 uint32, offset int32) uint32 {
	return encodeB(offset, rs2, rs1, c.funct3(), opBranch)
}

// encodeJal encodes `jal rd, offset`.
func encodeJal(rd uint32, offset int32) uint32 { return encodeJ(offset, rd, opJal) }

// encodeJalr encodes `jalr rd, imm(rs1)`.
func encodeJalr(rd, rs1 uint32, imm int32) uint32 { return encodeI(imm, rs1, 0b000, rd, opJalr) }

// encodeRet encodes `ret`, the canonical alias for `jalr zero, 0(ra)`.
func encodeRet() uint32 { return encodeJalr(0, 1, 0) }

// encodeNop encodes `nop`, the canonical alias for `addi zero, zero, 0`.
func encodeNop() uint32 { return encodeI(0, 0, 0b000, 0, opOpImm) }

// encodeEbreak encodes `ebreak`, used to fill unreachable padding.
func encodeEbreak() uint32 { return encodeI(1, 0, 0b000, 0, opSystem) }

// encodeFence encodes `fence pred, succ`. `fence rw, rw` (0b0011, 0b0011) is
// the sequential-consistency barrier the threads proposal needs.
func encodeFence(pred, succ uint32) uint32 {
	return encodeI(int32(pred<<4|succ), 0, 0b000, 0, opMiscMem)
}

// ---------------------------------------------------------------------------
// Floating point (RV64F + RV64D)
// ---------------------------------------------------------------------------

type fpuBinOp byte

const (
	fpuBinOpAdd fpuBinOp = iota
	fpuBinOpSub
	fpuBinOpMul
	fpuBinOpDiv
	fpuBinOpMin
	fpuBinOpMax
	fpuBinOpSgnj  // fsgnj: copy sign
	fpuBinOpSgnjn // fsgnjn: copy inverted sign
	fpuBinOpSgnjx // fsgnjx: xor signs
)

func (o fpuBinOp) String() string {
	switch o {
	case fpuBinOpAdd:
		return "fadd"
	case fpuBinOpSub:
		return "fsub"
	case fpuBinOpMul:
		return "fmul"
	case fpuBinOpDiv:
		return "fdiv"
	case fpuBinOpMin:
		return "fmin"
	case fpuBinOpMax:
		return "fmax"
	case fpuBinOpSgnj:
		return "fsgnj"
	case fpuBinOpSgnjn:
		return "fsgnjn"
	case fpuBinOpSgnjx:
		return "fsgnjx"
	}
	panic(fmt.Sprintf("BUG: unknown fpuBinOp %d", o))
}

// fmtBit is the low bit of the OP-FP funct7 field: 0 for single, 1 for double.
func fmtBit(_64bit bool) uint32 {
	if _64bit {
		return 1
	}
	return 0
}

// A hardware FP instruction is not the same thing as the wasm operator that
// shares its name. Three divergences the lowering above this layer must close,
// none of which the encoders here can:
//
//   - FMIN/FMAX return the non-NaN operand when one input is NaN; wasm requires
//     NaN out. Guard with fclass/feq.
//   - FCVT.W/L on a NaN or an out-of-range input returns a saturated integer
//     and raises a flag. wasm's non-saturating truncations must *trap* on both,
//     and the saturating ones must return 0 for NaN, so both need an explicit
//     fclass + range check first.
//   - f32 values in an f register must be NaN-boxed (upper 32 bits all ones).
//     FLW and FMV.W.X produce a box; FLD and FMV.D.X do not. So an f32 arriving
//     from memory or from a GPR must come in through the single-precision form,
//     or subsequent f32 arithmetic sees a NaN instead of the value. Register
//     copies and spills, by contrast, must move all 64 bits (fsgnj.d, fsd/fld)
//     to *preserve* an existing box.

// encodeFpuRRR encodes a two-operand FP instruction.
func encodeFpuRRR(o fpuBinOp, rd, rs1, rs2 uint32, _64bit bool) uint32 {
	f := fmtBit(_64bit)
	switch o {
	case fpuBinOpAdd:
		return encodeR(0b0000000|f, rs2, rs1, rmRNE, rd, opOpFP)
	case fpuBinOpSub:
		return encodeR(0b0000100|f, rs2, rs1, rmRNE, rd, opOpFP)
	case fpuBinOpMul:
		return encodeR(0b0001000|f, rs2, rs1, rmRNE, rd, opOpFP)
	case fpuBinOpDiv:
		return encodeR(0b0001100|f, rs2, rs1, rmRNE, rd, opOpFP)
	case fpuBinOpSgnj:
		return encodeR(0b0010000|f, rs2, rs1, 0b000, rd, opOpFP)
	case fpuBinOpSgnjn:
		return encodeR(0b0010000|f, rs2, rs1, 0b001, rd, opOpFP)
	case fpuBinOpSgnjx:
		return encodeR(0b0010000|f, rs2, rs1, 0b010, rd, opOpFP)
	case fpuBinOpMin:
		// NOTE: FMIN/FMAX are *not* wasm's f32.min/f32.max. RISC-V returns the
		// non-NaN operand when exactly one input is NaN; wasm requires a NaN
		// result. The lowering must add the fclass/feq guard -- see the
		// warning above encodeFpuRRR.
		return encodeR(0b0010100|f, rs2, rs1, 0b000, rd, opOpFP)
	case fpuBinOpMax:
		return encodeR(0b0010100|f, rs2, rs1, 0b001, rd, opOpFP)
	}
	panic(fmt.Sprintf("BUG: unknown fpuBinOp %d", o))
}

// encodeFsqrt encodes `fsqrt.{s,d} rd, rs1`.
func encodeFsqrt(rd, rs1 uint32, _64bit bool) uint32 {
	return encodeR(0b0101100|fmtBit(_64bit), 0, rs1, rmRNE, rd, opOpFP)
}

type fpuCmpOp byte

const (
	fpuCmpOpEq fpuCmpOp = iota
	fpuCmpOpLt
	fpuCmpOpLe
)

func (o fpuCmpOp) String() string {
	switch o {
	case fpuCmpOpEq:
		return "feq"
	case fpuCmpOpLt:
		return "flt"
	case fpuCmpOpLe:
		return "fle"
	}
	panic(fmt.Sprintf("BUG: unknown fpuCmpOp %d", o))
}

// encodeFpuCmp encodes `f{eq,lt,le}.{s,d} rd, rs1, rs2`. Note rd is an
// *integer* register: RISC-V has no FP flags register, comparisons write 0/1
// straight into a GPR, which is exactly the shape wasm's i32 result wants.
func encodeFpuCmp(o fpuCmpOp, rd, rs1, rs2 uint32, _64bit bool) uint32 {
	var funct3 uint32
	switch o {
	case fpuCmpOpEq:
		funct3 = 0b010
	case fpuCmpOpLt:
		funct3 = 0b001
	case fpuCmpOpLe:
		funct3 = 0b000
	default:
		panic(fmt.Sprintf("BUG: unknown fpuCmpOp %d", o))
	}
	return encodeR(0b1010000|fmtBit(_64bit), rs2, rs1, funct3, rd, opOpFP)
}

// Rounding-mode field values for the FP instructions that take one.
const (
	rmRDN = 0b010 // toward -inf, i.e. floor
	rmRUP = 0b011 // toward +inf, i.e. ceil
)

// encodeFcvtToIntRM is encodeFcvtToInt with an explicit rounding mode, which
// is what implements ceil/floor/trunc/nearest: RV64D has no rounding
// instruction of its own (that is Zfa's fround), so each is a convert to
// integer under the matching mode and a convert back.
func encodeFcvtToIntRM(rd, rs1 uint32, dst64, src64, signed bool, rm uint32) uint32 {
	rs2 := uint32(0)
	if dst64 {
		rs2 = 2
	}
	if !signed {
		rs2 |= 1
	}
	return encodeR(0b1100000|fmtBit(src64), rs2, rs1, rm, rd, opOpFP)
}

// encodeFcvtToInt encodes `fcvt.{w,wu,l,lu}.{s,d} rd, rs1, rtz`. Always
// round-towards-zero: wasm's i32.trunc_f32_s and friends truncate.
func encodeFcvtToInt(rd, rs1 uint32, dst64, src64, signed bool) uint32 {
	rs2 := uint32(0) // .w
	if dst64 {
		rs2 = 2 // .l
	}
	if !signed {
		rs2 |= 1 // .wu / .lu
	}
	return encodeR(0b1100000|fmtBit(src64), rs2, rs1, rmRTZ, rd, opOpFP)
}

// encodeFcvtFromInt encodes `fcvt.{s,d}.{w,wu,l,lu} rd, rs1`.
func encodeFcvtFromInt(rd, rs1 uint32, dst64, src64, signed bool) uint32 {
	rs2 := uint32(0)
	if src64 {
		rs2 = 2
	}
	if !signed {
		rs2 |= 1
	}
	return encodeR(0b1101000|fmtBit(dst64), rs2, rs1, rmRNE, rd, opOpFP)
}

// encodeFcvtSD encodes `fcvt.s.d rd, rs1` (demote) or `fcvt.d.s` (promote).
func encodeFcvtSD(rd, rs1 uint32, toDouble bool) uint32 {
	if toDouble {
		return encodeR(0b0100001, 0, rs1, rmRNE, rd, opOpFP) // fcvt.d.s
	}
	return encodeR(0b0100000, 1, rs1, rmRNE, rd, opOpFP) // fcvt.s.d
}

// encodeFmvToInt encodes `fmv.x.{w,d} rd, rs1`: raw bit reinterpretation into
// an integer register.
func encodeFmvToInt(rd, rs1 uint32, _64bit bool) uint32 {
	return encodeR(0b1110000|fmtBit(_64bit), 0, rs1, 0b000, rd, opOpFP)
}

// encodeFmvFromInt encodes `fmv.{w,d}.x rd, rs1`.
func encodeFmvFromInt(rd, rs1 uint32, _64bit bool) uint32 {
	return encodeR(0b1111000|fmtBit(_64bit), 0, rs1, 0b000, rd, opOpFP)
}

// encodeFclass encodes `fclass.{s,d} rd, rs1`, writing a one-hot class mask
// into an integer register. Bit 8 is quiet-NaN and bit 9 signaling-NaN, which
// is how the float->int saturating conversions detect NaN without a branchy
// comparison.
func encodeFclass(rd, rs1 uint32, _64bit bool) uint32 {
	return encodeR(0b1110000|fmtBit(_64bit), 0, rs1, 0b001, rd, opOpFP)
}

// ---------------------------------------------------------------------------
// RVV (vector extension)
// ---------------------------------------------------------------------------
//
// wasm's v128 is exactly 128 bits, while RVV's VLEN is an implementation
// choice the compiler does not know. The two are reconciled by never relying
// on VLEN: every vector operation is preceded by a vsetivli that pins the
// element width and count so that exactly 16 bytes are in play -- 16 e8, 8
// e16, 4 e32 or 2 e64 elements at LMUL=1 -- which holds for any VLEN >= 128.
// That minimum is what the platform gate checks before selecting this backend.

const opVec = 0b1010111 // OP-V

// vsew encodes the element width field of a vtype immediate.
const (
	vsew8 uint32 = iota
	vsew16
	vsew32
	vsew64
)

// vecAVLFor returns the element count that covers exactly 16 bytes at the
// given element width.
func vecAVLFor(sew uint32) uint32 {
	switch sew {
	case vsew8:
		return 16
	case vsew16:
		return 8
	case vsew32:
		return 4
	case vsew64:
		return 2
	}
	panic(fmt.Sprintf("BUG: unknown vsew %d", sew))
}

// encodeVsetivli encodes `vsetivli zero, avl, e<sew>, m1, ta, ma`: set the
// vector type and length from immediates, discarding the resulting vl (rd =
// zero) because the caller has already chosen an avl that fits.
//
// The vtype immediate is vma<<7 | vta<<6 | vsew<<3 | vlmul, with vlmul=0 for
// LMUL=1, and tail/mask-agnostic set so the tail beyond 16 bytes is explicitly
// don't-care rather than something we would have to preserve.
func encodeVsetivli(avl, sew uint32) uint32 {
	const vtypeTaMa = 1<<7 | 1<<6 // vma, vta
	zimm := vtypeTaMa | sew<<3    // vlmul = 0 (m1)
	return 0b11<<30 | zimm<<20 | avl<<15 | 0b111<<12 | 0<<7 | opVec
}

// vecWidthField is the `width` field of a vector load/store, which is not the
// same encoding as vsew.
func vecWidthField(sew uint32) uint32 {
	switch sew {
	case vsew8:
		return 0b000
	case vsew16:
		return 0b101
	case vsew32:
		return 0b110
	case vsew64:
		return 0b111
	}
	panic(fmt.Sprintf("BUG: unknown vsew %d", sew))
}

// encodeVectorLoad encodes `vle<sew>.v vd, (rs1)`, unmasked, unit stride.
func encodeVectorLoad(vd, rs1, sew uint32) uint32 {
	return 1<<25 | rs1<<15 | vecWidthField(sew)<<12 | vd<<7 | opLoadFP
}

// encodeVectorStore encodes `vse<sew>.v vs3, (rs1)`, unmasked, unit stride.
func encodeVectorStore(vs3, rs1, sew uint32) uint32 {
	return 1<<25 | rs1<<15 | vecWidthField(sew)<<12 | vs3<<7 | opStoreFP
}

// encodeVmv1r encodes `vmv1r.v vd, vs2`: a whole-register move that needs no
// preceding vsetivli, since it is defined in terms of the register itself
// rather than the current vtype.
func encodeVmv1r(vd, vs2 uint32) uint32 {
	return 0b100111<<26 | 1<<25 | vs2<<20 | 0<<15 | 0b011<<12 | vd<<7 | opVec
}

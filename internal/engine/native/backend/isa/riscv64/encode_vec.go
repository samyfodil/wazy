package riscv64

import "fmt"

// RVV instruction encoding.
//
// Every vector instruction shares one layout:
//
//	funct6[31:26] vm[25] vs2[24:20] vs1|rs1|imm[19:15] funct3[14:12] vd[11:7] opcode[6:0]
//
// funct3 selects the operand *form* rather than the operation, which is why
// the same funct6 can mean different things: vsll.vv and vmul.vv are both
// funct6=100101, distinguished only by OPIVV versus OPMVV. The unary
// operations reuse the vs1 field as a sub-opcode -- vfsqrt.v, the extensions
// and the float/integer conversions are all funct6=010010 or 010011 with the
// variant in vs1.
//
// vm=1 throughout means unmasked; the masked forms are only needed where a
// comparison result feeds a merge, which passes the mask in v0 explicitly.
const (
	opivv = 0b000 // vector-vector, integer
	opfvv = 0b001 // vector-vector, float
	opmvv = 0b010 // vector-vector, mask/multiply
	opivi = 0b011 // vector-immediate
	opivx = 0b100 // vector-scalar (integer register)
	opfvf = 0b101 // vector-scalar (float register)
	opmvx = 0b110 // vector-scalar, mask/multiply
)

func encodeVec(funct6, vm, vs2, vs1, funct3, vd uint32) uint32 {
	return funct6<<26 | vm<<25 | vs2<<20 | vs1<<15 | funct3<<12 | vd<<7 | opVec
}

// funct6 values, grouped by the form they are used with.
const (
	// OPIVV / OPIVX / OPIVI
	vfunctAdd     = 0b000000
	vfunctSub     = 0b000010
	vfunctRsub    = 0b000011
	vfunctMinu    = 0b000100
	vfunctMin     = 0b000101
	vfunctMaxu    = 0b000110
	vfunctMax     = 0b000111
	vfunctAnd     = 0b001001
	vfunctOr      = 0b001010
	vfunctXor     = 0b001011
	vfunctRgather = 0b001100
	vfunctMseq    = 0b011000
	vfunctMsne    = 0b011001
	vfunctMsltu   = 0b011010
	vfunctMslt    = 0b011011
	vfunctMsleu   = 0b011100
	vfunctMsle    = 0b011101
	vfunctMerge   = 0b010111 // also vmv.v.* when vm=1
	vfunctSaddu   = 0b100000
	vfunctSadd    = 0b100001
	vfunctSsubu   = 0b100010
	vfunctSsub    = 0b100011
	vfunctSll     = 0b100101
	vfunctSrl     = 0b101000
	vfunctSra     = 0b101001

	// OPMVV
	vfunctAaddu    = 0b001000
	vfunctMul      = 0b100101
	vfunctWrxunary = 0b010000 // vmv.x.s, vcpop.m, vfirst.m -- variant in vs1
	vfunctXunary   = 0b010010 // vzext/vsext -- variant in vs1
	vfunctMnand    = 0b011101

	// OPFVV
	vfunctFadd    = 0b000000
	vfunctFsub    = 0b000010
	vfunctFmin    = 0b000100
	vfunctFmax    = 0b000110
	vfunctFsgnjn  = 0b001001
	vfunctFsgnjx  = 0b001010
	vfunctMfeq    = 0b011000
	vfunctMfle    = 0b011001
	vfunctMflt    = 0b011011
	vfunctMfne    = 0b011100
	vfunctFdiv    = 0b100000
	vfunctFmul    = 0b100100
	vfunctFunary1 = 0b010011 // vfsqrt.v -- variant in vs1
	vfunctFunary0 = 0b010010 // conversions -- variant in vs1
)

// vs1 sub-opcodes for the unary families.
const (
	vsubFsqrt     = 0b00000
	vsubCvtFXu    = 0b00010
	vsubCvtFX     = 0b00011
	vsubCvtRtzXuF = 0b00110
	vsubCvtRtzXF  = 0b00111
	vsubZext2     = 0b00110
	vsubSext2     = 0b00111
	vsubMvXS      = 0b00000
	vsubCpop      = 0b10000
	vsubFirst     = 0b10001
)

// encodeVecVV encodes an unmasked vector-vector operation in the given form.
func encodeVecVV(funct6, vd, vs2, vs1, funct3 uint32) uint32 {
	return encodeVec(funct6, 1, vs2, vs1, funct3, vd)
}

// encodeVecVX encodes an unmasked vector-scalar operation, the scalar coming
// from an integer register.
func encodeVecVX(funct6, vd, vs2, rs1 uint32) uint32 {
	return encodeVec(funct6, 1, vs2, rs1, opivx, vd)
}

// encodeVecVI encodes an unmasked vector-immediate operation. The immediate is
// 5-bit signed.
func encodeVecVI(funct6, vd, vs2 uint32, imm int32) uint32 {
	if imm < -16 || imm > 15 {
		panic(fmt.Sprintf("BUG: %d does not fit RVV's 5-bit signed immediate", imm))
	}
	return encodeVec(funct6, 1, vs2, uint32(imm)&0x1f, opivi, vd)
}

// encodeVecUnary encodes the unary families, whose variant lives in vs1.
func encodeVecUnary(funct6, vd, vs2, variant, funct3 uint32) uint32 {
	return encodeVec(funct6, 1, vs2, variant, funct3, vd)
}

// encodeVmerge encodes `vmerge.vvm vd, vs2, vs1, v0`: lane-wise select, taking
// vs1 where the v0 mask bit is set and vs2 where it is clear. vm=0 is what
// makes it read the mask at all.
func encodeVmerge(vd, vs2, vs1 uint32) uint32 {
	return encodeVec(vfunctMerge, 0, vs2, vs1, opivv, vd)
}

// encodeVmvVV encodes `vmv.v.v vd, vs1`, a whole-vector copy under the current
// vtype.
func encodeVmvVV(vd, vs1 uint32) uint32 {
	return encodeVec(vfunctMerge, 1, 0, vs1, opivv, vd)
}

// encodeVmvVX encodes `vmv.v.x vd, rs1`: splat an integer register.
func encodeVmvVX(vd, rs1 uint32) uint32 {
	return encodeVec(vfunctMerge, 1, 0, rs1, opivx, vd)
}

// encodeVmvVI encodes `vmv.v.i vd, imm`: splat a 5-bit signed immediate.
func encodeVmvVI(vd uint32, imm int32) uint32 {
	return encodeVec(vfunctMerge, 1, 0, uint32(imm)&0x1f, opivi, vd)
}

// encodeVmvXS encodes `vmv.x.s rd, vs2`: move lane 0 into an integer register.
func encodeVmvXS(rd, vs2 uint32) uint32 {
	return encodeVec(vfunctWrxunary, 1, vs2, vsubMvXS, opmvv, rd)
}

// encodeVcpopM encodes `vcpop.m rd, vs2`: count the set bits of a mask.
func encodeVcpopM(rd, vs2 uint32) uint32 {
	return encodeVec(vfunctWrxunary, 1, vs2, vsubCpop, opmvv, rd)
}

// funct6 values for the remaining families.
const (
	vfunctSlideup   = 0b001110
	vfunctSlidedown = 0b001111
	vfunctSmul      = 0b100111
	vfunctNclipu    = 0b101110
	vfunctNclip     = 0b101111
	vfunctWmul      = 0b111011
	vfunctRedsum    = 0b000000
	vfunctCompress  = 0b010111
	// VMUNARY0, which is where vid.v lives -- a different funct6 from
	// VXUNARY0 (the extensions), despite both being "unary with the variant
	// in vs1".
	vfunctMunary0 = 0b010100
)

// vs1 sub-opcodes for VMUNARY0 and the dynamic-rounding conversion.
const (
	vsubVid   = 0b10001
	vsubCvtXF = 0b00001 // vfcvt.x.f.v: rounds per frm
	vsubFmvFS = 0b00000
)

// encodeVid encodes `vid.v vd`: write each lane its own index.
func encodeVid(vd uint32) uint32 {
	return encodeVec(vfunctMunary0, 1, 0, vsubVid, opmvv, vd)
}

// encodeVfmvFS encodes `vfmv.f.s rd, vs2`: lane 0 into a float register.
func encodeVfmvFS(rd, vs2 uint32) uint32 {
	return encodeVec(vfunctWrxunary, 1, vs2, vsubFmvFS, opfvv, rd)
}

// vs1 sub-opcodes for the wider extension and conversion variants.
const (
	vsubZext4   = 0b00100
	vsubSext4   = 0b00101
	vsubFwcvtFF = 0b01100
	vsubFncvtFF = 0b10100
	vsubMvSX    = 0b01010
)

// encodeVecVIu encodes a vector-immediate operation whose immediate is
// *unsigned* 5-bit: the slide and gather offsets, and the narrowing shift
// amounts, which index lanes rather than carry a value.
func encodeVecVIu(funct6, vd, vs2, uimm uint32) uint32 {
	if uimm > 31 {
		panic(fmt.Sprintf("BUG: %d does not fit RVV's 5-bit unsigned immediate", uimm))
	}
	return encodeVec(funct6, 1, vs2, uimm, opivi, vd)
}

// encodeVmergeVI encodes `vmerge.vim vd, vs2, imm, v0`.
func encodeVmergeVI(vd, vs2 uint32, imm int32) uint32 {
	return encodeVec(vfunctMerge, 0, vs2, uint32(imm)&0x1f, opivi, vd)
}

// encodeVmvSX encodes `vmv.s.x vd, rs1`: write an integer register into lane 0
// only, leaving the other lanes alone.
func encodeVmvSX(vd, rs1 uint32) uint32 {
	return encodeVec(vfunctWrxunary, 1, 0, rs1, opmvx, vd)
}

// ---------------------------------------------------------------------------
// Rounding mode
// ---------------------------------------------------------------------------
//
// RVV's vfcvt has no static rounding-mode field, unlike its scalar
// counterpart: it always rounds per the fcsr's frm, except for the explicit
// .rtz forms. So the vector ceil, floor and nearest have to set frm around the
// conversion and put it back, which is what these are for.

const csrFRM = 0x002

// encodeFsrmi encodes `fsrmi rd, imm`: set frm to imm, returning the old value
// in rd.
func encodeFsrmi(rd, imm uint32) uint32 {
	return csrFRM<<20 | imm<<15 | 0b101<<12 | rd<<7 | opSystem
}

// encodeFsrm encodes `fsrm rd, rs1`: set frm from a register, returning the
// old value.
func encodeFsrm(rd, rs1 uint32) uint32 {
	return csrFRM<<20 | rs1<<15 | 0b001<<12 | rd<<7 | opSystem
}

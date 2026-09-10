package riscv64

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestEncodings_againstClang checks every raw encoder in encode.go against a
// real RISC-V assembler, instruction for instruction.
//
// Hand-written instruction encoders are exactly the kind of code that looks
// right and is silently wrong in one bit of one funct7 field, and a wrong bit
// surfaces as a mysterious SIGILL a thousand lines of lowering later. clang
// ships a riscv64 target on every platform we develop on, so the ground truth
// is free; the test skips when it is unavailable rather than failing.
//
// Note where we deliberately differ from clang's default: for FP arithmetic
// clang assembles a bare `fadd.s` with rm=dyn (take the rounding mode from the
// fcsr), while this backend hard-codes rm=rne. wasm mandates roundTiesToEven
// and nothing guarantees the host left the fcsr alone, so the cases below
// spell out `, rne` to assert the encoding we actually want.
func TestEncodings_againstClang(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang not available; skipping the assembler-oracle encoding test")
	}

	type tc = struct {
		asmText string
		got     uint32
	}
	cases := []tc{
		// --- R-type integer, 64-bit ---
		{"add a0, a1, a2", encodeAluRRR(aluOpAdd, 10, 11, 12, true)},
		{"sub a0, a1, a2", encodeAluRRR(aluOpSub, 10, 11, 12, true)},
		{"sll a0, a1, a2", encodeAluRRR(aluOpSll, 10, 11, 12, true)},
		{"slt a0, a1, a2", encodeAluRRR(aluOpSlt, 10, 11, 12, true)},
		{"sltu a0, a1, a2", encodeAluRRR(aluOpSltu, 10, 11, 12, true)},
		{"xor a0, a1, a2", encodeAluRRR(aluOpXor, 10, 11, 12, true)},
		{"srl a0, a1, a2", encodeAluRRR(aluOpSrl, 10, 11, 12, true)},
		{"sra a0, a1, a2", encodeAluRRR(aluOpSra, 10, 11, 12, true)},
		{"or a0, a1, a2", encodeAluRRR(aluOpOr, 10, 11, 12, true)},
		{"and a0, a1, a2", encodeAluRRR(aluOpAnd, 10, 11, 12, true)},
		{"mul a0, a1, a2", encodeAluRRR(aluOpMul, 10, 11, 12, true)},
		{"mulh a0, a1, a2", encodeAluRRR(aluOpMulh, 10, 11, 12, true)},
		{"mulhu a0, a1, a2", encodeAluRRR(aluOpMulhu, 10, 11, 12, true)},
		{"div a0, a1, a2", encodeAluRRR(aluOpDiv, 10, 11, 12, true)},
		{"divu a0, a1, a2", encodeAluRRR(aluOpDivu, 10, 11, 12, true)},
		{"rem a0, a1, a2", encodeAluRRR(aluOpRem, 10, 11, 12, true)},
		{"remu a0, a1, a2", encodeAluRRR(aluOpRemu, 10, 11, 12, true)},
		// --- R-type integer, word forms ---
		{"addw a0, a1, a2", encodeAluRRR(aluOpAdd, 10, 11, 12, false)},
		{"subw a0, a1, a2", encodeAluRRR(aluOpSub, 10, 11, 12, false)},
		{"sllw a0, a1, a2", encodeAluRRR(aluOpSll, 10, 11, 12, false)},
		{"srlw a0, a1, a2", encodeAluRRR(aluOpSrl, 10, 11, 12, false)},
		{"sraw a0, a1, a2", encodeAluRRR(aluOpSra, 10, 11, 12, false)},
		{"mulw a0, a1, a2", encodeAluRRR(aluOpMul, 10, 11, 12, false)},
		{"divw a0, a1, a2", encodeAluRRR(aluOpDiv, 10, 11, 12, false)},
		{"divuw a0, a1, a2", encodeAluRRR(aluOpDivu, 10, 11, 12, false)},
		{"remw a0, a1, a2", encodeAluRRR(aluOpRem, 10, 11, 12, false)},
		{"remuw a0, a1, a2", encodeAluRRR(aluOpRemu, 10, 11, 12, false)},
		// --- I-type ---
		{"addi a0, a1, 100", encodeAluRRImm(aluOpAdd, 10, 11, 100, true)},
		{"addi a0, a1, -100", encodeAluRRImm(aluOpAdd, 10, 11, -100, true)},
		{"addi a0, a1, 2047", encodeAluRRImm(aluOpAdd, 10, 11, 2047, true)},
		{"addi a0, a1, -2048", encodeAluRRImm(aluOpAdd, 10, 11, -2048, true)},
		{"addiw a0, a1, -7", encodeAluRRImm(aluOpAdd, 10, 11, -7, false)},
		{"slti a0, a1, 5", encodeAluRRImm(aluOpSlt, 10, 11, 5, true)},
		{"sltiu a0, a1, 5", encodeAluRRImm(aluOpSltu, 10, 11, 5, true)},
		{"xori a0, a1, -1", encodeAluRRImm(aluOpXor, 10, 11, -1, true)},
		{"ori a0, a1, 15", encodeAluRRImm(aluOpOr, 10, 11, 15, true)},
		{"andi a0, a1, 255", encodeAluRRImm(aluOpAnd, 10, 11, 255, true)},
		{"slli a0, a1, 33", encodeAluRRImm(aluOpSll, 10, 11, 33, true)},
		{"srli a0, a1, 63", encodeAluRRImm(aluOpSrl, 10, 11, 63, true)},
		{"srai a0, a1, 1", encodeAluRRImm(aluOpSra, 10, 11, 1, true)},
		{"slliw a0, a1, 31", encodeAluRRImm(aluOpSll, 10, 11, 31, false)},
		{"srliw a0, a1, 5", encodeAluRRImm(aluOpSrl, 10, 11, 5, false)},
		{"sraiw a0, a1, 5", encodeAluRRImm(aluOpSra, 10, 11, 5, false)},
		// --- U-type ---
		{"lui a0, 0x12345", encodeLui(10, 0x12345000)},
		{"auipc a0, 0x1000", encodeAuipc(10, 0x1000000)},
		{"lui a0, 0xfffff", encodeLui(10, -4096)},
		// --- loads/stores ---
		{"lb a0, 8(a1)", encodeLoad(10, 11, 8, 8, true)},
		{"lh a0, 8(a1)", encodeLoad(10, 11, 8, 16, true)},
		{"lw a0, 8(a1)", encodeLoad(10, 11, 8, 32, true)},
		{"ld a0, 8(a1)", encodeLoad(10, 11, 8, 64, false)},
		{"lbu a0, -8(a1)", encodeLoad(10, 11, -8, 8, false)},
		{"lhu a0, -8(a1)", encodeLoad(10, 11, -8, 16, false)},
		{"lwu a0, 2047(a1)", encodeLoad(10, 11, 2047, 32, false)},
		{"sb a0, 8(a1)", encodeStore(10, 11, 8, 8)},
		{"sh a0, 8(a1)", encodeStore(10, 11, 8, 16)},
		{"sw a0, -8(a1)", encodeStore(10, 11, -8, 32)},
		{"sd a0, -2048(a1)", encodeStore(10, 11, -2048, 64)},
		{"flw fa0, 12(a1)", encodeFpuLoad(10, 11, 12, 32)},
		{"fld fa0, 12(a1)", encodeFpuLoad(10, 11, 12, 64)},
		{"fsw fa0, 12(a1)", encodeFpuStore(10, 11, 12, 32)},
		{"fsd fa0, -12(a1)", encodeFpuStore(10, 11, -12, 64)},
		// --- branches / jumps ---
		{"beq a0, a1, .+8", encodeBranch(condEQ, 10, 11, 8)},
		{"bne a0, a1, .-8", encodeBranch(condNE, 10, 11, -8)},
		{"blt a0, a1, .+4094", encodeBranch(condLT, 10, 11, 4094)},
		{"bge a0, a1, .-4096", encodeBranch(condGE, 10, 11, -4096)},
		{"bltu a0, a1, .+16", encodeBranch(condLTU, 10, 11, 16)},
		{"bgeu a0, a1, .+16", encodeBranch(condGEU, 10, 11, 16)},
		{"jal ra, .+2048", encodeJal(1, 2048)},
		{"jal zero, .-1048576", encodeJal(0, -1048576)},
		{"jalr ra, 0(a0)", encodeJalr(1, 10, 0)},
		{"jalr zero, 16(a0)", encodeJalr(0, 10, 16)},
		{"ret", encodeRet()},
		{"nop", encodeNop()},
		{"ebreak", encodeEbreak()},
		{"fence rw, rw", encodeFence(0b0011, 0b0011)},
		// --- FP arithmetic ---
		{"fadd.s fa0, fa1, fa2, rne", encodeFpuRRR(fpuBinOpAdd, 10, 11, 12, false)},
		{"fadd.d fa0, fa1, fa2, rne", encodeFpuRRR(fpuBinOpAdd, 10, 11, 12, true)},
		{"fsub.d fa0, fa1, fa2, rne", encodeFpuRRR(fpuBinOpSub, 10, 11, 12, true)},
		{"fmul.s fa0, fa1, fa2, rne", encodeFpuRRR(fpuBinOpMul, 10, 11, 12, false)},
		{"fdiv.d fa0, fa1, fa2, rne", encodeFpuRRR(fpuBinOpDiv, 10, 11, 12, true)},
		{"fmin.s fa0, fa1, fa2", encodeFpuRRR(fpuBinOpMin, 10, 11, 12, false)},
		{"fmax.d fa0, fa1, fa2", encodeFpuRRR(fpuBinOpMax, 10, 11, 12, true)},
		{"fsgnj.d fa0, fa1, fa2", encodeFpuRRR(fpuBinOpSgnj, 10, 11, 12, true)},
		{"fsgnjn.s fa0, fa1, fa2", encodeFpuRRR(fpuBinOpSgnjn, 10, 11, 12, false)},
		{"fsgnjx.d fa0, fa1, fa2", encodeFpuRRR(fpuBinOpSgnjx, 10, 11, 12, true)},
		{"fsqrt.s fa0, fa1, rne", encodeFsqrt(10, 11, false)},
		{"fsqrt.d fa0, fa1, rne", encodeFsqrt(10, 11, true)},
		// --- FP compare (integer destination) ---
		{"feq.s a0, fa1, fa2", encodeFpuCmp(fpuCmpOpEq, 10, 11, 12, false)},
		{"flt.d a0, fa1, fa2", encodeFpuCmp(fpuCmpOpLt, 10, 11, 12, true)},
		{"fle.s a0, fa1, fa2", encodeFpuCmp(fpuCmpOpLe, 10, 11, 12, false)},
		// --- FP conversions ---
		{"fcvt.w.s a0, fa1, rtz", encodeFcvtToInt(10, 11, false, false, true)},
		{"fcvt.wu.s a0, fa1, rtz", encodeFcvtToInt(10, 11, false, false, false)},
		{"fcvt.l.d a0, fa1, rtz", encodeFcvtToInt(10, 11, true, true, true)},
		{"fcvt.lu.d a0, fa1, rtz", encodeFcvtToInt(10, 11, true, true, false)},
		{"fcvt.s.w fa0, a1, rne", encodeFcvtFromInt(10, 11, false, false, true)},
		{"fcvt.d.lu fa0, a1, rne", encodeFcvtFromInt(10, 11, true, true, false)},
		{"fcvt.s.d fa0, fa1, rne", encodeFcvtSD(10, 11, false)},
		{"fcvt.d.s fa0, fa1", encodeFcvtSD(10, 11, true)},
		{"fmv.x.w a0, fa1", encodeFmvToInt(10, 11, false)},
		{"fmv.x.d a0, fa1", encodeFmvToInt(10, 11, true)},
		{"fmv.w.x fa0, a1", encodeFmvFromInt(10, 11, false)},
		{"fmv.d.x fa0, a1", encodeFmvFromInt(10, 11, true)},
		{"fclass.s a0, fa1", encodeFclass(10, 11, false)},
		{"fclass.d a0, fa1", encodeFclass(10, 11, true)},
		// --- RVV: the fixed-16-byte configurations v128 needs ---
		{"vsetivli zero, 16, e8, m1, ta, ma", encodeVsetivli(vecAVLFor(vsew8), vsew8)},
		{"vsetivli zero, 8, e16, m1, ta, ma", encodeVsetivli(vecAVLFor(vsew16), vsew16)},
		{"vsetivli zero, 4, e32, m1, ta, ma", encodeVsetivli(vecAVLFor(vsew32), vsew32)},
		{"vsetivli zero, 2, e64, m1, ta, ma", encodeVsetivli(vecAVLFor(vsew64), vsew64)},
		{"vle64.v v1, (a0)", encodeVectorLoad(1, 10, vsew64)},
		{"vse64.v v1, (a0)", encodeVectorStore(1, 10, vsew64)},
		{"vle32.v v3, (a2)", encodeVectorLoad(3, 12, vsew32)},
		{"vse8.v v31, (sp)", encodeVectorStore(31, 2, vsew8)},
		{"vmv1r.v v2, v1", encodeVmv1r(2, 1)},
		// --- A extension: sequentially consistent, hence aqrl throughout ---
		{"amoadd.w.aqrl a0, a1, (a2)", encodeAMO(amoFunctAdd, 10, 12, 11, false)},
		{"amoadd.d.aqrl a0, a1, (a2)", encodeAMO(amoFunctAdd, 10, 12, 11, true)},
		{"amoswap.w.aqrl a0, a1, (a2)", encodeAMO(amoFunctSwap, 10, 12, 11, false)},
		{"amoand.d.aqrl a0, a1, (a2)", encodeAMO(amoFunctAnd, 10, 12, 11, true)},
		{"amoor.w.aqrl a0, a1, (a2)", encodeAMO(amoFunctOr, 10, 12, 11, false)},
		{"amoxor.d.aqrl a0, a1, (a2)", encodeAMO(amoFunctXor, 10, 12, 11, true)},
		{"lr.w.aqrl a0, (a2)", encodeLR(10, 12, false)},
		{"lr.d.aqrl a0, (a2)", encodeLR(10, 12, true)},
		{"sc.w.aqrl a0, a1, (a2)", encodeSC(10, 12, 11, false)},
		{"sc.d.aqrl a0, a1, (a2)", encodeSC(10, 12, 11, true)},
		// --- RVV: integer arithmetic ---
		{"vadd.vv v1, v2, v3", encodeVecVV(vfunctAdd, 1, 2, 3, opivv)},
		{"vsub.vv v1, v2, v3", encodeVecVV(vfunctSub, 1, 2, 3, opivv)},
		{"vand.vv v1, v2, v3", encodeVecVV(vfunctAnd, 1, 2, 3, opivv)},
		{"vor.vv v1, v2, v3", encodeVecVV(vfunctOr, 1, 2, 3, opivv)},
		{"vxor.vv v1, v2, v3", encodeVecVV(vfunctXor, 1, 2, 3, opivv)},
		{"vmul.vv v1, v2, v3", encodeVecVV(vfunctMul, 1, 2, 3, opmvv)},
		{"vsll.vv v1, v2, v3", encodeVecVV(vfunctSll, 1, 2, 3, opivv)},
		{"vsrl.vv v1, v2, v3", encodeVecVV(vfunctSrl, 1, 2, 3, opivv)},
		{"vsra.vv v1, v2, v3", encodeVecVV(vfunctSra, 1, 2, 3, opivv)},
		{"vmin.vv v1, v2, v3", encodeVecVV(vfunctMin, 1, 2, 3, opivv)},
		{"vmax.vv v1, v2, v3", encodeVecVV(vfunctMax, 1, 2, 3, opivv)},
		{"vminu.vv v1, v2, v3", encodeVecVV(vfunctMinu, 1, 2, 3, opivv)},
		{"vmaxu.vv v1, v2, v3", encodeVecVV(vfunctMaxu, 1, 2, 3, opivv)},
		{"vsadd.vv v1, v2, v3", encodeVecVV(vfunctSadd, 1, 2, 3, opivv)},
		{"vsaddu.vv v1, v2, v3", encodeVecVV(vfunctSaddu, 1, 2, 3, opivv)},
		{"vssub.vv v1, v2, v3", encodeVecVV(vfunctSsub, 1, 2, 3, opivv)},
		{"vssubu.vv v1, v2, v3", encodeVecVV(vfunctSsubu, 1, 2, 3, opivv)},
		{"vaaddu.vv v1, v2, v3", encodeVecVV(vfunctAaddu, 1, 2, 3, opmvv)},
		{"vrgather.vv v1, v2, v3", encodeVecVV(vfunctRgather, 1, 2, 3, opivv)},
		{"vrsub.vx v1, v2, zero", encodeVecVX(vfunctRsub, 1, 2, 0)},
		{"vsll.vx v1, v2, a0", encodeVecVX(vfunctSll, 1, 2, 10)},
		{"vsrl.vx v1, v2, a0", encodeVecVX(vfunctSrl, 1, 2, 10)},
		{"vsra.vx v1, v2, a0", encodeVecVX(vfunctSra, 1, 2, 10)},
		// --- RVV: splat, copy, merge, mask readout ---
		{"vmv.v.x v1, a0", encodeVmvVX(1, 10)},
		{"vmv.v.i v1, 0", encodeVmvVI(1, 0)},
		{"vmv.v.v v1, v2", encodeVmvVV(1, 2)},
		{"vmerge.vvm v1, v2, v3, v0", encodeVmerge(1, 2, 3)},
		{"vmv.x.s a0, v1", encodeVmvXS(10, 1)},
		{"vcpop.m a0, v1", encodeVcpopM(10, 1)},
		{"vmnand.mm v1, v2, v2", encodeVecVV(vfunctMnand, 1, 2, 2, opmvv)},
		// --- RVV: comparisons produce a mask ---
		{"vmseq.vv v1, v2, v3", encodeVecVV(vfunctMseq, 1, 2, 3, opivv)},
		{"vmsne.vv v1, v2, v3", encodeVecVV(vfunctMsne, 1, 2, 3, opivv)},
		{"vmslt.vv v1, v2, v3", encodeVecVV(vfunctMslt, 1, 2, 3, opivv)},
		{"vmsle.vv v1, v2, v3", encodeVecVV(vfunctMsle, 1, 2, 3, opivv)},
		{"vmsltu.vv v1, v2, v3", encodeVecVV(vfunctMsltu, 1, 2, 3, opivv)},
		{"vmsleu.vv v1, v2, v3", encodeVecVV(vfunctMsleu, 1, 2, 3, opivv)},
		{"vmfeq.vv v1, v2, v3", encodeVecVV(vfunctMfeq, 1, 2, 3, opfvv)},
		{"vmflt.vv v1, v2, v3", encodeVecVV(vfunctMflt, 1, 2, 3, opfvv)},
		{"vmfle.vv v1, v2, v3", encodeVecVV(vfunctMfle, 1, 2, 3, opfvv)},
		{"vmfne.vv v1, v2, v3", encodeVecVV(vfunctMfne, 1, 2, 3, opfvv)},
		// --- RVV: floating point ---
		{"vfadd.vv v1, v2, v3", encodeVecVV(vfunctFadd, 1, 2, 3, opfvv)},
		{"vfsub.vv v1, v2, v3", encodeVecVV(vfunctFsub, 1, 2, 3, opfvv)},
		{"vfmul.vv v1, v2, v3", encodeVecVV(vfunctFmul, 1, 2, 3, opfvv)},
		{"vfdiv.vv v1, v2, v3", encodeVecVV(vfunctFdiv, 1, 2, 3, opfvv)},
		{"vfmin.vv v1, v2, v3", encodeVecVV(vfunctFmin, 1, 2, 3, opfvv)},
		{"vfmax.vv v1, v2, v3", encodeVecVV(vfunctFmax, 1, 2, 3, opfvv)},
		{"vfsgnjn.vv v1, v2, v2", encodeVecVV(vfunctFsgnjn, 1, 2, 2, opfvv)},
		{"vfsgnjx.vv v1, v2, v2", encodeVecVV(vfunctFsgnjx, 1, 2, 2, opfvv)},
		{"vfsqrt.v v1, v2", encodeVecUnary(vfunctFunary1, 1, 2, vsubFsqrt, opfvv)},
		{"vfcvt.rtz.x.f.v v1, v2", encodeVecUnary(vfunctFunary0, 1, 2, vsubCvtRtzXF, opfvv)},
		{"vfcvt.rtz.xu.f.v v1, v2", encodeVecUnary(vfunctFunary0, 1, 2, vsubCvtRtzXuF, opfvv)},
		{"vfcvt.f.x.v v1, v2", encodeVecUnary(vfunctFunary0, 1, 2, vsubCvtFX, opfvv)},
		{"vfcvt.f.xu.v v1, v2", encodeVecUnary(vfunctFunary0, 1, 2, vsubCvtFXu, opfvv)},
		{"vzext.vf2 v1, v2", encodeVecUnary(vfunctXunary, 1, 2, vsubZext2, opmvv)},
		{"vsext.vf2 v1, v2", encodeVecUnary(vfunctXunary, 1, 2, vsubSext2, opmvv)},
		// --- RVV: lane movement, narrowing, widening ---
		{"vslidedown.vi v1, v2, 3", encodeVecVIu(vfunctSlidedown, 1, 2, 3)},
		{"vslideup.vi v1, v2, 3", encodeVecVIu(vfunctSlideup, 1, 2, 3)},
		{"vslidedown.vx v1, v2, a0", encodeVecVX(vfunctSlidedown, 1, 2, 10)},
		{"vslideup.vx v1, v2, a0", encodeVecVX(vfunctSlideup, 1, 2, 10)},
		{"vrgather.vi v1, v2, 3", encodeVecVIu(vfunctRgather, 1, 2, 3)},
		{"vrgather.vx v1, v2, a0", encodeVecVX(vfunctRgather, 1, 2, 10)},
		{"vnclip.wi v1, v2, 0", encodeVecVIu(vfunctNclip, 1, 2, 0)},
		{"vnclipu.wi v1, v2, 0", encodeVecVIu(vfunctNclipu, 1, 2, 0)},
		{"vsext.vf4 v1, v2", encodeVecUnary(vfunctXunary, 1, 2, vsubSext4, opmvv)},
		{"vzext.vf4 v1, v2", encodeVecUnary(vfunctXunary, 1, 2, vsubZext4, opmvv)},
		{"vfwcvt.f.f.v v1, v2", encodeVecUnary(vfunctFunary0, 1, 2, vsubFwcvtFF, opfvv)},
		{"vfncvt.f.f.w v1, v2", encodeVecUnary(vfunctFunary0, 1, 2, vsubFncvtFF, opfvv)},
		{"vsmul.vv v1, v2, v3", encodeVecVV(vfunctSmul, 1, 2, 3, opivv)},
		{"vwmul.vv v1, v2, v3", encodeVecVV(vfunctWmul, 1, 2, 3, opmvv)},
		{"vredsum.vs v1, v2, v3", encodeVecVV(vfunctRedsum, 1, 2, 3, opmvv)},
		{"vcompress.vm v1, v2, v3", encodeVecVV(vfunctCompress, 1, 2, 3, opmvv)},
		{"vmv.s.x v1, a0", encodeVmvSX(1, 10)},
		{"vadd.vi v1, v2, 1", encodeVecVI(vfunctAdd, 1, 2, 1)},
		{"vand.vi v1, v2, 1", encodeVecVI(vfunctAnd, 1, 2, 1)},
		{"vmseq.vi v1, v2, 0", encodeVecVI(vfunctMseq, 1, 2, 0)},
		{"vmerge.vim v1, v2, 1, v0", encodeVmergeVI(1, 2, 1)},
		// --- RVV rounding needs the fcsr, since vfcvt has no static mode ---
		{"fsrmi a0, 3", encodeFsrmi(10, 3)},
		{"fsrm a0, a1", encodeFsrm(10, 11)},
	}

	var srcs []string
	for _, c := range cases {
		srcs = append(srcs, c.asmText)
	}
	want := assembleRV64(t, strings.Join(srcs, "\n")+"\n")
	if len(want) != len(cases) {
		t.Fatalf("expected %d words from the assembler, got %d", len(cases), len(want))
	}
	for i, c := range cases {
		if want[i] != c.got {
			t.Errorf("%s: clang encodes %#08x, we encode %#08x", c.asmText, want[i], c.got)
		}
	}
}

// assembleRV64 assembles RV64G source with clang and returns the .text words.
// -march=rv64gv enables the vector extension while (unlike rv64gcv) keeping
// the compressed extension off, so every
// instruction is the 4 bytes this backend emits, and -mno-relax stops the
// assembler rewriting sequences behind our back.
func assembleRV64(t *testing.T, src string) []uint32 {
	t.Helper()
	dir := t.TempDir()
	asmPath, objPath := dir+"/in.s", dir+"/out.o"
	if err := os.WriteFile(asmPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("clang", "--target=riscv64-unknown-elf", "-march=rv64gv", "-mno-relax",
		"-c", "-x", "assembler", asmPath, "-o", objPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clang failed: %v\n%s", err, out)
	}
	f, err := elf.Open(objPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := f.Section(".text").Data()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]uint32, len(data)/4)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(data[i*4:])
	}
	return out
}

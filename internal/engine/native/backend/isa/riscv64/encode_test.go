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

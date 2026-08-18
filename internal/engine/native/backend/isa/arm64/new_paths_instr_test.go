package arm64

import (
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// fpRealVReg returns the VReg of a real vector register, typed as a float
// register the way the lowering does.
func fpRealVReg(r regalloc.RealReg) regalloc.VReg {
	return regToVReg(r).SetRegType(regalloc.RegTypeFloat)
}

// fpVirtVReg returns an unallocated (virtual) float register, which the
// formatters print with a trailing "?".
func fpVirtVReg(id int) regalloc.VReg {
	return regalloc.VReg(id).SetRegType(regalloc.RegTypeFloat)
}

// The expected strings below are the A64 assembly syntax of each instruction as
// documented in the Arm ARM ("Data-processing (1 source)", "Floating-point
// data-processing (3 source)", "Advanced SIMD copy/permute/shift by immediate"
// and the two-register misc widening/narrowing groups), and every one of them
// was checked to assemble with `llvm-mc -triple=aarch64`.

func TestInstruction_String_fpuMovFromVec(t *testing.T) {
	// MOV (element to scalar), i.e. the alias of DUP (element, scalar):
	// MOV <V><d>, <Vn>.<T>[index].
	for _, tc := range []struct {
		exp string
		i   *instruction
	}{
		{
			exp: "mov d0, v1.d[1]",
			i: &instruction{
				kind: fpuMovFromVec, rd: fpRealVReg(v0), rn: operandNR(fpRealVReg(v1)),
				u1: uint64(vecArrangementD), u2: 1,
			},
		},
		{
			exp: "mov s29, v30.s[3]",
			i: &instruction{
				kind: fpuMovFromVec, rd: fpRealVReg(v29), rn: operandNR(fpRealVReg(v30)),
				u1: uint64(vecArrangementS), u2: 3,
			},
		},
		{
			exp: "mov b3, v4.b[15]",
			i: &instruction{
				kind: fpuMovFromVec, rd: fpRealVReg(v3), rn: operandNR(fpRealVReg(v4)),
				u1: uint64(vecArrangementB), u2: 15,
			},
		},
		{
			// Same instruction before register allocation.
			exp: "mov h5?, v6?.h[2]",
			i: &instruction{
				kind: fpuMovFromVec, rd: fpVirtVReg(5), rn: operandNR(fpVirtVReg(6)),
				u1: uint64(vecArrangementH), u2: 2,
			},
		},
	} {
		t.Run(tc.exp, func(t *testing.T) { require.Equal(t, tc.exp, tc.i.String()) })
	}
}

func TestInstruction_String_fpuRRI(t *testing.T) {
	// The scalar counterpart of vecShiftImm: SSHR <V><d>, <V><n>, #<shift>.
	for _, tc := range []struct {
		exp string
		i   *instruction
	}{
		{
			exp: "sshr d0, d1, #32",
			i: &instruction{
				kind: fpuRRI, rd: fpRealVReg(v0), rn: operandNR(fpRealVReg(v1)),
				rm: operandShiftImm(32),
				u1: uint64(vecOpSshr), u2: uint64(vecArrangementD),
			},
		},
		{
			exp: "sshr d2?, d3?, #63",
			i: &instruction{
				kind: fpuRRI, rd: fpVirtVReg(2), rn: operandNR(fpVirtVReg(3)),
				rm: operandShiftImm(63),
				u1: uint64(vecOpSshr), u2: uint64(vecArrangementD),
			},
		},
	} {
		t.Run(tc.exp, func(t *testing.T) { require.Equal(t, tc.exp, tc.i.String()) })
	}
}

func TestInstruction_String_fpuRRRR(t *testing.T) {
	// FMADD/FMSUB/FNMADD/FNMSUB <Vd>, <Vn>, <Vm>, <Va>, with the 64-bit size bit
	// of u1 selecting the double precision form.
	const is64 = uint64(1) << 32
	for _, tc := range []struct {
		exp string
		i   *instruction
	}{
		{
			exp: "fmadd d0, d1, d2, d3",
			i: &instruction{
				kind: fpuRRRR, rd: fpRealVReg(v0), rn: operandNR(fpRealVReg(v1)), rm: operandNR(fpRealVReg(v2)),
				u1: uint64(fpuTriOpMAdd) | is64, u2: uint64(fpRealVReg(v3)),
			},
		},
		{
			exp: "fmsub s4, s5, s6, s7",
			i: &instruction{
				kind: fpuRRRR, rd: fpRealVReg(v4), rn: operandNR(fpRealVReg(v5)), rm: operandNR(fpRealVReg(v6)),
				u1: uint64(fpuTriOpMSub), u2: uint64(fpRealVReg(v7)),
			},
		},
		{
			exp: "fnmadd d8, d9, d10, d11",
			i: &instruction{
				kind: fpuRRRR, rd: fpRealVReg(v8), rn: operandNR(fpRealVReg(v9)), rm: operandNR(fpRealVReg(v10)),
				u1: uint64(fpuTriOpNMAdd) | is64, u2: uint64(fpRealVReg(v11)),
			},
		},
		{
			exp: "fnmsub s12, s13, s14, s15",
			i: &instruction{
				kind: fpuRRRR, rd: fpRealVReg(v12), rn: operandNR(fpRealVReg(v13)), rm: operandNR(fpRealVReg(v14)),
				u1: uint64(fpuTriOpNMSub), u2: uint64(fpRealVReg(v15)),
			},
		},
		{
			exp: "fmadd s0?, s1?, s2?, s3?",
			i: &instruction{
				kind: fpuRRRR, rd: fpVirtVReg(0), rn: operandNR(fpVirtVReg(1)), rm: operandNR(fpVirtVReg(2)),
				u1: uint64(fpuTriOpMAdd), u2: uint64(fpVirtVReg(3)),
			},
		},
	} {
		t.Run(tc.exp, func(t *testing.T) { require.Equal(t, tc.exp, tc.i.String()) })
	}
}

func TestFpuTriOp_String(t *testing.T) {
	require.Equal(t, "fmadd", fpuTriOpMAdd.String())
	require.Equal(t, "fmsub", fpuTriOpMSub.String())
	require.Equal(t, "fnmadd", fpuTriOpNMAdd.String())
	require.Equal(t, "fnmsub", fpuTriOpNMSub.String())

	err := require.CapturePanic(func() { _ = fpuTriOp(4).String() })
	require.EqualError(t, err, "4")
}

func TestInstruction_String_vecDupFromFpu(t *testing.T) {
	// DUP (element): DUP <Vd>.<T>, <Vn>.<Ts>[0]. 1D is deliberately absent: it is
	// not an encodable destination arrangement for DUP.
	for _, tc := range []struct {
		exp string
		i   *instruction
	}{
		{
			exp: "dup v0.4s, v1.s[0]",
			i:   &instruction{kind: vecDupFromFpu, rd: fpRealVReg(v0), rn: operandNR(fpRealVReg(v1)), u1: uint64(vecArrangement4S)},
		},
		{
			exp: "dup v2.2d, v3.d[0]",
			i:   &instruction{kind: vecDupFromFpu, rd: fpRealVReg(v2), rn: operandNR(fpRealVReg(v3)), u1: uint64(vecArrangement2D)},
		},
		{
			exp: "dup v4.8b, v5.b[0]",
			i:   &instruction{kind: vecDupFromFpu, rd: fpRealVReg(v4), rn: operandNR(fpRealVReg(v5)), u1: uint64(vecArrangement8B)},
		},
		{
			exp: "dup v6.16b, v7.b[0]",
			i:   &instruction{kind: vecDupFromFpu, rd: fpRealVReg(v6), rn: operandNR(fpRealVReg(v7)), u1: uint64(vecArrangement16B)},
		},
		{
			exp: "dup v8.4h, v9.h[0]",
			i:   &instruction{kind: vecDupFromFpu, rd: fpRealVReg(v8), rn: operandNR(fpRealVReg(v9)), u1: uint64(vecArrangement4H)},
		},
		{
			exp: "dup v10.8h, v11.h[0]",
			i:   &instruction{kind: vecDupFromFpu, rd: fpRealVReg(v10), rn: operandNR(fpRealVReg(v11)), u1: uint64(vecArrangement8H)},
		},
		{
			exp: "dup v12.2s, v13.s[0]",
			i:   &instruction{kind: vecDupFromFpu, rd: fpRealVReg(v12), rn: operandNR(fpRealVReg(v13)), u1: uint64(vecArrangement2S)},
		},
		{
			exp: "dup v0?.4s, v1?.s[0]",
			i:   &instruction{kind: vecDupFromFpu, rd: fpVirtVReg(0), rn: operandNR(fpVirtVReg(1)), u1: uint64(vecArrangement4S)},
		},
	} {
		t.Run(tc.exp, func(t *testing.T) { require.Equal(t, tc.exp, tc.i.String()) })
	}
}

func TestInstruction_String_vecExtend(t *testing.T) {
	// SXTL/UXTL <Vd>.<Ta>, <Vn>.<Tb> and their "2" variants, which read the high
	// half of the source register. u1 != 0 selects the signed form.
	for _, tc := range []struct {
		exp string
		i   *instruction
	}{
		{
			exp: "uxtl v0.8h, v1.8b",
			i:   &instruction{kind: vecExtend, rd: fpRealVReg(v0), rn: operandNR(fpRealVReg(v1)), u1: 0, u2: uint64(vecArrangement8B)},
		},
		{
			exp: "sxtl v0.8h, v1.8b",
			i:   &instruction{kind: vecExtend, rd: fpRealVReg(v0), rn: operandNR(fpRealVReg(v1)), u1: 1, u2: uint64(vecArrangement8B)},
		},
		{
			exp: "uxtl2 v2.8h, v3.16b",
			i:   &instruction{kind: vecExtend, rd: fpRealVReg(v2), rn: operandNR(fpRealVReg(v3)), u1: 0, u2: uint64(vecArrangement16B)},
		},
		{
			exp: "sxtl2 v2.8h, v3.16b",
			i:   &instruction{kind: vecExtend, rd: fpRealVReg(v2), rn: operandNR(fpRealVReg(v3)), u1: 1, u2: uint64(vecArrangement16B)},
		},
		{
			exp: "uxtl v4.4s, v5.4h",
			i:   &instruction{kind: vecExtend, rd: fpRealVReg(v4), rn: operandNR(fpRealVReg(v5)), u1: 0, u2: uint64(vecArrangement4H)},
		},
		{
			exp: "sxtl2 v6.4s, v7.8h",
			i:   &instruction{kind: vecExtend, rd: fpRealVReg(v6), rn: operandNR(fpRealVReg(v7)), u1: 1, u2: uint64(vecArrangement8H)},
		},
		{
			exp: "sxtl v8.2d, v9.2s",
			i:   &instruction{kind: vecExtend, rd: fpRealVReg(v8), rn: operandNR(fpRealVReg(v9)), u1: 1, u2: uint64(vecArrangement2S)},
		},
		{
			exp: "uxtl2 v10.2d, v11.4s",
			i:   &instruction{kind: vecExtend, rd: fpRealVReg(v10), rn: operandNR(fpRealVReg(v11)), u1: 0, u2: uint64(vecArrangement4S)},
		},
	} {
		t.Run(tc.exp, func(t *testing.T) { require.Equal(t, tc.exp, tc.i.String()) })
	}
}

func TestInstruction_String_vecMiscNarrow(t *testing.T) {
	// XTN/SQXTN/UQXTN/SQXTUN/FCVTN <Vd>.<Tb>, <Vn>.<Ta> and their "2" variants,
	// which write the high half of the destination register.
	for _, tc := range []struct {
		exp string
		i   *instruction
	}{
		{
			exp: "xtn v0.8b, v1.8h",
			i:   &instruction{kind: vecMiscNarrow, rd: fpRealVReg(v0), rn: operandNR(fpRealVReg(v1)), u1: uint64(vecOpXtn), u2: uint64(vecArrangement8B)},
		},
		{
			exp: "xtn2 v0.16b, v1.8h",
			i:   &instruction{kind: vecMiscNarrow, rd: fpRealVReg(v0), rn: operandNR(fpRealVReg(v1)), u1: uint64(vecOpXtn), u2: uint64(vecArrangement16B)},
		},
		{
			exp: "sqxtn v2.4h, v3.4s",
			i:   &instruction{kind: vecMiscNarrow, rd: fpRealVReg(v2), rn: operandNR(fpRealVReg(v3)), u1: uint64(vecOpSqxtn), u2: uint64(vecArrangement4H)},
		},
		{
			exp: "sqxtn2 v2.4s, v3.2d",
			i:   &instruction{kind: vecMiscNarrow, rd: fpRealVReg(v2), rn: operandNR(fpRealVReg(v3)), u1: uint64(vecOpSqxtn), u2: uint64(vecArrangement4S)},
		},
		{
			exp: "uqxtn v4.2s, v5.2d",
			i:   &instruction{kind: vecMiscNarrow, rd: fpRealVReg(v4), rn: operandNR(fpRealVReg(v5)), u1: uint64(vecOpUqxtn), u2: uint64(vecArrangement2S)},
		},
		{
			exp: "uqxtn2 v4.8h, v5.4s",
			i:   &instruction{kind: vecMiscNarrow, rd: fpRealVReg(v4), rn: operandNR(fpRealVReg(v5)), u1: uint64(vecOpUqxtn), u2: uint64(vecArrangement8H)},
		},
		{
			exp: "sqxtun v6.8b, v7.8h",
			i:   &instruction{kind: vecMiscNarrow, rd: fpRealVReg(v6), rn: operandNR(fpRealVReg(v7)), u1: uint64(vecOpSqxtun), u2: uint64(vecArrangement8B)},
		},
		{
			exp: "sqxtun2 v6.8h, v7.4s",
			i:   &instruction{kind: vecMiscNarrow, rd: fpRealVReg(v6), rn: operandNR(fpRealVReg(v7)), u1: uint64(vecOpSqxtun), u2: uint64(vecArrangement8H)},
		},
		{
			exp: "fcvtn v8.2s, v9.2d",
			i:   &instruction{kind: vecMiscNarrow, rd: fpRealVReg(v8), rn: operandNR(fpRealVReg(v9)), u1: uint64(vecOpFcvtn), u2: uint64(vecArrangement2S)},
		},
		{
			exp: "fcvtn v8.4h, v9.4s",
			i:   &instruction{kind: vecMiscNarrow, rd: fpRealVReg(v8), rn: operandNR(fpRealVReg(v9)), u1: uint64(vecOpFcvtn), u2: uint64(vecArrangement4H)},
		},
		{
			exp: "fcvtn2 v8.4s, v9.2d",
			i:   &instruction{kind: vecMiscNarrow, rd: fpRealVReg(v8), rn: operandNR(fpRealVReg(v9)), u1: uint64(vecOpFcvtn), u2: uint64(vecArrangement4S)},
		},
		{
			exp: "xtn v0?.8b, v1?.8h",
			i:   &instruction{kind: vecMiscNarrow, rd: fpVirtVReg(0), rn: operandNR(fpVirtVReg(1)), u1: uint64(vecOpXtn), u2: uint64(vecArrangement8B)},
		},
	} {
		t.Run(tc.exp, func(t *testing.T) { require.Equal(t, tc.exp, tc.i.String()) })
	}
}

func TestInstruction_String_bitRR(t *testing.T) {
	// The "Data-processing (1 source)" members bitRR can hold. Note the two
	// spellings of opcode 0b000010: REV in the 32-bit form, REV32 in the 64-bit
	// one, and that opcode 0b000011 (REV/REV64) exists only in the 64-bit form.
	for _, tc := range []struct {
		exp string
		i   *instruction
	}{
		{
			exp: "rbit w0, w1",
			i:   &instruction{kind: bitRR, rd: regToVReg(x0), rn: operandNR(regToVReg(x1)), u1: uint64(bitOpRbit), u2: 0},
		},
		{
			exp: "rbit x0, x1",
			i:   &instruction{kind: bitRR, rd: regToVReg(x0), rn: operandNR(regToVReg(x1)), u1: uint64(bitOpRbit), u2: 1},
		},
		{
			exp: "clz w2, w3",
			i:   &instruction{kind: bitRR, rd: regToVReg(x2), rn: operandNR(regToVReg(x3)), u1: uint64(bitOpClz), u2: 0},
		},
		{
			exp: "clz x2, x3",
			i:   &instruction{kind: bitRR, rd: regToVReg(x2), rn: operandNR(regToVReg(x3)), u1: uint64(bitOpClz), u2: 1},
		},
		{
			exp: "cls w4, w5",
			i:   &instruction{kind: bitRR, rd: regToVReg(x4), rn: operandNR(regToVReg(x5)), u1: uint64(bitOpCls), u2: 0},
		},
		{
			exp: "cls x4, x5",
			i:   &instruction{kind: bitRR, rd: regToVReg(x4), rn: operandNR(regToVReg(x5)), u1: uint64(bitOpCls), u2: 1},
		},
		{
			exp: "rev16 w6, w7",
			i:   &instruction{kind: bitRR, rd: regToVReg(x6), rn: operandNR(regToVReg(x7)), u1: uint64(bitOpRev16), u2: 0},
		},
		{
			exp: "rev16 x6, x7",
			i:   &instruction{kind: bitRR, rd: regToVReg(x6), rn: operandNR(regToVReg(x7)), u1: uint64(bitOpRev16), u2: 1},
		},
		{
			// The 32-bit form of bitOpRev32 must print "rev": "rev32 w8, w9" is not
			// an instruction any assembler accepts.
			exp: "rev w8, w9",
			i:   &instruction{kind: bitRR, rd: regToVReg(x8), rn: operandNR(regToVReg(x9)), u1: uint64(bitOpRev32), u2: 0},
		},
		{
			exp: "rev32 x8, x9",
			i:   &instruction{kind: bitRR, rd: regToVReg(x8), rn: operandNR(regToVReg(x9)), u1: uint64(bitOpRev32), u2: 1},
		},
		{
			exp: "rev64 x10, x11",
			i:   &instruction{kind: bitRR, rd: regToVReg(x10), rn: operandNR(regToVReg(x11)), u1: uint64(bitOpRev64), u2: 1},
		},
		{
			exp: "clz w0?, w1?",
			i:   &instruction{kind: bitRR, rd: intToVReg(0), rn: operandNR(intToVReg(1)), u1: uint64(bitOpClz), u2: 0},
		},
	} {
		t.Run(tc.exp, func(t *testing.T) { require.Equal(t, tc.exp, tc.i.String()) })
	}
}

func TestBitOp_String(t *testing.T) {
	require.Equal(t, "rbit", bitOpRbit.String())
	require.Equal(t, "clz", bitOpClz.String())
	require.Equal(t, "cls", bitOpCls.String())
	require.Equal(t, "rev16", bitOpRev16.String())
	// The enum name is the 64-bit spelling; bitRR rewrites it to "rev" when the
	// operands are 32-bit.
	require.Equal(t, "rev32", bitOpRev32.String())
	require.Equal(t, "rev64", bitOpRev64.String())

	// bitOpRev64 is the last member, i.e. 5, so the first unhandled value is 6.
	err := require.CapturePanic(func() { _ = bitOp(bitOpRev64 + 1).String() })
	require.EqualError(t, err, "6")
}

func TestVecOp_String_permute(t *testing.T) {
	// The rest of the "Advanced SIMD permute" family, checked against the opcodes
	// encodeVecPermute assigns them (UZP1 = 0b001, TRN1 = 0b010, ZIP1 = 0b011,
	// UZP2 = 0b101, TRN2 = 0b110, ZIP2 = 0b111).
	require.Equal(t, "zip1", vecOpZip1.String())
	require.Equal(t, "zip2", vecOpZip2.String())
	require.Equal(t, "uzp1", vecOpUzp1.String())
	require.Equal(t, "uzp2", vecOpUzp2.String())
	require.Equal(t, "trn1", vecOpTrn1.String())
	require.Equal(t, "trn2", vecOpTrn2.String())

	for _, tc := range []struct {
		exp string
		op  vecOp
	}{
		{exp: "zip2 v0.4s, v1.4s, v2.4s", op: vecOpZip2},
		{exp: "uzp1 v0.4s, v1.4s, v2.4s", op: vecOpUzp1},
		{exp: "uzp2 v0.4s, v1.4s, v2.4s", op: vecOpUzp2},
		{exp: "trn1 v0.4s, v1.4s, v2.4s", op: vecOpTrn1},
		{exp: "trn2 v0.4s, v1.4s, v2.4s", op: vecOpTrn2},
	} {
		t.Run(tc.exp, func(t *testing.T) {
			i := &instruction{}
			i.asVecPermute(tc.op, fpRealVReg(v0), operandNR(fpRealVReg(v1)), operandNR(fpRealVReg(v2)), vecArrangement4S)
			require.Equal(t, tc.exp, i.String())
		})
	}
}

func TestElementArrangement(t *testing.T) {
	for _, tc := range []struct {
		in, exp vecArrangement
	}{
		{in: vecArrangement8B, exp: vecArrangementB},
		{in: vecArrangement16B, exp: vecArrangementB},
		{in: vecArrangement4H, exp: vecArrangementH},
		{in: vecArrangement8H, exp: vecArrangementH},
		{in: vecArrangement2S, exp: vecArrangementS},
		{in: vecArrangement4S, exp: vecArrangementS},
		{in: vecArrangement1D, exp: vecArrangementD},
		{in: vecArrangement2D, exp: vecArrangementD},
	} {
		t.Run(tc.in.String(), func(t *testing.T) {
			require.Equal(t, tc.exp, elementArrangement(tc.in))
		})
	}

	// A size specifier is already a single lane, so it has no element.
	err := require.CapturePanic(func() { _ = elementArrangement(vecArrangementQ) })
	require.EqualError(t, err, "unsupported arrangement: Q")
	err = require.CapturePanic(func() { _ = elementArrangement(vecArrangementNone) })
	require.EqualError(t, err, "unsupported arrangement: none")
}

func TestWidenArrangement(t *testing.T) {
	for _, tc := range []struct {
		in, exp vecArrangement
	}{
		// Both the 64-bit and the 128-bit narrow arrangements widen into the same
		// full-width one: the 128-bit ones are the "2" variants.
		{in: vecArrangement8B, exp: vecArrangement8H},
		{in: vecArrangement16B, exp: vecArrangement8H},
		{in: vecArrangement4H, exp: vecArrangement4S},
		{in: vecArrangement8H, exp: vecArrangement4S},
		{in: vecArrangement2S, exp: vecArrangement2D},
		{in: vecArrangement4S, exp: vecArrangement2D},
	} {
		t.Run(tc.in.String(), func(t *testing.T) {
			require.Equal(t, tc.exp, widenArrangement(tc.in))
		})
	}

	// There is nothing twice as wide as a 64-bit lane.
	err := require.CapturePanic(func() { _ = widenArrangement(vecArrangement2D) })
	require.EqualError(t, err, "unsupported arrangement: 2D")
	err = require.CapturePanic(func() { _ = widenArrangement(vecArrangement1D) })
	require.EqualError(t, err, "unsupported arrangement: 1D")
}

func TestExtModeOf(t *testing.T) {
	for _, tc := range []struct {
		name   string
		typ    ssa.Type
		signed bool
		exp    extMode
	}{
		{name: "i32 signed", typ: ssa.TypeI32, signed: true, exp: extModeSignExtend32},
		{name: "i32 unsigned", typ: ssa.TypeI32, signed: false, exp: extModeZeroExtend32},
		{name: "f32 signed", typ: ssa.TypeF32, signed: true, exp: extModeSignExtend32},
		{name: "f32 unsigned", typ: ssa.TypeF32, signed: false, exp: extModeZeroExtend32},
		{name: "i64 signed", typ: ssa.TypeI64, signed: true, exp: extModeSignExtend64},
		{name: "i64 unsigned", typ: ssa.TypeI64, signed: false, exp: extModeZeroExtend64},
		{name: "f64 signed", typ: ssa.TypeF64, signed: true, exp: extModeSignExtend64},
		{name: "f64 unsigned", typ: ssa.TypeF64, signed: false, exp: extModeZeroExtend64},
		// A v128 fills a whole vector register and is never the operand of a GPR
		// instruction, so there is no extension to apply to it.
		{name: "v128 signed", typ: ssa.TypeV128, signed: true, exp: extModeNone},
		{name: "v128 unsigned", typ: ssa.TypeV128, signed: false, exp: extModeNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode := extModeOf(tc.typ, tc.signed)
			require.Equal(t, tc.exp, mode)
			// The two accessors must agree with the mode picked: the width is the
			// one the comparison happens at, and signed() is what was asked for.
			if tc.typ == ssa.TypeV128 {
				require.Equal(t, byte(0), mode.bits())
				require.False(t, mode.signed())
			} else {
				require.Equal(t, tc.typ.Bits(), mode.bits())
				require.Equal(t, tc.signed, mode.signed())
			}
		})
	}
}

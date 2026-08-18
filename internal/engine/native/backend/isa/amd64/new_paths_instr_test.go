package amd64

import (
	"encoding/hex"
	"runtime"
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// encodeNewInstr encodes i into a fresh buffer and returns the machine code as
// hex together with the needsLabelResolution flag encode() reports.
func encodeNewInstr(i *instruction) (string, bool) {
	mc := &mockCompiler{}
	needsLabelResolution := i.encode(mc)
	return hex.EncodeToString(mc.buf), needsLabelResolution
}

// TestInstruction_xchgByte_regReg_encode pins the byte-sized register-register
// form of XCHG, `86 /r` (Intel SDM Vol.2B, XCHG).
//
// In 64-bit mode a byte operand encoded as 4..7 names AH/CH/DH/BH when the
// instruction carries no REX prefix and SPL/BPL/SIL/DIL when it carries one
// (Intel SDM Vol.2A, Table 3-1). Both operands of the register-register form
// are byte registers -- ModRM.reg and ModRM.rm -- so either one landing in that
// range forces an otherwise empty REX prefix (0x40). Checking only ModRM.reg,
// as the code used to, silently assembled `xchg %al, %dil` as `86 c7`, which is
// `xchg %al, %bh`: a different register, no error anywhere.
func TestInstruction_xchgByte_regReg_encode(t *testing.T) {
	for _, tc := range []struct {
		setup      func(*instruction)
		wantFormat string
		want       string
	}{
		{
			// The regression case. ModRM.reg = %al (0), ModRM.rm = %dil (7):
			// ModRM = 11 000 111 = 0xc7, and %dil needs the empty REX.
			// Without it the same two bytes mean `xchg %al, %bh`.
			setup:      func(i *instruction) { i.asXCHG(raxVReg, newOperandReg(rdiVReg), 1) },
			wantFormat: "xchg.b %rax, %rdi",
			want:       "4086c7",
		},
		{
			// Both operands in 4..7: ModRM = 11 110 111 = 0xf7.
			setup:      func(i *instruction) { i.asXCHG(rsiVReg, newOperandReg(rdiVReg), 1) },
			wantFormat: "xchg.b %rsi, %rdi",
			want:       "4086f7",
		},
		{
			// Only ModRM.reg in 4..7: ModRM = 11 110 000 = 0xf0.
			setup:      func(i *instruction) { i.asXCHG(rsiVReg, newOperandReg(raxVReg), 1) },
			wantFormat: "xchg.b %rsi, %rax",
			want:       "4086f0",
		},
		{
			// Neither operand in 4..7, so no REX at all: `xchg %al, %cl`.
			setup:      func(i *instruction) { i.asXCHG(raxVReg, newOperandReg(rcxVReg), 1) },
			wantFormat: "xchg.b %rax, %rcx",
			want:       "86c1",
		},
		{
			// `xchg %bl, %dl`: ModRM = 11 011 010 = 0xda, still no REX.
			setup:      func(i *instruction) { i.asXCHG(rbxVReg, newOperandReg(rdxVReg), 1) },
			wantFormat: "xchg.b %rbx, %rdx",
			want:       "86da",
		},
		{
			// `xchg %r11b, %dil`: REX.R for %r11b (0x44), and %dil is legal
			// because that same prefix is present.
			setup:      func(i *instruction) { i.asXCHG(r11VReg, newOperandReg(rdiVReg), 1) },
			wantFormat: "xchg.b %r11, %rdi",
			want:       "4486df",
		},
		{
			// The mirror image: `xchg %dil, %r11b`, REX.B = 1 (0x41).
			setup:      func(i *instruction) { i.asXCHG(rdiVReg, newOperandReg(r11VReg), 1) },
			wantFormat: "xchg.b %rdi, %r11",
			want:       "4186fb",
		},
		{
			// `xchg %r12b, %r13b`: REX.R and REX.B (0x45). The low three bits
			// of both encodings fall in 4..7, but the extension bits already
			// force a prefix, so forcing it again must not change the bytes.
			setup:      func(i *instruction) { i.asXCHG(r12VReg, newOperandReg(r13VReg), 1) },
			wantFormat: "xchg.b %r12, %r13",
			want:       "4586e5",
		},
		{
			// `xchg %spl, %bpl`: the other two registers that only exist with
			// a REX prefix. ModRM = 11 100 101 = 0xe5.
			setup:      func(i *instruction) { i.asXCHG(rspVReg, newOperandReg(rbpVReg), 1) },
			wantFormat: "xchg.b %rsp, %rbp",
			want:       "4086e5",
		},
	} {
		tc := tc
		t.Run(tc.wantFormat+"/"+tc.want, func(t *testing.T) {
			i := &instruction{}
			tc.setup(i)
			require.Equal(t, tc.wantFormat, i.String())

			got, needsLabelResolution := encodeNewInstr(i)
			require.False(t, needsLabelResolution)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestInstruction_xmmLoadConst_format_encode pins the load of a pooled 128-bit
// constant. It encodes as the memory form of the corresponding move:
// MOVUPS xmm1, m128 is `0F 10 /r`, MOVAPS is `0F 28 /r`, MOVDQU is
// `F3 0F 6F /r` and MOVDQA is `66 0F 6F /r` (Intel SDM Vol.2B).
//
// The RIP-relative cases are the ones that matter in practice, since the
// constant pool is emitted after the function body: ModRM.mod = 00 with
// ModRM.rm = 101 selects [RIP + disp32] in 64-bit mode, the displacement is
// left at zero here and encode() must report that it still needs resolving.
func TestInstruction_xmmLoadConst_format_encode(t *testing.T) {
	_, _, m := newSetupWithMockContext()
	defer func() { runtime.KeepAlive(m) }()

	for _, tc := range []struct {
		setup                   func(*instruction)
		wantFormat              string
		want                    string
		wantNeedsLabelResolutio bool
	}{
		{
			// movdqu L5(%rip), %xmm1 -- ModRM = 00 001 101 = 0x0d.
			setup: func(i *instruction) {
				i.asXmmLoadConst(sseOpcodeMovdqu, m.newAmodeRipRel(label(5)), xmm1VReg)
			},
			wantFormat:              "movdqu L5(%rip), %xmm1",
			want:                    "f30f6f0d00000000",
			wantNeedsLabelResolutio: true,
		},
		{
			// movaps L1(%rip), %xmm9 -- REX.R for %xmm9 (0x44), and the same
			// ModRM byte as above since %xmm9 & 7 == 1.
			setup: func(i *instruction) {
				i.asXmmLoadConst(sseOpcodeMovaps, m.newAmodeRipRel(label(1)), xmm9VReg)
			},
			wantFormat:              "movaps L1(%rip), %xmm9",
			want:                    "440f280d00000000",
			wantNeedsLabelResolutio: true,
		},
		{
			// movups 16(%rax), %xmm3 -- ModRM = 01 011 000 = 0x58 plus disp8.
			setup: func(i *instruction) {
				i.asXmmLoadConst(sseOpcodeMovups, m.newAmodeImmReg(16, raxVReg), xmm3VReg)
			},
			wantFormat: "movups 16(%rax), %xmm3",
			want:       "0f105810",
		},
		{
			// movdqa (%rax), %xmm9 -- 66 prefix, then REX.R, then the opcode;
			// ModRM = 00 001 000 = 0x08.
			setup: func(i *instruction) {
				i.asXmmLoadConst(sseOpcodeMovdqa, m.newAmodeImmReg(0, raxVReg), xmm9VReg)
			},
			wantFormat: "movdqa (%rax), %xmm9",
			want:       "66440f6f08",
		},
	} {
		tc := tc
		t.Run(tc.wantFormat, func(t *testing.T) {
			i := &instruction{}
			tc.setup(i)
			require.Equal(t, tc.wantFormat, i.String())

			got, needsLabelResolution := encodeNewInstr(i)
			require.Equal(t, tc.wantNeedsLabelResolutio, needsLabelResolution)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestInstruction_xmmLoadConst_regalloc checks the register allocator's view of
// the load: the constant's address is a use, the destination a definition.
func TestInstruction_xmmLoadConst_regalloc(t *testing.T) {
	_, _, m := newSetupWithMockContext()
	defer func() { runtime.KeepAlive(m) }()

	vBase := regalloc.VReg(1).SetRegType(regalloc.RegTypeInt)
	vDst := regalloc.VReg(2).SetRegType(regalloc.RegTypeFloat)

	i := &instruction{}
	i.asXmmLoadConst(sseOpcodeMovdqu, m.newAmodeImmReg(48, vBase), vDst)

	regs := &[]regalloc.VReg{}
	require.Equal(t, []regalloc.VReg{vBase}, i.Uses(regs))
	require.Equal(t, []regalloc.VReg{vDst}, i.Defs(regs))

	i.AssignUse(0, vBase.SetRealReg(rax))
	i.AssignDef(vDst.SetRealReg(xmm7))
	require.Equal(t, "movdqu 48(%rax), %xmm7", i.String())
}

// TestInstruction_xmmLoadConst_encode_requiresMem pins the guard: the whole
// point of the instruction is that the constant lives in memory, and the
// rewrite into xmmUnaryRmR is what makes the RIP-relative fixup resolvable.
func TestInstruction_xmmLoadConst_encode_requiresMem(t *testing.T) {
	i := &instruction{}
	i.asXmmLoadConst(sseOpcodeMovdqu, &amode{kindWithShift: uint32(amodeRipRel)}, xmm0VReg)
	i.op1 = newOperandReg(xmm1VReg)

	err := require.CapturePanic(func() { i.encode(&mockCompiler{}) })
	require.EqualError(t, err, "BUG: xmmLoadConst must address the constant through memory")
}

// TestInstruction_cvtUint64ToFloatSeq_format_encode pins the u64-to-float
// sequence. cvtsi2s{s,d} reads its source as signed, so the sequence branches
// on the sign bit and, for a negative (i.e. >= 2**63) source, converts
// (v>>1)|(v&1) and doubles the result:
//
//	testq %src, %src            REX.W 85 /r
//	js    negative              0F 88 cd
//	cvtsi2s{s,d}q %src, %dst    F2/F3 REX.W 0F 2A /r
//	jmp   done                  E9 cd
//
// negative:
//
//	movq  %src, %tmpGp          REX.W 89 /r
//	shrq  $1, %tmpGp            REX.W C1 /5 ib
//	movq  %src, %tmpGp2         REX.W 89 /r
//	andq  $1, %tmpGp2           REX.W 83 /4 ib
//	orq   %tmpGp2, %tmpGp       REX.W 09 /r
//	cvtsi2s{s,d}q %tmpGp, %dst
//	add{ss,sd} %dst, %dst       F3/F2 0F 58 /r
//
// done:
//
// All opcodes are from Intel SDM Vol.2; the rel32 displacements below are the
// distance from the end of each branch to its target, since RIP already points
// at the following instruction.
func TestInstruction_cvtUint64ToFloatSeq_format_encode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(*instruction)
		wantFormat string
		want       string
	}{
		{
			name: "f64",
			setup: func(i *instruction) {
				i.asCvtUint64ToFloatSeq(raxVReg, xmm0VReg, rcxVReg, rdxVReg, true)
			},
			wantFormat: "cvtUint64ToFloatSeq src=%rax, dst=%xmm0, tmpGp=%rcx, tmpGp2=%rdx, dst64=true",
			// 00: 48 85 c0             test %rax, %rax
			// 03: 0f 88 0a 00 00 00    js .+10 -> 0x13
			// 09: f2 48 0f 2a c0       cvtsi2sd %rax, %xmm0
			// 0e: e9 1a 00 00 00       jmp .+26 -> 0x2d (end)
			// 13: 48 89 c1             mov %rax, %rcx
			// 16: 48 c1 e9 01          shr $1, %rcx
			// 1a: 48 89 c2             mov %rax, %rdx
			// 1d: 48 83 e2 01          and $1, %rdx
			// 21: 48 09 d1             or %rdx, %rcx
			// 24: f2 48 0f 2a c1       cvtsi2sd %rcx, %xmm0
			// 29: f2 0f 58 c0          addsd %xmm0, %xmm0
			want: "4885c0" + "0f880a000000" + "f2480f2ac0" + "e91a000000" +
				"4889c1" + "48c1e901" + "4889c2" + "4883e201" + "4809d1" +
				"f2480f2ac1" + "f20f58c0",
		},
		{
			name: "f32/extended-registers",
			setup: func(i *instruction) {
				i.asCvtUint64ToFloatSeq(r14VReg, xmm11VReg, r8VReg, r9VReg, false)
			},
			wantFormat: "cvtUint64ToFloatSeq src=%r14, dst=%xmm11, tmpGp=%r8, tmpGp2=%r9, dst64=false",
			// 00: 4d 85 f6             test %r14, %r14
			// 03: 0f 88 0a 00 00 00    js .+10 -> 0x13
			// 09: f3 4d 0f 2a de       cvtsi2ss %r14, %xmm11
			// 0e: e9 1b 00 00 00       jmp .+27 -> 0x2e (end)
			// 13: 4d 89 f0             mov %r14, %r8
			// 16: 49 c1 e8 01          shr $1, %r8
			// 1a: 4d 89 f1             mov %r14, %r9
			// 1d: 49 83 e1 01          and $1, %r9
			// 21: 4d 09 c8             or %r9, %r8
			// 24: f3 4d 0f 2a d8       cvtsi2ss %r8, %xmm11
			// 29: f3 45 0f 58 db       addss %xmm11, %xmm11
			want: "4d85f6" + "0f880a000000" + "f34d0f2ade" + "e91b000000" +
				"4d89f0" + "49c1e801" + "4d89f1" + "4983e101" + "4d09c8" +
				"f34d0f2ad8" + "f3450f58db",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			i := &instruction{}
			tc.setup(i)
			require.Equal(t, tc.wantFormat, i.String())

			got, needsLabelResolution := encodeNewInstr(i)
			require.False(t, needsLabelResolution)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestInstruction_cvtUint64ToFloatSeq_regalloc pins the register allocator's
// view of the sequence: the source and the two scratch integer registers are
// uses, the destination is the definition, and the assignment order has to
// agree with the order Uses reports.
func TestInstruction_cvtUint64ToFloatSeq_regalloc(t *testing.T) {
	vSrc := regalloc.VReg(1).SetRegType(regalloc.RegTypeInt)
	vDst := regalloc.VReg(2).SetRegType(regalloc.RegTypeFloat)
	vTmpGp := regalloc.VReg(3).SetRegType(regalloc.RegTypeInt)
	vTmpGp2 := regalloc.VReg(4).SetRegType(regalloc.RegTypeInt)

	i := &instruction{}
	i.asCvtUint64ToFloatSeq(vSrc, vDst, vTmpGp, vTmpGp2, true)

	regs := &[]regalloc.VReg{}
	require.Equal(t, []regalloc.VReg{vSrc, vTmpGp, vTmpGp2}, i.Uses(regs))
	require.Equal(t, []regalloc.VReg{vDst}, i.Defs(regs))

	rs := []regalloc.RealReg{rax, rcx, rdx}
	for index, u := range i.Uses(regs) {
		i.AssignUse(index, u.SetRealReg(rs[index]))
	}
	i.AssignDef(vDst.SetRealReg(xmm5))
	require.Equal(t,
		"cvtUint64ToFloatSeq src=%rax, dst=%xmm5, tmpGp=%rcx, tmpGp2=%rdx, dst64=true",
		i.String())

	err := require.CapturePanic(func() { i.AssignUse(3, raxVReg) })
	require.EqualError(t, err, "BUG")

	require.Equal(t, "cvtUint64ToFloatSeq", useKinds[cvtUint64ToFloatSeq].String())
}

// TestInstruction_xmmMinMaxSeq_format_encode pins the scalar min/max sequence.
// mins{s,d}/maxs{s,d} return their second operand when the two are unordered or
// both zero, so ucomis{s,d} splits the three cases apart first (Intel SDM
// Vol.2B, UCOMISS/UCOMISD: ZF=1,PF=1 unordered; ZF=1,PF=0 equal; ZF=0 ordered
// and different):
//
//	ucomis{s,d} %lhs, %rhsDst   (66) 0F 2E /r
//	jnz  doMinMax               0F 85 cd
//	jp   isNaN                  0F 8A cd
//	{or,and}p{s,d} %lhs, %rhsDst  (66) 0F 56 /r or (66) 0F 54 /r
//	jmp  done                   E9 cd
//
// isNaN:
//
//	add{ss,sd} %lhs, %rhsDst    F3/F2 0F 58 /r
//	jmp  done
//
// doMinMax:
//
//	{min,max}s{s,d} %lhs, %rhsDst  F3/F2 0F 5D /r or F3/F2 0F 5F /r
//
// done:
func TestInstruction_xmmMinMaxSeq_format_encode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(*instruction)
		wantFormat string
		want       string
	}{
		{
			name: "min/f64",
			setup: func(i *instruction) {
				i.asXmmMinMaxSeq(true, true, newOperandReg(xmm0VReg), xmm1VReg)
			},
			wantFormat: "minsdSeq %xmm0, %xmm1",
			// 00: 66 0f 2e c8          ucomisd %xmm0, %xmm1
			// 04: 0f 85 18 00 00 00    jne .+24 -> 0x22
			// 0a: 0f 8a 09 00 00 00    jp .+9 -> 0x19
			// 10: 66 0f 56 c8          orpd %xmm0, %xmm1
			// 14: e9 0d 00 00 00       jmp .+13 -> 0x26 (end)
			// 19: f2 0f 58 c8          addsd %xmm0, %xmm1
			// 1d: e9 04 00 00 00       jmp .+4 -> 0x26 (end)
			// 22: f2 0f 5d c8          minsd %xmm0, %xmm1
			want: "660f2ec8" + "0f8518000000" + "0f8a09000000" + "660f56c8" +
				"e90d000000" + "f20f58c8" + "e904000000" + "f20f5dc8",
		},
		{
			name: "min/f32",
			setup: func(i *instruction) {
				i.asXmmMinMaxSeq(true, false, newOperandReg(xmm0VReg), xmm1VReg)
			},
			wantFormat: "minssSeq %xmm0, %xmm1",
			// 00: 0f 2e c8             ucomiss %xmm0, %xmm1
			// 03: 0f 85 17 00 00 00    jne .+23 -> 0x20
			// 09: 0f 8a 08 00 00 00    jp .+8 -> 0x17
			// 0f: 0f 56 c8             orps %xmm0, %xmm1
			// 12: e9 0d 00 00 00       jmp .+13 -> 0x24 (end)
			// 17: f3 0f 58 c8          addss %xmm0, %xmm1
			// 1b: e9 04 00 00 00       jmp .+4 -> 0x24 (end)
			// 20: f3 0f 5d c8          minss %xmm0, %xmm1
			want: "0f2ec8" + "0f8517000000" + "0f8a08000000" + "0f56c8" +
				"e90d000000" + "f30f58c8" + "e904000000" + "f30f5dc8",
		},
		{
			name: "max/f32/extended-register",
			setup: func(i *instruction) {
				i.asXmmMinMaxSeq(false, false, newOperandReg(xmm2VReg), xmm9VReg)
			},
			wantFormat: "maxssSeq %xmm2, %xmm9",
			// 00: 44 0f 2e ca          ucomiss %xmm2, %xmm9
			// 04: 0f 85 19 00 00 00    jne .+25 -> 0x23
			// 0a: 0f 8a 09 00 00 00    jp .+9 -> 0x19
			// 10: 44 0f 54 ca          andps %xmm2, %xmm9
			// 14: e9 0f 00 00 00       jmp .+15 -> 0x28 (end)
			// 19: f3 44 0f 58 ca       addss %xmm2, %xmm9
			// 1e: e9 05 00 00 00       jmp .+5 -> 0x28 (end)
			// 23: f3 44 0f 5f ca       maxss %xmm2, %xmm9
			want: "440f2eca" + "0f8519000000" + "0f8a09000000" + "440f54ca" +
				"e90f000000" + "f3440f58ca" + "e905000000" + "f3440f5fca",
		},
		{
			name: "max/f64/extended-register",
			setup: func(i *instruction) {
				i.asXmmMinMaxSeq(false, true, newOperandReg(xmm13VReg), xmm2VReg)
			},
			wantFormat: "maxsdSeq %xmm13, %xmm2",
			// 00: 66 41 0f 2e d5       ucomisd %xmm13, %xmm2
			// 05: 0f 85 1a 00 00 00    jne .+26 -> 0x25
			// 0b: 0f 8a 0a 00 00 00    jp .+10 -> 0x1b
			// 11: 66 41 0f 54 d5       andpd %xmm13, %xmm2
			// 16: e9 0f 00 00 00       jmp .+15 -> 0x2a (end)
			// 1b: f2 41 0f 58 d5       addsd %xmm13, %xmm2
			// 20: e9 05 00 00 00       jmp .+5 -> 0x2a (end)
			// 25: f2 41 0f 5f d5       maxsd %xmm13, %xmm2
			want: "66410f2ed5" + "0f851a000000" + "0f8a0a000000" + "66410f54d5" +
				"e90f000000" + "f2410f58d5" + "e905000000" + "f2410f5fd5",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			i := &instruction{}
			tc.setup(i)
			require.Equal(t, tc.wantFormat, i.String())

			got, needsLabelResolution := encodeNewInstr(i)
			require.False(t, needsLabelResolution)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestInstruction_xmmMinMaxSeq_regalloc_and_guard pins the register allocator's
// view (both operands are uses; the result register is the second one, which
// the sequence updates in place) and the constructor's requirement that the
// left-hand side be a register, since encode() reads it with operand.reg().
func TestInstruction_xmmMinMaxSeq_regalloc_and_guard(t *testing.T) {
	_, _, m := newSetupWithMockContext()
	defer func() { runtime.KeepAlive(m) }()

	vLhs := regalloc.VReg(1).SetRegType(regalloc.RegTypeFloat)
	vRhsDst := regalloc.VReg(2).SetRegType(regalloc.RegTypeFloat)

	i := &instruction{}
	i.asXmmMinMaxSeq(true, true, newOperandReg(vLhs), vRhsDst)

	regs := &[]regalloc.VReg{}
	require.Equal(t, []regalloc.VReg{vLhs, vRhsDst}, i.Uses(regs))
	require.Equal(t, []regalloc.VReg{}, i.Defs(regs))

	i.AssignUse(0, vLhs.SetRealReg(xmm4))
	i.AssignUse(1, vRhsDst.SetRealReg(xmm6))
	require.Equal(t, "minsdSeq %xmm4, %xmm6", i.String())

	err := require.CapturePanic(func() {
		(&instruction{}).asXmmMinMaxSeq(true, true, newOperandMem(m.newAmodeImmReg(0, raxVReg)), xmm1VReg)
	})
	require.EqualError(t, err, "BUG")
}

// TestInstruction_cvtFloatToSeq_format pins the String() of the two encode-time
// float-to-int sequences. They share their operand layout with the
// post-regalloc fcvtTo{S,U}intSequence pseudo-instructions -- execCtx and src
// in the first amode, the two scratch integer registers in the second, the
// scratch XMM registers in u1/u2 and the three flags packed as bits -- so a
// format that reads the wrong field, or a constructor that writes one, shows up
// as a permuted or mistyped operand here.
//
// The byte sequences these two expand into are pinned in
// encode_time_seq_test.go against the post-regalloc lowering instead.
func TestInstruction_cvtFloatToSeq_format(t *testing.T) {
	_, _, m := newSetupWithMockContext()
	defer func() { runtime.KeepAlive(m) }()

	t.Run("sint", func(t *testing.T) {
		i := m.allocateCvtFloatToSintSeq(r15VReg, xmm0VReg, rcxVReg, rdxVReg, xmm1VReg, true, false, true)
		require.Equal(t, cvtFloatToSintSeq, i.kind)
		require.Equal(t,
			"cvtFloatToSintSeq execCtx=%r15, src=%xmm0, tmpGp=%rcx, tmpGp2=%rdx, tmpXmm=%xmm1, src64=true, dst64=false, sat=true",
			i.String())
	})

	t.Run("sint/flags-cleared", func(t *testing.T) {
		i := m.allocateCvtFloatToSintSeq(r15VReg, xmm2VReg, raxVReg, rbxVReg, xmm3VReg, false, true, false)
		require.Equal(t,
			"cvtFloatToSintSeq execCtx=%r15, src=%xmm2, tmpGp=%rax, tmpGp2=%rbx, tmpXmm=%xmm3, src64=false, dst64=true, sat=false",
			i.String())
	})

	t.Run("uint", func(t *testing.T) {
		i := m.allocateCvtFloatToUintSeq(r15VReg, xmm0VReg, rcxVReg, rdxVReg, xmm1VReg, xmm2VReg, false, true, false)
		require.Equal(t, cvtFloatToUintSeq, i.kind)
		require.Equal(t,
			"cvtFloatToUintSeq execCtx=%r15, src=%xmm0, tmpGp=%rcx, tmpGp2=%rdx, tmpXmm=%xmm1, tmpXmm2=%xmm2, src64=false, dst64=true, sat=false",
			i.String())
	})

	t.Run("uint/flags-set", func(t *testing.T) {
		i := m.allocateCvtFloatToUintSeq(r15VReg, xmm4VReg, rsiVReg, rdiVReg, xmm5VReg, xmm6VReg, true, true, true)
		require.Equal(t,
			"cvtFloatToUintSeq execCtx=%r15, src=%xmm4, tmpGp=%rsi, tmpGp2=%rdi, tmpXmm=%xmm5, tmpXmm2=%xmm6, src64=true, dst64=true, sat=true",
			i.String())
	})
}

// TestInstruction_cvtFloatToSeq_regalloc pins the register-allocator
// descriptors the two new kinds were registered with. They carry their operands
// exactly where fcvtTo{S,U}intSequence does, so they have to reuse the same
// use/def kinds: getting that table row wrong would not fail to compile, it
// would hand the allocator the wrong registers.
func TestInstruction_cvtFloatToSeq_regalloc(t *testing.T) {
	_, _, m := newSetupWithMockContext()
	defer func() { runtime.KeepAlive(m) }()

	vExecCtx := regalloc.VReg(1).SetRegType(regalloc.RegTypeInt)
	vSrc := regalloc.VReg(2).SetRegType(regalloc.RegTypeFloat)
	vTmpGp := regalloc.VReg(3).SetRegType(regalloc.RegTypeInt)
	vTmpGp2 := regalloc.VReg(4).SetRegType(regalloc.RegTypeInt)
	vTmpXmm := regalloc.VReg(5).SetRegType(regalloc.RegTypeFloat)
	vTmpXmm2 := regalloc.VReg(6).SetRegType(regalloc.RegTypeFloat)

	regs := &[]regalloc.VReg{}

	t.Run("sint", func(t *testing.T) {
		i := m.allocateCvtFloatToSintSeq(vExecCtx, vSrc, vTmpGp, vTmpGp2, vTmpXmm, true, true, false)
		require.Equal(t, []regalloc.VReg{vExecCtx, vSrc, vTmpGp, vTmpGp2, vTmpXmm}, i.Uses(regs))
		require.Equal(t, []regalloc.VReg{}, i.Defs(regs))

		rs := []regalloc.RealReg{r15, xmm0, rax, rcx, xmm1}
		for index, u := range i.Uses(regs) {
			i.AssignUse(index, u.SetRealReg(rs[index]))
		}
		require.Equal(t,
			"cvtFloatToSintSeq execCtx=%r15, src=%xmm0, tmpGp=%rax, tmpGp2=%rcx, tmpXmm=%xmm1, src64=true, dst64=true, sat=false",
			i.String())
	})

	t.Run("uint", func(t *testing.T) {
		i := m.allocateCvtFloatToUintSeq(vExecCtx, vSrc, vTmpGp, vTmpGp2, vTmpXmm, vTmpXmm2, true, false, true)
		require.Equal(t, []regalloc.VReg{vExecCtx, vSrc, vTmpGp, vTmpGp2, vTmpXmm, vTmpXmm2}, i.Uses(regs))
		require.Equal(t, []regalloc.VReg{}, i.Defs(regs))

		rs := []regalloc.RealReg{r15, xmm0, rax, rcx, xmm1, xmm2}
		for index, u := range i.Uses(regs) {
			i.AssignUse(index, u.SetRealReg(rs[index]))
		}
		require.Equal(t,
			"cvtFloatToUintSeq execCtx=%r15, src=%xmm0, tmpGp=%rax, tmpGp2=%rcx, tmpXmm=%xmm1, tmpXmm2=%xmm2, src64=true, dst64=false, sat=true",
			i.String())
	})
}

package amd64

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// asBnot initializes instr as `v = Bnot x`.
//
// The SSA layer has no AsBnot constructor because nothing in the wasm frontend
// emits OpcodeBnot today, and the fields of ssa.Instruction are unexported, so a
// test outside that package cannot set the opcode directly. AsExtLoad is the one
// exported initializer that takes the opcode as a parameter, and it writes
// exactly the two fields the Bnot lowering reads: the opcode and the argument
// (i.v, returned by Arg()). Its offset argument lands in a field Bnot ignores.
func asBnot(instr *ssa.Instruction, x ssa.Value) {
	instr.AsExtLoad(ssa.OpcodeBnot, x, 0, x.Type() == ssa.TypeI64)
}

// TestMachine_LowerInstr_Bnot covers the OpcodeBnot lowering: NOT is expressed as
// XOR against an all-ones immediate, because the `not` instruction kind is not
// wired into the register allocator's def/use tables.
//
// The expected sequence follows from the lowering's three steps: copy the operand
// into a temp (copyTo always moves the full 64 bits), XOR it with $-1 at the
// operand's own width, then copy the temp to the destination. The operand width
// is what makes the middle instruction print as a 32- or 64-bit register.
func TestMachine_LowerInstr_Bnot(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  ssa.Type
		exp  []string
	}{
		{
			name: "i64",
			typ:  ssa.TypeI64,
			exp: []string{
				"movq %rax, %r100?",
				"xor $-1, %r100?",
				"movq %r100?, %rcx",
			},
		},
		{
			// The 32-bit XOR is enough for an i32: writing a 32-bit register
			// zeroes bits 63:32, and the backend keeps i32 values zero-extended.
			name: "i32",
			typ:  ssa.TypeI32,
			exp: []string{
				"movq %rax, %r100?",
				"xor $-1, %r100d?",
				"movq %r100?, %rcx",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, m := newSetupWithMockContext()
			ctx.vRegCounter = 99

			x := b.CurrentBlock().AddParam(b, tc.typ)
			ctx.vRegMap[x] = raxVReg
			ctx.definitions[x] = backend.SSAValueDefinition{V: x}
			ctx.typeOf[raxVReg.ID()] = tc.typ
			// An instruction that was not inserted into a block has no return
			// value assigned, so its Return() is ssa.ValueInvalid.
			ctx.vRegMap[ssa.ValueInvalid] = rcxVReg

			instr := b.AllocateInstruction()
			asBnot(instr, x)
			m.LowerInstr(instr)

			require.Equal(t, strings.Join(tc.exp, "\n"), formatEmittedInstructionsInCurrentBlock(m))
		})
	}
}

// TestMachine_lowerBnot_nonInteger pins the guard that rejects a Bnot on a
// non-integer type: without it the lowering would emit an integer XOR against a
// vector or float register.
func TestMachine_lowerBnot_nonInteger(t *testing.T) {
	ctx, b, m := newSetupWithMockContext()

	x := b.CurrentBlock().AddParam(b, ssa.TypeF64)
	ctx.vRegMap[x] = xmm0VReg
	ctx.definitions[x] = backend.SSAValueDefinition{V: x}

	instr := b.AllocateInstruction()
	asBnot(instr, x)

	err := require.CapturePanic(func() { m.LowerInstr(instr) })
	require.EqualError(t, err, "BUG: Bnot on a non-integer type")
}

// TestMachine_LowerInstr_IcmpImm covers the OpcodeIcmpImm lowering and, above
// all, its immediate-range handling.
//
// x64 CMP takes at most a 32-bit immediate and sign-extends it to the operand
// size (Intel SDM Vol.2A, CMP: "81 /7 id CMP r/m64, imm32" — imm32 is sign
// extended to 64 bits). So the immediate form is only usable when the value
// survives that sign extension. For a 32-bit compare every 32-bit pattern does;
// for a 64-bit compare only values in [0, 2^31) do, and everything else has to
// be materialized into a register first. Getting this wrong is silent: `cmpq
// $0x80000000, %rax` would compare against -2147483648.
func TestMachine_LowerInstr_IcmpImm(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  ssa.Type
		imm  uint64
		cond ssa.IntegerCmpCond
		exp  []string
	}{
		{
			name: "i32/eq/0",
			typ:  ssa.TypeI32,
			imm:  0,
			cond: ssa.IntegerCmpCondEqual,
			exp: []string{
				"cmpl $0, %eax",
				"setz %r100?",
				"movzx.bq %r100?, %rcx",
			},
		},
		{
			// 0xffffffff is a valid 32-bit compare immediate: it is the whole
			// operand, no sign extension is involved at this width.
			name: "i32/signed lt/-1",
			typ:  ssa.TypeI32,
			imm:  0xffffffff,
			cond: ssa.IntegerCmpCondSignedLessThan,
			exp: []string{
				"cmpl $-1, %eax",
				"setl %r100?",
				"movzx.bq %r100?, %rcx",
			},
		},
		{
			// The largest immediate a 64-bit CMP can carry: 2^31-1 sign-extends
			// to itself.
			name: "i64/signed lt/2^31-1",
			typ:  ssa.TypeI64,
			imm:  0x7fffffff,
			cond: ssa.IntegerCmpCondSignedLessThan,
			exp: []string{
				"cmpq $2147483647, %rax",
				"setl %r100?",
				"movzx.bq %r100?, %rcx",
			},
		},
		{
			// One past that: 2^31 does not survive sign extension, so it must be
			// materialized. This is the case an imm32 would silently corrupt.
			name: "i64/unsigned lt/2^31",
			typ:  ssa.TypeI64,
			imm:  0x80000000,
			cond: ssa.IntegerCmpCondUnsignedLessThan,
			exp: []string{
				"movabsq $2147483648, %r100?",
				"cmpq %r100?, %rax",
				"setb %r101?",
				"movzx.bq %r101?, %rcx",
			},
		},
		{
			// Wider than 32 bits: no imm32 encoding exists at all.
			name: "i64/ne/2^32",
			typ:  ssa.TypeI64,
			imm:  0x100000000,
			cond: ssa.IntegerCmpCondNotEqual,
			exp: []string{
				"movabsq $4294967296, %r100?",
				"cmpq %r100?, %rax",
				"setnz %r101?",
				"movzx.bq %r101?, %rcx",
			},
		},
		{
			// All ones. A 64-bit CMP against imm32 0xffffffff would in fact
			// compare against -1 correctly, but the lowering reuses the same
			// conservative test as Icmp's y operand and materializes it.
			name: "i64/eq/all ones",
			typ:  ssa.TypeI64,
			imm:  ^uint64(0),
			cond: ssa.IntegerCmpCondEqual,
			exp: []string{
				"movabsq $-1, %r100?",
				"cmpq %r100?, %rax",
				"setz %r101?",
				"movzx.bq %r101?, %rcx",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, m := newSetupWithMockContext()
			ctx.vRegCounter = 99

			x := b.CurrentBlock().AddParam(b, tc.typ)
			ctx.vRegMap[x] = raxVReg
			ctx.definitions[x] = backend.SSAValueDefinition{V: x}
			ctx.vRegMap[ssa.ValueInvalid] = rcxVReg

			instr := b.AllocateInstruction()
			instr.AsIcmpImm(x, tc.imm, tc.cond)
			m.LowerInstr(instr)

			require.Equal(t, strings.Join(tc.exp, "\n"), formatEmittedInstructionsInCurrentBlock(m))
		})
	}
}

// TestMachine_LowerInstr_Ireduce covers the Ireduce lowering, including the
// newly added i64 destination.
//
// Narrowing to i32 is a 32-bit move, which zeroes the upper half and so drops it
// as Ireduce requires. Narrowing to i64 keeps every bit, so it degenerates into a
// 64-bit move — and because the source operand may have been folded into a
// memory operand, that move has a register form and a load form.
func TestMachine_LowerInstr_Ireduce(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup returns the value to reduce.
		setup   func(*mockCompiler, ssa.Builder, *machine) ssa.Value
		dstType ssa.Type
		exp     []string
	}{
		{
			name: "i64 to i32",
			setup: func(ctx *mockCompiler, b ssa.Builder, m *machine) ssa.Value {
				x := b.CurrentBlock().AddParam(b, ssa.TypeI64)
				ctx.vRegMap[x] = raxVReg
				ctx.definitions[x] = backend.SSAValueDefinition{V: x}
				return x
			},
			dstType: ssa.TypeI32,
			exp:     []string{"movzx.lq %rax, %rcx"},
		},
		{
			name: "i64 to i64 (register)",
			setup: func(ctx *mockCompiler, b ssa.Builder, m *machine) ssa.Value {
				x := b.CurrentBlock().AddParam(b, ssa.TypeI64)
				ctx.vRegMap[x] = raxVReg
				ctx.definitions[x] = backend.SSAValueDefinition{V: x}
				return x
			},
			dstType: ssa.TypeI64,
			exp:     []string{"movq %rax, %rcx"},
		},
		{
			// getOperand_Mem_Reg folds a single-use load into a memory operand,
			// which the i64 arm has to load from rather than move between
			// registers.
			name: "i64 to i64 (folded load)",
			setup: func(ctx *mockCompiler, b ssa.Builder, m *machine) ssa.Value {
				ptr := b.CurrentBlock().AddParam(b, ssa.TypeI64)
				ctx.vRegMap[ptr] = raxVReg
				ctx.definitions[ptr] = backend.SSAValueDefinition{V: ptr}

				load := b.AllocateInstruction().AsLoad(ptr, 123, ssa.TypeI64).Insert(b)
				x := load.Return()
				ctx.definitions[x] = backend.SSAValueDefinition{V: x, Instr: load}
				return x
			},
			dstType: ssa.TypeI64,
			exp:     []string{"movq 123(%rax), %rcx"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, m := newSetupWithMockContext()
			ctx.vRegCounter = 99

			x := tc.setup(ctx, b, m)
			instr := b.AllocateInstruction().AsIreduce(x, tc.dstType).Insert(b)
			ctx.vRegMap[instr.Return()] = rcxVReg

			m.LowerInstr(instr)

			require.Equal(t, strings.Join(tc.exp, "\n"), formatEmittedInstructionsInCurrentBlock(m))
		})
	}
}

// TestMachine_LowerInstr_Ireduce_invalid pins the two guards on the Ireduce
// lowering. Widening to i64 from a narrower type is not a reduction at all, and
// would read past the end of a folded load; a non-integer destination has no
// reduction to perform.
func TestMachine_LowerInstr_Ireduce_invalid(t *testing.T) {
	for _, tc := range []struct {
		name    string
		srcType ssa.Type
		dstType ssa.Type
		expErr  string
	}{
		{
			name:    "i32 to i64",
			srcType: ssa.TypeI32,
			dstType: ssa.TypeI64,
			expErr:  "BUG: Ireduce to i64 from a narrower type",
		},
		{
			name:    "i64 to f64",
			srcType: ssa.TypeI64,
			dstType: ssa.TypeF64,
			expErr:  "BUG: Ireduce to a non-integer type",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, m := newSetupWithMockContext()

			x := b.CurrentBlock().AddParam(b, tc.srcType)
			ctx.vRegMap[x] = raxVReg
			ctx.definitions[x] = backend.SSAValueDefinition{V: x}

			instr := b.AllocateInstruction().AsIreduce(x, tc.dstType).Insert(b)
			ctx.vRegMap[instr.Return()] = rcxVReg

			err := require.CapturePanic(func() { m.LowerInstr(instr) })
			require.EqualError(t, err, tc.expErr)
		})
	}
}

// TestInstruction_encode_bnotAndIcmpImm pins the machine code behind the two
// lowerings above, which the Format()-level tests cannot see.
//
// Both lowerings lean on x64 sign-extending a short immediate to the operand
// size, and both would be silently wrong if the encoder picked a different form.
// Every expected byte string below was taken from the Intel SDM encoding of the
// instruction and cross-checked against GNU as (`as` + `objdump -d`).
func TestInstruction_encode_bnotAndIcmpImm(t *testing.T) {
	for _, tc := range []struct {
		setup      func(*instruction)
		want       string
		wantFormat string
	}{
		{
			// Bnot, 64-bit: XOR r/m64, imm8 = REX.W + 83 /6 ib, the imm8 0xff
			// sign-extended to 0xffff_ffff_ffff_ffff.
			// REX.W=48, 83, ModRM mod=11 reg=/6 rm=rcx(001) -> f1, ib=ff.
			setup:      func(i *instruction) { i.asAluRmiR(aluRmiROpcodeXor, newOperandImm32(0xffffffff), rcxVReg, true) },
			wantFormat: "xor $-1, %rcx",
			want:       "4883f1ff",
		},
		{
			// Bnot, 32-bit: XOR r/m32, imm8 = 83 /6 ib, imm8 sign-extended to
			// 0xffffffff. No REX byte is needed for %ecx.
			setup:      func(i *instruction) { i.asAluRmiR(aluRmiROpcodeXor, newOperandImm32(0xffffffff), rcxVReg, false) },
			wantFormat: "xor $-1, %ecx",
			want:       "83f1ff",
		},
		{
			// Same with an extended register, to pin REX.B: REX.W|REX.B=49 /
			// REX.B=41, then 83 /6 with rm=r15(111) -> f7.
			setup:      func(i *instruction) { i.asAluRmiR(aluRmiROpcodeXor, newOperandImm32(0xffffffff), r15VReg, true) },
			wantFormat: "xor $-1, %r15",
			want:       "4983f7ff",
		},
		{
			setup:      func(i *instruction) { i.asAluRmiR(aluRmiROpcodeXor, newOperandImm32(0xffffffff), r15VReg, false) },
			wantFormat: "xor $-1, %r15d",
			want:       "4183f7ff",
		},
		{
			// IcmpImm, immediate that fits: CMP r/m64, imm8 = REX.W + 83 /7 ib.
			// The imm8 0xff is sign-extended to -1 across the full 64 bits,
			// which is what makes the immediate form usable here at all.
			setup:      func(i *instruction) { i.asCmpRmiR(true, newOperandImm32(0xffffffff), rdiVReg, true) },
			wantFormat: "cmpq $-1, %rdi",
			want:       "4883ffff",
		},
		{
			setup:      func(i *instruction) { i.asCmpRmiR(true, newOperandImm32(0xffffffff), rdiVReg, false) },
			wantFormat: "cmpl $-1, %edi",
			want:       "83ffff",
		},
		{
			// The largest immediate the 64-bit compare can carry:
			// CMP r/m64, imm32 = REX.W + 81 /7 id, id little-endian 7fffffff.
			setup:      func(i *instruction) { i.asCmpRmiR(true, newOperandImm32(0x7fffffff), rdiVReg, true) },
			wantFormat: "cmpq $2147483647, %rdi",
			want:       "4881ffffffff7f",
		},
		{
			setup:      func(i *instruction) { i.asCmpRmiR(true, newOperandImm32(0x7fffffff), rdiVReg, false) },
			wantFormat: "cmpl $2147483647, %edi",
			want:       "81ffffffff7f",
		},
	} {
		tc := tc
		t.Run(tc.wantFormat, func(t *testing.T) {
			i := &instruction{}
			tc.setup(i)

			require.Equal(t, tc.wantFormat, i.String())

			mc := &mockCompiler{}
			m := &machine{c: mc}
			i.encode(m.c)
			require.Equal(t, tc.want, hex.EncodeToString(mc.buf))
		})
	}
}

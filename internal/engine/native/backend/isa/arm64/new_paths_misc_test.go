package arm64

import (
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// encodeChain encodes the instruction list starting at root and returns the machine code as a hex
// string, so a lowering can be checked against the encodings in the Arm ARM and not merely against
// its own printer.
func encodeChain(m *machine, root *instruction) string {
	m.encode(root)
	return hex.EncodeToString(m.compiler.Buf())
}

// Test_goCallSlotSize pins the stride of the arg[0]/ret[0]... region of a Go function call. The Go
// side reads and writes that region as a []uint64 (see backend.GoFunctionCallRequiredStackSize),
// so every basic type occupies exactly one 8-byte slot no matter how wide it really is, and a
// v128 occupies two. A wrong stride here silently misaligns every subsequent argument.
func Test_goCallSlotSize(t *testing.T) {
	for _, tc := range []struct {
		typ ssa.Type
		exp int64
	}{
		{typ: ssa.TypeI32, exp: 8},
		{typ: ssa.TypeI64, exp: 8},
		{typ: ssa.TypeF32, exp: 8},
		{typ: ssa.TypeF64, exp: 8},
		{typ: ssa.TypeV128, exp: 16},
	} {
		t.Run(tc.typ.String(), func(t *testing.T) {
			require.Equal(t, tc.exp, goCallSlotSize(tc.typ))
		})
	}
}

// Test_spillSlotBitsOf covers both arms of the spill-width rule: a VReg that stands for an SSA
// value is spilled at the width of its type, while a VReg that names a real register directly has
// no SSA type (backend.compiler.TypeOf returns typeInvalid for the reserved real-register IDs), so
// the whole architectural register is spilled -- 64 bits for an X register, 128 for a Q register.
func Test_spillSlotBitsOf(t *testing.T) {
	// typeInvalid is unexported in package ssa; it is the zero Type, which is exactly what
	// compiler.TypeOf returns for a VReg that was never given an SSA type.
	const typeInvalid = ssa.Type(0)
	for _, tc := range []struct {
		name string
		v    regalloc.VReg
		typ  ssa.Type
		exp  byte
	}{
		{name: "i32 in x1", v: x1VReg, typ: ssa.TypeI32, exp: 32},
		{name: "i64 in x1", v: x1VReg, typ: ssa.TypeI64, exp: 64},
		{name: "f32 in v1", v: v1VReg, typ: ssa.TypeF32, exp: 32},
		{name: "f64 in v1", v: v1VReg, typ: ssa.TypeF64, exp: 64},
		{name: "v128 in v1", v: v1VReg, typ: ssa.TypeV128, exp: 128},
		{name: "untyped x1 spills the whole X register", v: x1VReg, typ: typeInvalid, exp: 64},
		{name: "untyped v1 spills the whole Q register", v: v1VReg, typ: typeInvalid, exp: 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.exp, spillSlotBitsOf(tc.v, tc.typ))
		})
	}
}

// TestMachine_spillReload_untypedRealRegister covers the previously-panicking path of
// insertStoreRegisterAt/insertReloadRegisterAt: a real register with no SSA type, which is what the
// allocator hands them when it spills a register that does not stand for an SSA value.
//
// It asserts three things that the old code could not express at all:
//   - the whole register is saved and restored (x1 as 64 bits, v1 as the full 128-bit q1),
//   - the reload lands on exactly the slot the store used, and
//   - the untyped vector slot is 16 bytes wide, so the next spill slot starts 16 bytes higher.
//
// The expected machine code is derived from the Arm ARM, "LDR/STR (immediate, unsigned offset)":
//
//	31:30 size | 29:27 111 | 26 V | 25:24 01 | 23:22 opc | 21:10 imm12 | 9:5 Rn | 4:0 Rt
//
// with imm12 scaled by the transfer size, Rn = 31 (SP) and Rt = 1:
//
//	str q1, [sp, #16]  size=00 V=1 opc=10 imm12=16/16=1 -> 0x3d8007e1
//	str x1, [sp, #32]  size=11 V=0 opc=00 imm12=32/8=4  -> 0xf90013e1
//	ldr q1, [sp, #16]  size=00 V=1 opc=11 imm12=1       -> 0x3dc007e1
//	ldr x1, [sp, #32]  size=11 V=0 opc=01 imm12=4       -> 0xf94013e1
//
// Cross-checked with an assembler: llvm-mc -triple=aarch64 -show-encoding.
func TestMachine_spillReload_untypedRealRegister(t *testing.T) {
	ctx, _, m := newSetupWithMockContext()
	// No typeOf entries at all: TypeOf returns typeInvalid for both registers, exactly as the real
	// compiler does for a VReg that names a real register.
	require.Equal(t, ssa.Type(0), ctx.TypeOf(x1VReg))
	require.Equal(t, ssa.Type(0), ctx.TypeOf(v1VReg))

	head, tail := m.allocateNop(), m.allocateNop()
	head.next = tail
	tail.prev = head

	m.insertStoreRegisterAt(v1VReg, tail, false)
	m.insertStoreRegisterAt(x1VReg, tail, false)
	m.insertReloadRegisterAt(v1VReg, tail, false)
	m.insertReloadRegisterAt(x1VReg, tail, false)

	m.rootInstr = head
	require.Equal(t, `
	str q1, [sp, #0x10]
	str x1, [sp, #0x20]
	ldr q1, [sp, #0x10]
	ldr x1, [sp, #0x20]
`, m.Format())

	// v1 took a 16-byte slot (offsets 0..15 of the spill region) and x1 an 8-byte one after it.
	require.Equal(t, int64(24), m.spillSlotSize)

	require.Equal(t, "e107803d"+"e11300f9"+"e107c03d"+"e11340f9", encodeChain(m, head))
}

// TestMachine_insertLoadConstant covers insertLoadConstant for every ssa.Type, including the v128
// case added by this branch and the masking of the raw constant to the type's width.
func TestMachine_insertLoadConstant(t *testing.T) {
	// The first VReg the mock compiler hands out is 100, so the scratch GPRs below are x100?/w100?.
	const nextVRegID = 100
	for _, tc := range []struct {
		name            string
		v               uint64
		typ             ssa.Type
		vr              regalloc.VReg
		regAllocStarted bool
		exp             string
	}{
		{
			name: "i32 zero", v: 0, typ: ssa.TypeI32, vr: x1VReg,
			exp: "mov x1, xzr",
		},
		{
			// The upper 32 bits are noise from a sign-extended constant and must be masked off
			// before the value is materialized, so this must lower exactly like 0x80000001.
			name: "i32 masks the high half", v: 0xffff_ffff_8000_0001, typ: ssa.TypeI32, vr: x1VReg,
			exp: `movz w1, #0x1, lsl 0
movk w1, #0x8000, lsl 16`,
		},
		{
			name: "i64 zero", v: 0, typ: ssa.TypeI64, vr: x1VReg,
			exp: "mov x1, xzr",
		},
		{
			name: "i64 bitmask immediate", v: 1, typ: ssa.TypeI64, vr: x1VReg,
			exp: "orr x1, xzr, #0x1",
		},
		{
			name: "f32 zero", v: 0, typ: ssa.TypeF32, vr: v1VReg,
			exp: "ldr s1, #8; b 8; data.f32 0.000000",
		},
		{
			// 1.0f == 0x3f800000: one movz with a 16-bit-aligned immediate, then ins into lane 0.
			name: "f32 via gpr", v: 0x3f80_0000, typ: ssa.TypeF32, vr: v1VReg,
			exp: `movz w100?, #0x3f80, lsl 16
ins v1.s[0], w100?`,
		},
		{
			// Same masking rule as the i32 case: the bits above the type's width are discarded, so
			// this must lower exactly like the clean 0x3f800000 above.
			name: "f32 masks the high half", v: 0xdead_beef_3f80_0000, typ: ssa.TypeF32, vr: v1VReg,
			exp: `movz w100?, #0x3f80, lsl 16
ins v1.s[0], w100?`,
		},
		{
			name: "f64 zero", v: 0, typ: ssa.TypeF64, vr: v1VReg,
			exp: "ldr d1, #8; b 16; data.f64 0.000000",
		},
		{
			// 2.0 == 0x4000000000000000: one movz at lsl 48, then ins into the low 64-bit lane.
			name: "f64 via gpr", v: 0x4000_0000_0000_0000, typ: ssa.TypeF64, vr: v1VReg,
			exp: `movz x100?, #0x4000, lsl 48
ins v1.d[0], x100?`,
		},
		{
			name: "v128 zero", v: 0, typ: ssa.TypeV128, vr: v1VReg,
			exp: "ldr q1, #8; b 32; data.v128  0000000000000000 0000000000000000",
		},
		{
			// The constant names the low lane only, so the high lane must be zeroed explicitly --
			// otherwise whatever the allocator left in v1 would leak into the value.
			name: "v128 zero-extends the low lane", v: 0x2a, typ: ssa.TypeV128, vr: v1VReg,
			exp: `movz x100?, #0x2a, lsl 0
ins v1.d[0], x100?
ins v1.d[1], xzr`,
		},
		{
			// After register allocation there is no scratch GPR to build the constant in, so the
			// only admissible forms are the pooled ldr-literal ...
			name: "v128 after regalloc is pooled", v: 0x8970_5f41_36b4_a598, typ: ssa.TypeV128, vr: v1VReg,
			regAllocStarted: true,
			exp:             "ldr q1, L0 ;; pooled const 89705f4136b4a598 0000000000000000",
		},
		{
			// ... and the self-clearing eor for zero, which needs no scratch register either.
			name: "v128 zero after regalloc", v: 0, typ: ssa.TypeV128, vr: v1VReg,
			regAllocStarted: true,
			exp:             "ldr q1, #8; b 32; data.v128  0000000000000000 0000000000000000",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _, m := newSetupWithMockContext()
			ctx.vRegCounter = nextVRegID - 1
			m.regAllocStarted = tc.regAllocStarted
			m.insertLoadConstant(tc.v, tc.typ, tc.vr)
			require.Equal(t, tc.exp, formatEmittedInstructionsInCurrentBlock(m))
		})
	}
}

// TestMachine_insertLoadConstant_v128Zero_encoding checks that the zero v128 constant really is the
// self-clearing `eor v1.16b, v1.16b, v1.16b` and not a memory access. Arm ARM, EOR (vector):
//
//	0 Q=1 1 01110 00 1 Rm 000111 Rn Rd -> 0x6e201c00 | Rm<<16 | Rn<<5 | Rd
//
// with Rd = Rn = Rm = 1: 0x6e211c21. Cross-checked with llvm-mc -triple=aarch64.
func TestMachine_insertLoadConstant_v128Zero_encoding(t *testing.T) {
	_, _, m := newSetupWithMockContext()
	m.regAllocStarted = true
	m.insertLoadConstant(0, ssa.TypeV128, v1VReg)
	m.FlushPendingInstructions()
	require.Equal(t, "211c216e", encodeChain(m, m.perBlockHead))
}

// TestMachine_lowerLoad covers lowerLoad for every ssa.Type: the transfer width must be the type's
// width and the instruction form must match the class of register the result was allocated into.
//
// The expected machine code is derived from the Arm ARM, "LDR (immediate, unsigned offset)" and
// "LDR (immediate, SIMD&FP, unsigned offset)":
//
//	31:30 size | 29:27 111 | 26 V | 25:24 01 | 23:22 opc | 21:10 imm12 | 9:5 Rn | 4:0 Rt
//
// imm12 is the byte offset divided by the transfer size, so the same #16 encodes as 4, 2 or 1
// depending on the width. Here Rn = 2 (the pointer) and Rt = 1 (the destination):
//
//	ldr w1, [x2, #16]  size=10 V=0 opc=01 imm12=4 -> 0xb9401041
//	ldr x1, [x2, #16]  size=11 V=0 opc=01 imm12=2 -> 0xf9400841
//	ldr s1, [x2, #16]  size=10 V=1 opc=01 imm12=4 -> 0xbd401041
//	ldr d1, [x2, #16]  size=11 V=1 opc=01 imm12=2 -> 0xfd400841
//	ldr q1, [x2, #16]  size=00 V=1 opc=11 imm12=1 -> 0x3dc00441
//
// Cross-checked with an assembler: llvm-mc -triple=aarch64 -show-encoding.
func TestMachine_lowerLoad(t *testing.T) {
	for _, tc := range []struct {
		typ     ssa.Type
		dst     regalloc.VReg
		exp     string
		expCode string
	}{
		{typ: ssa.TypeI32, dst: x1VReg, exp: "ldr w1, [x2, #0x10]", expCode: "411040b9"},
		{typ: ssa.TypeI64, dst: x1VReg, exp: "ldr x1, [x2, #0x10]", expCode: "410840f9"},
		{typ: ssa.TypeF32, dst: v1VReg, exp: "ldr s1, [x2, #0x10]", expCode: "411040bd"},
		{typ: ssa.TypeF64, dst: v1VReg, exp: "ldr d1, [x2, #0x10]", expCode: "410840fd"},
		{typ: ssa.TypeV128, dst: v1VReg, exp: "ldr q1, [x2, #0x10]", expCode: "4104c03d"},
	} {
		t.Run(tc.typ.String(), func(t *testing.T) {
			ctx, b, m := newSetupWithMockContext()

			ptr := b.CurrentBlock().AddParam(b, ssa.TypeI64)
			ctx.vRegMap[ptr] = x2VReg
			ctx.definitions[ptr] = backend.SSAValueDefinition{V: ptr}
			ret := b.CurrentBlock().AddParam(b, tc.typ)
			ctx.vRegMap[ret] = tc.dst

			m.lowerLoad(ptr, 16, tc.typ, ret)

			require.Equal(t, tc.exp, formatEmittedInstructionsInCurrentBlock(m))
			require.Equal(t, tc.expCode, encodeChain(m, m.perBlockHead))
		})
	}
}

// TestMachine_lowerLoadConstantBlockArgAfterRegAlloc drives the v128 arm of insertLoadConstant
// through the path that actually reaches it in a compiled function: a constant block argument
// re-lowered after register allocation, where no scratch GPR can be allocated any more.
func TestMachine_lowerLoadConstantBlockArgAfterRegAlloc(t *testing.T) {
	for _, tc := range []struct {
		typ ssa.Type
		v   uint64
		exp string
	}{
		{typ: ssa.TypeI32, v: 1, exp: "orr w1, wzr, #0x1"},
		{typ: ssa.TypeI64, v: 1, exp: "orr x1, xzr, #0x1"},
		{typ: ssa.TypeV128, v: 0, exp: "ldr q1, #8; b 32; data.v128  0000000000000000 0000000000000000"},
		{typ: ssa.TypeV128, v: 0x2a, exp: "ldr q1, L0 ;; pooled const 000000000000002a 0000000000000000"},
	} {
		t.Run(fmt.Sprintf("%s/%#x", tc.typ, tc.v), func(t *testing.T) {
			_, _, m := newSetupWithMockContext()
			dst := x1VReg
			if !tc.typ.IsInt() {
				dst = v1VReg
			}
			i := m.allocateInstr().asLoadConstBlockArg(tc.v, tc.typ, dst)
			m.regAllocStarted = true
			m.lowerLoadConstantBlockArgAfterRegAlloc(i)
			require.Equal(t, tc.exp, formatEmittedInstructionsInCurrentBlock(m))
		})
	}
}

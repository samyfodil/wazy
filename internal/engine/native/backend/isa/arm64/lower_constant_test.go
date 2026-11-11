package arm64

import (
	"fmt"
	"math"
	"math/bits"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestMachine_lowerConstant(t *testing.T) {
	t.Run("zero i32", func(t *testing.T) {
		ssaB, m := newSetup()
		ssaConstInstr := ssaB.AllocateInstruction()
		ssaConstInstr.AsIconst32(0)
		ssaB.InsertInstruction(ssaConstInstr)

		vr := m.lowerConstant(ssaConstInstr)
		machInstr := getPendingInstr(m)
		require.Equal(t, regalloc.VRegIDNonReservedBegin, vr.ID())
		require.Equal(t, regalloc.RegTypeInt, vr.RegType())
		require.Equal(t, mov64, machInstr.kind)
		require.Equal(t, "mov x128?, xzr", formatEmittedInstructionsInCurrentBlock(m))
	})

	t.Run("zero i64", func(t *testing.T) {
		ssaB, m := newSetup()
		ssaConstInstr := ssaB.AllocateInstruction()
		ssaConstInstr.AsIconst64(0)
		ssaB.InsertInstruction(ssaConstInstr)

		vr := m.lowerConstant(ssaConstInstr)
		machInstr := getPendingInstr(m)
		require.Equal(t, regalloc.VRegIDNonReservedBegin, vr.ID())
		require.Equal(t, regalloc.RegTypeInt, vr.RegType())
		require.Equal(t, mov64, machInstr.kind)
		require.Equal(t, "mov x128?, xzr", formatEmittedInstructionsInCurrentBlock(m))
	})

	t.Run("TypeF32", func(t *testing.T) {
		ssaB, m := newSetup()
		ssaConstInstr := ssaB.AllocateInstruction()
		ssaConstInstr.AsF32const(1.1234)
		ssaB.InsertInstruction(ssaConstInstr)

		vr := m.lowerConstant(ssaConstInstr)
		require.Equal(t, regalloc.VRegIDNonReservedBegin, vr.ID())
		require.Equal(t, regalloc.RegTypeFloat, vr.RegType())
		// A 32-bit pattern is always <=2 movz/movk, so it is materialized in a GPR and moved into
		// the FP register with `ins`, avoiding the executed branch-over-literal.
		require.Equal(t, `movz w129?, #0xcb92, lsl 0
movk w129?, #0x3f8f, lsl 16
fmov s128?, w129?`, formatEmittedInstructionsInCurrentBlock(m))
	})

	t.Run("TypeF64", func(t *testing.T) {
		ssaB, m := newSetup()
		ssaConstInstr := ssaB.AllocateInstruction()
		ssaConstInstr.AsF64const(-9471.2)
		ssaB.InsertInstruction(ssaConstInstr)

		vr := m.lowerConstant(ssaConstInstr)
		machInstr := getPendingInstr(m)
		require.Equal(t, regalloc.VRegIDNonReservedBegin, vr.ID())
		require.Equal(t, regalloc.RegTypeFloat, vr.RegType())
		// -9471.2 is a high-entropy F64 (not cheap to build in a GPR), so it is
		// loaded from the shared literal pool via a single ldr-literal rather
		// than the inline ldr+branch-over-literal (C8 part 2b).
		require.Equal(t, loadFpuConstPooled, machInstr.kind)
		require.Equal(t, math.Float64bits(-9471.2), machInstr.u1)

		require.Equal(t, "ldr d128?, L0 ;; pooled const c0c27f999999999a 0000000000000000", formatEmittedInstructionsInCurrentBlock(m))
	})
}

func TestMachine_lowerConstantI32(t *testing.T) {
	for _, tc := range []struct {
		val uint32
		exp []string
	}{
		{val: 0, exp: []string{"movz w0, #0x0, lsl 0"}},
		{val: 0xffff, exp: []string{"movz w0, #0xffff, lsl 0"}},
		{val: 0xffff_0000, exp: []string{"movz w0, #0xffff, lsl 16"}},
		{val: 0xffff_fffe, exp: []string{"movn w0, #0x1, lsl 0"}},
		{val: 0x2, exp: []string{"orr w0, wzr, #0x2"}},
		// Two set bits that wrap round the 32-bit pattern: a logical immediate.
		{val: 0x80000001, exp: []string{"orr w0, wzr, #0x80000001"}},
		{val: 0x80000003, exp: []string{"orr w0, wzr, #0x80000003"}},
		{val: 0xf00000f, exp: []string{
			"movz w0, #0xf, lsl 0",
			"movk w0, #0xf00, lsl 16",
		}},
	} {
		tc := tc
		t.Run(fmt.Sprintf("%#x", tc.val), func(t *testing.T) {
			_, m := newSetup()
			m.lowerConstantI32(regToVReg(x0), int32(tc.val))
			exp := strings.Join(tc.exp, "\n")
			require.Equal(t, exp, formatEmittedInstructionsInCurrentBlock(m))
		})
	}
}

func TestMachine_lowerConstantI64(t *testing.T) {
	invert := func(v uint64) uint64 { return ^v }
	for _, tc := range []struct {
		val uint64
		exp []string
	}{
		{val: 0x0, exp: []string{"movz x0, #0x0, lsl 0"}},
		{val: 0x1, exp: []string{"orr x0, xzr, #0x1"}},
		{val: 0x3, exp: []string{"orr x0, xzr, #0x3"}},
		{val: 0xfff000, exp: []string{"orr x0, xzr, #0xfff000"}},
		{val: 0x8001 << 16, exp: []string{"movz x0, #0x8001, lsl 16"}},
		{val: 0x8001 << 32, exp: []string{"movz x0, #0x8001, lsl 32"}},
		{val: 0x8001 << 48, exp: []string{"movz x0, #0x8001, lsl 48"}},
		{val: invert(0x8001 << 16), exp: []string{"movn x0, #0x8001, lsl 16"}},
		{val: invert(0x8001 << 32), exp: []string{"movn x0, #0x8001, lsl 32"}},
		{val: invert(0x8001 << 48), exp: []string{"movn x0, #0x8001, lsl 48"}},
		{val: 0x80000001 << 16, exp: []string{
			"movz x0, #0x1, lsl 16",
			"movk x0, #0x8000, lsl 32",
		}},
		{val: 0x40000001, exp: []string{
			"movz x0, #0x1, lsl 0",
			"movk x0, #0x4000, lsl 16",
		}},
		{val: 0xffffffffff001000, exp: []string{
			"movn x0, #0xefff, lsl 0",
			"movk x0, #0xff00, lsl 16",
		}},
		{val: 0xffff0000c466361f, exp: []string{
			"movz x0, #0x361f, lsl 0",
			"movk x0, #0xc466, lsl 16",
			"movk x0, #0xffff, lsl 48",
		}},
		{val: 0x89705f4136b4a598, exp: []string{
			"movz x0, #0xa598, lsl 0",
			"movk x0, #0x36b4, lsl 16",
			"movk x0, #0x5f41, lsl 32",
			"movk x0, #0x8970, lsl 48",
		}},
		{val: 0xffff_0001_0001_0001, exp: []string{
			"movn x0, #0xfffe, lsl 0",
			"movk x0, #0x1, lsl 16",
			"movk x0, #0x1, lsl 32",
		}},
	} {
		tc := tc
		t.Run(fmt.Sprintf("%#x", tc.val), func(t *testing.T) {
			_, m := newSetup()
			m.lowerConstantI64(regToVReg(x0), int64(tc.val))
			exp := strings.Join(tc.exp, "\n")
			require.Equal(t, exp, formatEmittedInstructionsInCurrentBlock(m))
		})
	}
}

// TestFpuConstPooledDemotion verifies the ±1MB range fallback (C8 part 2b):
// demoting a pooled FP-const load back to the inline ldr+branch-over-literal
// form must preserve the constant, the destination register, AND the
// instruction's list links (prev/next) so the demoted instr stays in place.
func TestFpuConstPooledDemotion(t *testing.T) {
	for _, tc := range []struct {
		width  byte
		lo, hi uint64
		expK5  instructionKind
	}{
		{32, 0x3f8f_cb92, 0, loadFpuConst32},
		{64, 0xc0c2_7f99_9999_999a, 0, loadFpuConst64},
		{128, 0x0706_0504_0302_0100, 0x0f0e_0d0c_0b0a_0908, loadFpuConst128},
	} {
		_, _, m := newSetupWithMockContext()
		prev, next := m.allocateNop(), m.allocateNop()
		i := m.allocateInstr()
		i.asLoadFpuConstPooled(regToVReg(v0).SetRegType(regalloc.RegTypeFloat), tc.lo, tc.hi, tc.width, label(7))
		i.prev, i.next = prev, next

		require.Equal(t, loadFpuConstPooled, i.kind)
		i.demoteFpuConstPooledToInline()

		require.Equal(t, tc.expK5, i.kind)
		require.Equal(t, tc.lo, i.u1)
		if tc.width == 128 {
			require.Equal(t, tc.hi, i.u2)
		}
		// List links survive the rewrite.
		require.Equal(t, prev, i.prev)
		require.Equal(t, next, i.next)
	}
}

// decodeBitMask is the architecture's DecodeBitMasks for a logical immediate of an operation of width
// bits (32 or 64), reporting false for a reserved encoding.
func decodeBitMask(n, imms, immr byte, width uint) (uint64, bool) {
	v := uint32(n)<<6 | uint32(^imms&0x3f)
	if v == 0 {
		return 0, false
	}
	l := uint(bits.Len32(v)) - 1
	esize := uint(1) << l
	if esize > width {
		return 0, false
	}
	levels := byte(esize - 1)
	s, r := imms&levels, immr&levels
	if s == levels {
		return 0, false
	}
	welem := uint64(1)<<(s+1) - 1
	mask := uint64(1)<<esize - 1
	if esize == 64 {
		mask = ^uint64(0)
	}
	elem := (welem>>r | welem<<(esize-uint(r))) & mask
	var x uint64
	for i := uint(0); i < width; i += esize {
		x |= elem << i
	}
	return x, true
}

func TestBitMaskImmediate_everyEncoding(t *testing.T) {
	for _, width := range []uint{32, 64} {
		_64 := width == 64
		valid := map[uint64]bool{}
		for n := byte(0); n <= 1; n++ {
			if n == 1 && !_64 {
				continue
			}
			for imms := byte(0); imms < 64; imms++ {
				for immr := byte(0); immr < 64; immr++ {
					if !_64 && immr >= 32 {
						continue
					}
					if x, ok := decodeBitMask(n, imms, immr, width); ok {
						valid[x] = true
					}
				}
			}
		}
		for x := range valid {
			require.True(t, isBitMaskImmediate(x, _64), "%#x, %d bits", x, width)
			immr, imms, n := bitmaskImmediate(x, _64)
			got, ok := decodeBitMask(n, imms, immr, width)
			require.True(t, ok, "%#x, %d bits", x, width)
			require.Equal(t, x, got, "%#x, %d bits", x, width)
		}
		for _, x := range []uint64{0, 5, 0x12345678, 0xfffffffa, 0x8000_0001_0000_0001} {
			if !_64 {
				x = uint64(uint32(x))
			}
			require.Equal(t, valid[x], isBitMaskImmediate(x, _64), "%#x, %d bits", x, width)
		}
	}
}

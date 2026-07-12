package arm64

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy/internal/engine/wazevo/backend"
	"github.com/samyfodil/wazy/internal/engine/wazevo/backend/regalloc"
	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestMachine_setupPrologue(t *testing.T) {
	for _, tc := range []struct {
		spillSlotSize int64
		clobberedRegs []regalloc.VReg
		exp           string
		abi           backend.FunctionABI
	}{
		{
			spillSlotSize: 0,
			exp: `
	stp x30, xzr, [sp, #-0x10]!
	str xzr, [sp, #-0x10]!
	udf
`,
		},
		{
			spillSlotSize: 0,
			abi:           backend.FunctionABI{ArgStackSize: 16, RetStackSize: 16},
			exp: `
	orr x27, xzr, #0x20
	sub sp, sp, x27
	stp x30, x27, [sp, #-0x10]!
	str xzr, [sp, #-0x10]!
	udf
`,
		},
		{
			spillSlotSize: 16,
			exp: `
	stp x30, xzr, [sp, #-0x10]!
	sub sp, sp, #0x10
	orr x27, xzr, #0x10
	str x27, [sp, #-0x10]!
	udf
`,
		},
		{
			spillSlotSize: 0,
			clobberedRegs: []regalloc.VReg{v18VReg, v19VReg, x18VReg, x25VReg},
			exp: `
	stp x30, xzr, [sp, #-0x10]!
	str q18, [sp, #-0x10]!
	str q19, [sp, #-0x10]!
	str x18, [sp, #-0x10]!
	str x25, [sp, #-0x10]!
	orr x27, xzr, #0x40
	str x27, [sp, #-0x10]!
	udf
`,
		},
		{
			spillSlotSize: 320,
			clobberedRegs: []regalloc.VReg{v18VReg, v19VReg, x18VReg, x25VReg},
			exp: `
	stp x30, xzr, [sp, #-0x10]!
	str q18, [sp, #-0x10]!
	str q19, [sp, #-0x10]!
	str x18, [sp, #-0x10]!
	str x25, [sp, #-0x10]!
	sub sp, sp, #0x140
	orr x27, xzr, #0x180
	str x27, [sp, #-0x10]!
	udf
`,
		},
		{
			spillSlotSize: 320,
			abi:           backend.FunctionABI{ArgStackSize: 320, RetStackSize: 160},
			clobberedRegs: []regalloc.VReg{v18VReg, v19VReg, x18VReg, x25VReg},
			exp: `
	orr x27, xzr, #0x1e0
	sub sp, sp, x27
	stp x30, x27, [sp, #-0x10]!
	str q18, [sp, #-0x10]!
	str q19, [sp, #-0x10]!
	str x18, [sp, #-0x10]!
	str x25, [sp, #-0x10]!
	sub sp, sp, #0x140
	orr x27, xzr, #0x180
	str x27, [sp, #-0x10]!
	udf
`,
		},
	} {
		tc := tc
		t.Run(tc.exp, func(t *testing.T) {
			_, _, m := newSetupWithMockContext()
			m.DisableStackCheck()
			m.spillSlotSize = tc.spillSlotSize
			m.clobberedRegs = tc.clobberedRegs
			m.currentABI = &tc.abi

			root := m.allocateNop()
			m.rootInstr = root
			udf := m.allocateInstr()
			udf.asUDF()
			root.next = udf
			udf.prev = root

			m.setupPrologue()
			require.Equal(t, root, m.rootInstr)
			err := m.Encode(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.exp, m.Format())
		})
	}
}

// TestMachine_setupPrologue_ehContext asserts the P3.0 prologue stores: a
// function with an EH context stores execCtx (x0) and moduleCtx (x1) into the
// two reserved fixed slots at the bottom of the spill region ([sp+16] and
// [sp+24], i.e. spill offsets 0 and 8 above the 16-byte frame-size slot).
// RegAlloc reserves ehCtxReservedSlotSize bytes there (simulated here by
// seeding spillSlotSize=16), so the offsets are compile-time constant.
func TestMachine_setupPrologue_ehContext(t *testing.T) {
	_, _, m := newSetupWithMockContext()
	m.DisableStackCheck()
	m.spillSlotSize = ehCtxReservedSlotSize // as RegAlloc reserves for an EH function.
	m.hasEHContext = true
	m.currentABI = &backend.FunctionABI{}

	root := m.allocateNop()
	m.rootInstr = root
	udf := m.allocateInstr()
	udf.asUDF()
	root.next = udf
	udf.prev = root

	m.setupPrologue()
	require.Equal(t, root, m.rootInstr)
	err := m.Encode(context.Background())
	require.NoError(t, err)
	require.Equal(t, `
	stp x30, xzr, [sp, #-0x10]!
	sub sp, sp, #0x10
	orr x27, xzr, #0x10
	str x27, [sp, #-0x10]!
	str x0, [sp, #0x10]
	str x1, [sp, #0x18]
	udf
`, m.Format())
}

func TestMachine_postRegAlloc(t *testing.T) {
	for _, tc := range []struct {
		exp           string
		abi           backend.FunctionABI
		clobberedRegs []regalloc.VReg
		spillSlotSize int64
	}{
		{
			exp: `
	add sp, sp, #0x10
	ldr x30, [sp], #0x10
	ret
`,
			spillSlotSize: 0,
			clobberedRegs: nil,
		},
		{
			exp: `
	add sp, sp, #0x10
	add sp, sp, #0x50
	ldr x30, [sp], #0x10
	ret
`,
			spillSlotSize: 16 * 5,
			clobberedRegs: nil,
		},
		{
			exp: `
	add sp, sp, #0x10
	add sp, sp, #0x50
	ldr x30, [sp], #0x10
	add sp, sp, #0x20
	ret
`,
			abi:           backend.FunctionABI{ArgStackSize: 16, RetStackSize: 16},
			spillSlotSize: 16 * 5,
			clobberedRegs: nil,
		},
		{
			exp: `
	add sp, sp, #0x10
	ldr q27, [sp], #0x10
	ldr q18, [sp], #0x10
	ldr x30, [sp], #0x10
	ret
`,
			clobberedRegs: []regalloc.VReg{v18VReg, v27VReg},
		},
		{
			exp: `
	add sp, sp, #0x10
	ldr x25, [sp], #0x10
	ldr x18, [sp], #0x10
	ldr q27, [sp], #0x10
	ldr q18, [sp], #0x10
	ldr x30, [sp], #0x10
	ret
`,
			clobberedRegs: []regalloc.VReg{v18VReg, v27VReg, x18VReg, x25VReg},
		},
		{
			exp: `
	add sp, sp, #0x10
	add sp, sp, #0xa0
	ldr x25, [sp], #0x10
	ldr x18, [sp], #0x10
	ldr q27, [sp], #0x10
	ldr q18, [sp], #0x10
	ldr x30, [sp], #0x10
	ret
`,
			spillSlotSize: 16 * 10,
			clobberedRegs: []regalloc.VReg{v18VReg, v27VReg, x18VReg, x25VReg},
		},
		{
			exp: `
	add sp, sp, #0x10
	add sp, sp, #0xa0
	ldr x25, [sp], #0x10
	ldr x18, [sp], #0x10
	ldr q27, [sp], #0x10
	ldr q18, [sp], #0x10
	ldr x30, [sp], #0x10
	add sp, sp, #0x150
	ret
`,
			spillSlotSize: 16 * 10,
			abi:           backend.FunctionABI{ArgStackSize: 16, RetStackSize: 320},
			clobberedRegs: []regalloc.VReg{v18VReg, v27VReg, x18VReg, x25VReg},
		},
	} {
		tc := tc
		t.Run(tc.exp, func(t *testing.T) {
			_, _, m := newSetupWithMockContext()
			m.spillSlotSize = tc.spillSlotSize
			m.clobberedRegs = tc.clobberedRegs
			m.currentABI = &tc.abi

			root := m.allocateNop()
			m.rootInstr = root
			ret := m.allocateInstr()
			ret.asRet()
			root.next = ret
			ret.prev = root
			m.postRegAlloc()

			require.Equal(t, root, m.rootInstr)
			err := m.Encode(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.exp, m.Format())
		})
	}
}

func TestMachine_insertStackBoundsCheck(t *testing.T) {
	for _, tc := range []struct {
		exp               string
		requiredStackSize int64
	}{
		{
			requiredStackSize: 0xfff_0,
			exp: `
	movz x27, #0xfff0, lsl 0
	sub x27, sp, x27
	ldr x11, [x0, #0x28]
	subs xzr, x27, x11
	b.ge #0x14
	movz x27, #0xfff0, lsl 0
	str x27, [x0, #0x40]
	ldr x27, [x0, #0x50]
	bl x27
`,
		},
		{
			requiredStackSize: 0x10,
			exp: `
	sub x27, sp, #0x10
	ldr x11, [x0, #0x28]
	subs xzr, x27, x11
	b.ge #0x14
	orr x27, xzr, #0x10
	str x27, [x0, #0x40]
	ldr x27, [x0, #0x50]
	bl x27
`,
		},
	} {
		tc := tc
		t.Run(tc.exp, func(t *testing.T) {
			_, _, m := newSetupWithMockContext()
			m.rootInstr = m.allocateInstr()
			m.rootInstr.asNop0()
			m.insertStackBoundsCheck(tc.requiredStackSize, m.rootInstr)
			err := m.Encode(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.exp, m.Format())
		})
	}
}

func TestMachine_CompileStackGrowCallSequence(t *testing.T) {
	_, _, m := newSetupWithMockContext()
	_ = m.CompileStackGrowCallSequence()

	require.Equal(t, `
	stp x1, x2, [x0, #0x60]
	stp x3, x4, [x0, #0x70]
	stp x5, x6, [x0, #0x80]
	stp x7, x19, [x0, #0x90]
	stp x20, x21, [x0, #0xa0]
	stp x22, x23, [x0, #0xb0]
	stp x24, x25, [x0, #0xc0]
	stp x26, x28, [x0, #0xd0]
	str x30, [x0, #0xe0]
	stp q0, q1, [x0, #0xf0]
	stp q2, q3, [x0, #0x110]
	stp q4, q5, [x0, #0x130]
	stp q6, q7, [x0, #0x150]
	stp q18, q19, [x0, #0x170]
	stp q20, q21, [x0, #0x190]
	stp q22, q23, [x0, #0x1b0]
	stp q24, q25, [x0, #0x1d0]
	stp q26, q27, [x0, #0x1f0]
	stp q28, q29, [x0, #0x210]
	stp q30, q31, [x0, #0x230]
	mov x27, sp
	str x27, [x0, #0x38]
	orr w17, wzr, #0x1
	str w17, [x0]
	adr x27, #0x20
	str x27, [x0, #0x30]
	exit_sequence x0
	ldp x1, x2, [x0, #0x60]
	ldp x3, x4, [x0, #0x70]
	ldp x5, x6, [x0, #0x80]
	ldp x7, x19, [x0, #0x90]
	ldp x20, x21, [x0, #0xa0]
	ldp x22, x23, [x0, #0xb0]
	ldp x24, x25, [x0, #0xc0]
	ldp x26, x28, [x0, #0xd0]
	ldr x30, [x0, #0xe0]
	ldp q0, q1, [x0, #0xf0]
	ldp q2, q3, [x0, #0x110]
	ldp q4, q5, [x0, #0x130]
	ldp q6, q7, [x0, #0x150]
	ldp q18, q19, [x0, #0x170]
	ldp q20, q21, [x0, #0x190]
	ldp q22, q23, [x0, #0x1b0]
	ldp q24, q25, [x0, #0x1d0]
	ldp q26, q27, [x0, #0x1f0]
	ldp q28, q29, [x0, #0x210]
	ldp q30, q31, [x0, #0x230]
	ret
`, m.Format())
}

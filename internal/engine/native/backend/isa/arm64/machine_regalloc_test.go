package arm64

import (
	"encoding/hex"
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestRegAllocFunctionImpl_ReloadRegisterAfter(t *testing.T) {
	ctx, _, m := newSetupWithMockContext()

	ctx.typeOf = map[regalloc.VRegID]ssa.Type{x1VReg.ID(): ssa.TypeI64, v1VReg.ID(): ssa.TypeF64}
	i1, i2 := m.allocateNop(), m.allocateNop()
	i1.next = i2
	i2.prev = i1

	m.insertReloadRegisterAt(x1VReg, i1, true)
	m.insertReloadRegisterAt(v1VReg, i1, true)

	require.NotEqual(t, i1, i2.prev)
	require.NotEqual(t, i1.next, i2)
	fload, iload := i1.next, i1.next.next
	require.Equal(t, fload.prev, i1)
	require.Equal(t, i1, fload.prev)
	require.Equal(t, iload.next, i2)
	require.Equal(t, iload, i2.prev)

	require.Equal(t, iload.kind, uLoad64)
	require.Equal(t, fload.kind, fpuLoad64)

	m.rootInstr = i1
	require.Equal(t, `
	ldr d1, [sp, #0x18]
	ldr x1, [sp, #0x10]
`, m.Format())
}

func TestRegAllocFunctionImpl_StoreRegisterBefore(t *testing.T) {
	ctx, _, m := newSetupWithMockContext()

	ctx.typeOf = map[regalloc.VRegID]ssa.Type{x1VReg.ID(): ssa.TypeI64, v1VReg.ID(): ssa.TypeF64}
	i1, i2 := m.allocateNop(), m.allocateNop()
	i1.next = i2
	i2.prev = i1

	m.insertStoreRegisterAt(x1VReg, i2, false)
	m.insertStoreRegisterAt(v1VReg, i2, false)

	require.NotEqual(t, i1, i2.prev)
	require.NotEqual(t, i1.next, i2)
	iload, fload := i1.next, i1.next.next
	require.Equal(t, iload.prev, i1)
	require.Equal(t, i1, iload.prev)
	require.Equal(t, fload.next, i2)
	require.Equal(t, fload, i2.prev)

	require.Equal(t, iload.kind, store64)
	require.Equal(t, fload.kind, fpuStore64)

	m.rootInstr = i1
	require.Equal(t, `
	str x1, [sp, #0x10]
	str d1, [sp, #0x18]
`, m.Format())
}

func TestMachine_insertStoreRegisterAt(t *testing.T) {
	for _, tc := range []struct {
		spillSlotSize int64
		expected      string
	}{
		{
			spillSlotSize: 0,
			expected: `
	udf
	str x1, [sp, #0x10]
	str d1, [sp, #0x18]
	exit_sequence x30
`,
		},
		{
			spillSlotSize: 0xffff,
			expected: `
	udf
	movz x27, #0x10, lsl 0
	movk x27, #0x1, lsl 16
	str x1, [sp, x27]
	movz x27, #0x18, lsl 0
	movk x27, #0x1, lsl 16
	str d1, [sp, x27]
	exit_sequence x30
`,
		},
		{
			spillSlotSize: 0xffff_00,
			expected: `
	udf
	movz x27, #0xff10, lsl 0
	movk x27, #0xff, lsl 16
	str x1, [sp, x27]
	movz x27, #0xff18, lsl 0
	movk x27, #0xff, lsl 16
	str d1, [sp, x27]
	exit_sequence x30
`,
		},
	} {
		t.Run(tc.expected, func(t *testing.T) {
			ctx, _, m := newSetupWithMockContext()
			m.spillSlotSize = tc.spillSlotSize

			for _, after := range []bool{false, true} {
				var name string
				if after {
					name = "after"
				} else {
					name = "before"
				}
				t.Run(name, func(t *testing.T) {
					ctx.typeOf = map[regalloc.VRegID]ssa.Type{x1VReg.ID(): ssa.TypeI64, v1VReg.ID(): ssa.TypeF64}
					i1, i2 := m.allocateInstr().asUDF(), m.allocateInstr().asExitSequence(x30VReg)
					i1.next = i2
					i2.prev = i1

					if after {
						m.insertStoreRegisterAt(v1VReg, i1, after)
						m.insertStoreRegisterAt(x1VReg, i1, after)
					} else {
						m.insertStoreRegisterAt(x1VReg, i2, after)
						m.insertStoreRegisterAt(v1VReg, i2, after)
					}
					m.rootInstr = i1
					require.Equal(t, tc.expected, m.Format())
				})
			}
		})
	}
}

func TestMachine_insertReloadRegisterAt(t *testing.T) {
	for _, tc := range []struct {
		spillSlotSize int64
		expected      string
	}{
		{
			spillSlotSize: 0,
			expected: `
	udf
	ldr x1, [sp, #0x10]
	ldr d1, [sp, #0x18]
	exit_sequence x30
`,
		},
		{
			spillSlotSize: 0xffff,
			expected: `
	udf
	movz x27, #0x10, lsl 0
	movk x27, #0x1, lsl 16
	ldr x1, [sp, x27]
	movz x27, #0x18, lsl 0
	movk x27, #0x1, lsl 16
	ldr d1, [sp, x27]
	exit_sequence x30
`,
		},
		{
			spillSlotSize: 0xffff_00,
			expected: `
	udf
	movz x27, #0xff10, lsl 0
	movk x27, #0xff, lsl 16
	ldr x1, [sp, x27]
	movz x27, #0xff18, lsl 0
	movk x27, #0xff, lsl 16
	ldr d1, [sp, x27]
	exit_sequence x30
`,
		},
	} {
		t.Run(tc.expected, func(t *testing.T) {
			ctx, _, m := newSetupWithMockContext()
			m.spillSlotSize = tc.spillSlotSize

			for _, after := range []bool{false, true} {
				var name string
				if after {
					name = "after"
				} else {
					name = "before"
				}
				t.Run(name, func(t *testing.T) {
					ctx.typeOf = map[regalloc.VRegID]ssa.Type{x1VReg.ID(): ssa.TypeI64, v1VReg.ID(): ssa.TypeF64}
					i1, i2 := m.allocateInstr().asUDF(), m.allocateInstr().asExitSequence(x30VReg)
					i1.next = i2
					i2.prev = i1

					if after {
						m.insertReloadRegisterAt(v1VReg, i1, after)
						m.insertReloadRegisterAt(x1VReg, i1, after)
					} else {
						m.insertReloadRegisterAt(x1VReg, i2, after)
						m.insertReloadRegisterAt(v1VReg, i2, after)
					}
					m.rootInstr = i1

					require.Equal(t, tc.expected, m.Format())
				})
			}
		})
	}
}

func TestRegMachine_ClobberedRegisters(t *testing.T) {
	_, _, m := newSetupWithMockContext()
	m.regAllocFn.ClobberedRegisters([]regalloc.VReg{v19VReg, v19VReg, v19VReg, v19VReg})
	require.Equal(t, []regalloc.VReg{v19VReg, v19VReg, v19VReg, v19VReg}, m.clobberedRegs)
}

func TestMachineMachineswap(t *testing.T) {
	for _, tc := range []struct {
		x1, x2, tmp regalloc.VReg
		expected    string
	}{
		{
			x1:  x18VReg,
			x2:  x19VReg,
			tmp: x20VReg,
			expected: `
	udf
	mov x20, x18
	mov x18, x19
	mov x19, x20
	exit_sequence x30
`,
		},
		{
			x1: x18VReg,
			x2: x19VReg,
			// Tmp not given.
			expected: `
	udf
	mov x27, x18
	mov x18, x19
	mov x19, x27
	exit_sequence x30
`,
		},
		{
			x1:  v18VReg,
			x2:  v19VReg,
			tmp: v11VReg,
			expected: `
	udf
	mov v11.16b, v18.16b
	mov v18.16b, v19.16b
	mov v19.16b, v11.16b
	exit_sequence x30
`,
		},
		{
			x1: v18VReg,
			x2: v19VReg,
			// Tmp not given.
			expected: `
	udf
	str d18, [sp, #0x10]
	mov v18.16b, v19.16b
	ldr d19, [sp, #0x10]
	exit_sequence x30
`,
		},
	} {
		t.Run(tc.expected, func(t *testing.T) {
			ctx, _, m := newSetupWithMockContext()

			ctx.typeOf = map[regalloc.VRegID]ssa.Type{
				x18VReg.ID(): ssa.TypeI64, x19VReg.ID(): ssa.TypeI64,
				v18VReg.ID(): ssa.TypeF64, v19VReg.ID(): ssa.TypeF64,
			}
			cur, i2 := m.allocateInstr().asUDF(), m.allocateInstr().asExitSequence(x30VReg)
			cur.next = i2
			i2.prev = cur

			m.swap(cur, tc.x1, tc.x2, tc.tmp)
			m.rootInstr = cur

			require.Equal(t, tc.expected, m.Format())
		})
	}
}

// TestMachine_pairSpillAccesses covers the STP/LDP folding of neighbouring spill
// accesses. The expected encoding is "Load/store register pair (signed offset)"
// from the Arm ARM: the immediate is the low slot's offset in units of 8 bytes,
// Rt takes the low slot and Rt2 the high one -- so a descending pair has to swap
// the two registers, not the offset.
func TestMachine_pairSpillAccesses(t *testing.T) {
	// spillAccess builds `str/ldr <r>, [<base>, #off]` as the spill helpers emit it.
	spillAccess := func(m *machine, load bool, r, base regalloc.VReg, off int64) *instruction {
		amode := m.amodePool.Allocate()
		*amode = addressMode{kind: addressModeKindRegUnsignedImm12, rn: base, imm: off}
		i := m.allocateInstr()
		if load {
			i.asULoad(r, amode, 64)
		} else {
			i.asStore(operandNR(r), amode, 64)
		}
		return i
	}
	chain := func(m *machine, instrs ...*instruction) {
		for i := 0; i < len(instrs)-1; i++ {
			instrs[i].next = instrs[i+1]
			instrs[i+1].prev = instrs[i]
		}
		m.rootInstr = instrs[0]
	}

	for _, tc := range []struct {
		name  string
		build func(m *machine) []*instruction
		exp   string
	}{
		{
			name: "ascending stores",
			build: func(m *machine) []*instruction {
				return []*instruction{
					spillAccess(m, false, x1VReg, spVReg, 16),
					spillAccess(m, false, x2VReg, spVReg, 24),
				}
			},
			exp: "\n\tstp x1, x2, [sp, #0x10]\n",
		},
		{
			name: "descending stores swap the registers",
			build: func(m *machine) []*instruction {
				return []*instruction{
					spillAccess(m, false, x1VReg, spVReg, 24),
					spillAccess(m, false, x2VReg, spVReg, 16),
				}
			},
			exp: "\n\tstp x2, x1, [sp, #0x10]\n",
		},
		{
			name: "ascending reloads",
			build: func(m *machine) []*instruction {
				return []*instruction{
					spillAccess(m, true, x1VReg, spVReg, 16),
					spillAccess(m, true, x2VReg, spVReg, 24),
				}
			},
			exp: "\n\tldp x1, x2, [sp, #0x10]\n",
		},
		{
			name: "three in a row pair greedily",
			build: func(m *machine) []*instruction {
				return []*instruction{
					spillAccess(m, false, x1VReg, spVReg, 16),
					spillAccess(m, false, x2VReg, spVReg, 24),
					spillAccess(m, false, x3VReg, spVReg, 32),
				}
			},
			exp: "\n\tstp x1, x2, [sp, #0x10]\n\tstr x3, [sp, #0x20]\n",
		},
		{
			name: "non-neighbouring slots",
			build: func(m *machine) []*instruction {
				return []*instruction{
					spillAccess(m, false, x1VReg, spVReg, 16),
					spillAccess(m, false, x2VReg, spVReg, 32),
				}
			},
			exp: "\n\tstr x1, [sp, #0x10]\n\tstr x2, [sp, #0x20]\n",
		},
		{
			name: "a store and a reload",
			build: func(m *machine) []*instruction {
				return []*instruction{
					spillAccess(m, false, x1VReg, spVReg, 16),
					spillAccess(m, true, x2VReg, spVReg, 24),
				}
			},
			exp: "\n\tstr x1, [sp, #0x10]\n\tldr x2, [sp, #0x18]\n",
		},
		{
			// LDP with two identical destinations is CONSTRAINED UNPREDICTABLE.
			name: "the same register twice",
			build: func(m *machine) []*instruction {
				return []*instruction{
					spillAccess(m, true, x1VReg, spVReg, 16),
					spillAccess(m, true, x1VReg, spVReg, 24),
				}
			},
			exp: "\n\tldr x1, [sp, #0x10]\n\tldr x1, [sp, #0x18]\n",
		},
		{
			// imm7 is signed and scaled by 8, so 64*8 is one slot past the top.
			name: "offset out of the imm7 range",
			build: func(m *machine) []*instruction {
				return []*instruction{
					spillAccess(m, false, x1VReg, spVReg, 64*8),
					spillAccess(m, false, x2VReg, spVReg, 64*8+8),
				}
			},
			exp: "\n\tstr x1, [sp, #0x200]\n\tstr x2, [sp, #0x208]\n",
		},
		{
			name: "a base other than SP",
			build: func(m *machine) []*instruction {
				return []*instruction{
					spillAccess(m, false, x1VReg, x9VReg, 16),
					spillAccess(m, false, x2VReg, x9VReg, 24),
				}
			},
			exp: "\n\tstr x1, [x9, #0x10]\n\tstr x2, [x9, #0x18]\n",
		},
		{
			// A block boundary is a nop0 (see StartBlock/EndBlock), so it breaks the
			// run and keeps a spill from moving across a control-flow edge.
			name: "separated by a block bracket",
			build: func(m *machine) []*instruction {
				return []*instruction{
					spillAccess(m, false, x1VReg, spVReg, 16),
					m.allocateNop(),
					spillAccess(m, false, x2VReg, spVReg, 24),
				}
			},
			exp: "\n\tstr x1, [sp, #0x10]\n\tstr x2, [sp, #0x18]\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, m := newSetupWithMockContext()
			chain(m, tc.build(m)...)
			m.pairSpillAccesses()
			require.Equal(t, tc.exp, m.Format())
		})
	}

	t.Run("encoding", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			load bool
			exp  string
		}{
			{name: "stp", exp: "e10b01a9"},
			{name: "ldp", load: true, exp: "e10b41a9"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, _, m := newSetupWithMockContext()
				chain(m,
					spillAccess(m, tc.load, x1VReg, spVReg, 16),
					spillAccess(m, tc.load, x2VReg, spVReg, 24),
				)
				m.pairSpillAccesses()
				m.encode(m.rootInstr)
				require.Equal(t, tc.exp, hex.EncodeToString(m.compiler.Buf()))
			})
		}
	})
}

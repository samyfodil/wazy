package amd64

import (
	"fmt"
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/testing/require"
)

var gpRegs = [...]regalloc.RealReg{
	rax, rcx, rdx, rbx, rsp, rbp, rsi, rdi,
	r8, r9, r10, r11, r12, r13, r14, r15,
}

// TestExecCtxLoad64LenMatchesEncoder pins execCtxLoad64Len against the encoder
// it predicts. jccBranchLen uses it to place the RET at the end of an exit
// sequence, so if the two ever drift the workaround goes on silently producing
// code while quietly leaving that RET where the erratum bites -- which is the
// exact failure mode the whole change exists to remove.
func TestExecCtxLoad64LenMatchesEncoder(t *testing.T) {
	offsets := [...]nativeapi.Offset{
		nativeapi.ExecutionContextOffsetOriginalFramePointer,
		nativeapi.ExecutionContextOffsetOriginalStackPointer,
	}
	for _, off := range offsets {
		for _, base := range gpRegs {
			for _, dst := range [...]regalloc.RealReg{rbp, rsp} {
				t.Run(fmt.Sprintf("off=%d/base=%d/dst=%d", off, base, dst), func(t *testing.T) {
					c, _, _ := newSetupWithMockContext()
					am := amode{
						kindWithShift: uint32(amodeImmReg),
						base:          regalloc.FromRealReg(base, regalloc.RegTypeInt),
						imm32:         off.U32(),
					}
					encodeLoad64(c, &am, dst)
					require.Equal(t, execCtxLoad64Len(base), int64(len(c.buf)))
				})
			}
		}
	}
}

// TestExitSequenceLengthWithinBound checks the other half of the same claim: the
// whole exit sequence really is no longer than jccBranchLen says, for every
// execution-context register it could be handed.
func TestExitSequenceLengthWithinBound(t *testing.T) {
	for _, base := range gpRegs {
		t.Run(fmt.Sprintf("base=%d", base), func(t *testing.T) {
			c, _, m := newSetupWithMockContext()
			i := m.allocateExitSeq(regalloc.FromRealReg(base, regalloc.RegTypeInt))
			bound := jccBranchLen(i)
			i.encode(c)
			require.Equal(t, bound, int64(len(c.buf)),
				"exit sequence for base %d encoded to %d bytes, jccBranchLen said %d", base, len(c.buf), bound)
		})
	}
}

// Test_jccBranchLen_branchKinds pins the set of instruction kinds the workaround
// treats as jumps. Adding a new branch-emitting kind to the backend without
// adding it here leaves it unpadded, so the list is spelled out rather than
// derived.
func Test_jccBranchLen_branchKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		i    instruction
		exp  int64
	}{
		{"jmp label", instruction{kind: jmp, op1: newOperandLabel(1)}, 5},
		{"jmp imm32", instruction{kind: jmp, op1: newOperandImm32(0)}, 5},
		{"jmp reg", instruction{kind: jmp, op1: newOperandReg(raxVReg)}, 3},
		{"jmpIf", instruction{kind: jmpIf, op1: newOperandLabel(1)}, 6},
		{"call", instruction{kind: call}, 5},
		{"callIndirect reg", instruction{kind: callIndirect, op1: newOperandReg(raxVReg)}, 3},
		{"tailCall", instruction{kind: tailCall}, 5},
		{"tailCallIndirect", instruction{kind: tailCallIndirect, op1: newOperandReg(r11VReg)}, 3},
		{"ret", instruction{kind: ret}, 1},

		// Not jumps: UD2 faults rather than branching, a jump-table island is
		// data, and a nop0/sourceOffsetInfo emits nothing at all.
		{"ud2", instruction{kind: ud2}, 0},
		{"jmpTableIsland", instruction{kind: jmpTableIsland}, 0},
		{"nop0", instruction{kind: nop0}, 0},
		{"sourceOffsetInfo", instruction{kind: sourceOffsetInfo}, 0},
		{"movRR", instruction{kind: movRR}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.exp, jccBranchLen(&tc.i))
		})
	}
}

// Test_jccBranchLen_isUpperBound encodes each fixed-form branch and checks the
// bound really bounds it. An under-estimate would be invisible at runtime.
func Test_jccBranchLen_isUpperBound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(m *machine) *instruction
	}{
		{"jmp label", func(m *machine) *instruction { return m.allocateInstr().asJmp(newOperandLabel(1)) }},
		{"jmpIf", func(m *machine) *instruction { return m.allocateInstr().asJmpIf(condNZ, newOperandLabel(1)) }},
		{"ret", func(m *machine) *instruction { return m.allocateInstr().asRet() }},
		{"ud2", func(m *machine) *instruction { i := m.allocateInstr(); i.asUD2(); return i }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, m := newSetupWithMockContext()
			i := tc.build(m)
			bound := jccBranchLen(i)
			i.encode(c)
			if bound == 0 {
				return // Not classified as a branch; nothing is claimed.
			}
			require.True(t, int64(len(c.buf)) <= bound,
				"%s encoded to %d bytes, bound was %d", tc.name, len(c.buf), bound)
		})
	}
}

// Test_jmpReg_and_callIndirect_withinBound covers the variable-length indirect
// forms over every register, including the r8-r15 half that needs a REX byte.
func Test_jmpReg_and_callIndirect_withinBound(t *testing.T) {
	for _, base := range gpRegs {
		v := regalloc.FromRealReg(base, regalloc.RegTypeInt)
		for _, tc := range []struct {
			name string
			i    instruction
		}{
			{"jmp reg", instruction{kind: jmp, op1: newOperandReg(v)}},
			{"callIndirect reg", instruction{kind: callIndirect, op1: newOperandReg(v)}},
			{"tailCallIndirect", instruction{kind: tailCallIndirect, op1: newOperandReg(v)}},
		} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, base), func(t *testing.T) {
				c, _, _ := newSetupWithMockContext()
				bound := jccBranchLen(&tc.i)
				tc.i.encode(c)
				require.True(t, int64(len(c.buf)) <= bound,
					"%s encoded to %d bytes, bound was %d", tc.name, len(c.buf), bound)
			})
		}
	}
}

// Test_jccErratumPad walks every start offset in a window for a branch of every
// length the backend emits, and asserts the post-pad placement is always legal
// and the pad is never larger than it has to be.
func Test_jccErratumPad(t *testing.T) {
	for _, n := range []int64{1, 3, 5, 6, 8, 9, 11} {
		for off := int64(0); off < 128; off++ {
			c, _, _ := newSetupWithMockContext()
			c.buf = make([]byte, off)

			i := instruction{kind: jmpIf, op1: newOperandLabel(1)}
			padTo(&i, n)
			jccErratumPad(c, off, &i)

			pad := int64(len(c.buf)) - off
			start := off + pad
			end := start + n
			require.True(t, start/32 == (end-1)/32 && end%32 != 0,
				"n=%d off=%d: pad %d left the branch at [%d,%d)", n, off, pad, start, end)
			require.True(t, pad < 32, "n=%d off=%d: pad %d is larger than a window", n, off, pad)
			// Nothing is moved that did not have to be.
			if off/32 == (off+n-1)/32 && (off+n)%32 != 0 {
				require.Zero(t, pad, "n=%d off=%d: padded a branch that was already legal", n, off)
			} else {
				require.Equal(t, 32-off%32, pad, "n=%d off=%d: pad should reach exactly the next window", n, off)
			}
		}
	}
}

// padTo rewrites i so that jccBranchLen(i) == n, for the offsets sweep above.
func padTo(i *instruction, n int64) {
	switch n {
	case 1:
		i.kind, i.op1 = ret, operand{}
	case 3:
		i.kind, i.op1 = jmp, newOperandReg(raxVReg)
	case 5:
		i.kind, i.op1 = jmp, newOperandLabel(1)
	case 6:
		i.kind, i.op1 = jmpIf, newOperandLabel(1)
	case 8:
		i.kind, i.op1 = call, operand{}
		// call is 5; force 8 by using the memory-operand jmp form instead.
		i.kind, i.op1 = jmp, newOperandMem(&amode{kindWithShift: uint32(amodeImmReg), base: raxVReg, imm32: 0x1000})
	case 9, 11:
		// Exit-sequence lengths: 9 for a base needing no SIB, 11 for rsp/r12.
		base := raxVReg
		if n == 11 {
			base = rspVReg
		}
		i.kind, i.op1, i.op2 = exitSequence, newOperandReg(base), newOperandMem(&amode{})
	default:
		panic("unhandled length")
	}
	if got := jccBranchLen(i); got != n {
		panic(fmt.Sprintf("padTo(%d) produced length %d", n, got))
	}
}

// Test_emitNops asserts the padding is exactly the requested number of bytes and
// is made of real NOPs, for every size a pad can be.
func Test_emitNops(t *testing.T) {
	for n := 1; n <= 64; n++ {
		c, _, _ := newSetupWithMockContext()
		emitNops(c, n)
		require.Equal(t, n, len(c.buf), "emitNops(%d) emitted %d bytes", n, len(c.buf))

		// Every byte must belong to one of the canonical encodings, in order.
		i := 0
		for i < len(c.buf) {
			matched := false
			for k := len(canonicalNops); k >= 1; k-- {
				enc := canonicalNops[k-1]
				if i+len(enc) <= len(c.buf) && string(c.buf[i:i+len(enc)]) == string(enc) {
					i += len(enc)
					matched = true
					break
				}
			}
			require.True(t, matched, "emitNops(%d) produced a non-NOP byte at %d: % x", n, i, c.buf)
		}
	}
}

func Test_canonicalNops_lengths(t *testing.T) {
	for k, enc := range canonicalNops {
		require.Equal(t, k+1, len(enc), "canonicalNops[%d] should be %d bytes", k, k+1)
	}
}

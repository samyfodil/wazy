package amd64

import (
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestMachine_foldReloads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(m *machine, slot func(uint32) operand) []*instruction
		exp   string
	}{
		{
			name: "overwritten after",
			setup: func(m *machine, slot func(uint32) operand) []*instruction {
				return []*instruction{
					m.allocateInstr().asMovzxRmR(extModeLQ, slot(16), rcxVReg),
					m.allocateInstr().asAluRmiR(aluRmiROpcodeAdd, newOperandReg(rcxVReg), raxVReg, false),
					m.allocateInstr().asMovRR(rdxVReg, rcxVReg, false),
				}
			},
			exp: "add 16(%rsp), %eax\nmovl %edx, %ecx",
		},
		{
			name: "read after",
			setup: func(m *machine, slot func(uint32) operand) []*instruction {
				return []*instruction{
					m.allocateInstr().asMovzxRmR(extModeLQ, slot(16), rcxVReg),
					m.allocateInstr().asAluRmiR(aluRmiROpcodeAdd, newOperandReg(rcxVReg), raxVReg, false),
					m.allocateInstr().asAluRmiR(aluRmiROpcodeSub, newOperandReg(rcxVReg), rbxVReg, false),
					m.allocateInstr().asMovRR(rdxVReg, rcxVReg, false),
				}
			},
			exp: "movzx.lq 16(%rsp), %rcx\nadd %ecx, %eax\nsub %ecx, %ebx\nmovl %edx, %ecx",
		},
		{
			name: "block ends first",
			setup: func(m *machine, slot func(uint32) operand) []*instruction {
				return []*instruction{
					m.allocateInstr().asMovzxRmR(extModeLQ, slot(16), rcxVReg),
					m.allocateInstr().asCmpRmiR(true, newOperandReg(rcxVReg), raxVReg, false),
				}
			},
			exp: "movzx.lq 16(%rsp), %rcx\ncmpl %ecx, %eax",
		},
		{
			name: "64-bit read of a 32-bit slot",
			setup: func(m *machine, slot func(uint32) operand) []*instruction {
				return []*instruction{
					m.allocateInstr().asMovzxRmR(extModeLQ, slot(16), rcxVReg),
					m.allocateInstr().asAluRmiR(aluRmiROpcodeAdd, newOperandReg(rcxVReg), raxVReg, true),
					m.allocateInstr().asMovRR(rdxVReg, rcxVReg, true),
				}
			},
			exp: "movzx.lq 16(%rsp), %rcx\nadd %rcx, %rax\nmovq %rdx, %rcx",
		},
		{
			name: "32-bit read of a 64-bit slot",
			setup: func(m *machine, slot func(uint32) operand) []*instruction {
				return []*instruction{
					m.allocateInstr().asMov64MR(slot(24), rcxVReg),
					m.allocateInstr().asAluRmiR(aluRmiROpcodeXor, newOperandReg(rcxVReg), raxVReg, false),
					m.allocateInstr().asImm(rcxVReg, 1, false),
				}
			},
			exp: "xor 24(%rsp), %eax\nmovl $1, %ecx",
		},
		{
			name: "multiplication into the reloaded register",
			setup: func(m *machine, slot func(uint32) operand) []*instruction {
				return []*instruction{
					m.allocateInstr().asMovzxRmR(extModeLQ, slot(32), r15VReg),
					m.allocateInstr().asImulImm(newOperandReg(r15VReg), 7, r15VReg, false),
				}
			},
			exp: "imul $7, 32(%rsp), %r15d",
		},
		{
			name: "a partial write does not end the value",
			setup: func(m *machine, slot func(uint32) operand) []*instruction {
				return []*instruction{
					m.allocateInstr().asMovzxRmR(extModeLQ, slot(16), rcxVReg),
					m.allocateInstr().asAluRmiR(aluRmiROpcodeAnd, newOperandReg(rcxVReg), raxVReg, false),
					m.allocateInstr().asSetcc(condNZ, rcxVReg),
				}
			},
			exp: "movzx.lq 16(%rsp), %rcx\nand %ecx, %eax\nsetnz %rcx",
		},
		{
			name: "scalar float",
			setup: func(m *machine, slot func(uint32) operand) []*instruction {
				return []*instruction{
					m.allocateInstr().asXmmUnaryRmR(sseOpcodeMovsd, slot(8), xmm1VReg),
					m.allocateInstr().asXmmRmR(sseOpcodeMulsd, newOperandReg(xmm1VReg), xmm0VReg),
					m.allocateInstr().asXmmUnaryRmR(sseOpcodeMovsd, slot(40), xmm1VReg),
				}
			},
			exp: "mulsd 8(%rsp), %xmm0\nmovsd 40(%rsp), %xmm1",
		},
		{
			name: "single precision from a double's slot",
			setup: func(m *machine, slot func(uint32) operand) []*instruction {
				return []*instruction{
					m.allocateInstr().asXmmUnaryRmR(sseOpcodeMovsd, slot(8), xmm1VReg),
					m.allocateInstr().asXmmRmR(sseOpcodeMulss, newOperandReg(xmm1VReg), xmm0VReg),
					m.allocateInstr().asXmmUnaryRmR(sseOpcodeMovsd, slot(40), xmm1VReg),
				}
			},
			exp: "movsd 8(%rsp), %xmm1\nmulss %xmm1, %xmm0\nmovsd 40(%rsp), %xmm1",
		},
		{
			name: "three-operand float reading the register twice",
			setup: func(m *machine, slot func(uint32) operand) []*instruction {
				return []*instruction{
					m.allocateInstr().asXmmUnaryRmR(sseOpcodeMovss, slot(8), xmm1VReg),
					m.allocateInstr().asXmmRmRVex(sseOpcodeAddss, newOperandReg(xmm1VReg), xmm1VReg, xmm1VReg),
				}
			},
			exp: "movss 8(%rsp), %xmm1\nvaddss %xmm1, %xmm1, %xmm1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, m := newSetupWithMockContext()
			cur := m.allocateInstr().asUD2()
			m.rootInstr = cur
			slot := func(off uint32) operand { return newOperandMem(m.newAmodeImmReg(off, rspVReg)) }
			for _, i := range tc.setup(m, slot) {
				cur = linkInstr(cur, i)
			}
			linkInstr(cur, m.allocateInstr().asRet())
			m.foldReloads()
			var got []string
			for _, l := range strings.Split(strings.TrimSpace(m.Format()), "\n") {
				got = append(got, strings.TrimSpace(l))
			}
			require.Equal(t, "ud2\n"+tc.exp+"\nret", strings.Join(got, "\n"))
		})
	}
}

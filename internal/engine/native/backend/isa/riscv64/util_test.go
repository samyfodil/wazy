package riscv64

import (
	"context"
	"strings"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// The lowering-test scaffolding amd64 and arm64 both carry, ported here. It
// exists because a lowering decision is not always visible in the machine code
// a golden would capture -- whether a conditional trap shares an island or
// keeps its exit inline changes which address the backtracer sees, not what the
// instructions compute.
//
// The one difference from the other two: AllocateVReg puts a v128 in
// regalloc.RegTypeVec, which is where this backend keeps them.

func formatEmittedInstructionsInCurrentBlock(m *machine) string {
	m.FlushPendingInstructions()
	var strs []string
	for cur := m.perBlockHead; cur != nil; cur = cur.next {
		strs = append(strs, cur.String())
	}
	return strings.Join(strs, "\n")
}

func newSetupWithMockContext() (*mockCompiler, ssa.Builder, *machine) {
	ctx := newMockCompilationContext()
	m := NewBackend().(*machine)
	m.SetCompiler(ctx)
	ssaB := ssa.NewBuilder()
	ctx.ssaBuilder = ssaB
	blk := ssaB.AllocateBasicBlock()
	ssaB.SetCurrentBlock(blk)
	return ctx, ssaB, m
}

func intToVReg(i int) regalloc.VReg {
	return regalloc.VReg(i).SetRegType(regalloc.RegTypeInt)
}

// mockCompiler implements backend.Compiler for testing.
type mockCompiler struct {
	currentGID  ssa.InstructionGroupID
	vRegCounter int
	vRegMap     map[ssa.Value]regalloc.VReg
	definitions map[ssa.Value]backend.SSAValueDefinition
	sigs        map[ssa.SignatureID]*ssa.Signature
	typeOf      map[regalloc.VRegID]ssa.Type
	ssaBuilder  ssa.Builder
	relocs      []backend.RelocationInfo
	buf         []byte
}

func newMockCompilationContext() *mockCompiler {
	return &mockCompiler{
		vRegMap:     make(map[ssa.Value]regalloc.VReg),
		definitions: make(map[ssa.Value]backend.SSAValueDefinition),
		typeOf:      map[regalloc.VRegID]ssa.Type{},
	}
}

func (m *mockCompiler) BufPtr() *[]byte { return &m.buf }

func (m *mockCompiler) GetFunctionABI(*ssa.Signature) *backend.FunctionABI {
	panic("the lowering paths under test do not reach an ABI")
}

func (m *mockCompiler) SSABuilder() ssa.Builder                             { return m.ssaBuilder }
func (m *mockCompiler) LoopNestingForestRoots() []ssa.BasicBlock            { panic("not needed") }
func (m *mockCompiler) SourceOffsetInfo() []backend.SourceOffsetInfo        { return nil }
func (m *mockCompiler) AddSourceOffsetInfo(int64, ssa.SourceOffset)         {}
func (m *mockCompiler) CompiledBlockOffsets() []backend.CompiledBlockOffset { return nil }
func (m *mockCompiler) FrameSize() int64                                    { return 0 }
func (m *mockCompiler) SetHasEHContext(bool)                                {}

func (m *mockCompiler) AddRelocationInfo(funcRef ssa.FuncRef, isTailCall bool) {
	m.relocs = append(m.relocs, backend.RelocationInfo{FuncRef: funcRef, Offset: int64(len(m.buf))})
}

func (m *mockCompiler) Emit4Bytes(b uint32) {
	m.buf = append(m.buf, byte(b), byte(b>>8), byte(b>>16), byte(b>>24))
}

func (m *mockCompiler) EmitByte(b byte) { m.buf = append(m.buf, b) }

func (m *mockCompiler) Emit8Bytes(b uint64) {
	m.buf = append(m.buf, byte(b), byte(b>>8), byte(b>>16), byte(b>>24),
		byte(b>>32), byte(b>>40), byte(b>>48), byte(b>>56))
}

func (m *mockCompiler) Encode()                         {}
func (m *mockCompiler) Buf() []byte                     { return m.buf }
func (m *mockCompiler) TakeBuf() []byte                 { b := m.buf; m.buf = nil; return b }
func (m *mockCompiler) TypeOf(v regalloc.VReg) ssa.Type { return m.typeOf[v.ID()] }
func (m *mockCompiler) Finalize(context.Context) error  { return nil }
func (m *mockCompiler) RegAlloc()                       {}
func (m *mockCompiler) Lower()                          {}
func (m *mockCompiler) Format() string                  { return "" }
func (m *mockCompiler) Init()                           {}
func (m *mockCompiler) InitModule()                     {}

func (m *mockCompiler) ResolveSignature(id ssa.SignatureID) *ssa.Signature { return m.sigs[id] }

func (m *mockCompiler) AllocateVReg(typ ssa.Type) regalloc.VReg {
	m.vRegCounter++
	ret := regalloc.VReg(m.vRegCounter).SetRegType(regalloc.RegTypeOf(typ, regalloc.RegTypeVec))
	m.typeOf[ret.ID()] = typ
	return ret
}

func (m *mockCompiler) ValueDefinition(value ssa.Value) backend.SSAValueDefinition {
	return m.definitions[value]
}

func (m *mockCompiler) VRegOf(value ssa.Value) regalloc.VReg {
	vReg, exists := m.vRegMap[value]
	if !exists {
		panic("Value does not exist")
	}
	return vReg
}

func (m *mockCompiler) AliasVReg(value, src ssa.Value) { m.vRegMap[value] = m.VRegOf(src) }

func (m *mockCompiler) MatchInstr(def backend.SSAValueDefinition, opcode ssa.Opcode) bool {
	instr := def.Instr
	return def.IsFromInstr() &&
		instr.Opcode() == opcode &&
		instr.GroupID() == m.currentGID &&
		def.RefCount < 2
}

func (m *mockCompiler) MatchInstrOneOf(def backend.SSAValueDefinition, opcodes []ssa.Opcode) ssa.Opcode {
	for _, opcode := range opcodes {
		if m.MatchInstr(def, opcode) {
			return opcode
		}
	}
	return ssa.OpcodeInvalid
}

func (m *mockCompiler) Compile(context.Context) ([]byte, []backend.RelocationInfo, error) {
	return nil, nil, nil
}

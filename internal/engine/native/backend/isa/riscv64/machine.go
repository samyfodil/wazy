package riscv64

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

type (
	// machine implements backend.Machine for RV64G.
	machine struct {
		compiler   backend.Compiler
		currentABI *backend.FunctionABI
		instrPool  nativeapi.Pool[instruction]
		// labelPositionPool is the pool of labelPosition. Labels below
		// maxSSABlockID are ssa.BasicBlockIDs.
		labelPositionPool nativeapi.IDedPool[labelPosition]

		nextLabel label
		// rootInstr is the first instruction of the function.
		rootInstr *instruction
		// currentLabelPos is the currently-compiled ssa.BasicBlock's labelPosition.
		currentLabelPos *labelPosition
		// orderedSSABlockLabelPos is the ordered list of labelPosition in the
		// generated code for each ssa.BasicBlock.
		orderedSSABlockLabelPos []*labelPosition
		// returnLabelPos is the labelPosition for the return block.
		returnLabelPos labelPosition
		// perBlockHead and perBlockEnd bracket the instruction list of the
		// currently-compiled ssa.BasicBlock.
		perBlockHead, perBlockEnd *instruction
		// pendingInstructions are the instructions not yet emitted into the list.
		pendingInstructions []*instruction
		maxSSABlockID       label

		regAlloc   regalloc.Allocator[*instruction, *labelPosition, *regAllocFn]
		regAllocFn regAllocFn

		amodePool nativeapi.Pool[addressMode]

		unresolvedAddressModes []*instruction

		// jmpTableTargets holds the labels of the jump table targets.
		jmpTableTargets     [][]uint32
		jmpTableTargetsNext int

		// trapIslands are this function's shared conditional-trap exit
		// sequences, one per exit code (see emitTrapIslands).
		trapIslands []trapIsland

		// spillSlotSize is the size in bytes of the spill-slot region. See the
		// frame diagram in machine_pro_epi_logue.go. Multiple of 16.
		spillSlotSize int64
		spillSlots    map[regalloc.VRegID]int64
		// clobberedRegs holds callee-saved registers this function actually
		// writes, saved in the prologue and restored in the epilogue.
		clobberedRegs []regalloc.VReg

		maxRequiredStackSizeForCalls int64
		stackBoundsCheckDisabled     bool
		hasEHContext                 bool

		regAllocStarted bool
	}

	// trapIsland is a shared per-function exit sequence for conditional traps
	// with the given exit code, reachable via the label.
	trapIsland struct {
		code nativeapi.ExitCode
		l    label
	}
)

type (
	// label is a position in the generated code, exactly as in assembly.
	label uint32

	// labelPosition is the region of generated code a label covers.
	// It implements regalloc.Block.
	labelPosition struct {
		// sb is non-nil if this corresponds to an ssa.BasicBlock.
		sb ssa.BasicBlock
		// cur walks the block's instructions during register allocation.
		cur,
		// begin and end are the first and last instructions of the block.
		begin, end *instruction
		// binaryOffset is the offset in the binary where the label sits.
		binaryOffset int64
	}
)

const (
	labelReturn  label = math.MaxUint32
	labelInvalid       = labelReturn - 1
)

// String implements fmt.Stringer.
func (l label) String() string { return fmt.Sprintf("L%d", l) }

func resetLabelPosition(l *labelPosition) { *l = labelPosition{} }

// NewBackend returns a new backend.Machine for RV64G.
func NewBackend() backend.Machine {
	m := &machine{
		spillSlots:        make(map[regalloc.VRegID]int64),
		regAlloc:          regalloc.NewAllocator[*instruction, *labelPosition, *regAllocFn](regInfo),
		amodePool:         nativeapi.NewPool[addressMode](resetAddressMode),
		instrPool:         nativeapi.NewPool[instruction](resetInstruction),
		labelPositionPool: nativeapi.NewIDedPool[labelPosition](resetLabelPosition),
	}
	m.regAllocFn.m = m
	return m
}

func ssaBlockLabel(sb ssa.BasicBlock) label {
	if sb.ReturnBlock() {
		return labelReturn
	}
	return label(sb.ID())
}

func (m *machine) getOrAllocateSSABlockLabelPosition(sb ssa.BasicBlock) *labelPosition {
	if sb.ReturnBlock() {
		m.returnLabelPos.sb = sb
		return &m.returnLabelPos
	}
	l := ssaBlockLabel(sb)
	pos := m.labelPositionPool.GetOrAllocate(int(l))
	pos.sb = sb
	return pos
}

// LinkAdjacentBlocks implements backend.Machine.
func (m *machine) LinkAdjacentBlocks(prev, next ssa.BasicBlock) {
	prevPos, nextPos := m.getOrAllocateSSABlockLabelPosition(prev), m.getOrAllocateSSABlockLabelPosition(next)
	prevPos.end.next = nextPos.begin
}

// StartBlock implements backend.Machine.
func (m *machine) StartBlock(blk ssa.BasicBlock) {
	m.currentLabelPos = m.getOrAllocateSSABlockLabelPosition(blk)
	labelPos := m.currentLabelPos
	end := m.allocateNop()
	m.perBlockHead, m.perBlockEnd = end, end
	labelPos.begin, labelPos.end = end, end
	m.orderedSSABlockLabelPos = append(m.orderedSSABlockLabelPos, labelPos)
}

// EndBlock implements backend.Machine.
func (m *machine) EndBlock() {
	// A nop0 at the head simplifies inserting instructions before everything.
	m.insertAtPerBlockHead(m.allocateNop())
	m.currentLabelPos.begin = m.perBlockHead
	if m.currentLabelPos.sb.EntryBlock() {
		m.rootInstr = m.perBlockHead
	}
}

func (m *machine) insertAtPerBlockHead(i *instruction) {
	if m.perBlockHead == nil {
		m.perBlockHead = i
		m.perBlockEnd = i
		return
	}
	i.next = m.perBlockHead
	m.perBlockHead.prev = i
	m.perBlockHead = i
}

// FlushPendingInstructions implements backend.Machine.
func (m *machine) FlushPendingInstructions() {
	l := len(m.pendingInstructions)
	if l == 0 {
		return
	}
	for i := l - 1; i >= 0; i-- { // reverse: instructions are lowered in reverse order.
		m.insertAtPerBlockHead(m.pendingInstructions[i])
	}
	m.pendingInstructions = m.pendingInstructions[:0]
}

// ehCtxReservedSlotSize is the number of bytes at the bottom of the spill-slot
// region reserved for the fixed execCtx/moduleCtx slots when the function has
// an EH context. Two 8-byte words, keeping the 16-byte frame alignment.
const ehCtxReservedSlotSize = 16

// SetHasEHContext implements backend.Machine.
func (m *machine) SetHasEHContext(v bool) { m.hasEHContext = v }

// EhCtxSlotOffsets implements backend.Machine. Spill offsets are SP-relative
// but sit above the 16-byte frame-size slot, so the two reserved words land at
// [sp+16] and [sp+24], exactly as on arm64.
func (m *machine) EhCtxSlotOffsets() (execCtxOffset, moduleCtxOffset int64) { return 16, 24 }

// RegAlloc implements backend.Machine.
func (m *machine) RegAlloc() {
	if m.hasEHContext {
		m.spillSlotSize = ehCtxReservedSlotSize
	}
	m.regAllocStarted = true
	m.regAlloc.DoAllocation(&m.regAllocFn)
	m.spillSlotSize = (m.spillSlotSize + 15) &^ 15
}

// Reset implements backend.Machine.
func (m *machine) Reset() {
	m.clobberedRegs = m.clobberedRegs[:0]
	for key := range m.spillSlots {
		m.clobberedRegs = append(m.clobberedRegs, regalloc.VReg(key))
	}
	for _, key := range m.clobberedRegs {
		delete(m.spillSlots, regalloc.VRegID(key))
	}
	m.clobberedRegs = m.clobberedRegs[:0]
	m.regAllocStarted = false
	m.hasEHContext = false
	m.regAlloc.Reset()
	m.spillSlotSize = 0
	m.unresolvedAddressModes = m.unresolvedAddressModes[:0]
	m.maxRequiredStackSizeForCalls = 0
	m.jmpTableTargetsNext = 0
	m.amodePool.Reset()
	m.instrPool.Reset()
	m.labelPositionPool.Reset()
	m.pendingInstructions = m.pendingInstructions[:0]
	m.perBlockHead, m.perBlockEnd, m.rootInstr = nil, nil, nil
	m.orderedSSABlockLabelPos = m.orderedSSABlockLabelPos[:0]
	m.trapIslands = m.trapIslands[:0]
}

// getOrCreateTrapIsland returns the label of this function's shared trap island
// for the given exit code, allocating it on first use.
func (m *machine) getOrCreateTrapIsland(code nativeapi.ExitCode) label {
	for _, ti := range m.trapIslands {
		if ti.code == code {
			return ti.l
		}
	}
	l := m.nextLabel
	m.nextLabel++
	m.trapIslands = append(m.trapIslands, trapIsland{code: code, l: l})
	return l
}

// StartLoweringFunction implements backend.Machine.
func (m *machine) StartLoweringFunction(maxBlockID ssa.BasicBlockID) {
	m.maxSSABlockID = label(maxBlockID)
	m.nextLabel = label(maxBlockID) + 1
}

// SetCurrentABI implements backend.Machine.
func (m *machine) SetCurrentABI(abi *backend.FunctionABI) { m.currentABI = abi }

// DisableStackCheck implements backend.Machine.
func (m *machine) DisableStackCheck() { m.stackBoundsCheckDisabled = true }

// SetCompiler implements backend.Machine.
func (m *machine) SetCompiler(ctx backend.Compiler) {
	m.compiler = ctx
	m.regAllocFn.ssaB = ctx.SSABuilder()
}

func (m *machine) insert(i *instruction) {
	m.pendingInstructions = append(m.pendingInstructions, i)
}

func (m *machine) allocateBrTarget() (nop *instruction, l label) {
	l = m.nextLabel
	m.nextLabel++
	nop = m.allocateInstr()
	nop.asNop0WithLabel(l)
	pos := m.labelPositionPool.GetOrAllocate(int(l))
	pos.begin, pos.end = nop, nop
	return
}

func (m *machine) allocateInstr() *instruction {
	instr := m.instrPool.Allocate()
	if !m.regAllocStarted {
		instr.addedBeforeRegAlloc = true
	}
	return instr
}

func (m *machine) allocateNop() *instruction {
	instr := m.allocateInstr()
	instr.asNop0()
	return instr
}

// InsertMove implements backend.Machine.
func (m *machine) InsertMove(dst, src regalloc.VReg, typ ssa.Type) {
	if src.RegType() != dst.RegType() {
		panic("BUG: src and dst must have the same register type")
	}
	instr := m.allocateInstr()
	switch typ {
	case ssa.TypeI32, ssa.TypeI64:
		instr.asMove64(dst, src)
	case ssa.TypeF32, ssa.TypeF64:
		instr.asFpuMov(dst, src)
	case ssa.TypeV128:
		instr.asVecMov(dst, src)
	default:
		panic("BUG: unsupported move type on riscv64: " + typ.String())
	}
	m.insert(instr)
}

// InsertReturn implements backend.Machine.
func (m *machine) InsertReturn() {
	i := m.allocateInstr()
	i.asRet()
	m.insert(i)
}

// Format implements backend.Machine.
func (m *machine) Format() string {
	begins := map[*instruction]label{}
	for i := 0; i <= m.labelPositionPool.MaxIDEncountered(); i++ {
		if pos := m.labelPositionPool.Get(i); pos != nil {
			begins[pos.begin] = label(i)
		}
	}

	var lines []string
	for cur := m.rootInstr; cur != nil; cur = cur.next {
		if l, ok := begins[cur]; ok {
			lines = append(lines, fmt.Sprintf("%s:", l))
		}
		if cur.kind == nop0 {
			if l, ok := cur.nop0Label(); ok && l != labelInvalid {
				lines = append(lines, fmt.Sprintf("%s:", l))
			}
			continue
		}
		lines = append(lines, "\t"+cur.String())
	}
	return "\n" + strings.Join(lines, "\n") + "\n"
}

// FrameSize implements backend.Machine.
func (m *machine) FrameSize() int64 { return m.frameSize() }

// CompiledBlockOffsets implements backend.Machine.
func (m *machine) CompiledBlockOffsets() []backend.CompiledBlockOffset {
	out := make([]backend.CompiledBlockOffset, 0, len(m.orderedSSABlockLabelPos))
	for _, pos := range m.orderedSSABlockLabelPos {
		if pos.sb == nil {
			continue
		}
		out = append(out, backend.CompiledBlockOffset{
			BlockID: pos.sb.ID(),
			Offset:  pos.binaryOffset,
		})
	}
	return out
}

// CallTrampolineIslandInfo implements backend.Machine.
//
// jal reaches +/-1MiB, so a module whose compiled functions span more than
// that needs trampoline islands, exactly as on arm64 (whose bl reaches
// +/-128MiB). The interval is chosen so that a call from anywhere between two
// islands can reach at least one of them.
func (m *machine) CallTrampolineIslandInfo(numFunctions int) (interval, islandSize int, err error) {
	return callTrampolineIslandInterval, trampolineCallSize * numFunctions, nil
}

func (m *machine) resolveAddressingMode(arg0offset, ret0offset int64, i *instruction) {
	amode := i.getAmode()
	switch amode.kind {
	case addressModeKindResultStackSpace:
		amode.imm += ret0offset
	case addressModeKindArgStackSpace:
		amode.imm += arg0offset
	default:
		panic("BUG: unexpected address mode kind in resolveAddressingMode")
	}
	amode.kind = addressModeKindRegSignedImm12

	// A 12-bit displacement covers +/-2KiB from SP, which a function with
	// enough stack-passed arguments -- or simply a large spill frame beneath
	// them -- runs past. arm64 never hits this (its scaled imm12 reaches 32KiB
	// and it has a register+register fallback); RISC-V has neither, so the
	// load/store grows into a three-instruction form that materializes the
	// address in the reserved scratch first. This runs before the layout pass
	// in resolveRelativeAddresses, so the larger size() is accounted for.
	//
	// The scratch cannot collide with anything live: these address modes are
	// always SP-based, and for a store the value being stored lives in rd,
	// not in the scratch.
	if !fitsInSignedImm12(amode.imm) {
		if !fitsInAuipcPair(amode.imm) {
			panic(fmt.Sprintf("BUG: arg/result stack offset %d is beyond a 32-bit displacement", amode.imm))
		}
		i.setBigOffset(true)
	}
}

// resolveAddressModes resolves the argument/result stack address modes now
// that the frame layout is known.
func (m *machine) resolveAddressModes() {
	if len(m.unresolvedAddressModes) == 0 {
		return
	}
	arg0offset, ret0offset := m.arg0OffsetFromSP(), m.ret0OffsetFromSP()
	for _, i := range m.unresolvedAddressModes {
		m.resolveAddressingMode(arg0offset, ret0offset, i)
	}
}

// arg0OffsetFromSP returns the offset of the first argument slot from SP,
// which is everything the prologue pushed below it.
func (m *machine) arg0OffsetFromSP() int64 {
	return m.frameSize() +
		16 + // 16-byte aligned frame size slot.
		16 // ret addr + size of arg/ret.
}

func (m *machine) ret0OffsetFromSP() int64 {
	return m.arg0OffsetFromSP() + m.currentABI.ArgStackSize
}

func (m *machine) requiredStackSize() int64 {
	return m.maxRequiredStackSizeForCalls +
		m.frameSize() +
		16 + // 16-byte aligned frame size slot.
		16 // ret addr + size of arg/ret.
}

func (m *machine) frameSize() int64 {
	s := m.clobberedRegSlotSize() + m.spillSlotSize
	if s&0xf != 0 {
		panic(fmt.Errorf("BUG: frame size %d is not 16-byte aligned", s))
	}
	return s
}

// clobberedRegSlotSize is the size of the callee-saved save area. Every
// register RV64G can clobber -- integer or FP -- is 64 bits, so slots are 8
// bytes, unlike arm64 where a v-register forces 16. The *region* is still
// rounded to 16 so the frame keeps its 16-byte alignment with an odd number of
// saved registers.
func (m *machine) clobberedRegSlotSize() int64 {
	return (int64(len(m.clobberedRegs))*8 + 15) &^ 15
}

// Encode implements backend.Machine.
func (m *machine) Encode(ctx context.Context) error {
	m.resolveRelativeAddresses(ctx)
	m.encode(m.rootInstr)
	if l := len(m.compiler.Buf()); l > maxFunctionExecutableSize {
		return fmt.Errorf("function size exceeds the limit: %d > %d", l, maxFunctionExecutableSize)
	}
	return nil
}

func (m *machine) encode(root *instruction) {
	for cur := root; cur != nil; cur = cur.next {
		before := int64(len(m.compiler.Buf()))
		cur.encode(m)
		// size() is not a hint: resolveRelativeAddresses lays out every label
		// from it, so an encoder that emits a different number of bytes sends
		// every branch past it to the wrong address. The failure is silent and
		// arbitrarily distant from its cause, and the check costs one integer
		// compare per instruction on a path that runs once per function.
		if got, want := int64(len(m.compiler.Buf()))-before, cur.size(); got != want {
			panic(fmt.Sprintf("BUG: %s encoded %d bytes, size() says %d", cur, got, want))
		}
	}
}

// maxFunctionExecutableSize is chosen so that every intra-function jal (+/-1MiB)
// and conditional branch (after the long-branch expansion in
// resolveRelativeAddresses) is representable.
const maxFunctionExecutableSize = 1 << 27

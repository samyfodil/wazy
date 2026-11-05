package ssa

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
)

// Builder is used to builds SSA consisting of Basic Blocks per function.
type Builder interface {
	// Init must be called to reuse this builder for the next function.
	Init(typ *Signature)

	// Signature returns the Signature of the currently-compiled function.
	Signature() *Signature

	// BlockIDMax returns the maximum value of BasicBlocksID existing in the currently-compiled function.
	BlockIDMax() BasicBlockID

	// AllocateBasicBlock creates a basic block in SSA function.
	AllocateBasicBlock() BasicBlock

	// CurrentBlock returns the currently handled BasicBlock which is set by the latest call to SetCurrentBlock.
	CurrentBlock() BasicBlock

	// EntryBlock returns the entry BasicBlock of the currently-compiled function.
	EntryBlock() BasicBlock

	// MarkUnlikelyEdge records that blk rarely branches to succ, as a branch hint says, so the block layout
	// can move what only such edges reach out of the hot path's way. A wrong mark costs speed, never
	// correctness.
	MarkUnlikelyEdge(blk, succ BasicBlock)

	// SetCurrentBlock sets the instruction insertion target to the BasicBlock `b`.
	SetCurrentBlock(b BasicBlock)

	// DeclareVariable declares a Variable of the given Type.
	DeclareVariable(Type) Variable

	// DefineVariable defines a variable in the `block` with value.
	// The defining instruction will be inserted into the `block`.
	DefineVariable(variable Variable, value Value, block BasicBlock)

	// DefineVariableInCurrentBB is the same as DefineVariable except the definition is
	// inserted into the current BasicBlock. Alias to DefineVariable(x, y, CurrentBlock()).
	DefineVariableInCurrentBB(variable Variable, value Value)

	// AllocateInstruction returns a new Instruction.
	AllocateInstruction() *Instruction

	// InsertInstruction executes BasicBlock.InsertInstruction for the currently handled basic block.
	InsertInstruction(raw *Instruction)

	// allocateValue allocates an unused Value.
	allocateValue(typ Type) Value

	// MustFindValue searches the latest definition of the given Variable and returns the result.
	MustFindValue(variable Variable) Value

	// FindValueInLinearPath tries to find the latest definition of the given Variable in the linear path to the current BasicBlock.
	// If it cannot find the definition, or it's not sealed yet, it returns ValueInvalid.
	FindValueInLinearPath(variable Variable) Value

	// MoveToCurrentBlock takes instr, a pure instruction of the block from, out of it and appends it to the
	// current block.
	MoveToCurrentBlock(instr *Instruction, from BasicBlock)

	// Seal declares that we've known all the predecessors to this block and were added via AddPred.
	// After calling this, AddPred will be forbidden.
	Seal(blk BasicBlock)

	// AnnotateValue is for debugging purpose.
	AnnotateValue(value Value, annotation string)

	// DeclareSignature appends the *Signature to be referenced by various instructions (e.g. OpcodeCall).
	DeclareSignature(signature *Signature)

	// Signatures returns the slice of declared Signatures.
	Signatures() []*Signature

	// ResolveSignature returns the Signature which corresponds to SignatureID.
	ResolveSignature(id SignatureID) *Signature

	// RunPasses runs various passes on the constructed SSA function.
	RunPasses()

	// Format returns the debugging string of the SSA function.
	Format() string

	// BlockIteratorBegin initializes the state to iterate over all the valid BasicBlock(s) compiled.
	// Combined with BlockIteratorNext, we can use this like:
	//
	// 	for blk := builder.BlockIteratorBegin(); blk != nil; blk = builder.BlockIteratorNext() {
	// 		// ...
	//	}
	//
	// The returned blocks are ordered in the order of AllocateBasicBlock being called.
	BlockIteratorBegin() BasicBlock

	// BlockIteratorNext advances the state for iteration initialized by BlockIteratorBegin.
	// Returns nil if there's no unseen BasicBlock.
	BlockIteratorNext() BasicBlock

	// ValuesInfo returns the data per Value used to lower the SSA in backend.
	// This is indexed by ValueID.
	ValuesInfo() []ValueInfo

	// BlockIteratorReversePostOrderBegin is almost the same as BlockIteratorBegin except it returns the BasicBlock in the reverse post-order.
	// This is available after RunPasses is run.
	BlockIteratorReversePostOrderBegin() BasicBlock

	// BlockIteratorReversePostOrderNext is almost the same as BlockIteratorPostOrderNext except it returns the BasicBlock in the reverse post-order.
	// This is available after RunPasses is run.
	BlockIteratorReversePostOrderNext() BasicBlock

	// BlockIteratorPostOrderBegin returns the first BasicBlock in a depth-first post-order from the entry, which the
	// reverse of the layout order above is not sure to be. See builder.computePostOrder. This is available after
	// RunPasses is run.
	BlockIteratorPostOrderBegin() BasicBlock

	// BlockIteratorPostOrderNext returns the next BasicBlock in that post-order.
	BlockIteratorPostOrderNext() BasicBlock

	// ReturnBlock returns the BasicBlock which is used to return from the function.
	ReturnBlock() BasicBlock

	// InsertUndefined inserts an undefined instruction at the current position.
	InsertUndefined()

	// AddVecProgram records p for an OpcodeVecMap and returns its number; VecProgram returns it back.
	// Programs last until the next Init.
	AddVecProgram(p *VecProgram) int
	VecProgram(n int) *VecProgram

	// SetCurrentSourceOffset sets the current source offset. The incoming instruction will be annotated with this offset.
	SetCurrentSourceOffset(line SourceOffset)

	// LoopNestingForestRoots returns the roots of the loop nesting forest.
	LoopNestingForestRoots() []BasicBlock

	// LowestCommonAncestor returns the lowest common ancestor in the dominator tree of the given BasicBlock(s).
	LowestCommonAncestor(blk1, blk2 BasicBlock) BasicBlock

	// Idom returns the immediate dominator of the given BasicBlock.
	Idom(blk BasicBlock) BasicBlock

	// VarLengthPool returns the VarLengthPool of Value.
	VarLengthPool() *nativeapi.VarLengthPool[Value]

	// InsertZeroValue inserts a zero value constant instruction of the given type.
	InsertZeroValue(t Type)

	// BasicBlock returns the BasicBlock of the given ID.
	BasicBlock(id BasicBlockID) BasicBlock

	// InstructionOfValue returns the Instruction that produces the given Value or nil if the Value is not produced by any Instruction.
	InstructionOfValue(v Value) *Instruction

	// FindLoopCounter returns the counter of the loop headed by header, entered from preheader. See
	// LoopCounter.
	FindLoopCounter(header, preheader BasicBlock) (LoopCounter, bool)

	// TripBound returns at most how many times the loop counted by lc runs its body, if that is known.
	TripBound(lc LoopCounter) (uint64, bool)

	// StripMine makes the loop counted by lc check for preemption once every chunk iterations, if it can.
	// See builder.StripMine.
	StripMine(header, preheader, body BasicBlock, lc LoopCounter, chunk uint64, yield func(target BasicBlock, args Values)) bool

	// MaskedCheckOnEntry reports whether a check testing the bits mask of the counter of lc runs in full
	// on entry to its loop. See builder.MaskedCheckOnEntry.
	MaskedCheckOnEntry(lc LoopCounter, mask uint64) bool

	// ReturnToHeader makes the slow path of a loop's check return to its header rather than go on into
	// the body. See builder.ReturnToHeader.
	ReturnToHeader(slow, header BasicBlock)

	// CheckOnEntry makes a loop check for preemption once each time it is entered, if it can. See
	// builder.CheckOnEntry.
	CheckOnEntry(header, preheader BasicBlock, yield func(target BasicBlock, args Values)) bool
}

// NewBuilder returns a new Builder implementation.
func NewBuilder() Builder {
	return &builder{
		instructionsPool:        nativeapi.NewPool[Instruction](resetInstruction),
		basicBlocksPool:         nativeapi.NewPool[basicBlock](resetBasicBlock),
		varLengthBasicBlockPool: nativeapi.NewVarLengthPool[BasicBlock](),
		varLengthPool:           nativeapi.NewVarLengthPool[Value](),
		valueAnnotations:        make(map[ValueID]string),
		signatures:              make(map[SignatureID]*Signature),
		returnBlk:               &basicBlock{id: basicBlockIDReturnBlock},
	}
}

// builder implements Builder interface.
type builder struct {
	basicBlocksPool  nativeapi.Pool[basicBlock]
	instructionsPool nativeapi.Pool[Instruction]
	varLengthPool    nativeapi.VarLengthPool[Value]
	signatures       map[SignatureID]*Signature
	currentSignature *Signature

	// reversePostOrderedBasicBlocks are the BasicBlock(s) ordered in the reverse post-order after passCalculateImmediateDominators.
	reversePostOrderedBasicBlocks []*basicBlock
	currentBB                     *basicBlock
	returnBlk                     *basicBlock

	// unlikelyEdges is whether MarkUnlikelyEdge marked any edge of the current function. The rest are scratch
	// for the layout that then follows. See moveColdBlocksLast.
	unlikelyEdges bool
	layoutSeen    []bool
	layoutStack   []layoutFrame
	blkStack3     []*basicBlock

	// nextValueID is used by builder.AllocateValue.
	nextValueID ValueID
	// cloneMark, cloneStamp and cloneBlockStamp are passCloneSharedComparesOpt's scratch.
	cloneMark                   []cloneMark
	cloneStamp, cloneBlockStamp int32
	// nextVariable is used by builder.AllocateVariable.
	nextVariable Variable

	// valueAnnotations contains the annotations for each Value, only used for debugging.
	valueAnnotations map[ValueID]string

	// valuesInfo contains the data per Value used to lower the SSA in backend. This is indexed by ValueID.
	valuesInfo []ValueInfo

	// varDefs backs basicBlock.varDefsOff/varDefsLen: one window per block that defines
	// anything, indexed by Variable, holding the last definition of that Variable there.
	varDefs []Value

	// loopTestScratch, loopBodyScratch and loopAvoidScratch are reused by loopTests.
	loopTestScratch                   []loopTest
	loopBodyScratch, loopAvoidScratch []*basicBlock

	// boundsMoves is passMergeBoundsChecksOpt's scratch.
	boundsMoves []*Instruction
	// imuls counts the multiplications the function was built with. See passLinearFormOpt.
	imuls int
	// linearMark, linearStamp, linearBlockStamp and linearTerms are passLinearFormOpt's.
	linearMark                    []linearMark
	linearStamp, linearBlockStamp int32
	linearTerms                   []linearTerm

	// dominators stores the immediate dominator of each BasicBlock.
	// The index is blockID of the BasicBlock.
	dominators []*basicBlock
	sparseTree dominatorSparseTree

	varLengthBasicBlockPool nativeapi.VarLengthPool[BasicBlock]

	// loopNestingForestRoots are the roots of the loop nesting forest.
	loopNestingForestRoots []BasicBlock

	// The followings are used for optimization passes/deterministic compilation.
	vecPrograms  []*VecProgram
	licmLoops    []*basicBlock
	licmDefs     []*basicBlock
	licmClobbers []bool
	instStack    []*Instruction
	blkStack     []*basicBlock
	blkStack2    []*basicBlock
	// predIndex is passLayoutBlocks' index of long predecessor lists; see predInfoOf.
	predIndex                 map[*basicBlock]map[*basicBlock]int
	redundantParams           []redundantParam
	dominatedMemoryBounds     []dominatedMemoryBound
	dominatedMemoryBoundHeads []int

	// blockIterCur is used to implement blockIteratorBegin and blockIteratorNext.
	blockIterCur int

	// postOrder, postOrderIter, postOrderSeen and dfsStack implement BlockIteratorPostOrderBegin and
	// BlockIteratorPostOrderNext: see computePostOrder.
	postOrder     []*basicBlock
	postOrderIter int
	postOrderSeen []bool
	dfsStack      []dfsFrame

	// donePreBlockLayoutPasses is true if all the passes before LayoutBlocks are called.
	donePreBlockLayoutPasses bool
	// doneBlockLayout is true if LayoutBlocks is called.
	doneBlockLayout bool
	// donePostBlockLayoutPasses is true if all the passes after LayoutBlocks are called.
	donePostBlockLayoutPasses bool

	currentSourceOffset SourceOffset

	// zeros are the zero value constants for each type.
	zeros [typeEnd]Value
}

// ValueInfo contains the data per Value used to lower the SSA in backend.
type ValueInfo struct {
	// RefCount is the reference count of the Value.
	RefCount uint32
	alias    Value
}

// redundantParam is a pair of the index of the redundant parameter and the Value.
// This is used to eliminate the redundant parameters in the optimization pass.
type redundantParam struct {
	// index is the index of the redundant parameter in the basicBlock.
	index int
	// uniqueValue is the Value which is passed to the redundant parameter.
	uniqueValue Value
}

// BasicBlock implements Builder.BasicBlock.
func (b *builder) BasicBlock(id BasicBlockID) BasicBlock {
	return b.basicBlock(id)
}

func (b *builder) basicBlock(id BasicBlockID) *basicBlock {
	if id == basicBlockIDReturnBlock {
		return b.returnBlk
	}
	return b.basicBlocksPool.View(int(id))
}

// InsertZeroValue implements Builder.InsertZeroValue.
func (b *builder) InsertZeroValue(t Type) {
	if b.zeros[t].Valid() {
		return
	}
	zeroInst := b.AllocateInstruction()
	switch t {
	case TypeI32:
		zeroInst.AsIconst32(0)
	case TypeI64:
		zeroInst.AsIconst64(0)
	case TypeF32:
		zeroInst.AsF32const(0)
	case TypeF64:
		zeroInst.AsF64const(0)
	case TypeV128:
		zeroInst.AsVconst(0, 0)
	default:
		panic("TODO: " + t.String())
	}
	b.zeros[t] = zeroInst.Insert(b).Return()
}

func (b *builder) VarLengthPool() *nativeapi.VarLengthPool[Value] {
	return &b.varLengthPool
}

// ReturnBlock implements Builder.ReturnBlock.
func (b *builder) ReturnBlock() BasicBlock {
	return b.returnBlk
}

// Init implements Builder.Reset.
// AddVecProgram implements Builder.AddVecProgram.
func (b *builder) AddVecProgram(p *VecProgram) int {
	b.vecPrograms = append(b.vecPrograms, p)
	return len(b.vecPrograms) - 1
}

// VecProgram implements Builder.VecProgram.
func (b *builder) VecProgram(n int) *VecProgram { return b.vecPrograms[n] }

func (b *builder) Init(s *Signature) {
	clear(b.vecPrograms)
	b.vecPrograms = b.vecPrograms[:0]
	b.nextVariable = 0
	b.imuls = 0
	b.currentSignature = s
	b.zeros = [typeEnd]Value{ValueInvalid, ValueInvalid, ValueInvalid, ValueInvalid, ValueInvalid, ValueInvalid}
	resetBasicBlock(b.returnBlk)
	b.instructionsPool.Reset()
	b.basicBlocksPool.Reset()
	b.varLengthPool.Reset()
	b.varLengthBasicBlockPool.Reset()
	b.donePreBlockLayoutPasses = false
	b.doneBlockLayout = false
	b.unlikelyEdges = false
	b.donePostBlockLayoutPasses = false
	if nativeapi.SSALoggingEnabled {
		// sig.used only feeds Signature-printing in Format() (via usedSignatures), which is
		// itself only reachable from debug/print paths; skip the reset when nothing reads it.
		for _, sig := range b.signatures {
			sig.used = false
		}
	}

	b.varDefs = b.varDefs[:0]
	b.redundantParams = b.redundantParams[:0]
	b.dominatedMemoryBounds = b.dominatedMemoryBounds[:0]
	b.dominatedMemoryBoundHeads = b.dominatedMemoryBoundHeads[:0]
	b.blkStack = b.blkStack[:0]
	b.blkStack2 = b.blkStack2[:0]
	b.dominators = b.dominators[:0]
	b.loopNestingForestRoots = b.loopNestingForestRoots[:0]
	b.basicBlocksPool.Reset()

	// valueAnnotations is only ever populated for debug printing (see AnnotateValue callers,
	// gated behind nativeapi.SSALoggingEnabled), so it is normally empty; clear() handles
	// that in O(1) instead of a per-key delete loop, and is still correct when non-empty.
	clear(b.valueAnnotations)
	for v := ValueID(0); v < b.nextValueID; v++ {
		b.valuesInfo[v] = ValueInfo{alias: ValueInvalid}
	}
	b.nextValueID = 0
	b.reversePostOrderedBasicBlocks = b.reversePostOrderedBasicBlocks[:0]
	b.doneBlockLayout = false
	b.currentSourceOffset = sourceOffsetUnknown
}

// Signature implements Builder.Signature.
func (b *builder) Signature() *Signature {
	return b.currentSignature
}

// AnnotateValue implements Builder.AnnotateValue.
func (b *builder) AnnotateValue(value Value, a string) {
	b.valueAnnotations[value.ID()] = a
}

// AllocateInstruction implements Builder.AllocateInstruction.
func (b *builder) AllocateInstruction() *Instruction {
	instr := b.instructionsPool.Allocate()
	instr.id = b.instructionsPool.Allocated()
	return instr
}

// DeclareSignature implements Builder.AnnotateValue.
func (b *builder) DeclareSignature(s *Signature) {
	b.signatures[s.ID] = s
	s.used = false
}

// Signatures implements Builder.Signatures.
func (b *builder) Signatures() (ret []*Signature) {
	for _, sig := range b.signatures {
		ret = append(ret, sig)
	}
	sort.Slice(ret, func(i, j int) bool {
		return ret[i].ID < ret[j].ID
	})
	return
}

// SetCurrentSourceOffset implements Builder.SetCurrentSourceOffset.
func (b *builder) SetCurrentSourceOffset(l SourceOffset) {
	b.currentSourceOffset = l
}

func (b *builder) usedSignatures() (ret []*Signature) {
	for _, sig := range b.signatures {
		if sig.used {
			ret = append(ret, sig)
		}
	}
	sort.Slice(ret, func(i, j int) bool {
		return ret[i].ID < ret[j].ID
	})
	return
}

// ResolveSignature implements Builder.ResolveSignature.
func (b *builder) ResolveSignature(id SignatureID) *Signature {
	return b.signatures[id]
}

// AllocateBasicBlock implements Builder.AllocateBasicBlock.
func (b *builder) AllocateBasicBlock() BasicBlock {
	return b.allocateBasicBlock()
}

// allocateBasicBlock allocates a new basicBlock.
func (b *builder) allocateBasicBlock() *basicBlock {
	id := BasicBlockID(b.basicBlocksPool.Allocated())
	blk := b.basicBlocksPool.Allocate()
	blk.id = id
	return blk
}

// Idom implements Builder.Idom.
func (b *builder) Idom(blk BasicBlock) BasicBlock {
	return b.dominators[blk.ID()]
}

// MoveToCurrentBlock implements Builder.MoveToCurrentBlock.
func (b *builder) MoveToCurrentBlock(instr *Instruction, from BasicBlock) {
	from.(*basicBlock).removeInstruction(instr)
	instr.prev, instr.next = nil, nil
	b.currentBB.insertInstruction(b, instr)
}

// InsertInstruction implements Builder.InsertInstruction.
func (b *builder) InsertInstruction(instr *Instruction) {
	b.currentBB.insertInstruction(b, instr)
	if instr.opcode == OpcodeImul {
		b.imuls++
	}

	if l := b.currentSourceOffset; l.Valid() {
		// Emit the source offset info only when the instruction has side effect because
		// these are the only instructions that are accessed by stack unwinding.
		// This reduces the significant amount of the offset info in the binary.
		if instr.sideEffect() != sideEffectNone {
			instr.annotateSourceOffset(l)
		}
	}

	resultTypesFn := instructionReturnTypes[instr.opcode]
	if resultTypesFn == nil {
		panic("TODO: " + instr.Format(b))
	}

	t1, ts := resultTypesFn(b, instr)
	if t1.invalid() {
		return
	}

	r1 := b.allocateValue(t1)
	instr.rValue = r1.setInstructionID(instr.id)

	tsl := len(ts)
	if tsl == 0 {
		return
	}

	rValues := b.varLengthPool.Allocate(tsl)
	for i := 0; i < tsl; i++ {
		rn := b.allocateValue(ts[i])
		rValues = rValues.Append(&b.varLengthPool, rn.setInstructionID(instr.id))
	}
	instr.rValues = rValues
}

// DefineVariable implements Builder.DefineVariable.
func (b *builder) DefineVariable(variable Variable, value Value, block BasicBlock) {
	bb := block.(*basicBlock)
	i := uint32(variable) & variableIndexMask
	if i >= bb.varDefsLen {
		b.growVarDefs(bb)
	}
	b.varDefs[bb.varDefsOff+i] = value
}

// lastDefinition returns the definition of the variable in this block, if any.
func (b *builder) lastDefinition(bb *basicBlock, variable Variable) (Value, bool) {
	i := uint32(variable) & variableIndexMask
	if i >= bb.varDefsLen {
		return ValueInvalid, false
	}
	v := b.varDefs[bb.varDefsOff+i]
	return v, v.Valid()
}

// growVarDefs gives bb a window with room for every Variable declared so far. The frontend
// declares them all before lowering a body, so this runs at most once per block.
func (b *builder) growVarDefs(bb *basicBlock) {
	off, n := uint32(len(b.varDefs)), uint32(b.nextVariable)&variableIndexMask
	b.varDefs = append(b.varDefs, make([]Value, n)...)
	row := b.varDefs[off:]
	for i := range row {
		row[i] = ValueInvalid
	}
	copy(row, b.varDefs[bb.varDefsOff:bb.varDefsOff+bb.varDefsLen])
	bb.varDefsOff, bb.varDefsLen = off, n
}

// DefineVariableInCurrentBB implements Builder.DefineVariableInCurrentBB.
func (b *builder) DefineVariableInCurrentBB(variable Variable, value Value) {
	b.DefineVariable(variable, value, b.currentBB)
}

// SetCurrentBlock implements Builder.SetCurrentBlock.
func (b *builder) SetCurrentBlock(bb BasicBlock) {
	b.currentBB = bb.(*basicBlock)
}

// CurrentBlock implements Builder.CurrentBlock.
func (b *builder) CurrentBlock() BasicBlock {
	return b.currentBB
}

// EntryBlock implements Builder.EntryBlock.
func (b *builder) EntryBlock() BasicBlock {
	return b.entryBlk()
}

// DeclareVariable implements Builder.DeclareVariable.
func (b *builder) DeclareVariable(typ Type) Variable {
	v := b.nextVariable
	b.nextVariable++
	return v.setType(typ)
}

// allocateValue implements Builder.AllocateValue.
func (b *builder) allocateValue(typ Type) (v Value) {
	v = Value(b.nextValueID)
	v = v.setType(typ)
	b.nextValueID++
	return
}

// FindValueInLinearPath implements Builder.FindValueInLinearPath.
func (b *builder) FindValueInLinearPath(variable Variable) Value {
	return b.findValueInLinearPath(variable, b.currentBB)
}

func (b *builder) findValueInLinearPath(variable Variable, blk *basicBlock) Value {
	for {
		if val, ok := b.lastDefinition(blk, variable); ok {
			return val
		} else if !blk.sealed {
			return ValueInvalid
		}

		// If this block is sealed and have only one predecessor,
		// we can use the value in that block without ambiguity on definition.
		pred := blk.singlePred
		if pred == nil {
			if len(blk.preds) == 1 {
				panic("BUG")
			}
			return ValueInvalid
		}
		blk = pred
	}
}

// MustFindValue implements Builder.MustFindValue.
func (b *builder) MustFindValue(variable Variable) Value {
	return b.findValue(variable.getType(), variable, b.currentBB)
}

// findValue tries to find the latest definition of a `variable`. The algorithm is described in
// the section 2 of the paper https://link.springer.com/content/pdf/10.1007/978-3-642-37051-9_6.pdf.
//
// The walk up a chain of single-predecessor blocks is iterative; only the fork at a block with
// several predecessors recurses.
func (b *builder) findValue(typ Type, variable Variable, blk *basicBlock) Value {
	for {
		if val, ok := b.lastDefinition(blk, variable); ok {
			// The value is already defined in this block!
			return val
		} else if !blk.sealed { // Incomplete CFG as in the paper.
			// If this is not sealed, that means it might have additional unknown predecessor later on.
			// So we temporarily define the placeholder value here (not add as a parameter yet!),
			// and record it as unknown.
			// The unknown values are resolved when we call seal this block via BasicBlock.Seal().
			value := b.allocateValue(typ)
			if nativeapi.SSALoggingEnabled {
				fmt.Printf("adding unknown value placeholder for %s at %d\n", variable, blk.id)
			}
			b.DefineVariable(variable, value, blk)
			blk.unknownValues = append(blk.unknownValues, unknownValue{
				variable: variable,
				value:    value,
			})
			return value
		} else if blk.EntryBlock() {
			// If this is the entry block, we reach the uninitialized variable which has zero value.
			return b.zeros[variable.getType()]
		}

		// If this block is sealed and have only one predecessor,
		// we can use the value in that block without ambiguity on definition.
		pred := blk.singlePred
		if pred == nil {
			break
		}
		blk = pred
	}
	if len(blk.preds) == 0 {
		panic("BUG: value is not defined for " + variable.String())
	}

	// If this block has multiple predecessors, we have to gather the definitions,
	// and treat them as an argument to this block.
	//
	// But before that, we have to check if the possible definitions are the same Value.
	tmpValue := b.allocateValue(typ)
	// Break the cycle by defining the variable with the tmpValue.
	b.DefineVariable(variable, tmpValue, blk)
	// Check all the predecessors if they have the same definition.
	uniqueValue := ValueInvalid
	for i := range blk.preds {
		predValue := b.findValue(typ, variable, blk.preds[i].blk)
		if uniqueValue == ValueInvalid {
			uniqueValue = predValue
		} else if uniqueValue != predValue {
			uniqueValue = ValueInvalid
			break
		}
	}

	if uniqueValue != ValueInvalid {
		// If all the predecessors have the same definition, we can use that value.
		b.alias(tmpValue, uniqueValue)
		return uniqueValue
	} else {
		// Otherwise, add the tmpValue to this block as a parameter which may or may not be redundant, but
		// later we eliminate trivial params in an optimization pass. This must be done before finding the
		// definitions in the predecessors so that we can break the cycle.
		blk.addParamOn(b, tmpValue)
		// After the new param is added, we have to manipulate the original branching instructions
		// in predecessors so that they would pass the definition of `variable` as the argument to
		// the newly added PHI.
		for i := range blk.preds {
			pred := &blk.preds[i]
			value := b.findValue(typ, variable, pred.blk)
			pred.branch.addArgumentBranchInst(b, value)
		}
		return tmpValue
	}
}

// Seal implements Builder.Seal.
func (b *builder) Seal(raw BasicBlock) {
	blk := raw.(*basicBlock)
	switch len(blk.preds) {
	case 1:
		blk.singlePred = blk.preds[0].blk
	case 2:
		blk.singlePred = yieldJoin(blk.preds[0].blk, blk.preds[1].blk)
	}
	blk.sealed = true

	for _, v := range blk.unknownValues {
		variable, phiValue := v.variable, v.value
		typ := variable.getType()
		blk.addParamOn(b, phiValue)
		for i := range blk.preds {
			pred := &blk.preds[i]
			predValue := b.findValue(typ, variable, pred.blk)
			if !predValue.Valid() {
				panic("BUG: value is not defined anywhere in the predecessors in the CFG")
			}
			pred.branch.addArgumentBranchInst(b, predValue)
		}
	}
}

// yieldJoin returns p when q is the slow path of a preemption check in p, or q when
// p is that of one in q, and nil otherwise: a block that opens with OpcodeYield,
// defines no variable and is reached from the other alone. A block those two join
// at sees every variable as the check's block left it, so it can look through to
// that block as to a single predecessor, rather than define each it reads anew.
func yieldJoin(p, q *basicBlock) *basicBlock {
	slowOf := func(slow, check *basicBlock) bool {
		return slow.sealed && slow.singlePred == check && slow.varDefsLen == 0 && slow.yields()
	}
	switch {
	case slowOf(q, p):
		return p
	case slowOf(p, q):
		return q
	}
	return nil
}

// Format implements Builder.Format.
func (b *builder) Format() string {
	str := strings.Builder{}
	usedSigs := b.usedSignatures()
	if len(usedSigs) > 0 {
		str.WriteByte('\n')
		str.WriteString("signatures:\n")
		for _, sig := range usedSigs {
			str.WriteByte('\t')
			str.WriteString(sig.String())
			str.WriteByte('\n')
		}
	}

	var iterBegin, iterNext func() *basicBlock
	if b.doneBlockLayout {
		iterBegin, iterNext = b.blockIteratorReversePostOrderBegin, b.blockIteratorReversePostOrderNext
	} else {
		iterBegin, iterNext = b.blockIteratorBegin, b.blockIteratorNext
	}
	for bb := iterBegin(); bb != nil; bb = iterNext() {
		str.WriteByte('\n')
		str.WriteString(bb.formatHeader(b))
		str.WriteByte('\n')

		for cur := bb.Root(); cur != nil; cur = cur.Next() {
			str.WriteByte('\t')
			str.WriteString(cur.Format(b))
			str.WriteByte('\n')
		}
	}
	return str.String()
}

// BlockIteratorNext implements Builder.BlockIteratorNext.
func (b *builder) BlockIteratorNext() BasicBlock {
	if blk := b.blockIteratorNext(); blk == nil {
		return nil // BasicBlock((*basicBlock)(nil)) != BasicBlock(nil)
	} else {
		return blk
	}
}

// BlockIteratorNext implements Builder.BlockIteratorNext.
func (b *builder) blockIteratorNext() *basicBlock {
	index := b.blockIterCur
	for {
		if index == b.basicBlocksPool.Allocated() {
			return nil
		}
		ret := b.basicBlocksPool.View(index)
		index++
		if !ret.invalid {
			b.blockIterCur = index
			return ret
		}
	}
}

// BlockIteratorBegin implements Builder.BlockIteratorBegin.
func (b *builder) BlockIteratorBegin() BasicBlock {
	return b.blockIteratorBegin()
}

// BlockIteratorBegin implements Builder.BlockIteratorBegin.
func (b *builder) blockIteratorBegin() *basicBlock {
	b.blockIterCur = 0
	return b.blockIteratorNext()
}

// BlockIteratorReversePostOrderBegin implements Builder.BlockIteratorReversePostOrderBegin.
func (b *builder) BlockIteratorReversePostOrderBegin() BasicBlock {
	return b.blockIteratorReversePostOrderBegin()
}

// BlockIteratorBegin implements Builder.BlockIteratorBegin.
func (b *builder) blockIteratorReversePostOrderBegin() *basicBlock {
	b.blockIterCur = 0
	return b.blockIteratorReversePostOrderNext()
}

// BlockIteratorReversePostOrderNext implements Builder.BlockIteratorReversePostOrderNext.
func (b *builder) BlockIteratorReversePostOrderNext() BasicBlock {
	if blk := b.blockIteratorReversePostOrderNext(); blk == nil {
		return nil // BasicBlock((*basicBlock)(nil)) != BasicBlock(nil)
	} else {
		return blk
	}
}

// BlockIteratorNext implements Builder.BlockIteratorNext.
func (b *builder) blockIteratorReversePostOrderNext() *basicBlock {
	if b.blockIterCur >= len(b.reversePostOrderedBasicBlocks) {
		return nil
	} else {
		ret := b.reversePostOrderedBasicBlocks[b.blockIterCur]
		b.blockIterCur++
		return ret
	}
}

// BlockIteratorPostOrderBegin implements Builder.BlockIteratorPostOrderBegin.
func (b *builder) BlockIteratorPostOrderBegin() BasicBlock {
	b.computePostOrder()
	b.postOrderIter = 0
	return b.BlockIteratorPostOrderNext()
}

// BlockIteratorPostOrderNext implements Builder.BlockIteratorPostOrderNext.
func (b *builder) BlockIteratorPostOrderNext() BasicBlock {
	if b.postOrderIter >= len(b.postOrder) {
		return nil
	}
	ret := b.postOrder[b.postOrderIter]
	b.postOrderIter++
	return ret
}

// dfsFrame is a block on computePostOrder's stack, and the index of the next
// successor to look at.
type dfsFrame struct {
	blk  *basicBlock
	next int
}

// computePostOrder lists the blocks in a depth-first post-order from the entry
// block. Register allocation's liveness needs that order, in which an edge to a
// block not yet listed is a loop's back edge. The layout order is not one: its
// search marks a block when it is pushed rather than when it is reached, and sorts
// the slow paths of preemption checks last (see sortBlocks), so the edge from one
// of those to the block it rejoins comes after that block. A function with no
// such slow path takes the layout order reversed.
func (b *builder) computePostOrder() {
	order := b.postOrder[:0]
	if !slices.ContainsFunc(b.reversePostOrderedBasicBlocks, (*basicBlock).yields) {
		for i := len(b.reversePostOrderedBasicBlocks) - 1; i >= 0; i-- {
			order = append(order, b.reversePostOrderedBasicBlocks[i])
		}
		b.postOrder = order
		return
	}
	seen := slices.Grow(b.postOrderSeen[:0], b.basicBlocksPool.Allocated())[:b.basicBlocksPool.Allocated()]
	clear(seen)
	stack := b.dfsStack[:0]
	entry := b.entryBlk()
	seen[entry.id] = true
	stack = append(stack, dfsFrame{blk: entry})
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		if top.next == len(top.blk.success) {
			order = append(order, top.blk)
			stack = stack[:len(stack)-1]
			continue
		}
		succ := top.blk.success[top.next]
		top.next++
		if succ.ReturnBlock() || succ.invalid || seen[succ.id] {
			continue
		}
		seen[succ.id] = true
		stack = append(stack, dfsFrame{blk: succ})
	}
	b.postOrder, b.dfsStack, b.postOrderSeen = order, stack, seen
}

// ValuesInfo implements Builder.ValuesInfo.
func (b *builder) ValuesInfo() []ValueInfo {
	return b.valuesInfo
}

// alias records the alias of the given values. The alias(es) will be
// eliminated in the optimization pass via resolveArgumentAlias.
func (b *builder) alias(dst, src Value) {
	did := int(dst.ID())
	if did >= len(b.valuesInfo) {
		l := did + 1 - len(b.valuesInfo)
		b.valuesInfo = append(b.valuesInfo, make([]ValueInfo, l)...)
		view := b.valuesInfo[len(b.valuesInfo)-l:]
		for i := range view {
			view[i].alias = ValueInvalid
		}
	}
	b.valuesInfo[did].alias = src
}

// resolveArgumentAlias resolves the alias of the arguments of the given instruction.
func (b *builder) resolveArgumentAlias(instr *Instruction) {
	if instr.v.Valid() {
		instr.v = b.resolveAlias(instr.v)
	}

	if instr.v2.Valid() {
		instr.v2 = b.resolveAlias(instr.v2)
	}

	if instr.v3.Valid() {
		instr.v3 = b.resolveAlias(instr.v3)
	}

	view := instr.vs.View()
	for i, v := range view {
		view[i] = b.resolveAlias(v)
	}
}

// resolveAlias resolves the alias of the given value.
func (b *builder) resolveAlias(v Value) Value {
	info := b.valuesInfo
	l := ValueID(len(info))
	// Some aliases are chained, so we need to resolve them recursively.
	for {
		vid := v.ID()
		if vid < l && info[vid].alias.Valid() {
			v = info[vid].alias
		} else {
			break
		}
	}
	return v
}

// entryBlk returns the entry block of the function.
func (b *builder) entryBlk() *basicBlock {
	return b.basicBlocksPool.View(0)
}

// isDominatedBy returns true if the given block `n` is dominated by the given block `d`.
// Before calling this, the builder must pass by passCalculateImmediateDominators.
func (b *builder) isDominatedBy(n *basicBlock, d *basicBlock) bool {
	if len(b.dominators) == 0 {
		panic("BUG: passCalculateImmediateDominators must be called before calling isDominatedBy")
	}
	ent := b.entryBlk()
	doms := b.dominators
	for n != d && n != ent {
		n = doms[n.id]
	}
	return n == d
}

// BlockIDMax implements Builder.BlockIDMax.
func (b *builder) BlockIDMax() BasicBlockID {
	return BasicBlockID(b.basicBlocksPool.Allocated())
}

// InsertUndefined implements Builder.InsertUndefined.
func (b *builder) InsertUndefined() {
	instr := b.AllocateInstruction()
	instr.opcode = OpcodeUndefined
	b.InsertInstruction(instr)
}

// LoopNestingForestRoots implements Builder.LoopNestingForestRoots.
func (b *builder) LoopNestingForestRoots() []BasicBlock {
	return b.loopNestingForestRoots
}

// LowestCommonAncestor implements Builder.LowestCommonAncestor.
func (b *builder) LowestCommonAncestor(blk1, blk2 BasicBlock) BasicBlock {
	return b.sparseTree.findLCA(blk1.ID(), blk2.ID())
}

// InstructionOfValue returns the instruction that produces the given Value, or nil
// if the Value is not produced by any instruction.
func (b *builder) InstructionOfValue(v Value) *Instruction {
	instrID := v.instructionID()
	if instrID <= 0 {
		return nil
	}
	return b.instructionsPool.View(instrID - 1)
}

// MarkUnlikelyEdge implements Builder.MarkUnlikelyEdge.
func (b *builder) MarkUnlikelyEdge(blk, succ BasicBlock) {
	blk.(*basicBlock).unlikelySucc = succ.(*basicBlock)
	b.unlikelyEdges = true
}

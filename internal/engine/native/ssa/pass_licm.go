package ssa

import "runtime"

// passLoopInvariantCodeMotionOpt hoists loop-invariant instructions into the
// block that dominates their loop.
//
// wazy deliberately skips the middle-end optimizations a wasm producer has
// already run (see the note in runPreBlockLayoutPasses), and loop-invariant
// code motion is usually one of them. It earns a place here because most of
// what it moves is code wazy introduces itself: lowering one wasm instruction
// often expands to several SSA instructions, and where the wasm operands are
// loop-invariant so is the expansion. i32x4.relaxed_dot_i8x16_i7x16_add_s, for
// instance, expands to four widenings and two dot products, none of which exist
// at wasm level for a producer to have hoisted.
//
// Only instructions that are free of side effects, cannot trap and do not read
// memory are moved, so a hoist is never observable: the worst it can do is
// compute a value for a loop that turns out to run zero times.
//
// What it must NOT move matters as much as what it moves, because in this
// compiler hoisting is not free. Both backends fold a single-use producer into
// its consumer only when the two share an InstructionGroupID, and groups never
// span blocks, so moving a value to another block permanently costs it its
// fold. An address computation that previously emitted no instruction at all
// becomes a register live across the whole loop, and the allocator, which
// picks its spill victim by furthest next use with no loop weighting, then
// spills something hot. Hoisting everything measured 4.94% slower over the
// TinyGo case.wasm benchmarks and 64.84% slower on matmul, whose compiled code
// went from 209 stack references to 274. Refusing the fold-sensitive opcodes
// below brings matmul back to byte-identical output while keeping the SIMD
// case, whose loop body still drops from fourteen machine instructions to five.
//
// This must run before passDeadCodeEliminationOpt, which stamps every
// instruction with the InstructionGroupID the backend reads. Moving an
// instruction after that point would leave it carrying a group from the block
// it came from.
func passLoopInvariantCodeMotionOpt(b *builder) {
	loopOf := b.licmLoopMembership()
	if loopOf == nil {
		return
	}
	defBlk := b.licmDefBlk()
	clobbers := b.licmContextClobbers(loopOf)
	floatConsts, intConsts := make(map[*basicBlock]int), make(map[*basicBlock]int)

	for _, blk := range b.reversePostOrderedBasicBlocks {
		if blk.invalid || loopOf[blk.id] == nil {
			continue
		}
		for cur := blk.rootInstr; cur != nil; {
			next := cur.next
			target := b.licmTarget(cur, loopOf[blk.id], loopOf, defBlk, clobbers)
			if cur.opcode == OpcodeF32const || cur.opcode == OpcodeF64const {
				target = b.floatConstTarget(loopOf[blk.id], floatConsts)
			}
			if cur.opcode == OpcodeIconst && hoistIntConsts && multiInstructionConst(cur) && b.everyIteration(blk, loopOf[blk.id]) {
				target = b.floatConstTarget(loopOf[blk.id], intConsts)
			}
			if target != nil {
				blk.removeInstruction(cur)
				b.resolveArgumentAlias(cur)
				if cur.opcode == OpcodeIsub {
					if same := b.limitLike(target, cur); same != nil {
						b.alias(cur.rValue, same.rValue)
						defBlk[cur.rValue.ID()] = target
						cur = next
						continue
					}
					b.cloneConstOperand(target, cur)
				}
				if same := target.contextLoadLike(cur); same != nil {
					// Every block of a loop reloads a memory's base, since none of them is on a linear
					// path from the one before the loop; one load before the loop serves them all.
					b.alias(cur.rValue, same.rValue)
					defBlk[cur.rValue.ID()] = target
					cur = next
					continue
				}
				target.insertBeforeTerminator(cur)
				if cur.opcode == OpcodeF32const || cur.opcode == OpcodeF64const || cur.opcode == OpcodeIconst {
					cur.pinned = true
				}
				// Later instructions in this block may now be invariant too,
				// since one of their operands just moved out of the loop.
				if first, rest := cur.Returns(); first.Valid() {
					defBlk[first.ID()] = target
					for _, v := range rest {
						defBlk[v.ID()] = target
					}
				}
			}
			cur = next
		}
	}
}

// licmLoopMembership maps each block to the innermost loop containing it, or
// nil where it is in no loop at all. It returns nil outright when the function
// has no loops, which is the common case and the cheapest thing to detect.
//
// Membership is the natural loop of each back edge, not merely "dominated by a
// loop header": a block sitting after the loop is dominated by the header
// without being inside it, and hoisting out of one of those would stretch a
// value's live range across the whole loop to no benefit.
func (b *builder) licmLoopMembership() []*basicBlock {
	var headers bool
	for _, blk := range b.reversePostOrderedBasicBlocks {
		if blk.loopHeader && !blk.invalid {
			headers = true
			break
		}
	}
	if !headers {
		return nil
	}

	loopOf := b.licmLoops
	if n := int(b.BlockIDMax()); cap(loopOf) < n {
		loopOf = make([]*basicBlock, n)
	} else {
		loopOf = loopOf[:n]
		clear(loopOf)
	}
	b.licmLoops = loopOf
	stack := b.blkStack[:0]
	// Reverse post order reaches an outer header before the headers nested in
	// it, so an inner loop overwrites the outer one and each block ends up
	// mapped to its innermost loop.
	for _, h := range b.reversePostOrderedBasicBlocks {
		if !h.loopHeader || h.invalid {
			continue
		}
		// Seed the walk with the sources of the back edges into h, being the
		// predecessors h itself dominates.
		stack = stack[:0]
		for i := range h.preds {
			if p := h.preds[i].blk; !p.invalid && b.isDominatedBy(p, h) {
				stack = append(stack, p)
			}
		}
		if len(stack) == 0 {
			continue
		}
		// Claiming the header first stops the walk from stepping over it into
		// the blocks before the loop.
		loopOf[h.id] = h
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if loopOf[n.id] == h {
				continue
			}
			loopOf[n.id] = h
			for i := range n.preds {
				if p := n.preds[i].blk; !p.invalid {
					stack = append(stack, p)
				}
			}
		}
	}
	b.blkStack = stack
	return loopOf
}

// licmDefBlk maps each Value to the block defining it. Aliases left behind by
// passRedundantPhiEliminationOpt are resolved on the way, because this runs
// before the dead code pass that would otherwise have done it.
func (b *builder) licmDefBlk() []*basicBlock {
	defBlk := b.licmDefs
	if n := int(b.nextValueID); cap(defBlk) < n {
		defBlk = make([]*basicBlock, n)
	} else {
		defBlk = defBlk[:n]
		clear(defBlk)
	}
	b.licmDefs = defBlk
	entry := b.entryBlk()
	for _, blk := range b.reversePostOrderedBasicBlocks {
		if blk.invalid {
			continue
		}
		for _, p := range blk.params.View() {
			if id := p.ID(); int(id) < len(defBlk) {
				defBlk[id] = blk
			}
		}
		for cur := blk.rootInstr; cur != nil; cur = cur.next {
			b.resolveArgumentAlias(cur)
			first, rest := cur.Returns()
			if first.Valid() && int(first.ID()) < len(defBlk) {
				defBlk[first.ID()] = blk
			}
			for _, v := range rest {
				if int(v.ID()) < len(defBlk) {
					defBlk[v.ID()] = blk
				}
			}
		}
	}
	// Function parameters live from the entry onwards, and so does anything
	// left unmapped: the entry dominates every block, so treating it as the
	// definition site is the conservative choice for a value with no
	// instruction behind it.
	for i, d := range defBlk {
		if d == nil {
			defBlk[i] = entry
		}
	}
	return defBlk
}

// licmTarget returns the block instr should be hoisted into, or nil to leave it
// alone. It walks outwards through enclosing loops for as long as instr stays
// invariant, so an instruction in a nested loop lands outside the outermost
// loop it does not depend on rather than one level up.
func (b *builder) licmTarget(instr *Instruction, header *basicBlock, loopOf, defBlk []*basicBlock, clobbers []bool) *basicBlock {
	if !instr.contextLoad && !hoistableFromLoop(instr) {
		return nil
	}
	var target *basicBlock
	for header != nil {
		if !b.invariantIn(instr, header, defBlk) || instr.contextLoad && clobbers[header.id] {
			break
		}
		preheader := b.dominators[header.id]
		if preheader == nil || preheader == header || preheader.invalid {
			break
		}
		target = preheader
		header = loopOf[preheader.id]
	}
	return target
}

// invariantIn reports whether every operand of instr is defined outside the
// loop headed by header. A natural loop is dominated by its header, so a value
// is outside it exactly when its defining block strictly dominates the header.
// A subtraction of a constant counts its constant as outside, which hoisting
// clones: a memory's length less an access size is a bounds check's limit, the
// same for every access of that size in the loop (see cloneConstOperand).
func (b *builder) invariantIn(instr *Instruction, header *basicBlock, defBlk []*basicBlock) bool {
	v1, v2, v3, vs := instr.Args()
	if instr.opcode == OpcodeIsub && b.isConstValue(v2) {
		return b.definedOutside(v1, header, defBlk)
	}
	for _, v := range [...]Value{v1, v2, v3} {
		if v.Valid() && !b.definedOutside(v, header, defBlk) {
			return false
		}
	}
	for _, v := range vs {
		if v.Valid() && !b.definedOutside(v, header, defBlk) {
			return false
		}
	}
	return true
}

func (b *builder) definedOutside(v Value, header *basicBlock, defBlk []*basicBlock) bool {
	id := v.ID()
	if int(id) >= len(defBlk) {
		return false
	}
	d := defBlk[id]
	return d != nil && d != header && b.isDominatedBy(header, d)
}

// hoistableFromLoop reports whether moving instr earlier can never be observed.
// Anything that traps or has a side effect is excluded by its classification;
// loads need excluding by hand because they are marked as having no side effect
// even though they can trap and can be invalidated by a store in the loop.
func hoistableFromLoop(instr *Instruction) bool {
	if instr.sideEffect() != sideEffectNone {
		return false
	}
	switch instr.opcode {
	// The three groups below are all the same trade. Each of these folds into
	// the instruction that consumes it, folding only happens inside one
	// instruction group, and a group never spans blocks: hoisting one turns
	// something that emitted no machine instruction at all into a register
	// live across the whole loop. All three are cheap to leave where they are.
	//
	// A comparison folds into the branch, select or trap consuming it. The
	// backends lower a moved comparison correctly, so this is a cost choice and
	// not a correctness rule.
	case OpcodeIcmp, OpcodeFcmp:
		return false
	// A constant is rematerialized at each use by the backend. A float one is a load from the constant
	// pool there, so it is hoisted, but by passLoopInvariantCodeMotionOpt itself; see floatConstTarget.
	case OpcodeIconst, OpcodeF32const, OpcodeF64const, OpcodeVconst:
		return false
	// Address arithmetic folds into a load or store's addressing mode.
	//
	// Rejecting all four outright looks coarser than it is. The obvious
	// refinement is to reject only the ones that could actually still fold,
	// which means a single use, since MatchInstr refuses to fold a value used
	// twice. Measured over case.wasm that is worse, not better: 22130
	// instructions and 4231 stack references against 21812 and 3880. A
	// multi-use address computation is live only within one iteration where it
	// stands, and hoisting stretches it across every iteration, so the pressure
	// costs more than the repeated arithmetic saves. The blunt rule wins.
	case OpcodeIadd, OpcodeIshl, OpcodeUExtend, OpcodeSExtend:
		return false
	// A load can trap and a store in the loop can invalidate it, even though
	// loads are classified as having no side effect.
	case OpcodeLoad, OpcodeLoadSplat, OpcodeVZeroExtLoad,
		OpcodeUload8, OpcodeUload16, OpcodeUload32,
		OpcodeSload8, OpcodeSload16, OpcodeSload32:
		return false
	}
	// A branching instruction stays where it is even when it is classified as
	// pure, and an instruction producing nothing is not worth moving.
	if instr.IsBranching() {
		return false
	}
	first, _ := instr.Returns()
	return first.Valid()
}

// licmContextClobbers reports, per loop header, whether its loop may change what a context load
// (see Instruction.MarkContextLoad) reads: it makes a call, which can grow a memory or run any code,
// or it stores through the module context pointer, as memory.grow's native path and global.set do.
// A loop inherits the clobbers of the loops nested in it.
func (b *builder) licmContextClobbers(loopOf []*basicBlock) []bool {
	clobbers := b.licmClobbers
	if n := int(b.BlockIDMax()); cap(clobbers) < n {
		clobbers = make([]bool, n)
	} else {
		clobbers = clobbers[:n]
		clear(clobbers)
	}
	b.licmClobbers = clobbers
	moduleCtx := ValueInvalid
	if params := b.entryBlk().params.View(); len(params) > 1 {
		moduleCtx = params[1]
	}
	for _, blk := range b.reversePostOrderedBasicBlocks {
		header := loopOf[blk.id]
		if blk.invalid || header == nil || clobbers[header.id] {
			continue
		}
		for cur := blk.rootInstr; cur != nil; cur = cur.next {
			if !clobbersContext(cur, moduleCtx) {
				continue
			}
			for h := header; h != nil && !clobbers[h.id]; {
				clobbers[h.id] = true
				preheader := b.dominators[h.id]
				if preheader == nil || preheader == h {
					break
				}
				h = loopOf[preheader.id]
			}
			break
		}
	}
	return clobbers
}

func clobbersContext(instr *Instruction, moduleCtx Value) bool {
	switch instr.opcode {
	case OpcodeCall, OpcodeCallIndirect:
		return true
	case OpcodeStore, OpcodeIstore8, OpcodeIstore16, OpcodeIstore32:
		_, ptr, _, _ := instr.StoreData()
		return !moduleCtx.Valid() || ptr == moduleCtx
	}
	return false
}

// contextLoadLike returns a context load already in blk that reads what instr does, or nil.
// isConstValue reports whether v is an integer constant.
func (b *builder) isConstValue(v Value) bool {
	def := b.InstructionOfValue(b.resolveAlias(v))
	return def != nil && def.opcode == OpcodeIconst
}

// cloneConstOperand gives sub, a subtraction of a constant being hoisted into target, a copy of the constant
// there, since the constant itself stays where it is defined.
func (b *builder) cloneConstOperand(target *basicBlock, sub *Instruction) {
	k := b.InstructionOfValue(b.resolveAlias(sub.v2))
	if k == nil || k.opcode != OpcodeIconst {
		return
	}
	clone := b.AllocateInstruction()
	if k.typ == TypeI32 {
		clone.AsIconst32(uint32(k.ConstantVal()))
	} else {
		clone.AsIconst64(k.ConstantVal())
	}
	clone.rValue = b.allocateValue(k.typ).setInstructionID(clone.id)
	target.insertBeforeTerminator(clone)
	sub.v2 = clone.rValue
}

// limitLike returns the subtraction already in blk of the same constant from the same value as sub, if any.
func (b *builder) limitLike(blk *basicBlock, sub *Instruction) *Instruction {
	k := b.InstructionOfValue(b.resolveAlias(sub.v2))
	if k == nil || k.opcode != OpcodeIconst {
		return nil
	}
	for cur := blk.rootInstr; cur != nil; cur = cur.next {
		if cur.opcode != OpcodeIsub || cur == sub || b.resolveAlias(cur.v) != b.resolveAlias(sub.v) {
			continue
		}
		if c := b.InstructionOfValue(b.resolveAlias(cur.v2)); c != nil && c.opcode == OpcodeIconst && c.ConstantVal() == k.ConstantVal() && c.typ == k.typ {
			return cur
		}
	}
	return nil
}

func (blk *basicBlock) contextLoadLike(instr *Instruction) *Instruction {
	for cur := blk.rootInstr; cur != nil; cur = cur.next {
		if cur.contextLoad && cur.opcode == instr.opcode && cur.v == instr.v && cur.u1 == instr.u1 && cur.typ == instr.typ {
			return cur
		}
	}
	return nil
}

// floatConstTarget returns where a float constant in the loop headed by header goes: the block before
// the loop, where it is computed once into a register the loop reads instead of loaded from the constant
// pool at each use. Where the loop needs that register for something else, the allocator rematerializes
// the constant rather than spilling it. It goes no further out, since a constant live across a whole
// loop nest measured worse than one live across its own loop, and it stays put once maxHoistedFloatConsts
// went before it: a loop using more constants than there are registers keeps few of them in one, and
// loads the rest before the loop for nothing. hoisted counts them by header.
func (b *builder) floatConstTarget(header *basicBlock, hoisted map[*basicBlock]int) *basicBlock {
	preheader := b.dominators[header.id]
	if preheader == nil || preheader == header || preheader.invalid || hoisted[header] == maxHoistedFloatConsts {
		return nil
	}
	hoisted[header]++
	return preheader
}

// maxHoistedFloatConsts is how many float constants floatConstTarget moves out of one loop: half the
// vector registers amd64 has. It counts integer constants separately.
const maxHoistedFloatConsts = 8

// hoistIntConsts is whether a loop's integer constants that take more than one instruction to make are
// computed before the loop, pinned like float ones: on arm64, whose instructions take only small
// immediates, a 32-bit hash multiplier is a movz and a movk at each use otherwise. amd64 takes 32-bit
// immediates in place.
var hoistIntConsts = runtime.GOARCH == "arm64"

// everyIteration reports whether blk, in the loop headed by header, runs on every iteration: it dominates
// every block the loop branches back from, but for the path that yields to the runtime when it asks. A
// constant used only in some of a dispatch loop's cases, kept in a register across the whole loop, costs
// the other cases a register.
func (b *builder) everyIteration(blk, header *basicBlock) bool {
	for _, p := range header.preds {
		pred := p.blk
		if !b.isDominatedBy(pred, header) || b.isDominatedBy(pred, blk) || yields(pred) {
			continue
		}
		return false
	}
	return true
}

// yields reports whether blk yields to the runtime: the slow path of a loop's preemption check.
func yields(blk *basicBlock) bool {
	for cur := blk.rootInstr; cur != nil; cur = cur.next {
		if cur.opcode == OpcodeYield {
			return true
		}
	}
	return false
}

// multiInstructionConst reports whether the integer constant instr takes more than one instruction to
// make on arm64: more than one of its 16-bit halfwords is neither all zeros nor, all at once, all ones.
// A logical immediate takes one too, which this counts as more: one such kept in a register costs only
// the register.
func multiInstructionConst(instr *Instruction) bool {
	v, chunks := instr.ConstantVal(), 4
	if instr.typ == TypeI32 {
		v, chunks = uint64(uint32(v)), 2
	}
	zero, ones := 0, 0
	for k := 0; k < chunks; k++ {
		switch uint16(v >> (16 * k)) {
		case 0:
			zero++
		case 0xffff:
			ones++
		}
	}
	return zero < chunks-1 && ones < chunks-1
}

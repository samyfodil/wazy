package ssa

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

	for _, blk := range b.reversePostOrderedBasicBlocks {
		if blk.invalid || loopOf[blk.id] == nil {
			continue
		}
		for cur := blk.rootInstr; cur != nil; {
			next := cur.next
			if target := b.licmTarget(cur, loopOf[blk.id], loopOf, defBlk); target != nil {
				blk.removeInstruction(cur)
				target.insertBeforeTerminator(cur)
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

	loopOf := make([]*basicBlock, b.BlockIDMax())
	var stack []*basicBlock
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
	return loopOf
}

// licmDefBlk maps each Value to the block defining it. Aliases left behind by
// passRedundantPhiEliminationOpt are resolved on the way, because this runs
// before the dead code pass that would otherwise have done it.
func (b *builder) licmDefBlk() []*basicBlock {
	defBlk := make([]*basicBlock, b.nextValueID)
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
func (b *builder) licmTarget(instr *Instruction, header *basicBlock, loopOf, defBlk []*basicBlock) *basicBlock {
	if !hoistableFromLoop(instr) {
		return nil
	}
	var target *basicBlock
	for header != nil {
		if !b.invariantIn(instr, header, defBlk) {
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
func (b *builder) invariantIn(instr *Instruction, header *basicBlock, defBlk []*basicBlock) bool {
	v1, v2, v3, vs := instr.Args()
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
	// A constant is rematerialized at each use by the backend.
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

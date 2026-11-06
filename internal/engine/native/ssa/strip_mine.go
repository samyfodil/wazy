package ssa

import "math/bits"

// StripMine makes the loop headed by header, entered from preheader and counted by
// lc, check for preemption once every chunk iterations at no cost to the others.
//
// It needs a test that ends the loop by comparing a value of the counter against
// a bound n that does not change in it: the loop's back edge, or the first branch
// in body, the block the header's check falls through to. That test then compares
// against a limit instead, which the loop carries in a new header parameter: the
// value the counter reaches in chunk iterations, or n itself once that is nearer.
// When the counter reaches the limit, a refill block makes the original test,
// which leaves the loop, or goes on to check for preemption and compute the next
// limit:
//
//	preheader: lim = limit(first value tested, n)
//	test:      x cc lim, and on to the loop as before; otherwise refill
//	refill:    x cc n, and on to more; otherwise leave as before
//	more:      lim' = limit(x, n); yield if preemption is requested; loop with lim'
//
// The loop compares against lim just as it compared against n, so the iterations
// in between pay nothing for the check. yield emits, into the current block, the
// slow path that returns to Go and then jumps to target with args.
//
// The test must go on with the loop when the counter is != n, with a step whose
// magnitude is a power of two; or when it is <u n, stepping up by one; or >u n,
// stepping down by one. StripMine changes nothing and returns false when the loop does not have
// that shape.
func (b *builder) StripMine(header, preheader, body BasicBlock, lc LoopCounter, chunk uint64, yield func(target BasicBlock, args Values)) bool {
	h, pre := header.(*basicBlock), preheader.(*basicBlock)
	t, ok := b.findExitTest(h, body.(*basicBlock), lc)
	if !ok {
		return false
	}
	if !t.backEdge && t.nIsConst && t.nConst == 0 {
		// Counting down to zero, tested first: the backend folds that test into
		// a masked check, which costs nothing, not even on the way in.
		return false
	}

	typ := lc.Param.Type()
	mask := widthMask(typ)
	up := lc.Step < mask>>1+1
	dist := lc.Step
	if !up {
		dist = -lc.Step & mask
	}
	switch {
	case t.cc == IntegerCmpCondNotEqual && dist&(dist-1) == 0:
	// With a step of one the counter meets the bound on its way; a longer step
	// can carry it past the top of the range and around, never reaching it.
	case t.cc == IntegerCmpCondUnsignedLessThan && up && dist == 1:
	case t.cc == IntegerCmpCondUnsignedGreaterThan && !up && dist == 1:
	default:
		return false
	}
	// span is how far the counter moves in chunk iterations. It stays well inside
	// the type, so the limit computation below cannot wrap past the bound.
	if chunk == 0 || bits.Len64(chunk)+bits.Len64(dist) > int(typ.Bits())-2 {
		return false
	}
	lm := limitMaker{b: b, typ: typ, up: up, cc: t.cc, step: lc.Step, span: chunk * dist, dist: dist}

	entry := loopEntry(h, pre)
	if entry == nil {
		return false
	}
	var n0 Value
	if !t.nIsConst {
		if n0, ok = b.valueOnEntry(h, entry, t.n); !ok {
			return false
		}
	}

	saved := b.currentBB
	defer func() { b.currentBB = saved }()

	// The limit for the first test, computed before the loop is entered.
	pre.removeInstruction(entry)
	b.currentBB = pre
	x0 := lc.Start
	if lc.isNext(t.x) {
		x0 = b.AllocateInstruction().AsIadd(lc.Start, b.iconst(typ, lc.Step)).Insert(b).Return()
	}
	if t.nIsConst {
		n0 = b.iconst(typ, t.nConst)
	}
	lim0 := lm.emit(x0, n0)
	pre.appendRaw(entry)
	entry.addArgumentBranchInst(b, lim0)

	// The new parameter, and what every other way into the header passes for it:
	// the limit it already has.
	lim := h.AddParam(b, typ)
	for _, p := range h.preds {
		if p.branch != entry {
			p.branch.addArgumentBranchInst(b, lim)
		}
	}

	refill, more, slow := b.allocateBasicBlock(), b.allocateBasicBlock(), b.allocateBasicBlock()
	var args Values
	if t.backEdge {
		// The next iteration's values, before lim joined them above.
		vs := t.branch.vs.View()
		for _, v := range vs[:len(vs)-1] {
			args = args.Append(&b.varLengthPool, v)
		}
		// blk: x cc lim -> header; else -> refill. refill: the original test,
		// on to more or out through the original exit.
		origOpcode := t.branch.opcode
		b.currentBB = t.blk
		hot := b.AllocateInstruction().AsIcmp(t.x, lim, t.cc).Insert(b)
		t.blk.removeInstruction(hot)
		t.blk.insertBefore(hot, t.branch)
		t.branch.opcode, t.branch.v = OpcodeBrnz, hot.Return()
		t.blk.removeInstruction(t.exit)
		b.AllocateInstruction().AsJump(ValuesNil, refill).Insert(b)
		b.moveEdge(t.exit, t.blk, refill)

		b.currentBB = refill
		cont := b.AllocateInstruction()
		cont.opcode, cont.v, cont.rValue = origOpcode, b.recompute(t.cond), Value(more.ID())
		cont.Insert(b)
		refill.appendRaw(t.exit)
	} else {
		for _, p := range h.params.View() {
			if p != lim {
				args = args.Append(&b.varLengthPool, p)
			}
		}
		// blk: x !cc lim -> refill; else on into the loop. refill: the original
		// test, out through its exit or on to more.
		b.currentBB = t.blk
		stop := b.AllocateInstruction().AsIcmp(t.x, lim, t.cc.Inverted()).Insert(b)
		toRefill := b.AllocateInstruction().AsBrnz(stop.Return(), ValuesNil, refill).Insert(b)
		t.blk.removeInstruction(stop)
		t.blk.removeInstruction(toRefill)
		t.blk.insertBefore(stop, t.branch)
		t.blk.insertBefore(toRefill, t.branch)
		t.blk.removeInstruction(t.branch)

		// The exit, taken over by a branch of refill's own: blocks are laid out
		// by their first instructions, and the original is older than the loop
		// body, which would put refill in the way of the loop.
		b.currentBB = refill
		leave := b.AllocateInstruction()
		leave.opcode, leave.v, leave.rValue = t.branch.opcode, b.recompute(t.cond), t.branch.rValue
		leave.vs = b.copyValues(t.branch.vs)
		refill.appendRaw(leave)
		b.moveEdge(t.branch, t.blk, refill)
		exit := b.basicBlock(BasicBlockID(t.branch.rValue))
		for i := range exit.preds {
			if exit.preds[i].branch == t.branch {
				exit.preds[i].branch = leave
			}
		}
		b.AllocateInstruction().AsJump(ValuesNil, more).Insert(b)
	}

	// more: the next limit, and the check the loop no longer makes per iteration.
	b.currentBB = more
	n := t.n
	if t.nIsConst {
		n = b.iconst(typ, t.nConst)
	}
	next := lm.emit(t.x, n)
	args = args.Append(&b.varLengthPool, next)
	requested := b.AllocateInstruction().AsPreemptRequested().Insert(b).Return()
	b.AllocateInstruction().AsBrnz(requested, ValuesNil, slow).Insert(b)
	// It gains two predecessors, which pass all its parameters. Each gets its own
	// copy of them: passes edit a branch's arguments in place.
	h.sealed = false
	b.AllocateInstruction().AsJump(b.copyValues(args), h).Insert(b)
	b.currentBB = slow
	yield(h, b.copyValues(args))
	h.sealed = true

	b.Seal(refill)
	b.Seal(more)
	b.Seal(slow)
	return true
}

// CheckOnEntry makes the loop headed by header, entered from preheader, check for
// preemption in preheader, once each time it is entered. yield emits the slow path,
// as for StripMine, which leaves the preheader in the shape this needs. It changes
// nothing and returns false when preheader does not end by jumping to header.
func (b *builder) CheckOnEntry(header, preheader BasicBlock, yield func(target BasicBlock, args Values)) bool {
	h, pre := header.(*basicBlock), preheader.(*basicBlock)
	entry := loopEntry(h, pre)
	if entry == nil {
		return false
	}
	saved := b.currentBB
	defer func() { b.currentBB = saved }()

	pre.removeInstruction(entry)
	b.currentBB = pre
	requested := b.AllocateInstruction().AsPreemptRequested().Insert(b).Return()
	slow := b.allocateBasicBlock()
	b.AllocateInstruction().AsBrnz(requested, ValuesNil, slow).Insert(b)
	pre.appendRaw(entry)

	b.currentBB = slow
	h.sealed = false
	yield(h, b.copyValues(entry.vs))
	h.sealed = true
	b.Seal(slow)
	return true
}

// loopEntry returns the jump that ends pre and enters the loop headed by h, or nil.
func loopEntry(h, pre *basicBlock) *Instruction {
	for _, p := range h.preds {
		if p.blk == pre && pre.currentInstr == p.branch {
			return p.branch
		}
	}
	return nil
}

// exitTest is a test that can end a loop. branch, in blk, is followed by exit, a
// jump. cond, which branch branches on, is the test, and the loop goes on when
// x cc n holds, x being a value of the counter and n either a value or, if
// nIsConst, the constant nConst. A back-edge test branches to the header and
// leaves through exit; the other leaves through branch and goes on through exit.
type exitTest struct {
	blk          *basicBlock
	branch, exit *Instruction
	backEdge     bool
	cond, x, n   Value
	nConst       uint64
	nIsConst     bool
	cc           IntegerCmpCond
}

// findExitTest finds a test of lc's counter that can end the loop: its back
// edge, if that is a conditional branch, or else the first branch in body, if
// nothing before it has an effect.
func (b *builder) findExitTest(h, body *basicBlock, lc LoopCounter) (t exitTest, ok bool) {
	// goOnIfTrue is whether the loop goes on when cond is non-zero.
	var goOnIfTrue bool
	if back := lc.BackEdge; back.opcode == OpcodeBrnz || back.opcode == OpcodeBrz {
		t.branch, t.backEdge = back, true
		for _, p := range h.preds {
			if p.branch == back {
				t.blk = p.blk
			}
		}
		goOnIfTrue = back.opcode == OpcodeBrnz
	} else {
		cur := body.rootInstr
		for cur != nil && cur.opcode != OpcodeBrnz && cur.opcode != OpcodeBrz {
			if cur.sideEffect() != sideEffectNone {
				return
			}
			cur = cur.next
		}
		if cur == nil || BasicBlockID(cur.rValue) == h.id {
			return
		}
		t.branch, t.blk = cur, body
		goOnIfTrue = cur.opcode == OpcodeBrz // the branch leaves the loop
	}
	t.exit = t.branch.next
	if t.blk == nil || t.exit == nil || t.exit.opcode != OpcodeJump || t.blk.currentInstr != t.exit {
		return
	}

	t.cond = b.resolveAlias(t.branch.v)
	var x, y Value
	var cc IntegerCmpCond
	if def := b.InstructionOfValue(t.cond); def != nil && def.opcode == OpcodeIcmp {
		x, y, cc = def.IcmpData()
		x, y = b.resolveAlias(x), b.resolveAlias(y)
	} else {
		// A branch on the counter itself tests it against zero.
		x, y, cc = t.cond, ValueInvalid, IntegerCmpCondNotEqual
		t.nIsConst = true
	}
	if !goOnIfTrue {
		cc = cc.Inverted()
	}
	counter := func(v Value) bool { return v == lc.Param || (t.backEdge && lc.isNext(v)) }
	switch {
	case counter(x):
	case y.Valid() && counter(y):
		x, y, cc = y, x, cc.Swapped()
	default:
		return
	}
	if !x.Type().IsInt() || x.Type() != lc.Param.Type() {
		return
	}
	if c, isConst := b.constantOf(y); isConst {
		t.nConst, t.nIsConst = c, true
	}
	t.x, t.n, t.cc = x, y, cc
	return t, true
}

// valueOnEntry returns what n, a bound the loop headed by h compares against, is
// when the loop is entered through entry, if n is a parameter of h that every
// other way in passes back unchanged.
func (b *builder) valueOnEntry(h *basicBlock, entry *Instruction, n Value) (Value, bool) {
	for i, p := range h.params.View() {
		if p != n {
			continue
		}
		for _, pred := range h.preds {
			if pred.branch != entry && b.resolveAlias(pred.branch.vs.View()[i]) != n {
				return ValueInvalid, false
			}
		}
		return b.resolveAlias(entry.vs.View()[i]), true
	}
	return ValueInvalid, false
}

// limitMaker emits the limit a strip-mined loop compares its counter against.
type limitMaker struct {
	b          *builder
	typ        Type
	up         bool
	cc         IntegerCmpCond
	step, dist uint64
	span       uint64
}

// emit emits, into the current block, the limit for a counter at x: x moved by
// span, if that is short of n, and n otherwise.
func (m limitMaker) emit(x, n Value) Value {
	b := m.b
	ins := func() *Instruction { return b.AllocateInstruction() }
	var rem Value
	switch c, isConst := b.constantOf(n); {
	case m.up:
		rem = ins().AsIsub(n, x).Insert(b).Return()
	case isConst && c == 0:
		rem = x // counting down to zero
	default:
		rem = ins().AsIsub(x, n).Insert(b).Return()
	}
	far := ins().AsIcmp(rem, b.iconst(m.typ, m.span), IntegerCmpCondUnsignedGreaterThan).Insert(b).Return()
	switch m.cc {
	case IntegerCmpCondNotEqual:
		if m.dist > 1 {
			// A counter whose distance to n is no multiple of the step never
			// reaches it: keep stepping by span.
			off := ins().AsBand(rem, b.iconst(m.typ, m.dist-1)).Insert(b).Return()
			misaligned := ins().AsIcmp(off, b.iconst(m.typ, 0), IntegerCmpCondNotEqual).Insert(b).Return()
			bor := ins()
			bor.AsBor(far, misaligned)
			far = bor.Insert(b).Return()
		}
	default:
		// x <u n (or >u n) is what keeps the loop going; past n, the limit is n.
		inRange := ins().AsIcmp(x, n, m.cc).Insert(b).Return()
		far = ins().AsBand(far, inRange).Insert(b).Return()
	}
	moved := ins().AsIadd(x, b.iconst(m.typ, m.step*(m.span/m.dist)&widthMask(m.typ))).Insert(b).Return()
	return ins().AsSelect(far, moved, n).Insert(b).Return()
}

// iconst emits an integer constant of typ into the current block.
func (b *builder) iconst(typ Type, v uint64) Value {
	i := b.AllocateInstruction()
	if typ.Bits() == 32 {
		i.AsIconst32(uint32(v))
	} else {
		i.AsIconst64(v)
	}
	return i.Insert(b).Return()
}

// recompute returns cond, the condition of a loop's exit test, computed again in
// the current block when it is a compare: the loop's own copy then has no use
// left on the path it takes every iteration.
func (b *builder) recompute(cond Value) Value {
	def := b.InstructionOfValue(cond)
	if def == nil || def.opcode != OpcodeIcmp {
		return cond
	}
	x, y, c := def.IcmpData()
	return b.AllocateInstruction().AsIcmp(x, y, c).Insert(b).Return()
}

// copyValues returns a copy of vs with a backing of its own.
func (b *builder) copyValues(vs Values) Values {
	view := vs.View()
	out := b.varLengthPool.Allocate(len(view))
	return out.Append(&b.varLengthPool, view...)
}

// ReturnToHeader makes slow, the slow path of a check heading the loop at header,
// which goes on into the loop's body, return to the header instead with the values
// the loop carries, so that the header runs its check again. A masked check needs
// that: the backend may fold the loop's exit test into the check's out-of-line half
// (foldZeroExit in each backend), and the body, which goes on past where the test
// was, would skip it.
func (b *builder) ReturnToHeader(slow, header BasicBlock) {
	s, h := slow.(*basicBlock), header.(*basicBlock)
	jump := s.currentInstr
	removeEdge(s, b.basicBlock(BasicBlockID(jump.rValue)), jump)
	s.removeInstruction(jump)
	saved := b.currentBB
	b.currentBB = s
	h.sealed = false
	b.AllocateInstruction().AsJump(b.copyValues(h.params), h).Insert(b)
	h.sealed = true
	b.currentBB = saved
}

// insertBefore links instr into bb right before at.
func (bb *basicBlock) insertBefore(instr, at *Instruction) {
	instr.prev, instr.next = at.prev, at
	if at.prev != nil {
		at.prev.next = instr
	} else {
		bb.rootInstr = instr
	}
	at.prev = instr
}

// appendRaw links instr at the end of bb without recording an edge: for a branch
// whose edge was recorded already.
func (bb *basicBlock) appendRaw(instr *Instruction) {
	instr.prev, instr.next = bb.currentInstr, nil
	if bb.currentInstr != nil {
		bb.currentInstr.next = instr
	} else {
		bb.rootInstr = instr
	}
	bb.currentInstr = instr
}

// moveEdge records that branch, which went from from, now goes from to.
func (b *builder) moveEdge(branch *Instruction, from, to *basicBlock) {
	target := b.basicBlock(BasicBlockID(branch.rValue))
	for i := range target.preds {
		if target.preds[i].branch == branch {
			target.preds[i].blk = to
		}
	}
	for i, s := range from.success {
		if s == target {
			from.success = append(from.success[:i], from.success[i+1:]...)
			break
		}
	}
	to.success = append(to.success, target)
}

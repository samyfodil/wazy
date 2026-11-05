package arm64

// threadJumps sends each branch to a block that only branches on, an edge block register allocation
// left empty say, straight to where that block goes, and drops a branch to the code right after it. Both
// save an instruction on every pass over the edge: a loop's back edge often goes through such a block.
func (m *machine) threadJumps() {
	onward := m.threadTargets[:0]
	defer func() { m.threadTargets = onward[:0] }()
	at := func(l label) label {
		if int(l) < len(onward) {
			return onward[l]
		}
		return labelInvalid
	}
	// A label starts at its position's first instruction, which is a nop0 that may or may not carry it.
	// Clearing a map costs as much as the most it ever held: after a large function, a small one makes a
	// new one.
	begins := m.threadBegins
	if len(begins) > 4*int(m.nextLabel)+64 {
		begins = make(map[*instruction]label, m.nextLabel)
		m.threadBegins = begins
	} else {
		clear(begins)
	}
	for l := label(0); l < m.nextLabel; l++ {
		if pos := m.labelPositionPool.Get(int(l)); pos != nil && pos.begin != nil {
			begins[pos.begin] = l
		}
	}
	// A label whose next instruction, past other labels, is a branch goes where that branch does.
	for cur := m.rootInstr; cur != nil; cur = cur.next {
		l, ok := begins[cur]
		if !ok {
			continue
		}
		next := cur.next
		for next != nil && next.kind == nop0 {
			next = next.next
		}
		if next != nil && next.kind == br {
			for int(l) >= len(onward) {
				onward = append(onward, labelInvalid)
			}
			onward[l] = next.brLabel()
		}
	}
	// final follows a label to the end of its chain of branches, stopping on a cycle.
	final := func(l label) label {
		for hops := 0; hops < 8; hops++ {
			n := at(l)
			if n == labelInvalid || n == l {
				break
			}
			l = n
		}
		return l
	}
	// The branches and conditional branches, which every step below takes from here rather than walking
	// the whole function again.
	jumps, condJumps := m.threadJumpsBuf[:0], m.threadCondJumpsBuf[:0]
	for cur := m.rootInstr; cur != nil; cur = cur.next {
		switch {
		case cur.kind == br:
			cur.u1 = uint64(final(cur.brLabel()))
			jumps = append(jumps, cur)
		case cur.kind == condBr && !cur.condBrOffsetResolved():
			cur.u2 = cur.u2&^0xffffffff | uint64(final(cur.condBrLabel()))
			condJumps = append(condJumps, cur)
		}
	}
	for _, targets := range m.jmpTableTargets[:m.jmpTableTargetsNext] {
		for i, t := range targets {
			targets[i] = uint32(final(label(t)))
		}
	}
	// A branch to a label the next instructions are anyway is not needed. Dropping one can leave the
	// branch before it falling through to its target in turn.
	// Taking them last first sees one falling through at once.
	for i := len(jumps) - 1; i >= 0; i-- {
		cur := jumps[i]
		for next := cur.next; next != nil && next.kind == nop0; next = next.next {
			if l, ok := begins[next]; ok && l == cur.brLabel() {
				cur.kind = nop0
				cur.u1 = uint64(labelInvalid)
				break
			}
		}
	}
	// A conditional branch over an unconditional one, to the code right after both, becomes the opposite
	// conditional branch to where the unconditional one goes: a loop's back edge often falls through this
	// way, one branch shorter. A label between the two that nothing branches to any more does not stop it;
	// one a branch still goes to does, as does the return block's.
	// The labels a branch or a jump table still goes to, after threading: one of them between the two
	// would lose the branch it falls through to.
	referenced := m.threadReferenced[:0]
	if n := int(m.nextLabel); cap(referenced) < n {
		referenced = make([]bool, n)
	} else {
		referenced = referenced[:n]
		clear(referenced)
	}
	mark := func(l label) {
		if int(l) < len(referenced) {
			referenced[l] = true
		}
	}
	for _, cur := range jumps {
		if cur.kind == br {
			mark(cur.brLabel())
		}
	}
	for _, cur := range condJumps {
		mark(cur.condBrLabel())
	}
	for _, targets := range m.jmpTableTargets[:m.jmpTableTargetsNext] {
		for _, t := range targets {
			mark(label(t))
		}
	}
	isReferenced := func(l label) bool { return int(l) < len(referenced) && referenced[l] }
	for _, cur := range condJumps {
		j := cur.next
		for j != nil && j.kind == nop0 {
			// A nop0 without a label of its own reads as label 0, the entry block's, which nothing branches to.
			own := j.nop0Label()
			if l, ok := begins[j]; ok && isReferenced(l) || own != 0 && own != labelInvalid && isReferenced(own) || j == m.returnLabelPos.begin {
				break
			}
			j = j.next
		}
		if j == nil || j.kind != br {
			continue
		}
		for next := j.next; next != nil && next.kind == nop0; next = next.next {
			if l, ok := begins[next]; ok && l == cur.condBrLabel() {
				cur.u1 = invertTrapCond(cur.condBrCond()).asUint64()
				cur.u2 = cur.u2&^0xffffffff | uint64(j.brLabel())
				j.kind = nop0
				j.u1 = uint64(labelInvalid)
				break
			}
		}
	}
	m.copyReturns(jumps)
	m.threadJumpsBuf, m.threadCondJumpsBuf, m.threadReferenced = jumps[:0], condJumps[:0], referenced[:0]
}

// copyReturns turns a branch to an epilogue, a few instructions ending in ret, into a copy of it: the
// path that branched returns on its own, one branch shorter.
func (m *machine) copyReturns(jumps []*instruction) {
	const maxEpilogue = 10
	var epilogue [maxEpilogue]*instruction
	for _, cur := range jumps {
		if cur.kind != br {
			continue // dropped
		}
		pos := m.labelPositionPool.Get(int(cur.brLabel()))
		if pos == nil || pos.begin == nil {
			continue
		}
		n, returns := 0, false
	scan:
		for i := pos.begin; i != nil && n < maxEpilogue; i = i.next {
			switch i.kind {
			case nop0:
			case aluRRImm12, aluRRR, movZ, movN, movK, uLoad64, fpuLoad128, loadP64, loadP128:
				if !epilogueInstr(i) {
					break scan
				}
				epilogue[n] = i
				n++
			case ret:
				epilogue[n] = i
				n++
				returns = true
				break scan
			default:
				break scan
			}
		}
		if !returns {
			continue
		}
		prev, next := cur.prev, cur.next
		*cur = *epilogue[0]
		cur.prev, cur.next = prev, next
		for _, e := range epilogue[1:n] {
			c := m.allocateInstr()
			*c = *e
			c.prev, c.next = nil, nil
			cur = linkInstr(cur, c)
		}
		linkInstr(cur, next)
	}
}

// epilogueInstr reports whether i is one an epilogue is made of: a stack pointer adjustment, the constant
// a large one is made of, or a register restored from the stack.
func epilogueInstr(i *instruction) bool {
	switch i.kind {
	case aluRRImm12, aluRRR:
		return i.rd == spVReg
	case movZ, movN, movK:
		return i.rd == tmpRegVReg
	}
	return i.getAmode().rn == spVReg
}

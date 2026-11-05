package amd64

// threadJumps sends each jump to a block that only jumps on, an edge block register allocation left
// empty say, straight to where that block goes, and drops a jump to the code right after it. Both save
// an instruction on every pass over the edge: a loop's back edge often goes through such a block.
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
	begins := m.threadBegins
	clear(begins)
	for l := label(0); l < m.nextLabel; l++ {
		if pos := m.labelPositionPool.Get(int(l)); pos != nil && pos.begin != nil {
			begins[pos.begin] = l
		}
	}
	isLabel := func(cur *instruction) (label, bool) {
		l, ok := begins[cur]
		return l, ok
	}
	// A label whose next instruction, past other labels, is a jump, goes where that jump does.
	for cur := m.rootInstr; cur != nil; cur = cur.next {
		if cur.kind != nop0 {
			continue
		}
		l, ok := isLabel(cur)
		if !ok {
			continue
		}
		next := cur.next
		for next != nil && next.kind == nop0 {
			next = next.next
		}
		if next != nil && next.kind == jmp && next.op1.kind == operandKindLabel {
			for int(l) >= len(onward) {
				onward = append(onward, labelInvalid)
			}
			onward[l] = next.op1.label()
		}
	}
	// final follows a label to the end of its chain of jumps, stopping on a cycle.
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
	for cur := m.rootInstr; cur != nil; cur = cur.next {
		if (cur.kind == jmp || cur.kind == jmpIf) && cur.op1.kind == operandKindLabel {
			if t := final(cur.op1.label()); t != cur.op1.label() {
				cur.op1 = newOperandLabel(t)
			}
		}
	}
	for _, targets := range m.jmpTableTargets[:m.jmpTableTargetsNext] {
		for i, t := range targets {
			targets[i] = uint32(final(label(t)))
		}
	}
	// A jump to a label the next instructions are anyway is not needed. Dropping one can leave the jump
	// before it falling through to its target in turn.
	for dropped := true; dropped; {
		dropped = false
		for cur := m.rootInstr; cur != nil; cur = cur.next {
			if cur.kind != jmp || cur.op1.kind != operandKindLabel {
				continue
			}
			for next := cur.next; next != nil && next.kind == nop0; next = next.next {
				if l, ok := isLabel(next); ok && l == cur.op1.label() {
					cur.kind = nop0
					cur.u1 = uint64(labelInvalid)
					dropped = true
					break
				}
			}
		}
	}
	// A conditional jump over an unconditional one, to the code right after both, becomes the opposite
	// conditional jump to where the unconditional one goes: a loop's back edge often falls through this
	// way, one jump shorter.
	for cur := m.rootInstr; cur != nil; cur = cur.next {
		if cur.kind != jmpIf || cur.op1.kind != operandKindLabel {
			continue
		}
		j := cur.next
		if j == nil || j.kind != jmp || j.op1.kind != operandKindLabel {
			continue
		}
		for next := j.next; next != nil && next.kind == nop0; next = next.next {
			if l, ok := isLabel(next); ok && l == cur.op1.label() {
				cur.u1 = uint64(cond(cur.u1).invert())
				cur.op1 = j.op1
				j.kind = nop0
				j.u1 = uint64(labelInvalid)
				break
			}
		}
	}
	m.copyReturns()
}

// copyReturns turns a jump to an epilogue, a few instructions ending in ret, into a copy of it: the
// path that jumped returns on its own, one jump shorter.
func (m *machine) copyReturns() {
	const maxEpilogue = 8
	var epilogue [maxEpilogue]*instruction
	for cur := m.rootInstr; cur != nil; cur = cur.next {
		if cur.kind != jmp || cur.op1.kind != operandKindLabel {
			continue
		}
		pos := m.labelPositionPool.Get(int(cur.op1.label()))
		if pos == nil || pos.begin == nil {
			continue
		}
		n, returns := 0, false
	scan:
		for i := pos.begin; i != nil && n < maxEpilogue; i = i.next {
			switch i.kind {
			case nop0:
			case movRR, pop64, aluRmiR, xmmUnaryRmR:
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

// epilogueInstr reports whether i is one an epilogue is made of: rsp restored from rbp or moved up, or a
// register popped or restored from the stack.
func epilogueInstr(i *instruction) bool {
	switch i.kind {
	case movRR:
		return i.op1.reg() == rbpVReg && i.op2.reg() == rspVReg
	case aluRmiR:
		return i.op2.reg() == rspVReg
	case xmmUnaryRmR:
		return i.op1.kind == operandKindMem && i.op1.addressMode().kind() == amodeImmReg && i.op1.addressMode().base == rspVReg
	}
	return true
}

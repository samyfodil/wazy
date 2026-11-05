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
}

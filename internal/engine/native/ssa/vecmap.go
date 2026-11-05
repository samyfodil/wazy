package ssa

// VecMapRegs is how many vector registers a VecProgram's expansion has: its invariants and the
// temporaries live across its ops.
const VecMapRegs = 8

// Assign gives each op of p a vector register in [0, VecMapRegs): an invariant the register it arrives
// in, numbered as the invariant is, and every other op one free when it runs, the register of an operand
// whose last use it is first. It reports false when the program needs more registers than that.
func (p *VecProgram) Assign() ([]uint8, bool) {
	if p.Invs > VecMapRegs {
		return nil, false
	}
	last := make([]int, len(p.Ops))
	for i := range last {
		last[i] = -1
	}
	for i, op := range p.Ops {
		switch op.Kind {
		case VecOpLoad, VecOpInv:
		case VecOpAdd, VecOpSub, VecOpMul, VecOpDiv:
			last[op.A], last[op.B] = i, i
		default:
			last[op.A] = i
		}
	}
	regs := make([]uint8, len(p.Ops))
	var used [VecMapRegs]bool
	for r := 0; r < p.Invs; r++ {
		used[r] = true
	}
	free := func(op uint8, i int) {
		if p.Ops[op].Kind != VecOpInv && last[op] == i {
			used[regs[op]] = false
		}
	}
	for i, op := range p.Ops {
		if op.Kind == VecOpInv {
			regs[i] = op.Arg
			continue
		}
		// The result may take the register of operand A when this is A's last use, which is what a
		// two-operand instruction set needs, but never B's: A's value is read after B's register is written.
		r := -1
		if op.Kind != VecOpLoad && p.Ops[op.A].Kind != VecOpInv && last[op.A] == i {
			r = int(regs[op.A])
		}
		if r < 0 {
			for k := p.Invs; k < VecMapRegs; k++ {
				if !used[k] {
					r = k
					break
				}
			}
			if r < 0 {
				return nil, false
			}
		}
		switch op.Kind {
		case VecOpLoad:
		case VecOpAdd, VecOpSub, VecOpMul, VecOpDiv:
			free(op.B, i)
		}
		used[r] = true
		regs[i] = uint8(r)
	}
	return regs, true
}

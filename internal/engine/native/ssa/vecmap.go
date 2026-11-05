package ssa

// TypeInvalid is the result type of an OpcodeVecMap that does not reduce: it returns nothing.
const TypeInvalid = typeInvalid

// VecProgram is the per-element computation of an OpcodeVecMap, on lanes of type Elem. Ops run in order,
// each producing one vector of lanes, an op's A and B naming earlier ops. The last op is the element's
// value: stored through pointer Store, unless Store is negative, or folded into the returned total with
// Reduce, an integer VecOpAdd, VecOpBand, VecOpBor or VecOpBxor, when that is not zero. Every pointer
// advances one element per element, and each load reads the element its pointer is at.
type VecProgram struct {
	Elem       Type
	Ptrs, Invs int
	Store      int
	Reduce     uint8
	Ops        []VecOp
}

// VecOp kinds.
const (
	VecOpLoad = iota + 1 // Arg: pointer index
	VecOpInv             // Arg: invariant index, broadcast to every lane
	VecOpAdd
	VecOpSub
	VecOpMul
	VecOpDiv
	VecOpXor  // A ^ invariant Arg: a float sign flip, with the sign mask as the invariant
	VecOpAnd  // A & invariant Arg: a float absolute value, with the magnitude mask as the invariant
	VecOpSqrt // float
	VecOpBand // integer A & B
	VecOpBor  // integer A | B
	VecOpBxor // integer A ^ B
	VecOpShl  // integer A << Arg
	VecOpShrU // integer A >> Arg, unsigned
	VecOpShrS // integer A >> Arg, signed
)

// VecOp is one step of a VecProgram.
type VecOp struct {
	Kind, A, B, Arg uint8
}

// Binary reports whether op reads both A and B.
func (op VecOp) Binary() bool {
	switch op.Kind {
	case VecOpAdd, VecOpSub, VecOpMul, VecOpDiv, VecOpBand, VecOpBor, VecOpBxor:
		return true
	}
	return false
}

// VecMapRegs is how many vector registers a VecProgram's expansion has: its invariants, its accumulator
// when it reduces, and the temporaries live across its ops.
const VecMapRegs = 8

// Folded reports whether op i is a load the op using it reads from memory instead: a load used once, as
// the second operand of a binary op. Only a backend that folds such loads asks.
func (p *VecProgram) Folded(i int) bool {
	if p.Ops[i].Kind != VecOpLoad || i == len(p.Ops)-1 {
		return false
	}
	uses, asB := 0, false
	for _, op := range p.Ops[i+1:] {
		switch {
		case op.Binary():
			if int(op.A) == i {
				uses++
			}
			if int(op.B) == i {
				uses++
				asB = true
			}
		case op.Kind != VecOpLoad && op.Kind != VecOpInv && int(op.A) == i:
			uses++
		}
	}
	return uses == 1 && asB
}

// Assign gives each op of p a vector register in [0, VecMapRegs): an invariant the register it arrives
// in, numbered as the invariant is, and every other op one free when it runs, the register of an operand
// whose last use it is first. With fold, a load Folded reports gets none. A reducing program's
// accumulator takes register Invs. It reports false when the program needs more registers than there are.
func (p *VecProgram) Assign(fold bool) ([]uint8, bool) {
	fixed := p.Invs
	if p.Reduce != 0 {
		fixed++
	}
	if fixed > VecMapRegs {
		return nil, false
	}
	last := make([]int, len(p.Ops))
	for i := range last {
		last[i] = -1
	}
	for i, op := range p.Ops {
		switch {
		case op.Kind == VecOpLoad, op.Kind == VecOpInv:
		case op.Binary():
			last[op.A], last[op.B] = i, i
		default:
			last[op.A] = i
		}
	}
	regs := make([]uint8, len(p.Ops))
	var used [VecMapRegs]bool
	for r := 0; r < fixed; r++ {
		used[r] = true
	}
	for i, op := range p.Ops {
		if op.Kind == VecOpInv {
			regs[i] = op.Arg
			continue
		}
		if fold && p.Folded(i) {
			continue
		}
		// The result may take the register of operand A when this is A's last use, which is what a
		// two-operand instruction set needs, but never B's: A's value is read after B's register is written.
		r := -1
		if op.Kind != VecOpLoad && p.Ops[op.A].Kind != VecOpInv && last[op.A] == i {
			r = int(regs[op.A])
		}
		if r < 0 {
			for k := fixed; k < VecMapRegs; k++ {
				if !used[k] {
					r = k
					break
				}
			}
			if r < 0 {
				return nil, false
			}
		}
		if op.Binary() && p.Ops[op.B].Kind != VecOpInv && last[op.B] == i && !(fold && p.Folded(int(op.B))) {
			used[regs[op.B]] = false
		}
		used[r] = true
		regs[i] = uint8(r)
	}
	return regs, true
}

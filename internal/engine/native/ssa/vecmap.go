package ssa

// TypeInvalid is the result type of an OpcodeVecMap that does not reduce: it returns nothing.
const TypeInvalid = typeInvalid

// VecProgram is the per-element computation of an OpcodeVecMap, on lanes of type Elem. Ops run in order,
// each producing one vector of lanes, an op's A and B naming earlier ops. The last op is the element's
// value, stored through pointer Store unless Store is negative, and the program folds the values of the
// ops Reduces names into the totals it returns, in that order. Every pointer advances one element per
// element, and each load reads the element its pointer is at.
//
// In a Spaced program Elem is TypeI64 and an op W says is 4 bytes wide is an i32 in the low half of each
// lane, whatever the high half holds. Its invariants of that width arrive zero-extended, so an and with
// one leaves the high half zero.
type VecProgram struct {
	Elem       Type
	Spaced     bool
	Ptrs, Invs int
	Store      int
	Reduces    []VecReduce
	Ops        []VecOp
}

// VecReduce is one total of a VecProgram: op Root's values folded with Op, an integer VecOpAdd, VecOpBand,
// VecOpBor or VecOpBxor, which give the same bits in any order.
type VecReduce struct {
	Op, Root uint8
}

// ReduceType returns the type of total k.
func (p *VecProgram) ReduceType(k int) Type {
	if p.Ops[p.Reduces[k].Root].W == 4 {
		return TypeI32
	}
	return TypeI64
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
	// VecOpIota is, for the element j of the elements an OpcodeVecMap does, the i32 invariant Arg plus j
	// times the i32 invariant B, wrapping. The expansion keeps it in Arg's register, and B's.
	VecOpIota
	VecOpZext // A, an i32 of a Spaced program, zero-extended to i64
	VecOpMulU // the low 32 bits of each lane of A times those of B, an i64 product
)

// VecOp is one step of a VecProgram. W is how many bytes wide its value is: the size of Elem, but for the
// i32 ops of a Spaced program.
type VecOp struct {
	Kind, A, B, Arg, W uint8
}

// Binary reports whether op reads both A and B.
func (op VecOp) Binary() bool {
	switch op.Kind {
	case VecOpAdd, VecOpSub, VecOpMul, VecOpDiv, VecOpBand, VecOpBor, VecOpBxor, VecOpMulU:
		return true
	}
	return false
}

// NeedsScratch reports whether the expansion needs a register of its own: to set up an iota, to narrow
// the operands of a VecOpMulU on an instruction set whose multiplication wants them so, or to fold totals.
func (p *VecProgram) NeedsScratch() bool {
	if len(p.Reduces) > 0 {
		return true
	}
	for _, op := range p.Ops {
		if op.Kind == VecOpIota || op.Kind == VecOpMulU {
			return true
		}
	}
	return false
}

// Scratch returns the register Assign leaves for the expansion, when it needs one.
func (p *VecProgram) Scratch() uint8 { return uint8(p.Invs + len(p.Reduces)) }

// fixedReg reports whether op stays in the register its invariant Arg arrives in.
func (op VecOp) fixedReg() bool { return op.Kind == VecOpInv || op.Kind == VecOpIota }

// VecMapRegs is how many vector registers a VecProgram's expansion has: its invariants, its accumulators,
// and the temporaries live across its ops.
const VecMapRegs = 16

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
		case op.Kind != VecOpLoad && !op.fixedReg() && int(op.A) == i:
			uses++
		}
	}
	return uses == 1 && asB
}

// Assign gives each op of p a vector register in [0, VecMapRegs): an invariant, or an iota, the register
// its invariant Arg arrives in, numbered as the invariant is, and every other op one free when it runs, the
// register of an operand whose last use it is first. With fold, a load Folded reports gets none. Total k
// accumulates in register Invs+k, and Scratch is left for the expansion's own use. It reports false when
// the program needs more registers than there are.
func (p *VecProgram) Assign(fold bool) ([]uint8, bool) {
	fixed := p.Invs + len(p.Reduces)
	if p.NeedsScratch() {
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
		case op.Kind == VecOpLoad, op.fixedReg():
		case op.Binary():
			last[op.A], last[op.B] = i, i
		default:
			last[op.A] = i
		}
	}
	// A reduced op is live until the end of its copy of the program, where it is folded in.
	for _, red := range p.Reduces {
		last[red.Root] = len(p.Ops)
	}
	regs := make([]uint8, len(p.Ops))
	var used [VecMapRegs]bool
	for r := 0; r < fixed; r++ {
		used[r] = true
	}
	for i, op := range p.Ops {
		if op.fixedReg() {
			regs[i] = op.Arg
			continue
		}
		if fold && p.Folded(i) {
			continue
		}
		// The result may take the register of operand A when this is A's last use, which is what a
		// two-operand instruction set needs, but never B's: A's value is read after B's register is written.
		r := -1
		if op.Kind != VecOpLoad && !p.Ops[op.A].fixedReg() && last[op.A] == i {
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
		if op.Binary() && !p.Ops[op.B].fixedReg() && last[op.B] == i && !(fold && p.Folded(int(op.B))) {
			used[regs[op.B]] = false
		}
		used[r] = true
		regs[i] = uint8(r)
	}
	return regs, true
}

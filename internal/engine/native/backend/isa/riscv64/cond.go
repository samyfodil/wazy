package riscv64

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// RISC-V has no condition-code register. A comparison is either fused straight
// into a branch (the six B-type instructions compare two registers and jump in
// one go) or materialized as a 0/1 in a general register by slt/sltu/feq/flt/fle.
//
// That removes the compare-then-branch-on-flags dance the other backends need,
// but it also means only six comparison directions exist in hardware: eq, ne,
// lt, ge, ltu, geu. The four "greater" forms are obtained by swapping the
// operands, which is what the `swap` result below is for.

// condFlagFromSSAIntegerCmpCond maps an SSA integer condition to the branch
// instruction that implements it, and reports whether the operands must be
// exchanged first.
func condFlagFromSSAIntegerCmpCond(c ssa.IntegerCmpCond) (flag condFlag, swap bool) {
	switch c {
	case ssa.IntegerCmpCondEqual:
		return condEQ, false
	case ssa.IntegerCmpCondNotEqual:
		return condNE, false
	case ssa.IntegerCmpCondSignedLessThan:
		return condLT, false
	case ssa.IntegerCmpCondSignedGreaterThanOrEqual:
		return condGE, false
	case ssa.IntegerCmpCondSignedGreaterThan:
		// x > y is y < x.
		return condLT, true
	case ssa.IntegerCmpCondSignedLessThanOrEqual:
		// x <= y is y >= x.
		return condGE, true
	case ssa.IntegerCmpCondUnsignedLessThan:
		return condLTU, false
	case ssa.IntegerCmpCondUnsignedGreaterThanOrEqual:
		return condGEU, false
	case ssa.IntegerCmpCondUnsignedGreaterThan:
		return condLTU, true
	case ssa.IntegerCmpCondUnsignedLessThanOrEqual:
		return condGEU, true
	}
	panic(fmt.Sprintf("BUG: unhandled integer comparison condition %s", c))
}

// lowerFcmpToReg materializes an SSA float comparison as 0 or 1 in rd.
//
// The three hardware comparisons are feq, flt and fle, all of which are
// *quiet* in the IEEE sense that matters here: they return false when either
// operand is NaN, which is exactly what wasm's eq/lt/le/gt/ge require. The two
// missing directions come from swapping the operands, and ne is the negation
// of eq -- a negation that is only correct because feq already answers false
// for NaN, so `1 - feq` yields the true that wasm's ne demands for unordered
// operands.
func (m *machine) lowerFcmpToReg(x, y ssa.Value, c ssa.FloatCmpCond, rd regalloc.VReg) {
	rx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	ry := m.getOperand_NR(m.compiler.ValueDefinition(y))
	_64bit := x.Type() == ssa.TypeF64

	var op fpuCmpOp
	var negate bool
	switch c {
	case ssa.FloatCmpCondEqual:
		op = fpuCmpOpEq
	case ssa.FloatCmpCondNotEqual:
		op, negate = fpuCmpOpEq, true
	case ssa.FloatCmpCondLessThan:
		op = fpuCmpOpLt
	case ssa.FloatCmpCondLessThanOrEqual:
		op = fpuCmpOpLe
	case ssa.FloatCmpCondGreaterThan:
		op, rx, ry = fpuCmpOpLt, ry, rx
	case ssa.FloatCmpCondGreaterThanOrEqual:
		op, rx, ry = fpuCmpOpLe, ry, rx
	default:
		panic(fmt.Sprintf("BUG: unhandled float comparison condition %s", c))
	}

	cmp := m.allocateInstr()
	cmp.asFpuCmp(op, rd, rx, ry, _64bit)
	m.insert(cmp)

	if negate {
		// xori rd, rd, 1: the result is known to be 0 or 1.
		x := m.allocateInstr()
		x.asALU(aluOpXor, rd, operandNR(rd), operandImm(1), true)
		m.insert(x)
	}
}

// lowerIcmpToReg materializes an SSA integer comparison as 0 or 1 in rd.
//
// Only slt and sltu exist, so the other eight conditions are built from them:
// the "greater" directions by swapping operands, and equality by the standard
// trick of testing whether the difference is (non-)zero -- sltu against zero
// is "is non-zero", and sltiu against 1 is "is zero".
func (m *machine) lowerIcmpToReg(x, y ssa.Value, c ssa.IntegerCmpCond, rd regalloc.VReg) {
	rx := m.getOperand_NR(m.compiler.ValueDefinition(x))
	ry := m.getOperand_NR(m.compiler.ValueDefinition(y))

	switch c {
	case ssa.IntegerCmpCondEqual, ssa.IntegerCmpCondNotEqual:
		// rd = x ^ y, then test against zero.
		xor := m.allocateInstr()
		xor.asALU(aluOpXor, rd, rx, ry, true)
		m.insert(xor)
		test := m.allocateInstr()
		if c == ssa.IntegerCmpCondEqual {
			// sltiu rd, rd, 1  ->  1 when rd == 0.
			test.asALU(aluOpSltu, rd, operandNR(rd), operandImm(1), true)
		} else {
			// sltu rd, zero, rd  ->  1 when rd != 0.
			test.asALU(aluOpSltu, rd, operandNR(zeroVReg), operandNR(rd), true)
		}
		m.insert(test)
		return
	}

	flag, swap := condFlagFromSSAIntegerCmpCond(c)
	if swap {
		rx, ry = ry, rx
	}

	var op aluOp
	var invert bool
	switch flag {
	case condLT:
		op = aluOpSlt
	case condLTU:
		op = aluOpSltu
	case condGE:
		op, invert = aluOpSlt, true
	case condGEU:
		op, invert = aluOpSltu, true
	default:
		panic(fmt.Sprintf("BUG: unexpected flag %s for a materialized comparison", flag))
	}

	slt := m.allocateInstr()
	slt.asALU(op, rd, rx, ry, true)
	m.insert(slt)

	if invert {
		// x >= y is !(x < y), and the value is known to be 0 or 1.
		inv := m.allocateInstr()
		inv.asALU(aluOpXor, rd, operandNR(rd), operandImm(1), true)
		m.insert(inv)
	}
}

package frontend

import (
	"runtime"

	"github.com/samyfodil/wazy/internal/wasm"
)

// ifSelect is whether short pure ifs are lowered as selects: arm64's csel takes the zero register and
// costs one instruction, where amd64's cmov needs a register for each arm and the branch, predicted,
// costs less.
var ifSelect = runtime.GOARCH == "arm64"

// ifSelectMaxOps is the most instructions each arm of an if lowered as a select may have.
const ifSelectMaxOps = 5

// lowerIfAsSelect lowers the if at the current instruction as a select of what its arms compute, where
// it has one integer result, no params and no hint, and each arm is at most ifSelectMaxOps instructions
// of locals, constants and arithmetic that cannot trap: both arms then run, which costs less than the
// branches around them. It leaves the program counter on the if's end.
func (c *Compiler) lowerIfAsSelect() bool {
	state := c.state()
	body := c.wasmFunctionBody
	pc := state.pc
	if !ifSelect || pc+1 >= len(body) || body[pc+1] != byte(wasm.ValueTypeI32) && body[pc+1] != byte(wasm.ValueTypeI64) {
		return false
	}
	if _, hinted := c.branchHint(pc); hinted {
		return false
	}
	elsePos, ok := c.pureArm(pc+2, wasm.OpcodeElse)
	if !ok {
		return false
	}
	endPos, ok := c.pureArm(elsePos+1, wasm.OpcodeEnd)
	if !ok {
		return false
	}
	cond := state.pop()
	for state.pc = pc + 2; state.pc < elsePos; {
		c.lowerCurrentOpcode()
	}
	then := state.pop()
	for state.pc = elsePos + 1; state.pc < endPos; {
		c.lowerCurrentOpcode()
	}
	els := state.pop()
	sel := c.ssaBuilder.AllocateInstruction().AsSelect(cond, then, els).Insert(c.ssaBuilder)
	state.push(sel.Return())
	state.pc = endPos
	return true
}

// pureArm returns where the instruction term is, past the arm of an if from pc on, if the arm is at most
// ifSelectMaxOps instructions skipPureOp takes that leave one value.
func (c *Compiler) pureArm(pc int, term wasm.Opcode) (int, bool) {
	body := c.wasmFunctionBody
	depth := 0
	for ops := 0; pc < len(body); ops++ {
		op := body[pc]
		if op == term {
			return pc, depth == 1
		}
		n := c.skipPureOp(pc)
		if n == 0 || ops == ifSelectMaxOps {
			return 0, false
		}
		switch {
		case op == wasm.OpcodeLocalGet || op == wasm.OpcodeI32Const || op == wasm.OpcodeI64Const:
			depth++
		case op == wasm.OpcodeI32Eqz || op == wasm.OpcodeI64Eqz:
		default: // a comparison or a binary operation
			depth--
		}
		if depth < 1 {
			return 0, false
		}
		pc += n
	}
	return 0, false
}

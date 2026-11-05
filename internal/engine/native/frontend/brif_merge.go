package frontend

import (
	"runtime"

	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// brIfMerge is whether br_ifs are merged: arm64 branches on either of two comparisons with one conditional
// branch, where amd64 would need two, as it does for them apart.
var brIfMerge = runtime.GOARCH == "arm64"

// mergeBrIfs returns the condition of the br_if to labelIndex at the current instruction, cond, or-ed with
// those of the br_ifs to the same label right after it whose conditions are one comparison of locals and
// constants: one branch where there were several, which arm64 makes with conditional compares. It lowers
// those instructions, the br_ifs' included, in the current block, as nothing of theirs can trap or
// change anything.
func (c *Compiler) mergeBrIfs(cond ssa.Value, labelIndex uint32) ssa.Value {
	state := c.state()
	builder := c.ssaBuilder
	if !c.isIcmp(cond) {
		return cond
	}
	for {
		end, ok := c.pureBrIf(state.pc+1, labelIndex)
		if !ok {
			return cond
		}
		for state.pc++; state.pc < end; {
			c.lowerCurrentOpcode()
		}
		// To the br_if's label's last byte, as reading it would leave the pc.
		_, n, _ := leb128.LoadUint32(c.wasmFunctionBody[state.pc+1:])
		state.pc += int(n)
		next := state.pop()
		or := builder.AllocateInstruction()
		or.AsBor(cond, next)
		builder.InsertInstruction(or)
		cond = or.Return()
	}
}

// isIcmp reports whether v is the result of an integer comparison.
func (c *Compiler) isIcmp(v ssa.Value) bool {
	instr := c.ssaBuilder.InstructionOfValue(v)
	return instr != nil && instr.Opcode() == ssa.OpcodeIcmp
}

// pureBrIf reports whether the instructions from pc on are locals and constants that one integer
// comparison or eqz takes, and then a br_if to labelIndex, which has no hint, and returns where that
// br_if is.
func (c *Compiler) pureBrIf(pc int, labelIndex uint32) (int, bool) {
	body := c.wasmFunctionBody
	depth := 0
	compared := false
	for ops := 0; pc < len(body) && ops < 8; ops++ {
		op := body[pc]
		switch {
		case op == wasm.OpcodeLocalGet:
			_, n, err := leb128.LoadUint32(body[pc+1:])
			if err != nil {
				return 0, false
			}
			depth++
			compared = false
			pc += 1 + int(n)
		case op == wasm.OpcodeI32Const:
			_, n, err := leb128.LoadInt32(body[pc+1:])
			if err != nil {
				return 0, false
			}
			depth++
			compared = false
			pc += 1 + int(n)
		case op == wasm.OpcodeI64Const:
			_, n, err := leb128.LoadInt64(body[pc+1:])
			if err != nil {
				return 0, false
			}
			depth++
			compared = false
			pc += 1 + int(n)
		case op == wasm.OpcodeI32Eqz || op == wasm.OpcodeI64Eqz:
			if depth < 1 {
				return 0, false
			}
			compared = true
			pc++
		case wasm.OpcodeI32Eq <= op && op <= wasm.OpcodeI32GeU, wasm.OpcodeI64Eq <= op && op <= wasm.OpcodeI64GeU:
			if depth < 2 {
				return 0, false
			}
			depth--
			compared = true
			pc++
		case op == wasm.OpcodeBrIf:
			l, _, err := leb128.LoadUint32(body[pc+1:])
			if err != nil || l != labelIndex || depth != 1 || !compared {
				return 0, false
			}
			if _, hinted := c.branchHint(pc); hinted {
				return 0, false
			}
			return pc, true
		default:
			return 0, false
		}
	}
	return 0, false
}

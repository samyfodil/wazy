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
// those of the br_ifs to the same label right after it whose conditions are a comparison of arithmetic of
// locals and constants: one branch where there were several, which arm64 makes with conditional
// compares. It lowers those instructions, the br_ifs' included, in the current block, as nothing of
// theirs can trap or change anything.
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

// mergeBrIfOverBlockEnd returns, for the br_if to labelIndex at the current instruction on cond, a
// condition and a label to branch on instead of it and the br_if right after it, where that one, as
// mergeBrIfs takes, is the innermost block's last instruction, and this one ends the
// block: if not cond, then the next condition, to the next label. It lowers the instructions between,
// the second br_if's included, in the current block.
func (c *Compiler) mergeBrIfOverBlockEnd(cond ssa.Value, labelIndex uint32) (ssa.Value, uint32, bool) {
	state := c.state()
	builder := c.ssaBuilder
	if labelIndex != 0 || c.inlinedCallee != 0 {
		return cond, labelIndex, false
	}
	if f := state.ctrlPeekAt(0); f.kind != controlFrameKindBlock || len(f.blockType.Results) != 0 {
		return cond, labelIndex, false
	}
	end, l2, ok := c.pureBrIfAny(state.pc + 1)
	if !ok || l2 == 0 || int(l2) >= len(state.controlFrames) {
		return cond, labelIndex, false
	}
	_, n, _ := leb128.LoadUint32(c.wasmFunctionBody[end+1:])
	if after := end + 1 + int(n); after >= len(c.wasmFunctionBody) || c.wasmFunctionBody[after] != wasm.OpcodeEnd {
		return cond, labelIndex, false
	}
	if f := state.ctrlPeekAt(int(l2)); f.isLoop() && len(f.blockType.Params) != 0 || !f.isLoop() && len(f.blockType.Results) != 0 {
		return cond, labelIndex, false
	}
	for state.pc++; state.pc < end; {
		c.lowerCurrentOpcode()
	}
	state.pc += int(n)
	next := state.pop()
	not := builder.AllocateInstruction()
	if cmp := builder.InstructionOfValue(cond); cmp != nil && cmp.Opcode() == ssa.OpcodeIcmp {
		x, y, cc := cmp.IcmpData()
		not.AsIcmp(x, y, cc.Inverted())
	} else {
		not.AsIcmp(cond, builder.AllocateInstruction().AsIconst32(0).Insert(builder).Return(), ssa.IntegerCmpCondEqual)
	}
	both := builder.AllocateInstruction().AsBand(not.Insert(builder).Return(), next).Insert(builder).Return()
	return both, l2, true
}

// pureBrIfAny is pureBrIf for a br_if to any label, which it returns.
func (c *Compiler) pureBrIfAny(pc int) (int, uint32, bool) {
	at := pc
	for ops := 0; at < len(c.wasmFunctionBody) && ops < 8; ops++ {
		if c.wasmFunctionBody[at] == wasm.OpcodeBrIf {
			l, _, err := leb128.LoadUint32(c.wasmFunctionBody[at+1:])
			if err != nil {
				return 0, 0, false
			}
			end, ok := c.pureBrIf(pc, l)
			return end, l, ok
		}
		n := c.skipPureOp(at)
		if n == 0 {
			return 0, 0, false
		}
		at += n
	}
	return 0, 0, false
}

// skipPureOp returns the length of the instruction at pc where pureBrIf takes it, or 0.
func (c *Compiler) skipPureOp(pc int) int {
	body := c.wasmFunctionBody
	op := body[pc]
	switch {
	case op == wasm.OpcodeLocalGet:
		if _, n, err := leb128.LoadUint32(body[pc+1:]); err == nil {
			return 1 + int(n)
		}
	case op == wasm.OpcodeI32Const:
		if _, n, err := leb128.LoadInt32(body[pc+1:]); err == nil {
			return 1 + int(n)
		}
	case op == wasm.OpcodeI64Const:
		if _, n, err := leb128.LoadInt64(body[pc+1:]); err == nil {
			return 1 + int(n)
		}
	case op == wasm.OpcodeI32Eqz || op == wasm.OpcodeI64Eqz,
		wasm.OpcodeI32Eq <= op && op <= wasm.OpcodeI32GeU, wasm.OpcodeI64Eq <= op && op <= wasm.OpcodeI64GeU,
		pureBinaryOp(op):
		return 1
	}
	return 0
}

// pureBinaryOp reports whether op is integer arithmetic of two values that cannot trap: anything but a
// division or a remainder.
func pureBinaryOp(op wasm.Opcode) bool {
	switch {
	case wasm.OpcodeI32Add <= op && op <= wasm.OpcodeI32Rotr:
		return op < wasm.OpcodeI32DivS || op > wasm.OpcodeI32RemU
	case wasm.OpcodeI64Add <= op && op <= wasm.OpcodeI64Rotr:
		return op < wasm.OpcodeI64DivS || op > wasm.OpcodeI64RemU
	}
	return false
}

// isIcmp reports whether v is the result of an integer comparison.
func (c *Compiler) isIcmp(v ssa.Value) bool {
	instr := c.ssaBuilder.InstructionOfValue(v)
	return instr != nil && instr.Opcode() == ssa.OpcodeIcmp
}

// pureBrIf reports whether the instructions from pc on are integer arithmetic of locals and constants
// that cannot trap, ending in a comparison or eqz, and then a br_if to labelIndex, which has no hint, and returns where that
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
		case pureBinaryOp(op):
			if depth < 2 {
				return 0, false
			}
			depth--
			compared = false
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

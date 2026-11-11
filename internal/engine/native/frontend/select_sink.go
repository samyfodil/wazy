package frontend

import (
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// selectSinkMinCost is the least work, a multiply counting four and any other arithmetic one, sinkSelect
// branches around: below it, a select does it for less than a branch the predictor might miss.
const selectSinkMinCost = 5

// sinkSelect lowers `local.set x (select v1 v2 cond)`, where one of v1 and v2 is x's value and the other
// is the result of pure integer arithmetic in the current block nothing else uses, as a branch around
// that arithmetic: x keeps its value unless cond picks the other, and only then is it computed. A
// conditional update of a local, such as a hash folded in for each bit set, then costs nothing where it
// does not happen, and a loop it is in waits on the work only when it does. It reports false, and does
// nothing, for any other select.
func (c *Compiler) sinkSelect(cond, v1, v2 ssa.Value) (ssa.Value, bool) {
	state := c.state()
	body := c.wasmFunctionBody
	pc := state.pc + 1
	if c.inlinedCallee != 0 || pc >= len(body) || body[pc] != wasm.OpcodeLocalSet && body[pc] != wasm.OpcodeLocalTee {
		return ssa.ValueInvalid, false
	}
	if t := v1.Type(); t != ssa.TypeI32 && t != ssa.TypeI64 {
		return ssa.ValueInvalid, false
	}
	idx, _, err := leb128.LoadUint32(body[pc+1:])
	if err != nil || int(idx) >= len(c.wasmLocalToVariable) {
		return ssa.ValueInvalid, false
	}
	builder := c.ssaBuilder
	cur := builder.FindValueInLinearPath(c.localVariable(idx))
	var work, keep ssa.Value
	workWhenTrue := true
	switch {
	case !cur.Valid():
		return ssa.ValueInvalid, false
	case cur == v2:
		work, keep = v1, v2
	case cur == v1:
		work, keep, workWhenTrue = v2, v1, false
	default:
		return ssa.ValueInvalid, false
	}

	// What a later instruction could still take a value from: the stack, the locals, and the block's
	// instructions. The work takes an instruction along only when nothing else does.
	blk := builder.CurrentBlock()
	held := map[ssa.Value]bool{cond: true, keep: true}
	for _, v := range state.values {
		held[v] = true
	}
	for _, variable := range c.wasmLocalToVariable {
		if v := builder.FindValueInLinearPath(variable); v.Valid() {
			held[v] = true
		}
	}
	inBlock := map[*ssa.Instruction]bool{}
	uses := map[ssa.Value]int{}
	for instr := blk.Root(); instr != nil; instr = instr.Next() {
		inBlock[instr] = true
		a1, a2, a3, vs := instr.Args()
		for _, v := range [...]ssa.Value{a1, a2, a3} {
			if v.Valid() {
				uses[v]++
			}
		}
		for _, v := range vs {
			uses[v]++
		}
	}
	tree := map[*ssa.Instruction]bool{}
	cost := 0
	var walk func(v ssa.Value, users int)
	walk = func(v ssa.Value, users int) {
		instr := builder.InstructionOfValue(v)
		if instr == nil || !inBlock[instr] || tree[instr] || held[v] || uses[v] != users {
			return // A leaf: computed either way.
		}
		switch instr.Opcode() {
		case ssa.OpcodeImul:
			cost += 4
		case ssa.OpcodeIadd, ssa.OpcodeIsub, ssa.OpcodeBand, ssa.OpcodeBor, ssa.OpcodeBxor,
			ssa.OpcodeIshl, ssa.OpcodeUshr, ssa.OpcodeSshr, ssa.OpcodeRotl, ssa.OpcodeRotr:
			cost++
		default:
			return
		}
		tree[instr] = true
		x, y := instr.Arg2()
		if x == y {
			walk(x, 2)
		} else {
			walk(x, 1)
			walk(y, 1)
		}
	}
	walk(work, 0) // The select it goes to is not in the block yet.
	if cost < selectSinkMinCost {
		return ssa.ValueInvalid, false
	}

	moved := make([]*ssa.Instruction, 0, len(tree))
	for instr := blk.Root(); instr != nil; instr = instr.Next() {
		if tree[instr] {
			moved = append(moved, instr)
		}
	}
	workBlk, join := builder.AllocateBasicBlock(), builder.AllocateBasicBlock()
	result := join.AddParam(builder, work.Type())
	skip := builder.AllocateInstruction()
	if workWhenTrue {
		skip.AsBrz(cond, c.allocateVarLengthValues(1, keep), join)
	} else {
		skip.AsBrnz(cond, c.allocateVarLengthValues(1, keep), join)
	}
	builder.InsertInstruction(skip)
	c.insertJumpToBlock(ssa.ValuesNil, workBlk)

	c.finalizeKnownSafeBoundsAtTheEndOfBlock(blk.ID())
	builder.SetCurrentBlock(workBlk)
	builder.Seal(workBlk)
	c.initializeCurrentBlockKnownBounds()
	for _, instr := range moved {
		builder.MoveToCurrentBlock(instr, blk)
	}
	c.insertJumpToBlock(c.allocateVarLengthValues(1, work), join)

	c.finalizeKnownSafeBoundsAtTheEndOfBlock(workBlk.ID())
	builder.SetCurrentBlock(join)
	builder.Seal(join)
	c.initializeCurrentBlockKnownBounds()
	return result, true
}

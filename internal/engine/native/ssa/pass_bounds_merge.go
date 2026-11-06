package ssa

import (
	"runtime"

	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
)

// mergeBoundsChecks is whether passMergeBoundsChecksOpt runs: arm64 tests two comparisons with one
// conditional compare and branches once, where amd64 would test and branch twice still.
var mergeBoundsChecks = runtime.GOARCH == "arm64"

// passMergeBoundsChecksOpt makes two bounds checks of a block that only pure instructions and loads run
// between one check of either condition, at the first, where what the second compares can be computed
// there: it is, or arithmetic between the two computes it from what is. A check that would have trapped
// later traps earlier instead, as out of bounds still, with nothing observable having happened between.
// Fields read from two places then branch once to check both.
func passMergeBoundsChecksOpt(b *builder) {
	if !mergeBoundsChecks {
		return
	}
	for _, blk := range b.reversePostOrderedBasicBlocks {
		var first *Instruction
		for cur := blk.rootInstr; cur != nil; {
			next := cur.next
			switch {
			case isBoundsCheck(cur):
				if first != nil && b.mergeBoundsCheck(blk, first, cur) {
					first = nil
				} else {
					first = cur
				}
			case cur.sideEffect() != sideEffectNone:
				first = nil
			}
			cur = next
		}
	}
}

func isBoundsCheck(instr *Instruction) bool {
	return instr.opcode == OpcodeExitIfTrueWithCode && nativeapi.ExitCode(instr.u1) == nativeapi.ExitCodeMemoryOutOfBounds
}

// mergeBoundsCheckMax is the most instructions mergeBoundsCheck moves up for the second check.
const mergeBoundsCheckMax = 8

// mergeBoundsCheck makes second, a bounds check of blk after first, part of first, and reports whether it
// did: the arithmetic between them that its condition takes moves up ahead of first, where it is all of
// it; loads and anything else stay, and so does second, if it takes one.
func (b *builder) mergeBoundsCheck(blk *basicBlock, first, second *Instruction) bool {
	cond := b.resolveAlias(second.v2)
	moved := b.boundsMoves[:0]
	defer func() { b.boundsMoves = moved[:0] }()
	// between reports whether instr is one of those after first and before second.
	between := func(instr *Instruction) bool {
		for cur := first.next; cur != nil && cur != second; cur = cur.next {
			if cur == instr {
				return true
			}
		}
		return false
	}
	var walk func(v Value) bool
	walk = func(v Value) bool {
		instr := b.InstructionOfValue(b.resolveAlias(v))
		if instr == nil || !between(instr) {
			return true
		}
		switch instr.opcode {
		case OpcodeIconst, OpcodeIadd, OpcodeIsub, OpcodeImul, OpcodeIshl, OpcodeUshr, OpcodeSshr,
			OpcodeBand, OpcodeBor, OpcodeBxor, OpcodeUExtend, OpcodeSExtend, OpcodeIcmp:
		default:
			return false
		}
		for _, m := range moved {
			if m == instr {
				return true
			}
		}
		if len(moved) == mergeBoundsCheckMax {
			return false
		}
		v1, v2, _, _ := instr.Args()
		if v1.Valid() && !walk(v1) || v2.Valid() && !walk(v2) {
			return false
		}
		moved = append(moved, instr)
		return true
	}
	if !walk(cond) {
		return false
	}
	// In the order the block had them, which keeps each after what it takes.
	for cur := first.next; cur != second; {
		next := cur.next
		for _, m := range moved {
			if m == cur {
				blk.removeInstruction(cur)
				cur.prev, cur.next = first.prev, first
				if first.prev != nil {
					first.prev.next = cur
				} else {
					blk.rootInstr = cur
				}
				first.prev = cur
				break
			}
		}
		cur = next
	}
	or := b.AllocateInstruction()
	or.AsBor(b.resolveAlias(first.v2), cond)
	first.v2 = b.insertBefore(blk, first, or)
	blk.removeInstruction(second)
	return true
}

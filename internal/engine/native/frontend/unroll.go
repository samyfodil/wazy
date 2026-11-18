package frontend

import (
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// A small loop with no counter checks for preemption in full on every iteration, and spends a branch
// back to its header on each. Lowered twice in a row, the first copy branching back into the second
// and the second to the header, it does both once every two iterations, the way V8 unrolls such
// loops: a linked list's walk, say. The first copy is lowered with its branches back going to the
// start of the second; at its end, the loop is unrolled if it turned out small and simple, and the
// second copy is lowered from the same code. Otherwise those branches go to the header, as if the
// loop had been lowered once.

// UnrollHook, when set, is told of each loop the frontend unrolls.
var UnrollHook func()

// unrollMaxBody is the largest loop, in bytes of wasm from its loop instruction, that is unrolled. The
// smaller the body, the more of each iteration the check and the branch back are; and a second copy of a
// larger one costs a loop that seldom goes round twice, nested in another, more in code its outer loop
// has to fetch than it saves: a search's failure links, say.
const unrollMaxBody = 32

// unrollCandidate reports whether the loop frame just opened, with no parameters or results and a
// check of its own, is to be lowered twice, if it turns out fit (see lowerSecondCopy).
func (c *Compiler) unrollCandidate(frame *controlFrame) bool {
	return c.unrollLoops && c.inlinedCallee == 0 && !frame.dispatch
}

// looksCounted reports whether the code from start to end steps a local by a constant: local.get x, a
// constant, an add or a sub, and local.set or local.tee x. It reads bytes, not instructions, so it may
// see the pattern where there is none, which only costs a loop its unrolling. A loop with a counter is
// not unrolled: its check costs less already (see tunePreemptCheck), which it no longer gets once the
// counter steps twice an iteration.
func (c *Compiler) looksCounted(start, end int) bool {
	body := c.wasmFunctionBody[:end]
	for at := start; at < end; at++ {
		if body[at] != wasm.OpcodeLocalGet {
			continue
		}
		x, n, err := leb128.LoadUint32(body[at+1:])
		if err != nil {
			continue
		}
		k := at + 1 + int(n)
		var add, sub wasm.Opcode
		switch {
		case k < len(body) && body[k] == wasm.OpcodeI32Const:
			_, n, err = leb128.LoadInt32(body[k+1:])
			add, sub = wasm.OpcodeI32Add, wasm.OpcodeI32Sub
		case k < len(body) && body[k] == wasm.OpcodeI64Const:
			_, n, err = leb128.LoadInt64(body[k+1:])
			add, sub = wasm.OpcodeI64Add, wasm.OpcodeI64Sub
		default:
			continue
		}
		if err != nil {
			continue
		}
		op := k + 1 + int(n)
		if op+1 >= len(body) || body[op] != add && body[op] != sub ||
			body[op+1] != wasm.OpcodeLocalSet && body[op+1] != wasm.OpcodeLocalTee {
			continue
		}
		if y, _, err := leb128.LoadUint32(body[op+2:]); err == nil && y == x {
			return true
		}
	}
	return false
}

// lowerSecondCopy is reached at the end of the first copy of a loop's body, ctrl the loop frame just
// popped. If the loop is fit to unroll, small, with no counter, and with no call, bulk operation, loop or
// try_table inside, it leaves the first copy to the code after the loop where it falls off its end (see
// leaveFirstCopy), puts ctrl back, and lowers the second copy from the start of the body, where a branch
// back goes to the header; and it reports true. Otherwise the branches back go to the header, and it
// reports false, for the loop to end as it would have.
func (c *Compiler) lowerSecondCopy(ctrl *controlFrame) bool {
	state := c.state()
	builder := c.ssaBuilder
	copy2 := ctrl.copy2
	ctrl.copy2 = nil
	if ctrl.notSimple || ctrl.work != 0 || state.pc-ctrl.loopStart > unrollMaxBody || copy2.Preds() == 0 ||
		len(c.vecSum.cands) != ctrl.vecSums || len(c.pendingEhEntries) != ctrl.ehEntries ||
		c.looksCounted(ctrl.bodyStart+1, state.pc) {
		builder.Redirect(copy2, ctrl.blk)
		builder.Seal(copy2)
		return false
	}
	if UnrollHook != nil {
		UnrollHook()
	}
	if state.unreachable {
		state.unreachable = false
	} else {
		if !ctrl.yielded {
			ctrl.exitMissed = true
		}
		ctrl.copy1Exit = builder.CurrentBlock()
	}
	builder.Seal(copy2)
	state.ctrlPush(*ctrl)
	c.switchTo(ctrl.originalStackLenWithoutParam, copy2)
	state.pc = ctrl.bodyStart
	return true
}

// leaveFirstCopy has the first copy of ctrl's body, where it falls off its end, go on after the loop. That
// is only done once the second copy is lowered, so that the block it is done in comes after it: blocks are
// laid out in the order of their first instructions, and the second copy is the one to fall through to.
func (c *Compiler) leaveFirstCopy(ctrl *controlFrame) {
	builder := c.ssaBuilder
	current := builder.CurrentBlock()
	builder.SetCurrentBlock(ctrl.copy1Exit)
	c.insertJumpToBlock(ssa.ValuesNil, ctrl.followingBlock)
	builder.SetCurrentBlock(current)
	ctrl.copy1Exit = nil
}

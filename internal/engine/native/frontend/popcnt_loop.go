package frontend

import (
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// A popcount loop is the do-while that counts the bits of x by clearing its lowest one at a time:
//
//	loop
//	  n = n + 1
//	  x = x & (x - 1)
//	  br_if 0 (x)
//	end
//
// It goes round once for each bit x has, and once when it has none: it leaves n increased by x's
// population count, or by one where x is zero, and x zero. lowerPopcountLoop lowers it as that, with no
// loop at all, the way LLVM's loop idiom recognition does.

// PopcountLoopHook, when set, is told of each popcount loop the frontend lowers.
var PopcountLoopHook func()

// popcountLoop reads the loop at pc, the loop instruction, and returns its counter's and its value's
// locals and where its end is, if it is a popcount loop of i32 locals.
func (c *Compiler) popcountLoop(pc int) (n, x uint32, end int, ok bool) {
	body := c.wasmFunctionBody
	if pc+1 >= len(body) || body[pc+1] != 0x40 { // An empty block type: no params, no results.
		return
	}
	s := newVsSim()
	at := pc + 2
	for ops := 0; ; ops++ {
		if at >= len(body) || ops > 16 {
			return
		}
		if body[at] == wasm.OpcodeBrIf {
			break
		}
		next, stepped := s.step(body, at)
		if !stepped {
			return
		}
		at = next
	}
	label, w, err := leb128.LoadUint32(body[at+1:])
	if err != nil || label != 0 || len(s.stack) != 1 || len(s.loads) != 0 || len(s.locals) != 2 {
		return
	}
	end = at + 1 + int(w)
	if end >= len(body) || body[end] != wasm.OpcodeEnd {
		return
	}
	cond := s.stack[0].id
	isEntry := func(id int32, k uint32) bool {
		nd := s.nodes[id]
		return nd.kind == vsEntry && nd.k == uint64(k)
	}
	isConst := func(id int32, k uint32) bool {
		nd := s.nodes[id]
		return nd.kind == vsConst && uint32(nd.k) == k
	}
	// bin reports whether id is op of a and b, in either order where op commutes, and returns them.
	bin := func(id int32, op wasm.Opcode) (a, b int32, is bool) {
		nd := s.nodes[id]
		return nd.a, nd.b, nd.kind == vsBin && nd.op == op
	}
	var nOK, xOK bool
	for k, id := range s.locals {
		if a, b, is := bin(id, wasm.OpcodeI32Add); is && (isEntry(a, k) && isConst(b, 1) || isConst(a, 1) && isEntry(b, k)) {
			n, nOK = k, true
			continue
		}
		a, b, is := bin(id, wasm.OpcodeI32And)
		if !is || id != cond {
			continue
		}
		if !isEntry(a, k) {
			a, b = b, a
		}
		if !isEntry(a, k) {
			continue
		}
		if p, q, is := bin(b, wasm.OpcodeI32Add); is && (isEntry(p, k) && isConst(q, 0xffffffff) || isConst(p, 0xffffffff) && isEntry(q, k)) {
			x, xOK = k, true
		}
	}
	if !nOK || !xOK || n == x {
		return
	}
	if c.localType(n) != wasm.ValueTypeI32 || c.localType(x) != wasm.ValueTypeI32 {
		return
	}
	return n, x, end, true
}

// localType returns the type of local k of the function being compiled.
func (c *Compiler) localType(k uint32) wasm.ValueType {
	if params := c.wasmFunctionTyp.Params; int(k) < len(params) {
		return params[k]
	} else if k -= uint32(len(params)); int(k) < len(c.wasmFunctionLocalTypes) {
		return c.wasmFunctionLocalTypes[k]
	}
	return 0
}

// lowerPopcountLoop lowers the popcount loop at the current instruction, if it is one, and leaves the
// program counter on its end.
func (c *Compiler) lowerPopcountLoop() bool {
	state := c.state()
	n, x, end, ok := c.popcountLoop(state.pc)
	if !ok {
		return false
	}
	if PopcountLoopHook != nil {
		PopcountLoopHook()
	}
	builder := c.ssaBuilder
	ins := builder.AllocateInstruction
	xv := builder.MustFindValue(c.localVariable(x))
	nv := builder.MustFindValue(c.localVariable(n))
	popcnt := ins()
	popcnt.AsPopcnt(xv)
	bits := popcnt.Insert(builder).Return()
	zero := ins().AsIconst32(0).Insert(builder).Return()
	none := ins().AsIcmp(xv, zero, ssa.IntegerCmpCondEqual).Insert(builder).Return()
	nv = ins().AsIadd(nv, bits).Insert(builder).Return()
	nv = ins().AsIadd(nv, none).Insert(builder).Return()
	builder.DefineVariableInCurrentBB(c.localVariable(n), nv)
	builder.DefineVariableInCurrentBB(c.localVariable(x), zero)
	state.pc = end
	return true
}

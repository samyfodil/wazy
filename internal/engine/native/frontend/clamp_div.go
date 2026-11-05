package frontend

import (
	"math"

	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// A clamped division is an i32.div_s whose quotient q the code after it only clamps: max(min(q, hi), lo)
// with lo < 0 < hi, written with selects, such as an ADPCM encoder's code for a sample. Where the divisor
// d is positive the clamp's result is known without dividing whenever x/d is out of the range: hi where
// x ≥ hi·d, lo where x < lo·d, and otherwise q itself. Codecs mostly land outside it, and then skip a
// division that is the longest step of the loop around it.

// ClampedDivHook, when set, is told of each clamped division the frontend lowers.
var ClampedDivHook func(lo, hi int64)

// clampVal is a value the code after the division computes: q clamped to [lo, hi], a constant k, or the
// comparison op of the values x and y.
type clampVal struct {
	kind   uint8
	lo, hi int64
	k      int64
	op     wasm.Opcode
	x, y   int
}

const (
	cvQuotient = iota + 1
	cvConst
	cvCmp
)

// clampAfterDiv reads the instructions after the i32.div_s at pc, and returns the clamp they compute of
// its quotient, the instruction after them, the locals they leave holding it, which is every local they
// set, and whether they leave it on the stack too.
func (c *Compiler) clampAfterDiv(pc int) (lo, hi int64, end int, locals []uint32, pushes, ok bool) {
	body := c.wasmFunctionBody
	vals := []clampVal{{kind: cvQuotient, lo: math.MinInt32, hi: math.MaxInt32}}
	stack := []int{0}
	set := map[uint32]int{}
	same := func(a, b int) bool {
		va, vb := vals[a], vals[b]
		switch {
		case a == b:
			return true
		case va.kind == cvConst && vb.kind == cvConst:
			return va.k == vb.k
		case va.kind == cvQuotient && vb.kind == cvQuotient:
			return va.lo == vb.lo && va.hi == vb.hi
		}
		return false
	}
	// bound returns max (or min) of the values a and b, a clamped quotient or a constant each.
	bound := func(a, b int, isMax bool) (int, bool) {
		va, vb := vals[a], vals[b]
		if va.kind == cvConst && vb.kind == cvQuotient {
			va, vb = vb, va
		}
		switch {
		case va.kind == cvQuotient && vb.kind == cvConst:
			if isMax {
				va.lo, va.hi = max(va.lo, vb.k), max(va.hi, vb.k)
			} else {
				va.lo, va.hi = min(va.lo, vb.k), min(va.hi, vb.k)
			}
		case va.kind == cvConst && vb.kind == cvConst:
			if isMax {
				va.k = max(va.k, vb.k)
			} else {
				va.k = min(va.k, vb.k)
			}
		default:
			return 0, false
		}
		vals = append(vals, va)
		return len(vals) - 1, true
	}
	for at, ops := pc+1, 0; at < len(body) && ops < 32; ops++ {
		op := body[at]
		next := at + 1
		switch op {
		case wasm.OpcodeLocalGet, wasm.OpcodeLocalSet, wasm.OpcodeLocalTee:
			k, n, err := leb128.LoadUint32(body[at+1:])
			if err != nil {
				return
			}
			next += int(n)
			switch op {
			case wasm.OpcodeLocalGet:
				v, mine := set[k]
				if !mine {
					return
				}
				stack = append(stack, v)
			case wasm.OpcodeLocalSet:
				if len(stack) == 0 {
					return
				}
				set[k] = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			default:
				if len(stack) == 0 {
					return
				}
				set[k] = stack[len(stack)-1]
			}
		case wasm.OpcodeI32Const:
			k, n, err := leb128.LoadInt32(body[at+1:])
			if err != nil {
				return
			}
			next += int(n)
			vals = append(vals, clampVal{kind: cvConst, k: int64(k)})
			stack = append(stack, len(vals)-1)
		case wasm.OpcodeI32GtS, wasm.OpcodeI32GeS, wasm.OpcodeI32LtS, wasm.OpcodeI32LeS:
			if len(stack) < 2 {
				return
			}
			vals = append(vals, clampVal{kind: cvCmp, op: op, x: stack[len(stack)-2], y: stack[len(stack)-1]})
			stack = append(stack[:len(stack)-2], len(vals)-1)
		case wasm.OpcodeSelect:
			if len(stack) < 3 {
				return
			}
			a, b, cond := stack[len(stack)-3], stack[len(stack)-2], vals[stack[len(stack)-1]]
			if cond.kind != cvCmp {
				return
			}
			greater := cond.op == wasm.OpcodeI32GtS || cond.op == wasm.OpcodeI32GeS
			var isMax bool
			switch {
			case same(a, cond.x) && same(b, cond.y):
				isMax = greater // x when x > y: the greater
			case same(a, cond.y) && same(b, cond.x):
				isMax = !greater
			default:
				return
			}
			v, okb := bound(a, b, isMax)
			if !okb {
				return
			}
			stack = append(stack[:len(stack)-3], v)
		default:
			return lo, hi, end, locals, pushes, ok
		}
		at = next
		// The clamp is on the stack, or, where the stack is empty, in a local.
		v := -1
		switch len(stack) {
		case 1:
			v = stack[0]
		case 0:
			for _, w := range set {
				v = w
			}
		}
		if v < 0 {
			continue
		}
		top := vals[v]
		if top.kind != cvQuotient || !(math.MinInt32 < top.lo && top.lo < 0 && 0 < top.hi && top.hi < math.MaxInt32) {
			continue
		}
		all := true
		for _, w := range set {
			all = all && same(w, v)
		}
		if all {
			lo, hi, end, pushes, ok = top.lo, top.hi, at, len(stack) == 1, true
			locals = locals[:0]
			for k := range set {
				locals = append(locals, k)
			}
		}
	}
	return
}

// lowerClampedSdiv lowers x / d, at an i32.div_s the code after which clamps, and that code, and reports
// whether it did.
func (c *Compiler) lowerClampedSdiv(x, d ssa.Value) bool {
	state := c.state()
	if x.Type() != ssa.TypeI32 {
		return false
	}
	lo, hi, end, locals, pushes, ok := c.clampAfterDiv(state.pc)
	if !ok {
		return false
	}
	if ClampedDivHook != nil {
		ClampedDivHook(lo, hi)
	}
	builder := c.ssaBuilder
	ins := builder.AllocateInstruction
	k32 := func(v int64) ssa.Value { return ins().AsIconst32(uint32(v)).Insert(builder).Return() }
	k64 := func(v int64) ssa.Value { return ins().AsIconst64(uint64(v)).Insert(builder).Return() }
	enter := func(blk ssa.BasicBlock) {
		c.finalizeKnownSafeBoundsAtTheEndOfBlock(builder.CurrentBlock().ID())
		builder.SetCurrentBlock(blk)
		builder.Seal(blk)
		c.initializeCurrentBlockKnownBounds()
	}

	positive, inRange, other, join := builder.AllocateBasicBlock(), builder.AllocateBasicBlock(), builder.AllocateBasicBlock(), builder.AllocateBasicBlock()
	result := join.AddParam(builder, ssa.TypeI32)
	pos := ins().AsIcmp(d, k32(0), ssa.IntegerCmpCondSignedGreaterThan).Insert(builder).Return()
	br := ins()
	br.AsBrz(pos, ssa.ValuesNil, other)
	builder.InsertInstruction(br)
	builder.MarkUnlikelyEdge(builder.CurrentBlock(), other)
	c.insertJumpToBlock(ssa.ValuesNil, positive)

	// lo·d ≤ x < hi·d, as one unsigned comparison of x - lo·d with (hi - lo)·d, in 64 bits where none
	// of them overflows.
	enter(positive)
	x64 := ins().AsSExtend(x, 32, 64).Insert(builder).Return()
	d64 := ins().AsSExtend(d, 32, 64).Insert(builder).Return()
	from := ins().AsIadd(x64, ins().AsImul(d64, k64(-lo)).Insert(builder).Return()).Insert(builder).Return()
	span := ins().AsImul(d64, k64(hi-lo)).Insert(builder).Return()
	in := ins().AsIcmp(from, span, ssa.IntegerCmpCondUnsignedLessThan).Insert(builder).Return()
	neg := ins().AsIcmp(x, k32(0), ssa.IntegerCmpCondSignedLessThan).Insert(builder).Return()
	clamped := ins().AsSelect(neg, k32(lo), k32(hi)).Insert(builder).Return()
	// The division is laid out of line, so that skipping it falls through to what follows.
	ins().AsBrnz(in, ssa.ValuesNil, inRange).Insert(builder)
	builder.MarkUnlikelyEdge(positive, inRange)
	c.insertJumpToBlock(c.allocateVarLengthValues(1, clamped), join)

	enter(inRange)
	q := ins().AsSDiv(x, d, c.execCtxPtrValue).Insert(builder).Return()
	c.insertJumpToBlock(c.allocateVarLengthValues(1, q), join)

	// A divisor of zero or less: the division, trapping where it does, and the clamp.
	enter(other)
	q = ins().AsSDiv(x, d, c.execCtxPtrValue).Insert(builder).Return()
	above := ins().AsIcmp(q, k32(lo), ssa.IntegerCmpCondSignedGreaterThan).Insert(builder).Return()
	q = ins().AsSelect(above, q, k32(lo)).Insert(builder).Return()
	below := ins().AsIcmp(q, k32(hi), ssa.IntegerCmpCondSignedLessThan).Insert(builder).Return()
	q = ins().AsSelect(below, q, k32(hi)).Insert(builder).Return()
	c.insertJumpToBlock(c.allocateVarLengthValues(1, q), join)

	enter(join)
	for _, k := range locals {
		builder.DefineVariableInCurrentBB(c.localVariable(k), result)
	}
	if pushes {
		state.push(result)
	}
	state.pc = end - 1
	return true
}

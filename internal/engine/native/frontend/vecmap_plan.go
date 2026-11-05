package frontend

import (
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// A vector map loop is a wasm loop whose iterations are independent element computations: each one
// loads float elements at addresses that advance by one element per iteration, combines them and
// loop-invariant values with exact per-lane arithmetic, and stores the result the same way. Such a loop
// computes the same bits whether its elements are done one at a time or several per instruction, as long
// as the elements it stores do not overlap the ones it loads (see vecMapLoop's guards).
//
// planVecMap recognizes one from the loop's bytecode, before the loop is lowered.

const (
	vecMaxLocals = 8
	vecMaxNodes  = 64
	vecMaxLoads  = 16
	vecMaxStores = 8
)

// vecAffine is an i32 value inside one iteration: konst + Σ coeff[s]·L[s] modulo 2^32, where L[s] is
// the value the local in slot s held when the iteration started.
type vecAffine struct {
	konst uint32
	coeff [vecMaxLocals]uint32
}

func (a vecAffine) add(b vecAffine, sign uint32) vecAffine {
	a.konst += sign * b.konst
	for i := range a.coeff {
		a.coeff[i] += sign * b.coeff[i]
	}
	return a
}

func (a vecAffine) scale(k uint32) vecAffine {
	a.konst *= k
	for i := range a.coeff {
		a.coeff[i] *= k
	}
	return a
}

func (a vecAffine) isConst() bool { return a.coeff == [vecMaxLocals]uint32{} }

// vecNode ops.
const (
	vecOpLoad  = iota + 1 // arg: index into plan.loads
	vecOpInv              // arg: local slot, a float local the loop does not write
	vecOpConst            // bits: the constant
	vecOpAdd
	vecOpSub
	vecOpMul
	vecOpDiv
	vecOpNeg
	vecOpAbs
	vecOpSqrt
)

type vecNode struct {
	op   uint8
	a, b uint8 // operand node indices, for arithmetic
	arg  uint8
	bits uint64
}

type vecAccess struct {
	addr   vecAffine
	offset uint64
	node   uint8 // for a store, the value stored
}

// vecVal is a value on the planner's stack or in a local: an i32 affine form or a float node.
type vecVal struct {
	isFloat bool
	// isCond marks the result of the comparison feeding the loop's br_if.
	isCond bool
	aff    vecAffine
	node   uint8
}

type vecPlan struct {
	f64 bool

	slotLocal [vecMaxLocals]wasm.Index
	slotN     int
	written   [vecMaxLocals]bool
	isFloat   [vecMaxLocals]bool
	// readL0 marks the slots whose value at the start of an iteration something reads.
	readL0 [vecMaxLocals]bool
	// final is each written slot's value at the end of an iteration.
	final [vecMaxLocals]vecVal
	// step is an induction slot's increment per iteration; induction marks those slots.
	step      [vecMaxLocals]uint32
	induction [vecMaxLocals]bool

	nodes  []vecNode
	loads  []vecAccess
	stores []vecAccess

	// The loop continues while cond holds, cond being condOp applied to condA and condB after the
	// iteration's updates.
	condOp       wasm.Opcode
	condA, condB vecAffine

	// unroll is how many element computations one source iteration does; elements are elemSize bytes.
	unroll   uint32
	elemSize uint32
	// endPC is the offset just past the loop's end opcode.
	endPC int
}

type vecPlanner struct {
	why   string
	p     vecPlan
	body  []byte
	pc    int
	stack []vecVal
	cur   [vecMaxLocals]vecVal
	set   [vecMaxLocals]bool
	types []wasm.ValueType
}

// planVecMap plans the loop whose body starts at body[pc], just past the loop's block type. types are the
// function's locals' types, parameters first. It reports false, and why, for a loop it does not take.
func planVecMap(body []byte, pc int, types []wasm.ValueType) (*vecPlan, string, bool) {
	pl := &vecPlanner{body: body, pc: pc, types: types}
	pl.p.nodes = make([]vecNode, 0, 16)
	if !pl.run() {
		if pl.why == "" && pl.pc > 0 && pl.pc <= len(body) {
			pl.why = "opcode " + wasm.InstructionName(body[pl.pc-1])
		}
		return nil, pl.why, false
	}
	if !pl.finish() {
		if pl.why == "" {
			pl.why = "finish"
		}
		return nil, pl.why, false
	}
	return &pl.p, "", true
}

func (pl *vecPlanner) slot(l wasm.Index) (int, bool) {
	for i := 0; i < pl.p.slotN; i++ {
		if pl.p.slotLocal[i] == l {
			return i, true
		}
	}
	if pl.p.slotN == vecMaxLocals || int(l) >= len(pl.types) {
		return 0, false
	}
	t := pl.types[l]
	if t != wasm.ValueTypeI32 && t != wasm.ValueTypeF64 && t != wasm.ValueTypeF32 {
		return 0, false
	}
	s := pl.p.slotN
	pl.p.slotN++
	pl.p.slotLocal[s] = l
	pl.p.isFloat[s] = t != wasm.ValueTypeI32
	return s, true
}

func (pl *vecPlanner) node(n vecNode) (uint8, bool) {
	if len(pl.p.nodes) == vecMaxNodes {
		return 0, false
	}
	pl.p.nodes = append(pl.p.nodes, n)
	return uint8(len(pl.p.nodes) - 1), true
}

func (pl *vecPlanner) push(v vecVal) { pl.stack = append(pl.stack, v) }

func (pl *vecPlanner) pop() (vecVal, bool) {
	if len(pl.stack) == 0 {
		return vecVal{}, false
	}
	v := pl.stack[len(pl.stack)-1]
	pl.stack = pl.stack[:len(pl.stack)-1]
	return v, true
}

func (pl *vecPlanner) u32() (uint32, bool) {
	v, n, err := leb128.LoadUint32(pl.body[pl.pc:])
	if err != nil {
		return 0, false
	}
	pl.pc += int(n)
	return v, true
}

// floatType reports, for a float instruction, whether it is f64, and records the element type the loop
// uses, refusing a loop that mixes the two.
func (pl *vecPlanner) floatType(f64 bool) bool {
	size := uint32(4)
	if f64 {
		size = 8
	}
	if pl.p.elemSize == 0 {
		pl.p.elemSize, pl.p.f64 = size, f64
	}
	return pl.p.elemSize == size
}

func (pl *vecPlanner) run() bool {
	for pl.pc < len(pl.body) {
		op := pl.body[pl.pc]
		pl.pc++
		switch op {
		case wasm.OpcodeLocalGet:
			l, ok := pl.u32()
			if !ok {
				return false
			}
			s, ok := pl.slot(l)
			if !ok {
				return false
			}
			if pl.set[s] {
				pl.push(pl.cur[s])
				continue
			}
			pl.p.readL0[s] = true
			if pl.p.isFloat[s] {
				if !pl.floatType(pl.types[l] == wasm.ValueTypeF64) {
					return false
				}
				n, ok := pl.node(vecNode{op: vecOpInv, arg: uint8(s)})
				if !ok {
					return false
				}
				pl.push(vecVal{isFloat: true, node: n})
			} else {
				var a vecAffine
				a.coeff[s] = 1
				pl.push(vecVal{aff: a})
			}
		case wasm.OpcodeLocalSet, wasm.OpcodeLocalTee:
			l, ok := pl.u32()
			if !ok {
				return false
			}
			s, ok := pl.slot(l)
			if !ok {
				return false
			}
			v, ok := pl.pop()
			if !ok || v.isCond || v.isFloat != pl.p.isFloat[s] {
				return false
			}
			pl.cur[s], pl.set[s], pl.p.written[s] = v, true, true
			if op == wasm.OpcodeLocalTee {
				pl.push(v)
			}
		case wasm.OpcodeI32Const:
			v, n, err := leb128.LoadInt32(pl.body[pl.pc:])
			if err != nil {
				return false
			}
			pl.pc += int(n)
			pl.push(vecVal{aff: vecAffine{konst: uint32(v)}})
		case wasm.OpcodeI32Add, wasm.OpcodeI32Sub, wasm.OpcodeI32Mul, wasm.OpcodeI32Shl:
			y, ok1 := pl.pop()
			x, ok2 := pl.pop()
			if !ok1 || !ok2 || x.isFloat || y.isFloat || x.isCond || y.isCond {
				return false
			}
			var r vecAffine
			switch op {
			case wasm.OpcodeI32Add:
				r = x.aff.add(y.aff, 1)
			case wasm.OpcodeI32Sub:
				r = x.aff.add(y.aff, ^uint32(0))
			case wasm.OpcodeI32Mul:
				switch {
				case y.aff.isConst():
					r = x.aff.scale(y.aff.konst)
				case x.aff.isConst():
					r = y.aff.scale(x.aff.konst)
				default:
					return false
				}
			case wasm.OpcodeI32Shl:
				if !y.aff.isConst() {
					return false
				}
				r = x.aff.scale(1 << (y.aff.konst & 31))
			}
			pl.push(vecVal{aff: r})
		case wasm.OpcodeF64Const, wasm.OpcodeF32Const:
			f64 := op == wasm.OpcodeF64Const
			if !pl.floatType(f64) {
				return false
			}
			var bits uint64
			if f64 {
				if pl.pc+8 > len(pl.body) {
					return false
				}
				for i := 7; i >= 0; i-- {
					bits = bits<<8 | uint64(pl.body[pl.pc+i])
				}
				pl.pc += 8
			} else {
				if pl.pc+4 > len(pl.body) {
					return false
				}
				for i := 3; i >= 0; i-- {
					bits = bits<<8 | uint64(pl.body[pl.pc+i])
				}
				pl.pc += 4
			}
			n, ok := pl.node(vecNode{op: vecOpConst, bits: bits})
			if !ok {
				return false
			}
			pl.push(vecVal{isFloat: true, node: n})
		case wasm.OpcodeF64Load, wasm.OpcodeF32Load, wasm.OpcodeF64Store, wasm.OpcodeF32Store:
			f64 := op == wasm.OpcodeF64Load || op == wasm.OpcodeF64Store
			if !pl.floatType(f64) {
				return false
			}
			align, ok := pl.u32()
			if !ok || align&0x40 != 0 { // A memory index follows: multi-memory.
				return false
			}
			offset, n, err := leb128.LoadUint64(pl.body[pl.pc:])
			if err != nil || offset >= 1<<32 {
				return false
			}
			pl.pc += int(n)
			if op == wasm.OpcodeF64Load || op == wasm.OpcodeF32Load {
				addr, ok := pl.pop()
				if !ok || addr.isFloat || addr.isCond || len(pl.p.loads) == vecMaxLoads {
					return false
				}
				pl.p.loads = append(pl.p.loads, vecAccess{addr: addr.aff, offset: offset})
				nd, ok := pl.node(vecNode{op: vecOpLoad, arg: uint8(len(pl.p.loads) - 1)})
				if !ok {
					return false
				}
				pl.push(vecVal{isFloat: true, node: nd})
			} else {
				v, ok1 := pl.pop()
				addr, ok2 := pl.pop()
				if !ok1 || !ok2 || !v.isFloat || addr.isFloat || addr.isCond || len(pl.p.stores) == vecMaxStores {
					return false
				}
				pl.p.stores = append(pl.p.stores, vecAccess{addr: addr.aff, offset: offset, node: v.node})
			}
		case wasm.OpcodeF64Add, wasm.OpcodeF64Sub, wasm.OpcodeF64Mul, wasm.OpcodeF64Div,
			wasm.OpcodeF32Add, wasm.OpcodeF32Sub, wasm.OpcodeF32Mul, wasm.OpcodeF32Div:
			if !pl.floatType(op >= wasm.OpcodeF64Add) {
				return false
			}
			y, ok1 := pl.pop()
			x, ok2 := pl.pop()
			if !ok1 || !ok2 || !x.isFloat || !y.isFloat {
				return false
			}
			var o uint8
			switch op {
			case wasm.OpcodeF64Add, wasm.OpcodeF32Add:
				o = vecOpAdd
			case wasm.OpcodeF64Sub, wasm.OpcodeF32Sub:
				o = vecOpSub
			case wasm.OpcodeF64Mul, wasm.OpcodeF32Mul:
				o = vecOpMul
			default:
				o = vecOpDiv
			}
			n, ok := pl.node(vecNode{op: o, a: x.node, b: y.node})
			if !ok {
				return false
			}
			pl.push(vecVal{isFloat: true, node: n})
		case wasm.OpcodeF64Neg, wasm.OpcodeF64Abs, wasm.OpcodeF64Sqrt,
			wasm.OpcodeF32Neg, wasm.OpcodeF32Abs, wasm.OpcodeF32Sqrt:
			if !pl.floatType(op >= wasm.OpcodeF64Abs) {
				return false
			}
			x, ok := pl.pop()
			if !ok || !x.isFloat {
				return false
			}
			o := uint8(vecOpSqrt)
			switch op {
			case wasm.OpcodeF64Neg, wasm.OpcodeF32Neg:
				o = vecOpNeg
			case wasm.OpcodeF64Abs, wasm.OpcodeF32Abs:
				o = vecOpAbs
			}
			n, ok := pl.node(vecNode{op: o, a: x.node})
			if !ok {
				return false
			}
			pl.push(vecVal{isFloat: true, node: n})
		case wasm.OpcodeI32Ne, wasm.OpcodeI32LtU, wasm.OpcodeI32LtS, wasm.OpcodeI32GtU, wasm.OpcodeI32GtS,
			wasm.OpcodeI32LeU, wasm.OpcodeI32LeS, wasm.OpcodeI32GeU, wasm.OpcodeI32GeS:
			y, ok1 := pl.pop()
			x, ok2 := pl.pop()
			if !ok1 || !ok2 || x.isFloat || y.isFloat || x.isCond || y.isCond || pl.p.condOp != 0 {
				return false
			}
			pl.p.condOp, pl.p.condA, pl.p.condB = op, x.aff, y.aff
			pl.push(vecVal{isCond: true})
		case wasm.OpcodeBrIf:
			l, ok := pl.u32()
			if !ok || l != 0 {
				return false
			}
			c, ok := pl.pop()
			if !ok || c.isFloat {
				return false
			}
			if !c.isCond { // br_if on an i32 itself: continue while it is not zero.
				if pl.p.condOp != 0 {
					return false
				}
				pl.p.condOp, pl.p.condA = wasm.OpcodeI32Ne, c.aff
			}
			if pl.pc >= len(pl.body) || pl.body[pl.pc] != wasm.OpcodeEnd || len(pl.stack) != 0 {
				return false
			}
			pl.p.endPC = pl.pc + 1
			return true
		default:
			return false
		}
	}
	return false
}

// finish checks what one iteration did against what a vector map loop may do.
func (pl *vecPlanner) finish() bool {
	p := &pl.p
	if len(p.stores) == 0 || p.elemSize == 0 {
		return false
	}
	for s := 0; s < p.slotN; s++ {
		if !p.written[s] {
			continue
		}
		v := pl.cur[s]
		p.final[s] = v
		if p.isFloat[s] {
			if p.readL0[s] { // A float carried from one iteration to the next.
				pl.why = "float recurrence"
				return false
			}
			continue
		}
		var want vecAffine
		want.coeff[s] = 1
		want.konst = v.aff.konst
		if v.aff == want && v.aff.konst != 0 {
			p.induction[s], p.step[s] = true, v.aff.konst
		} else if p.readL0[s] {
			pl.why = "i32 recurrence"
			return false // An i32 carried from one iteration to the next that is not an induction.
		}
	}
	switch p.condOp {
	case wasm.OpcodeI32Ne, wasm.OpcodeI32LtS, wasm.OpcodeI32LtU:
	default:
		pl.why = "exit condition"
		return false
	}
	// A counter may count down only to an exact equality: lt_s and lt_u need it to count up.
	if d := int32(p.stride(p.condA)); d == 0 || d < 0 && p.condOp != wasm.OpcodeI32Ne || p.stride(p.condB) != 0 {
		pl.why = "exit step"
		return false
	}
	// Every local an address or the exit condition depends on is an induction or left alone.
	for _, a := range pl.affines() {
		for s, c := range a.coeff {
			if c != 0 && p.written[s] && !p.induction[s] {
				return false
			}
		}
	}
	return pl.reroll()
}

func (pl *vecPlanner) affines() []vecAffine {
	p := &pl.p
	out := make([]vecAffine, 0, len(p.loads)+len(p.stores)+2)
	for _, l := range p.loads {
		out = append(out, l.addr)
	}
	for _, s := range p.stores {
		out = append(out, s.addr)
	}
	return append(out, p.condA, p.condB)
}

// stride returns how far, in bytes, an address advances per source iteration.
func (p *vecPlan) stride(a vecAffine) uint32 {
	var s uint32
	for i, c := range a.coeff {
		if p.induction[i] {
			s += c * p.step[i]
		}
	}
	return s
}

// reroll accepts a body that is unroll copies of one element computation, copy k touching each moving
// stream k elements further on, and keeps copy zero's stores as the computation. Every moving access
// must then advance by exactly unroll elements per iteration.
func (pl *vecPlanner) reroll() bool {
	p := &pl.p
	u := uint32(len(p.stores))
	stride := u * p.elemSize
	for _, l := range p.loads {
		if s := p.stride(l.addr); s != 0 && s != stride {
			pl.why = "load stride"
			return false
		}
	}
	for k, s := range p.stores {
		if p.stride(s.addr) != stride {
			pl.why = "store stride"
			return false
		}
		if d, ok := shiftOf(p.stores[0], s); !ok || d != int64(k)*int64(p.elemSize) {
			pl.why = "store shift"
			return false
		}
		if k > 0 && !pl.sameShifted(p.stores[0].node, s.node, int64(k)*int64(p.elemSize)) {
			pl.why = "copies differ"
			return false
		}
	}
	p.unroll = u
	return true
}

// shiftOf returns how many bytes further than a the access b reaches, given the same moving terms: the
// difference of their constants, as a signed i32, plus that of their offsets. Folding the constant in is
// exact as long as the address computation does not wrap, which the loop's bounds guard establishes.
func shiftOf(a, b vecAccess) (int64, bool) {
	ca, cb := a.addr, b.addr
	ca.konst, cb.konst = 0, 0
	if ca != cb {
		return 0, false
	}
	return int64(int32(b.addr.konst-a.addr.konst)) + int64(b.offset) - int64(a.offset), true
}

// sameShifted reports whether node b computes what node a does on the element shift bytes further on:
// the same tree, with each moving load shift bytes further and every other leaf identical.
func (pl *vecPlanner) sameShifted(a, b uint8, shift int64) bool {
	p := &pl.p
	na, nb := p.nodes[a], p.nodes[b]
	if na.op != nb.op {
		return false
	}
	switch na.op {
	case vecOpLoad:
		la, lb := p.loads[na.arg], p.loads[nb.arg]
		d, ok := shiftOf(la, lb)
		if !ok {
			return false
		}
		if p.stride(la.addr) == 0 {
			return d == 0
		}
		return d == shift
	case vecOpInv:
		return na.arg == nb.arg
	case vecOpConst:
		return na.bits == nb.bits
	case vecOpNeg, vecOpAbs, vecOpSqrt:
		return pl.sameShifted(na.a, nb.a, shift)
	default:
		return pl.sameShifted(na.a, nb.a, shift) && pl.sameShifted(na.b, nb.b, shift)
	}
}

// VecPlanHook, when set, is told of each loop the frontend considers, and whether planVecMap took it.
// It exists for tests that survey a corpus.
var VecPlanHook func(fn wasm.Index, pc int, ok bool, why string)

// localTypes returns the types of the function's locals, parameters first.
func (c *Compiler) localTypes() []wasm.ValueType {
	types := make([]wasm.ValueType, 0, len(c.wasmFunctionTyp.Params)+len(c.wasmFunctionLocalTypes))
	types = append(types, c.wasmFunctionTyp.Params...)
	return append(types, c.wasmFunctionLocalTypes...)
}

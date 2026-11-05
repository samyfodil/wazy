package frontend

import (
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// A vector map loop is a wasm loop whose iterations are independent element computations on lanes of
// one type: each loads elements at addresses that advance by one element per iteration, combines them and
// loop-invariant values with arithmetic that is exact lane by lane, and either stores the result the same
// way or folds it into a reduction local with an operation that gives the same bits in any order. Such a
// loop computes the same bits whether its elements are done one at a time or several per instruction, as
// long as the elements it stores do not overlap the ones it loads (see lowerVecMap's guards).
//
// A float reduction is not one: rounding makes the order of a float sum show.
//
// planVecMap recognizes one from the loop's bytecode, before the loop is lowered. The loop either tests
// its exit at the bottom, `br_if 0` just before its end, or at the top, `br_if 1` out of the block that
// ends right after it, before it does anything else, with `br 0` closing the body.

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

// vecNode ops. The arithmetic ones are the element type's.
const (
	vecOpLoad   = iota + 1 // arg: index into plan.loads
	vecOpInv               // arg: the slot of a local, at the start of the iteration
	vecOpInvAff            // arg: index into plan.invAffs, an i32 value the iteration does not change
	vecOpConst             // bits: the constant
	vecOpAdd
	vecOpSub
	vecOpMul
	vecOpDiv
	vecOpNeg
	vecOpAbs
	vecOpSqrt
	vecOpBand
	vecOpBor
	vecOpBxor
	vecOpShl  // arg: the shift
	vecOpShrU // arg: the shift
	vecOpShrS // arg: the shift
)

type vecNode struct {
	op   uint8
	a, b uint8 // operand node indices
	arg  uint8
	bits uint64
}

func vecBinaryOp(op uint8) bool {
	switch op {
	case vecOpAdd, vecOpSub, vecOpMul, vecOpDiv, vecOpBand, vecOpBor, vecOpBxor:
		return true
	}
	return false
}

func vecUnaryOp(op uint8) bool {
	switch op {
	case vecOpNeg, vecOpAbs, vecOpSqrt, vecOpShl, vecOpShrU, vecOpShrS:
		return true
	}
	return false
}

type vecAccess struct {
	addr   vecAffine
	offset uint64
	node   uint8 // for a store, the value stored
}

// vecVal kinds.
const (
	vkAff  = iota // an i32 affine form
	vkNode        // an element node
	vkCond        // the result of the comparison deciding the loop's exit
)

type vecVal struct {
	kind uint8
	aff  vecAffine
	node uint8
}

type vecPlan struct {
	// elem is the type of the loop's lanes, and elemSize its size in bytes.
	elem     wasm.ValueType
	elemSize uint32

	slotLocal [vecMaxLocals]wasm.Index
	slotType  [vecMaxLocals]wasm.ValueType
	slotN     int
	written   [vecMaxLocals]bool
	// readL0 marks the slots whose value at the start of an iteration something reads.
	readL0 [vecMaxLocals]bool
	// step is an induction slot's increment per iteration; induction marks those slots.
	step      [vecMaxLocals]uint32
	induction [vecMaxLocals]bool

	nodes   []vecNode
	loads   []vecAccess
	stores  []vecAccess
	invAffs []vecAffine

	// The loop goes round again while condOp(condA, condB) holds: evaluated at the start of each
	// iteration when top is set, after its updates otherwise. condOp is ne, lt_s or lt_u.
	condOp       wasm.Opcode
	condA, condB vecAffine
	top          bool

	// reduceSlot is the local a reducing loop folds its elements into with reduceOp, reduceNode being each
	// element's value; reduceSlot is -1 for a loop that stores instead.
	reduceSlot int
	reduceOp   uint8
	reduceNode uint8

	// partner is, for each load the element computation makes, the load of copy zero it corresponds to
	// and how many bytes further on it reads: itself and zero for copy zero's loads, -1 for a load no
	// computation uses.
	partner [vecMaxLoads]int8
	pshift  [vecMaxLoads]int64

	// unroll is how many element computations one source iteration does.
	unroll uint32
	// endPC is the offset just past the loop's end opcode.
	endPC int
}

type vecPlanner struct {
	why      string
	p        vecPlan
	body     []byte
	pc       int
	stack    []vecVal
	cur      [vecMaxLocals]vecVal
	set      [vecMaxLocals]bool
	types    []wasm.ValueType
	exitSeen bool
}

// planVecMap plans the loop whose body starts at body[pc], just past the loop's block type. types are the
// function's locals' types, parameters first. It reports false, and why, for a loop it does not take.
func planVecMap(body []byte, pc int, types []wasm.ValueType) (*vecPlan, string, bool) {
	pl := &vecPlanner{body: body, pc: pc, types: types}
	pl.p.nodes = make([]vecNode, 0, 16)
	pl.p.reduceSlot = -1
	for i := range pl.p.partner {
		pl.p.partner[i] = -1
	}
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
	switch t {
	case wasm.ValueTypeI32, wasm.ValueTypeI64, wasm.ValueTypeF32, wasm.ValueTypeF64:
	default:
		return 0, false
	}
	s := pl.p.slotN
	pl.p.slotN++
	pl.p.slotLocal[s], pl.p.slotType[s] = l, t
	return s, true
}

// elemType records t as the loop's lane type, refusing a loop that mixes two.
func (pl *vecPlanner) elemType(t wasm.ValueType) bool {
	if pl.p.elem == 0 {
		pl.p.elem = t
		pl.p.elemSize = 4
		if t == wasm.ValueTypeI64 || t == wasm.ValueTypeF64 {
			pl.p.elemSize = 8
		}
	}
	return pl.p.elem == t
}

func (pl *vecPlanner) node(n vecNode) (vecVal, bool) {
	if len(pl.p.nodes) == vecMaxNodes {
		return vecVal{}, false
	}
	pl.p.nodes = append(pl.p.nodes, n)
	return vecVal{kind: vkNode, node: uint8(len(pl.p.nodes) - 1)}, true
}

// toNode turns an i32 value into an element node of an i32 loop: a constant, the starting value of one
// local, or an invariant the lowering computes before the loop.
func (pl *vecPlanner) toNode(v vecVal) (uint8, bool) {
	switch v.kind {
	case vkNode:
		return v.node, true
	case vkCond:
		return 0, false
	}
	if !pl.elemType(wasm.ValueTypeI32) {
		return 0, false
	}
	a := v.aff
	if a.isConst() {
		n, ok := pl.node(vecNode{op: vecOpConst, bits: uint64(a.konst)})
		return n.node, ok
	}
	for s, k := range a.coeff {
		if k == 1 && a.konst == 0 {
			var unit vecAffine
			unit.coeff[s] = 1
			if a == unit {
				n, ok := pl.node(vecNode{op: vecOpInv, arg: uint8(s)})
				return n.node, ok
			}
		}
	}
	pl.p.invAffs = append(pl.p.invAffs, a)
	n, ok := pl.node(vecNode{op: vecOpInvAff, arg: uint8(len(pl.p.invAffs) - 1)})
	return n.node, ok
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

func (pl *vecPlanner) pop2() (x, y vecVal, ok bool) {
	y, ok1 := pl.pop()
	x, ok2 := pl.pop()
	return x, y, ok1 && ok2 && x.kind != vkCond && y.kind != vkCond
}

func (pl *vecPlanner) u32() (uint32, bool) {
	v, n, err := leb128.LoadUint32(pl.body[pl.pc:])
	if err != nil {
		return 0, false
	}
	pl.pc += int(n)
	return v, true
}

// shiftNode returns the node of x shifted by the constant y, for a lane of bits.
func (pl *vecPlanner) shiftNode(op uint8, x, y vecVal, bits uint32) (vecVal, bool) {
	var k uint64
	switch {
	case y.kind == vkAff && y.aff.isConst():
		k = uint64(y.aff.konst)
	case y.kind == vkNode && pl.p.nodes[y.node].op == vecOpConst:
		k = pl.p.nodes[y.node].bits
	default:
		return vecVal{}, false
	}
	xn, ok := pl.toNode(x)
	if !ok {
		return vecVal{}, false
	}
	return pl.node(vecNode{op: op, a: xn, arg: uint8(k % uint64(bits))})
}

func (pl *vecPlanner) binaryNode(op uint8, x, y vecVal) (vecVal, bool) {
	xn, ok1 := pl.toNode(x)
	yn, ok2 := pl.toNode(y)
	if !ok1 || !ok2 {
		return vecVal{}, false
	}
	return pl.node(vecNode{op: op, a: xn, b: yn})
}

// constBits reads a little-endian constant of n bytes.
func (pl *vecPlanner) constBits(n int) (uint64, bool) {
	if pl.pc+n > len(pl.body) {
		return 0, false
	}
	var bits uint64
	for i := n - 1; i >= 0; i-- {
		bits = bits<<8 | uint64(pl.body[pl.pc+i])
	}
	pl.pc += n
	return bits, true
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
			if t := pl.p.slotType[s]; t == wasm.ValueTypeI32 {
				var a vecAffine
				a.coeff[s] = 1
				pl.push(vecVal{kind: vkAff, aff: a})
			} else {
				if !pl.elemType(t) {
					return false
				}
				v, ok := pl.node(vecNode{op: vecOpInv, arg: uint8(s)})
				if !ok {
					return false
				}
				pl.push(v)
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
			if !ok || v.kind == vkCond || v.kind == vkAff && pl.p.slotType[s] != wasm.ValueTypeI32 {
				return false
			}
			if v.kind == vkNode && pl.p.slotType[s] != pl.p.elem {
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
			pl.push(vecVal{kind: vkAff, aff: vecAffine{konst: uint32(v)}})
		case wasm.OpcodeI64Const:
			v, n, err := leb128.LoadInt64(pl.body[pl.pc:])
			if err != nil || !pl.elemType(wasm.ValueTypeI64) {
				return false
			}
			pl.pc += int(n)
			nd, ok := pl.node(vecNode{op: vecOpConst, bits: uint64(v)})
			if !ok {
				return false
			}
			pl.push(nd)
		case wasm.OpcodeF64Const, wasm.OpcodeF32Const:
			t, size := wasm.ValueTypeF32, 4
			if op == wasm.OpcodeF64Const {
				t, size = wasm.ValueTypeF64, 8
			}
			bits, ok := pl.constBits(size)
			if !ok || !pl.elemType(t) {
				return false
			}
			nd, ok := pl.node(vecNode{op: vecOpConst, bits: bits})
			if !ok {
				return false
			}
			pl.push(nd)
		case wasm.OpcodeI32Add, wasm.OpcodeI32Sub, wasm.OpcodeI32Mul:
			x, y, ok := pl.pop2()
			if !ok {
				return false
			}
			if x.kind == vkAff && y.kind == vkAff {
				switch {
				case op == wasm.OpcodeI32Add:
					pl.push(vecVal{kind: vkAff, aff: x.aff.add(y.aff, 1)})
					continue
				case op == wasm.OpcodeI32Sub:
					pl.push(vecVal{kind: vkAff, aff: x.aff.add(y.aff, ^uint32(0))})
					continue
				case y.aff.isConst():
					pl.push(vecVal{kind: vkAff, aff: x.aff.scale(y.aff.konst)})
					continue
				case x.aff.isConst():
					pl.push(vecVal{kind: vkAff, aff: y.aff.scale(x.aff.konst)})
					continue
				}
			}
			o := map[wasm.Opcode]uint8{wasm.OpcodeI32Add: vecOpAdd, wasm.OpcodeI32Sub: vecOpSub, wasm.OpcodeI32Mul: vecOpMul}[op]
			v, ok := pl.binaryNode(o, x, y)
			if !ok {
				return false
			}
			pl.push(v)
		case wasm.OpcodeI32Shl:
			x, y, ok := pl.pop2()
			if !ok {
				return false
			}
			if x.kind == vkAff && y.kind == vkAff && y.aff.isConst() {
				pl.push(vecVal{kind: vkAff, aff: x.aff.scale(1 << (y.aff.konst & 31))})
				continue
			}
			v, ok := pl.shiftNode(vecOpShl, x, y, 32)
			if !ok {
				return false
			}
			pl.push(v)
		case wasm.OpcodeI32ShrU, wasm.OpcodeI32ShrS:
			x, y, ok := pl.pop2()
			if !ok {
				return false
			}
			o := uint8(vecOpShrU)
			if op == wasm.OpcodeI32ShrS {
				o = vecOpShrS
			}
			v, ok := pl.shiftNode(o, x, y, 32)
			if !ok {
				return false
			}
			pl.push(v)
		case wasm.OpcodeI32And, wasm.OpcodeI32Or, wasm.OpcodeI32Xor:
			x, y, ok := pl.pop2()
			if !ok {
				return false
			}
			if x.kind == vkAff && y.kind == vkAff && x.aff.isConst() && y.aff.isConst() {
				a, b := x.aff.konst, y.aff.konst
				r := map[wasm.Opcode]uint32{wasm.OpcodeI32And: a & b, wasm.OpcodeI32Or: a | b, wasm.OpcodeI32Xor: a ^ b}[op]
				pl.push(vecVal{kind: vkAff, aff: vecAffine{konst: r}})
				continue
			}
			o := map[wasm.Opcode]uint8{wasm.OpcodeI32And: vecOpBand, wasm.OpcodeI32Or: vecOpBor, wasm.OpcodeI32Xor: vecOpBxor}[op]
			v, ok := pl.binaryNode(o, x, y)
			if !ok {
				return false
			}
			pl.push(v)
		case wasm.OpcodeI64Add, wasm.OpcodeI64Sub, wasm.OpcodeI64And, wasm.OpcodeI64Or, wasm.OpcodeI64Xor:
			x, y, ok := pl.pop2()
			if !ok || x.kind != vkNode || y.kind != vkNode || pl.p.elem != wasm.ValueTypeI64 {
				return false
			}
			o := map[wasm.Opcode]uint8{
				wasm.OpcodeI64Add: vecOpAdd, wasm.OpcodeI64Sub: vecOpSub,
				wasm.OpcodeI64And: vecOpBand, wasm.OpcodeI64Or: vecOpBor, wasm.OpcodeI64Xor: vecOpBxor,
			}[op]
			v, ok := pl.node(vecNode{op: o, a: x.node, b: y.node})
			if !ok {
				return false
			}
			pl.push(v)
		case wasm.OpcodeI64Shl, wasm.OpcodeI64ShrU:
			// i64 has no signed vector shift before AVX-512, and no multiply.
			x, y, ok := pl.pop2()
			if !ok || x.kind != vkNode || pl.p.elem != wasm.ValueTypeI64 {
				return false
			}
			o := uint8(vecOpShl)
			if op == wasm.OpcodeI64ShrU {
				o = vecOpShrU
			}
			v, ok := pl.shiftNode(o, x, y, 64)
			if !ok {
				return false
			}
			pl.push(v)
		case wasm.OpcodeI32Load, wasm.OpcodeI64Load, wasm.OpcodeF32Load, wasm.OpcodeF64Load,
			wasm.OpcodeI32Store, wasm.OpcodeI64Store, wasm.OpcodeF32Store, wasm.OpcodeF64Store:
			t := map[wasm.Opcode]wasm.ValueType{
				wasm.OpcodeI32Load: wasm.ValueTypeI32, wasm.OpcodeI64Load: wasm.ValueTypeI64,
				wasm.OpcodeF32Load: wasm.ValueTypeF32, wasm.OpcodeF64Load: wasm.ValueTypeF64,
				wasm.OpcodeI32Store: wasm.ValueTypeI32, wasm.OpcodeI64Store: wasm.ValueTypeI64,
				wasm.OpcodeF32Store: wasm.ValueTypeF32, wasm.OpcodeF64Store: wasm.ValueTypeF64,
			}[op]
			if !pl.elemType(t) {
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
			if op <= wasm.OpcodeF64Load {
				addr, ok := pl.pop()
				if !ok || addr.kind != vkAff || len(pl.p.loads) == vecMaxLoads {
					return false
				}
				pl.p.loads = append(pl.p.loads, vecAccess{addr: addr.aff, offset: offset})
				v, ok := pl.node(vecNode{op: vecOpLoad, arg: uint8(len(pl.p.loads) - 1)})
				if !ok {
					return false
				}
				pl.push(v)
			} else {
				v, ok1 := pl.pop()
				addr, ok2 := pl.pop()
				if !ok1 || !ok2 || addr.kind != vkAff || len(pl.p.stores) == vecMaxStores {
					return false
				}
				vn, ok := pl.toNode(v)
				if !ok {
					return false
				}
				pl.p.stores = append(pl.p.stores, vecAccess{addr: addr.aff, offset: offset, node: vn})
			}
		case wasm.OpcodeF64Add, wasm.OpcodeF64Sub, wasm.OpcodeF64Mul, wasm.OpcodeF64Div,
			wasm.OpcodeF32Add, wasm.OpcodeF32Sub, wasm.OpcodeF32Mul, wasm.OpcodeF32Div:
			t := wasm.ValueTypeF32
			if op >= wasm.OpcodeF64Add {
				t = wasm.ValueTypeF64
			}
			x, y, ok := pl.pop2()
			if !ok || x.kind != vkNode || y.kind != vkNode || !pl.elemType(t) {
				return false
			}
			o := map[wasm.Opcode]uint8{
				wasm.OpcodeF64Add: vecOpAdd, wasm.OpcodeF64Sub: vecOpSub, wasm.OpcodeF64Mul: vecOpMul, wasm.OpcodeF64Div: vecOpDiv,
				wasm.OpcodeF32Add: vecOpAdd, wasm.OpcodeF32Sub: vecOpSub, wasm.OpcodeF32Mul: vecOpMul, wasm.OpcodeF32Div: vecOpDiv,
			}[op]
			v, ok := pl.node(vecNode{op: o, a: x.node, b: y.node})
			if !ok {
				return false
			}
			pl.push(v)
		case wasm.OpcodeF64Neg, wasm.OpcodeF64Abs, wasm.OpcodeF64Sqrt,
			wasm.OpcodeF32Neg, wasm.OpcodeF32Abs, wasm.OpcodeF32Sqrt:
			t := wasm.ValueTypeF32
			if op >= wasm.OpcodeF64Abs {
				t = wasm.ValueTypeF64
			}
			x, ok := pl.pop()
			if !ok || x.kind != vkNode || !pl.elemType(t) {
				return false
			}
			o := map[wasm.Opcode]uint8{
				wasm.OpcodeF64Neg: vecOpNeg, wasm.OpcodeF64Abs: vecOpAbs, wasm.OpcodeF64Sqrt: vecOpSqrt,
				wasm.OpcodeF32Neg: vecOpNeg, wasm.OpcodeF32Abs: vecOpAbs, wasm.OpcodeF32Sqrt: vecOpSqrt,
			}[op]
			v, ok := pl.node(vecNode{op: o, a: x.node})
			if !ok {
				return false
			}
			pl.push(v)
		case wasm.OpcodeI32Eqz:
			x, ok := pl.pop()
			if !ok || x.kind != vkAff || pl.p.condOp != 0 {
				return false
			}
			pl.p.condOp, pl.p.condA, pl.p.condB = wasm.OpcodeI32Eq, x.aff, vecAffine{}
			pl.push(vecVal{kind: vkCond})
		case wasm.OpcodeI32Eq, wasm.OpcodeI32Ne, wasm.OpcodeI32LtU, wasm.OpcodeI32LtS, wasm.OpcodeI32GtU, wasm.OpcodeI32GtS,
			wasm.OpcodeI32LeU, wasm.OpcodeI32LeS, wasm.OpcodeI32GeU, wasm.OpcodeI32GeS:
			x, y, ok := pl.pop2()
			if !ok || x.kind != vkAff || y.kind != vkAff || pl.p.condOp != 0 {
				return false
			}
			pl.p.condOp, pl.p.condA, pl.p.condB = op, x.aff, y.aff
			pl.push(vecVal{kind: vkCond})
		case wasm.OpcodeBrIf:
			l, ok := pl.u32()
			if !ok || l > 1 {
				return false
			}
			c, ok := pl.pop()
			if !ok || c.kind == vkNode || len(pl.stack) != 0 {
				return false
			}
			if c.kind == vkAff { // br_if on an i32 itself: taken while it is not zero.
				if pl.p.condOp != 0 {
					return false
				}
				pl.p.condOp, pl.p.condA, pl.p.condB = wasm.OpcodeI32Ne, c.aff, vecAffine{}
			}
			if l == 1 {
				// The top exit: the first thing the iteration does, then the body, closed by br 0.
				if pl.exitSeen || len(pl.p.stores) > 0 || pl.anySet() {
					return false
				}
				pl.exitSeen, pl.p.top = true, true
				continue
			}
			if pl.p.top || pl.pc >= len(pl.body) || pl.body[pl.pc] != wasm.OpcodeEnd {
				return false
			}
			pl.p.endPC = pl.pc + 1
			return true
		case wasm.OpcodeBr:
			l, ok := pl.u32()
			if !ok || l != 0 || !pl.p.top || len(pl.stack) != 0 {
				return false
			}
			// The loop's end, then the end of the block the top exit leaves.
			if pl.pc+1 >= len(pl.body) || pl.body[pl.pc] != wasm.OpcodeEnd || pl.body[pl.pc+1] != wasm.OpcodeEnd {
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

func (pl *vecPlanner) anySet() bool {
	for _, s := range pl.set {
		if s {
			return true
		}
	}
	return false
}

// finish checks what one iteration did against what a vector map loop may do.
func (pl *vecPlanner) finish() bool {
	p := &pl.p
	if p.elem == 0 {
		return false
	}
	for s := 0; s < p.slotN; s++ {
		if !p.written[s] {
			continue
		}
		v := pl.cur[s]
		if v.kind == vkAff {
			var want vecAffine
			want.coeff[s] = 1
			want.konst = v.aff.konst
			if v.aff == want && v.aff.konst != 0 {
				p.induction[s], p.step[s] = true, v.aff.konst
				continue
			}
		}
		if !p.readL0[s] {
			continue // A temporary, set before it is read.
		}
		if v.kind != vkNode || !pl.reduction(s, v.node) {
			pl.why = "recurrence"
			return false
		}
	}
	if !pl.normalizeExit() {
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
	if !pl.lanesOK() {
		return false
	}
	// An element reads no local the loop writes as it was when the iteration started: an induction's
	// value differs from one element to the next, and anything else written is a recurrence.
	roots := make([]uint8, 0, len(p.stores)+1)
	for _, st := range p.stores {
		roots = append(roots, st.node)
	}
	if p.reduceSlot >= 0 {
		roots = append(roots, p.reduceNode)
	}
	for _, n := range roots {
		for s := 0; s < p.slotN; s++ {
			if p.written[s] && pl.mentions(n, s) {
				pl.why = "written local in an element"
				return false
			}
		}
	}
	if p.reduceSlot >= 0 {
		if len(p.stores) != 0 {
			pl.why = "reduction and stores"
			return false
		}
		p.unroll = 1
		for _, l := range p.loads {
			if st := p.stride(l.addr); st != 0 && st != p.elemSize {
				pl.why = "load stride"
				return false
			}
		}
		if !pl.onlyReads(p.reduceNode, p.reduceSlot) {
			return false
		}
		return pl.selfPartners(p.reduceNode)
	}
	if len(p.stores) == 0 {
		return false
	}
	return pl.reroll()
}

// reduction accepts the local in slot s as the loop's reduction when its value at the end of an iteration
// is its value at the start combined, by an integer operation that gives the same bits in any order, with
// an element value that does not involve it.
func (pl *vecPlanner) reduction(s int, n uint8) bool {
	p := &pl.p
	if p.reduceSlot >= 0 || p.elem == wasm.ValueTypeF32 || p.elem == wasm.ValueTypeF64 || p.slotType[s] != p.elem {
		return false
	}
	nd := p.nodes[n]
	switch nd.op {
	case vecOpAdd, vecOpBand, vecOpBor, vecOpBxor:
	default:
		return false
	}
	isAcc := func(m uint8) bool { return p.nodes[m].op == vecOpInv && p.nodes[m].arg == uint8(s) }
	var elem uint8
	switch {
	case isAcc(nd.a):
		elem = nd.b
	case isAcc(nd.b):
		elem = nd.a
	default:
		return false
	}
	if pl.mentions(elem, s) {
		return false
	}
	p.reduceSlot, p.reduceOp, p.reduceNode = s, nd.op, elem
	return true
}

// mentions reports whether node n's tree reads slot s's starting value.
func (pl *vecPlanner) mentions(n uint8, s int) bool {
	nd := pl.p.nodes[n]
	switch {
	case nd.op == vecOpInv:
		return nd.arg == uint8(s)
	case nd.op == vecOpInvAff:
		return pl.p.invAffs[nd.arg].coeff[s] != 0
	case vecBinaryOp(nd.op):
		return pl.mentions(nd.a, s) || pl.mentions(nd.b, s)
	case vecUnaryOp(nd.op):
		return pl.mentions(nd.a, s)
	}
	return false
}

// onlyReads checks that nothing but the reduction reads the reduction local: no address, no exit
// condition and no other node.
func (pl *vecPlanner) onlyReads(elem uint8, s int) bool {
	for _, a := range pl.affines() {
		if a.coeff[s] != 0 {
			return false
		}
	}
	return !pl.mentions(elem, s)
}

// lanesOK refuses the operations a lane type has no vector instruction for.
func (pl *vecPlanner) lanesOK() bool {
	p := &pl.p
	for _, nd := range p.nodes {
		switch p.elem {
		case wasm.ValueTypeI64:
			if nd.op == vecOpMul || nd.op == vecOpShrS {
				pl.why = "i64 lane op"
				return false
			}
		case wasm.ValueTypeI32:
			if nd.op == vecOpDiv {
				return false
			}
		}
	}
	return true
}

// normalizeExit turns the exit test into the condition the loop goes round on: ne, lt_s or lt_u, on a left
// side that moves by a fixed step and a right side that does not move.
func (pl *vecPlanner) normalizeExit() bool {
	p := &pl.p
	if p.top {
		// The loop leaves when the condition holds: it goes round on its negation.
		switch p.condOp {
		case wasm.OpcodeI32Eq:
			p.condOp = wasm.OpcodeI32Ne
		case wasm.OpcodeI32GeS:
			p.condOp = wasm.OpcodeI32LtS
		case wasm.OpcodeI32GeU:
			p.condOp = wasm.OpcodeI32LtU
		case wasm.OpcodeI32Ne: // br_if 1 on an i32 itself: it leaves while it is not zero.
			pl.why = "exit condition"
			return false
		default:
			pl.why = "exit condition"
			return false
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
	return true
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
	return pl.selfPartners(p.stores[0].node)
}

// selfPartners makes each load of copy zero's computation, the tree under n, its own partner.
func (pl *vecPlanner) selfPartners(n uint8) bool {
	p := &pl.p
	nd := p.nodes[n]
	switch {
	case nd.op == vecOpLoad:
		return pl.pair(nd.arg, nd.arg, 0)
	case vecBinaryOp(nd.op):
		return pl.selfPartners(nd.a) && pl.selfPartners(nd.b)
	case vecUnaryOp(nd.op):
		return pl.selfPartners(nd.a)
	}
	return true
}

// pair records load b as reading what load a of copy zero does, shift bytes further on.
func (pl *vecPlanner) pair(a, b uint8, shift int64) bool {
	p := &pl.p
	if p.partner[b] >= 0 {
		return p.partner[b] == int8(a) && p.pshift[b] == shift
	}
	p.partner[b], p.pshift[b] = int8(a), shift
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
	if na.op != nb.op || na.arg != nb.arg && na.op != vecOpLoad && na.op != vecOpInvAff {
		return false
	}
	switch {
	case na.op == vecOpLoad:
		la, lb := p.loads[na.arg], p.loads[nb.arg]
		d, ok := shiftOf(la, lb)
		if !ok {
			return false
		}
		if p.stride(la.addr) == 0 {
			return d == 0 && pl.pair(na.arg, nb.arg, 0)
		}
		return d == shift && pl.pair(na.arg, nb.arg, shift)
	case na.op == vecOpInvAff:
		return p.invAffs[na.arg] == p.invAffs[nb.arg]
	case na.op == vecOpConst:
		return na.bits == nb.bits
	case vecUnaryOp(na.op):
		return pl.sameShifted(na.a, nb.a, shift)
	case vecBinaryOp(na.op):
		return pl.sameShifted(na.a, nb.a, shift) && pl.sameShifted(na.b, nb.b, shift)
	}
	return true // vecOpInv: the same slot, checked above.
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

package frontend

import (
	"math"

	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/wasm"
)

// vecChunk is how many elements one OpcodeVecMap does before the loop around it checks whether the
// runtime asked it to stop: a few thousand floats, microseconds of work.
const vecChunk = 4096

// vecGroup is the multiple of elements the vector part always does, so that every vector width a
// backend picks, up to eight lanes, divides it.
const vecGroup = 8

// vecMaxPtrs and vecMaxRegs bound a program by the registers its expansion has: pointers live in
// general registers beside the element count, invariants and temporaries in vector registers.
const (
	vecMaxPtrs = 7
	vecMaxRegs = 8
)

// vecStream is one moving pointer of a vector map loop: the loop's accesses that walk the same elements.
type vecStream struct {
	acc   vecAccess
	ptr   ssa.Value // the absolute address of element zero, once the guards passed
	base  ssa.Value // its i64 offset into memory
	store bool
}

// lowerVecMap runs the iterations a vector map loop can do as vectors, before the loop itself: when the
// trip count, the memory bounds and the streams' disjointness check out, it does all but the last
// iteration or more in OpcodeVecMap chunks and advances the induction locals past them, and the loop then
// runs the rest as it would have. When any check fails, it does nothing and the loop runs all of them.
// So the loop always runs at least one iteration itself, which leaves every local it writes holding what
// its last iteration wrote, and a check that fails leaves the scalar loop to trap where it would have.
func (c *Compiler) lowerVecMap(p *vecPlan) bool {
	prog, invSrc, streams := vecProgramOf(p)
	if _, ok := prog.Assign(); !ok || prog.Ptrs > vecMaxPtrs {
		return false
	}
	ftyp := ssa.TypeF32
	if p.f64 {
		ftyp = ssa.TypeF64
	}
	builder := c.ssaBuilder
	var l0 [vecMaxLocals]ssa.Value
	for s := 0; s < p.slotN; s++ {
		if p.readL0[s] || p.induction[s] {
			l0[s] = builder.MustFindValue(c.localVariable(p.slotLocal[s]))
		}
	}
	aff := func(a vecAffine) ssa.Value {
		v := builder.AllocateInstruction().AsIconst32(a.konst).Insert(builder).Return()
		for s, k := range a.coeff {
			if k == 0 {
				continue
			}
			term := l0[s]
			if k != 1 {
				kv := builder.AllocateInstruction().AsIconst32(k).Insert(builder).Return()
				term = builder.AllocateInstruction().AsImul(term, kv).Insert(builder).Return()
			}
			v = builder.AllocateInstruction().AsIadd(v, term).Insert(builder).Return()
		}
		return v
	}
	i64 := func(v uint64) ssa.Value { return builder.AllocateInstruction().AsIconst64(v).Insert(builder).Return() }
	zext := func(v ssa.Value) ssa.Value {
		return builder.AllocateInstruction().AsUExtend(v, 32, 64).Insert(builder).Return()
	}
	sext := func(v ssa.Value) ssa.Value {
		return builder.AllocateInstruction().AsSExtend(v, 32, 64).Insert(builder).Return()
	}
	bin := func(op ssa.Opcode, x, y ssa.Value) ssa.Value {
		instr := builder.AllocateInstruction()
		switch op {
		case ssa.OpcodeIadd:
			instr.AsIadd(x, y)
		case ssa.OpcodeIsub:
			instr.AsIsub(x, y)
		case ssa.OpcodeImul:
			instr.AsImul(x, y)
		case ssa.OpcodeUdiv:
			instr.AsUDiv(x, y, c.execCtxPtrValue)
		case ssa.OpcodeUrem:
			instr.AsURem(x, y, c.execCtxPtrValue)
		}
		return instr.Insert(builder).Return()
	}
	skip, join := builder.AllocateBasicBlock(), builder.AllocateBasicBlock()
	// skipIf leaves for the scalar loop when cond holds.
	skipIf := func(x, y ssa.Value, cond ssa.IntegerCmpCond) {
		cmp := builder.AllocateInstruction().AsIcmp(x, y, cond).Insert(builder).Return()
		builder.AllocateInstruction().AsBrnz(cmp, ssa.ValuesNil, skip).Insert(builder)
		next := builder.AllocateBasicBlock()
		c.insertJumpToBlock(ssa.ValuesNil, next)
		builder.SetCurrentBlock(next)
		builder.Seal(next)
	}

	// The trip count n: the loop goes round again while condOp(A(t), B) holds, A(t) = A(0) + t·dA
	// being the condition's left side after iteration t.
	dA := p.stride(p.condA)
	a0, b0 := aff(p.condA), aff(p.condB)
	var n ssa.Value
	switch p.condOp {
	case wasm.OpcodeI32Ne:
		// A(t) == B ends it: t = (B - A(0)) / dA, which must be exact. A counter counting down divides the
		// distance the other way by the magnitude of its step.
		diff, step := zext(bin(ssa.OpcodeIsub, b0, a0)), uint64(dA)
		if int32(dA) < 0 {
			diff, step = zext(bin(ssa.OpcodeIsub, a0, b0)), uint64(-dA)
		}
		skipIf(bin(ssa.OpcodeUrem, diff, i64(step)), i64(0), ssa.IntegerCmpCondNotEqual)
		n = bin(ssa.OpcodeIadd, bin(ssa.OpcodeUdiv, diff, i64(step)), i64(1))
	default: // lt_s, lt_u: A(t) >= B ends it.
		signed := p.condOp == wasm.OpcodeI32LtS
		ext, maxV, ge := zext, uint64(1<<32-1), ssa.IntegerCmpCondUnsignedGreaterThanOrEqual
		if signed {
			ext, maxV, ge = sext, uint64(1<<31-1), ssa.IntegerCmpCondSignedGreaterThanOrEqual
		}
		a, b := ext(a0), ext(b0)
		skipIf(a, b, ge) // It ends after one iteration: nothing to vectorize.
		// No step may carry A past the largest value before it reaches B.
		skipIf(bin(ssa.OpcodeIadd, b, i64(uint64(dA)-1)), i64(maxV), ssa.IntegerCmpCondSignedGreaterThan)
		t := bin(ssa.OpcodeUdiv, bin(ssa.OpcodeIadd, bin(ssa.OpcodeIsub, b, a), i64(uint64(dA)-1)), i64(uint64(dA)))
		n = bin(ssa.OpcodeIadd, t, i64(1))
	}

	// m source iterations go to the vector part: all but at least one, in whole groups of elements.
	u := uint64(p.unroll)
	q := uint64(vecGroup) / gcd(u, vecGroup)
	m := bin(ssa.OpcodeImul, bin(ssa.OpcodeUdiv, bin(ssa.OpcodeIsub, n, i64(1)), i64(q)), i64(q))
	skipIf(m, i64(0), ssa.IntegerCmpCondEqual)
	e := uint64(p.elemSize)
	elems := bin(ssa.OpcodeImul, m, i64(u))
	bytes := bin(ssa.OpcodeImul, elems, i64(e))

	memLen := c.getMemoryLenValue(0, false)
	for i := range streams {
		st := &streams[i]
		addr := zext(aff(st.acc.addr))
		// The i32 address computation does not wrap anywhere in the range, which is what makes an unrolled
		// copy's shift the same bytes whether it sits in the constant or the offset.
		skipIf(bin(ssa.OpcodeIadd, addr, bytes), i64(1<<32), ssa.IntegerCmpCondUnsignedGreaterThan)
		st.base = bin(ssa.OpcodeIadd, addr, i64(st.acc.offset))
		skipIf(bin(ssa.OpcodeIadd, st.base, bytes), memLen, ssa.IntegerCmpCondUnsignedGreaterThan)
	}
	var store *vecStream
	for i := range streams {
		if streams[i].store {
			store = &streams[i]
		}
	}
	storeEnd := bin(ssa.OpcodeIadd, store.base, bytes)
	// disjoint leaves unless [lo, lo+size) misses the stored range.
	disjoint := func(lo, size ssa.Value) {
		hi := bin(ssa.OpcodeIadd, lo, size)
		before := builder.AllocateInstruction().AsIcmp(hi, store.base, ssa.IntegerCmpCondUnsignedLessThanOrEqual).Insert(builder).Return()
		after := builder.AllocateInstruction().AsIcmp(storeEnd, lo, ssa.IntegerCmpCondUnsignedLessThanOrEqual).Insert(builder).Return()
		orInstr := builder.AllocateInstruction()
		orInstr.AsBor(before, after)
		either := orInstr.Insert(builder).Return()
		skipIf(either, builder.AllocateInstruction().AsIconst32(0).Insert(builder).Return(), ssa.IntegerCmpCondEqual)
	}
	for i := range streams {
		if st := &streams[i]; !st.store && !sameElements(st.acc, store.acc) {
			disjoint(st.base, bytes)
		}
	}

	memBase := c.getMemoryBaseValue(0, false)
	var ptrs []ssa.Value
	for i := range streams {
		streams[i].ptr = bin(ssa.OpcodeIadd, memBase, streams[i].base)
		ptrs = append(ptrs, streams[i].ptr)
	}
	invs := make([]ssa.Value, len(invSrc))
	for k, src := range invSrc {
		switch src.kind {
		case vecInvLoad:
			// The same element every iteration, which no store of the loop reaches: load it once.
			acc := p.loads[src.arg]
			lo := bin(ssa.OpcodeIadd, zext(aff(acc.addr)), i64(acc.offset))
			disjoint(lo, i64(e))
			skipIf(bin(ssa.OpcodeIadd, lo, i64(e)), memLen, ssa.IntegerCmpCondUnsignedGreaterThan)
			invs[k] = builder.AllocateInstruction().AsLoad(bin(ssa.OpcodeIadd, memBase, lo), 0, ftyp).Insert(builder).Return()
		case vecInvLocal:
			invs[k] = l0[src.arg]
		default:
			instr := builder.AllocateInstruction()
			if p.f64 {
				instr.AsF64const(math.Float64frombits(src.bits))
			} else {
				instr.AsF32const(math.Float32frombits(uint32(src.bits)))
			}
			invs[k] = instr.Insert(builder).Return()
		}
	}
	progN := builder.AddVecProgram(prog)

	// The vector part, in chunks with a preemption check between them.
	header, done := builder.AllocateBasicBlock(), builder.AllocateBasicBlock()
	doneSoFar := header.AddParam(builder, ssa.TypeI64)
	c.insertJumpToBlock(c.allocateVarLengthValues(1, i64(0)), header)
	builder.SetCurrentBlock(header)
	left := bin(ssa.OpcodeIsub, elems, doneSoFar)
	chunk := i64(vecChunk)
	big := builder.AllocateInstruction().AsIcmp(left, chunk, ssa.IntegerCmpCondUnsignedGreaterThan).Insert(builder).Return()
	count := builder.AllocateInstruction().AsSelect(big, chunk, left).Insert(builder).Return()
	offset := bin(ssa.OpcodeImul, doneSoFar, i64(e))
	args := c.allocateVarLengthValues(len(ptrs) + 1 + len(invs))
	for _, ptr := range ptrs {
		args = args.Append(builder.VarLengthPool(), bin(ssa.OpcodeIadd, ptr, offset))
	}
	args = args.Append(builder.VarLengthPool(), count)
	for _, v := range invs {
		args = args.Append(builder.VarLengthPool(), v)
	}
	builder.AllocateInstruction().AsVecMap(progN, args).Insert(builder)
	next := bin(ssa.OpcodeIadd, doneSoFar, count)
	c.emitPreemptCheck()
	more := builder.AllocateInstruction().AsIcmp(next, elems, ssa.IntegerCmpCondUnsignedLessThan).Insert(builder).Return()
	builder.AllocateInstruction().AsBrnz(more, c.allocateVarLengthValues(1, next), header).Insert(builder)
	c.insertJumpToBlock(ssa.ValuesNil, done)
	builder.Seal(header)

	// The loop goes on from where the vector part stopped.
	builder.SetCurrentBlock(done)
	builder.Seal(done)
	m32 := builder.AllocateInstruction().AsIreduce(m, ssa.TypeI32).Insert(builder).Return()
	for s := 0; s < p.slotN; s++ {
		if !p.induction[s] {
			continue
		}
		stepV := builder.AllocateInstruction().AsIconst32(p.step[s]).Insert(builder).Return()
		adv := builder.AllocateInstruction().AsImul(m32, stepV).Insert(builder).Return()
		v := builder.AllocateInstruction().AsIadd(l0[s], adv).Insert(builder).Return()
		builder.DefineVariableInCurrentBB(c.localVariable(p.slotLocal[s]), v)
	}
	c.insertJumpToBlock(ssa.ValuesNil, join)

	builder.SetCurrentBlock(skip)
	builder.Seal(skip)
	c.insertJumpToBlock(ssa.ValuesNil, join)

	builder.SetCurrentBlock(join)
	builder.Seal(join)
	return true
}

// Where an invariant of a VecProgram comes from.
const (
	vecInvLoad  = iota // an element the loop loads every iteration: arg is the load
	vecInvLocal        // a float local the loop does not write: arg is its slot
	vecInvConst        // a constant: bits
)

type vecInvSource struct {
	kind uint8
	arg  uint8
	bits uint64
}

// vecProgramOf turns the element computation of p into a VecProgram, its invariants' sources and the
// streams its pointers walk, the store's first.
func vecProgramOf(p *vecPlan) (*ssa.VecProgram, []vecInvSource, []vecStream) {
	streams := vecStreamsOf(p)
	prog := &ssa.VecProgram{F64: p.f64, Ptrs: len(streams)}
	var invSrc []vecInvSource
	addInv := func(src vecInvSource) uint8 {
		for k, s := range invSrc {
			if s == src {
				return uint8(k)
			}
		}
		invSrc = append(invSrc, src)
		return uint8(len(invSrc) - 1)
	}
	opOf := make([]int, len(p.nodes))
	for i := range opOf {
		opOf[i] = -1
	}
	var emit func(n uint8) uint8
	emit = func(n uint8) uint8 {
		if o := opOf[n]; o >= 0 {
			return uint8(o)
		}
		nd := p.nodes[n]
		var op ssa.VecOp
		switch nd.op {
		case vecOpLoad:
			if acc := p.loads[nd.arg]; p.stride(acc.addr) == 0 {
				op = ssa.VecOp{Kind: ssa.VecOpInv, Arg: addInv(vecInvSource{kind: vecInvLoad, arg: nd.arg})}
			} else {
				op = ssa.VecOp{Kind: ssa.VecOpLoad, Arg: uint8(streamOf(streams, acc))}
			}
		case vecOpInv:
			op = ssa.VecOp{Kind: ssa.VecOpInv, Arg: addInv(vecInvSource{kind: vecInvLocal, arg: nd.arg})}
		case vecOpConst:
			op = ssa.VecOp{Kind: ssa.VecOpInv, Arg: addInv(vecInvSource{kind: vecInvConst, bits: nd.bits})}
		case vecOpNeg, vecOpAbs:
			a := emit(nd.a)
			// A sign flip and an absolute value are bit operations on the sign bit, as the scalar ones are.
			sign := uint64(1) << 31
			if p.f64 {
				sign = 1 << 63
			}
			mask, kind := sign, uint8(ssa.VecOpXor)
			if nd.op == vecOpAbs {
				mask, kind = sign-1, ssa.VecOpAnd
			}
			op = ssa.VecOp{Kind: kind, A: a, Arg: addInv(vecInvSource{kind: vecInvConst, bits: mask})}
		case vecOpSqrt:
			op = ssa.VecOp{Kind: ssa.VecOpSqrt, A: emit(nd.a)}
		default:
			a, b := emit(nd.a), emit(nd.b)
			kind := map[uint8]uint8{vecOpAdd: ssa.VecOpAdd, vecOpSub: ssa.VecOpSub, vecOpMul: ssa.VecOpMul, vecOpDiv: ssa.VecOpDiv}[nd.op]
			op = ssa.VecOp{Kind: kind, A: a, B: b}
		}
		prog.Ops = append(prog.Ops, op)
		opOf[n] = len(prog.Ops) - 1
		return uint8(opOf[n])
	}
	emit(p.stores[0].node)
	prog.Invs = len(invSrc)
	return prog, invSrc, streams
}

// vecStreamsOf returns the moving accesses of the element computation, the store's first: the loads its
// value tree makes, one stream per distinct element walk. The other unrolled copies' accesses are these
// shifted by whole elements, so the same streams cover them.
func vecStreamsOf(p *vecPlan) []vecStream {
	streams := []vecStream{{acc: p.stores[0], store: true}}
	var walk func(n uint8)
	walk = func(n uint8) {
		nd := p.nodes[n]
		switch nd.op {
		case vecOpLoad:
			if l := p.loads[nd.arg]; p.stride(l.addr) != 0 && streamOf(streams, l) < 0 {
				streams = append(streams, vecStream{acc: l})
			}
		case vecOpInv, vecOpConst:
		case vecOpNeg, vecOpAbs, vecOpSqrt:
			walk(nd.a)
		default:
			walk(nd.a)
			walk(nd.b)
		}
	}
	walk(p.stores[0].node)
	return streams
}

// streamOf returns the index of the stream walking the elements acc reads, or -1.
func streamOf(streams []vecStream, acc vecAccess) int {
	for i := range streams {
		if sameElements(streams[i].acc, acc) {
			return i
		}
	}
	return -1
}

// sameElements reports whether two accesses with the same stride touch the same element every iteration.
func sameElements(a, b vecAccess) bool {
	d, ok := shiftOf(a, b)
	return ok && d == 0
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// vecMapMemory reports whether the function's memory is one a vector map loop can address: memory zero,
// 32-bit and not shared, with no listener watching each access.
func (c *Compiler) vecMapMemory() bool {
	return c.needMemory && len(c.memoryShared) > 0 && !c.memoryShared[0] && !c.memoryIsIndex64(0) && !c.needListener
}

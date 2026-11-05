package frontend

import (
	"math"
	"math/bits"

	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/wasm"
)

// vecChunk is how many elements one OpcodeVecMap does before the loop around it checks whether the
// runtime asked it to stop: a few thousand elements, microseconds of work.
const vecChunk = 4096

// vecGroupBytes is the multiple of bytes the vector part always does, so that every vector width a
// backend picks, up to 32 bytes, unrolled twice, divides it.
const vecGroupBytes = 64

// vecMaxPtrs bounds a program by the general registers its expansion has for pointers, beside the
// element count.
const vecMaxPtrs = 7

// vecStream is one moving pointer of a vector map loop: the loop's accesses that walk the same elements.
type vecStream struct {
	acc   vecAccess
	base  ssa.Value // its i64 offset into memory in the first iteration
	store bool
}

func vecSSAType(t wasm.ValueType) ssa.Type {
	switch t {
	case wasm.ValueTypeI32:
		return ssa.TypeI32
	case wasm.ValueTypeI64:
		return ssa.TypeI64
	case wasm.ValueTypeF32:
		return ssa.TypeF32
	default:
		return ssa.TypeF64
	}
}

// lowerVecMap runs the iterations a vector map loop can do as vectors, before the loop itself: when the
// trip count, the memory bounds and the streams' disjointness check out, it does all but the last
// iteration or more in OpcodeVecMap chunks and advances the induction and reduction locals past them, and
// the loop then runs the rest as it would have. When any check fails, it does nothing and the loop runs
// all of them. So the loop always runs at least one iteration itself, which leaves every local it writes
// holding what its last iteration wrote, and a check that fails leaves the scalar loop to trap where it
// would have.
func (c *Compiler) lowerVecMap(p *vecPlan) bool {
	prog, invSrc, streams, iotas := vecProgramOf(p)
	if _, ok := prog.Assign(false); !ok || prog.Ptrs > vecMaxPtrs || prog.Ptrs+len(prog.Reduces)-1 > vecMaxPtrs {
		return false
	}
	// Invariants reach the program as the float type of their size, which is what carries them in a
	// vector register.
	vtyp := ssa.TypeF32
	if p.elemSize == 8 {
		vtyp = ssa.TypeF64
	}
	builder := c.ssaBuilder
	var l0 [vecMaxLocals]ssa.Value
	for s := range l0 {
		l0[s] = ssa.ValueInvalid
	}
	for s := 0; s < p.slotN; s++ {
		if p.readL0[s] || p.induction[s] {
			l0[s] = builder.MustFindValue(c.localVariable(p.slotLocal[s]))
		}
	}
	// kOf reports v's value when it is a constant: the helpers below fold those, which nothing after
	// them would.
	kOf := func(v ssa.Value) (uint64, bool) {
		if instr := builder.InstructionOfValue(v); instr != nil && instr.Opcode() == ssa.OpcodeIconst {
			return instr.ConstantVal(), true
		}
		return 0, false
	}
	// constL0 reports the value a slot holds on entry when it is a constant.
	constL0 := func(s int) (uint32, bool) {
		k, ok := kOf(l0[s])
		return uint32(k), ok
	}
	// constAff evaluates a on entry, when every local it reads holds a constant then.
	constAff := func(a vecAffine) (uint32, bool) {
		v := a.konst
		for s, k := range a.coeff {
			if k != 0 {
				l, ok := constL0(s)
				if !ok {
					return 0, false
				}
				v += k * l
			}
		}
		return v, true
	}
	i32 := func(v uint32) ssa.Value { return builder.AllocateInstruction().AsIconst32(v).Insert(builder).Return() }
	i64 := func(v uint64) ssa.Value { return builder.AllocateInstruction().AsIconst64(v).Insert(builder).Return() }
	// mulK multiplies by a constant, shifting for a power of two.
	mulK := func(x ssa.Value, k uint64) ssa.Value {
		if kx, ok := kOf(x); ok {
			if x.Type() == ssa.TypeI32 {
				return i32(uint32(kx * k))
			}
			return i64(kx * k)
		}
		switch {
		case k == 1:
			return x
		case k&(k-1) == 0:
			sh := i64(uint64(bits.TrailingZeros64(k)))
			if x.Type() == ssa.TypeI32 {
				sh = i32(uint32(bits.TrailingZeros64(k)))
			}
			return builder.AllocateInstruction().AsIshl(x, sh).Insert(builder).Return()
		case x.Type() == ssa.TypeI32:
			return builder.AllocateInstruction().AsImul(x, i32(uint32(k))).Insert(builder).Return()
		}
		return builder.AllocateInstruction().AsImul(x, i64(k)).Insert(builder).Return()
	}
	aff := func(a vecAffine) ssa.Value {
		v := ssa.ValueInvalid
		for s, k := range a.coeff {
			if k == 0 {
				continue
			}
			if l, ok := constL0(s); ok {
				a.konst += k * l
				continue
			}
			term := mulK(l0[s], uint64(k))
			if v.Valid() {
				v = builder.AllocateInstruction().AsIadd(v, term).Insert(builder).Return()
			} else {
				v = term
			}
		}
		switch {
		case !v.Valid():
			return i32(a.konst)
		case a.konst != 0:
			return builder.AllocateInstruction().AsIadd(v, i32(a.konst)).Insert(builder).Return()
		}
		return v
	}
	zext := func(v ssa.Value) ssa.Value {
		if k, ok := kOf(v); ok {
			return i64(uint64(uint32(k)))
		}
		return builder.AllocateInstruction().AsUExtend(v, 32, 64).Insert(builder).Return()
	}
	sext := func(v ssa.Value) ssa.Value {
		if k, ok := kOf(v); ok {
			return i64(uint64(int64(int32(k))))
		}
		return builder.AllocateInstruction().AsSExtend(v, 32, 64).Insert(builder).Return()
	}
	bin := func(op ssa.Opcode, x, y ssa.Value) ssa.Value {
		if op == ssa.OpcodeIadd || op == ssa.OpcodeIsub {
			ky, ok := kOf(y)
			if kx, okx := kOf(x); ok && okx {
				if op == ssa.OpcodeIsub {
					ky = -ky
				}
				if x.Type() == ssa.TypeI32 {
					return i32(uint32(kx + ky))
				}
				return i64(kx + ky)
			}
			if ok && ky == 0 {
				return x
			}
		}
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
		case ssa.OpcodeBand:
			instr.AsBand(x, y)
		case ssa.OpcodeBor:
			instr.AsBor(x, y)
		case ssa.OpcodeBxor:
			instr.AsBxor(x, y)
		}
		return instr.Insert(builder).Return()
	}
	// udiv and urem divide by a constant: a shift or a mask for the powers of two counts and strides
	// usually are, rather than a divide instruction with its zero check.
	udiv := func(x ssa.Value, k uint64) ssa.Value {
		if k == 1 {
			return x
		}
		if k&(k-1) == 0 {
			sh := builder.AllocateInstruction().AsIconst64(uint64(bits.TrailingZeros64(k))).Insert(builder).Return()
			return builder.AllocateInstruction().AsUshr(x, sh).Insert(builder).Return()
		}
		return bin(ssa.OpcodeUdiv, x, i64(k))
	}
	urem := func(x ssa.Value, k uint64) ssa.Value {
		if k&(k-1) == 0 {
			return bin(ssa.OpcodeBand, x, i64(k-1))
		}
		return bin(ssa.OpcodeUrem, x, i64(k))
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

	// The trip count: t iterations after the first, for a loop that tests at the bottom, after its
	// updates, whether to go round again with condOp(A, B), A moving by dA per iteration. A loop that
	// tests at the top does one fewer, since it tests A before its first. m of them, at most t so that
	// the loop does at least one itself, go to the vector part in whole groups of q, and only when there
	// are enough to pay for the checks.
	dA := p.stride(p.condA)
	u, e := uint64(p.unroll), uint64(p.elemSize)
	q := uint64(vecGroupBytes) / e / gcd(u, vecGroupBytes/e)
	minM := vecMinIterations(p, q, len(streams))
	need := minM
	if p.top {
		need++
	}
	t := ssa.ValueInvalid
	// A counter counting up from zero or more to a small positive bound reaches it in few steps. Starting
	// below zero is legal, and unsigned lt rules it out, but it is unusual: a loop that would pay off only
	// then is not worth its checks.
	if cb, ok := constAff(p.condB); ok && int32(cb) > 0 && int32(dA) > 0 && uint64(cb)/uint64(dA)+1 < need {
		return false
	}
	if ca, ok := constAff(p.condA); ok {
		if cb, ok := constAff(p.condB); ok {
			ct, ok := vecConstTrips(p.condOp, ca, cb, dA)
			if !ok || ct < need {
				return false
			}
			if p.top {
				ct--
			}
			t = i64(ct)
		}
	}
	if !t.Valid() {
		a0, b0 := aff(p.condA), aff(p.condB)
		switch p.condOp {
		case wasm.OpcodeI32Ne:
			// A == B ends it: t = (B - A) / dA, which must be exact. A counter counting down divides the
			// distance the other way by the magnitude of its step.
			diff, step := zext(bin(ssa.OpcodeIsub, b0, a0)), uint64(dA)
			if int32(dA) < 0 {
				diff, step = zext(bin(ssa.OpcodeIsub, a0, b0)), uint64(-dA)
			}
			if step != 1 {
				skipIf(urem(diff, step), i64(0), ssa.IntegerCmpCondNotEqual)
			}
			t = udiv(diff, step)
		default: // lt_s, lt_u: A >= B ends it.
			signed := p.condOp == wasm.OpcodeI32LtS
			ext, maxV := zext, uint64(1<<32-1)
			if signed {
				ext, maxV = sext, uint64(1<<31-1)
			}
			// No step may carry A past the largest value before it reaches B.
			if cb, ok := constAff(p.condB); ok {
				b := uint64(cb)
				if signed {
					b = uint64(int64(int32(cb)))
				}
				if int64(b)+int64(dA)-1 > int64(maxV) {
					return false
				}
			} else if dA > 1 {
				skipIf(bin(ssa.OpcodeIadd, ext(b0), i64(uint64(dA)-1)), i64(maxV), ssa.IntegerCmpCondSignedGreaterThan)
			}
			// When A starts at or past B the distance is negative, as a signed i64: it ends at once.
			dist := bin(ssa.OpcodeIsub, ext(b0), ext(a0))
			skipIf(dist, i64(0), ssa.IntegerCmpCondSignedLessThanOrEqual)
			if dA > 1 {
				dist = bin(ssa.OpcodeIadd, dist, i64(uint64(dA)-1))
			}
			t = udiv(dist, uint64(dA))
		}
		skipIf(t, i64(need), ssa.IntegerCmpCondUnsignedLessThan)
		if p.top {
			t = bin(ssa.OpcodeIsub, t, i64(1))
		}
	}
	var m ssa.Value
	if kt, ok := kOf(t); ok {
		m = i64(kt &^ (q - 1))
	} else {
		m = bin(ssa.OpcodeBand, t, i64(^(q - 1)))
	}
	elems := mulK(m, u)
	bytes := mulK(elems, e)

	memLen := c.getMemoryLenValue(0, false)
	// walk returns a moving access's i64 offset into memory in the first iteration. Its i32 address
	// computation cannot wrap anywhere in the range once the range is in bounds, which every access's is
	// or is pinned to one that is: memory is at most 4 GiB.
	walk := func(acc vecAccess) ssa.Value {
		addr := zext(aff(acc.addr))
		if acc.offset == 0 {
			return addr
		}
		return bin(ssa.OpcodeIadd, addr, i64(acc.offset))
	}
	inBounds := func(base, size ssa.Value) {
		skipIf(bin(ssa.OpcodeIadd, base, size), memLen, ssa.IntegerCmpCondUnsignedGreaterThan)
	}
	for i := range streams {
		streams[i].base = walk(streams[i].acc)
		inBounds(streams[i].base, bytes)
	}

	// Every access the scalar loop makes is one the guards cover, used by the computation or not: a load
	// whose value nothing uses still traps. One the program reads through a stream must reach exactly the
	// element the stream does, through its own address computation: two i32 addresses that differ only
	// in a constant agree up to wrapping, which pin checks at run time.
	streamOfLoad := make([]int, len(p.loads))
	for i := range p.loads {
		streamOfLoad[i] = -1
		if p.partner[i] == int8(i) && p.stride(p.loads[i].addr) != 0 {
			streamOfLoad[i] = streamOf(streams, p.loads[i])
		}
	}
	// pin leaves unless acc's first element is shift bytes past stream st's, which the planner found
	// it is but for wrapping. It cannot wrap when its address computation adds at most less than one
	// iteration's stride to the stream's: the stream's range, in bounds, ends below 4 GiB by at least
	// that much in every iteration.
	pin := func(acc vecAccess, st int, shift int64) {
		rep := streams[st].acc
		if d := int32(acc.addr.konst - rep.addr.konst); d >= 0 && uint64(d) < u*e {
			return
		}
		want := bin(ssa.OpcodeIadd, streams[st].base, i64(uint64(shift)))
		skipIf(walk(acc), want, ssa.IntegerCmpCondNotEqual)
	}
	for k := 1; k < len(p.stores); k++ {
		pin(p.stores[k], 0, int64(k)*int64(e))
	}
	for i, l := range p.loads {
		if p.stride(l.addr) == 0 {
			continue
		}
		switch k := int(p.partner[i]); {
		case k < 0:
			inBounds(walk(l), bytes)
		default:
			pin(l, streamOfLoad[k], p.pshift[i])
		}
	}

	var store *vecStream
	if len(streams) > 0 && streams[0].store {
		store = &streams[0]
	}
	var storeEnd ssa.Value
	if store != nil {
		storeEnd = bin(ssa.OpcodeIadd, store.base, bytes)
	}
	// disjoint leaves unless [lo, lo+size) misses the stored range: it ends before the range starts, or
	// starts after the range ends.
	disjoint := func(lo, size ssa.Value) {
		if store == nil {
			return
		}
		hi := bin(ssa.OpcodeIadd, lo, size)
		ok := builder.AllocateBasicBlock()
		before := builder.AllocateInstruction().AsIcmp(hi, store.base, ssa.IntegerCmpCondUnsignedLessThanOrEqual).Insert(builder).Return()
		builder.AllocateInstruction().AsBrnz(before, ssa.ValuesNil, ok).Insert(builder)
		next := builder.AllocateBasicBlock()
		c.insertJumpToBlock(ssa.ValuesNil, next)
		builder.SetCurrentBlock(next)
		builder.Seal(next)
		skipIf(storeEnd, lo, ssa.IntegerCmpCondUnsignedGreaterThan)
		c.insertJumpToBlock(ssa.ValuesNil, ok)
		builder.SetCurrentBlock(ok)
		builder.Seal(ok)
	}
	for i := range streams {
		if st := &streams[i]; !st.store {
			disjoint(st.base, bytes)
		}
	}

	memBase := c.getMemoryBaseValue(0, false)
	// The loads every iteration makes of one element: the ones the program takes as invariants are loaded
	// once, after the checks a moving load gets, and every other one must read its partner's element, or
	// be in bounds when nothing uses it.
	invLo := make([]ssa.Value, len(p.loads))
	for i := range invLo {
		invLo[i] = ssa.ValueInvalid
	}
	invLoOf := func(i int) ssa.Value {
		if !invLo[i].Valid() {
			acc := p.loads[i]
			invLo[i] = bin(ssa.OpcodeIadd, zext(aff(acc.addr)), i64(acc.offset))
		}
		return invLo[i]
	}
	// toVec turns an invariant into the float type that carries it in a vector register: an i32 into an
	// i64 lane zero-extended, as a Spaced program wants it.
	toVec := func(v ssa.Value) ssa.Value {
		if v.Type() == vtyp {
			return v
		}
		if v.Type() == ssa.TypeI32 && vtyp == ssa.TypeF64 {
			v = zext(v)
		}
		return builder.AllocateInstruction().AsBitcast(v, vtyp).Insert(builder).Return()
	}
	// An iota starts each chunk where the elements before it left it, from iotaBase.
	iotaBase := make([]ssa.Value, len(iotas))
	for k, a := range iotas {
		iotaBase[k] = aff(a)
	}
	invs := make([]ssa.Value, len(invSrc))
	for k, src := range invSrc {
		switch src.kind {
		case vecInvIotaBase:
			continue
		case vecInvIotaStep:
			invs[k] = i32(p.stride(iotas[src.arg]))
		case vecInvLoad:
			lo := invLoOf(int(src.arg))
			disjoint(lo, i64(e))
			inBounds(lo, i64(e))
			invs[k] = builder.AllocateInstruction().AsLoad(bin(ssa.OpcodeIadd, memBase, lo), 0, vtyp).Insert(builder).Return()
		case vecInvLocal:
			invs[k] = l0[src.arg]
		case vecInvAff:
			invs[k] = aff(p.invAffs[src.arg])
		default:
			instr := builder.AllocateInstruction()
			if p.elemSize == 8 {
				instr.AsF64const(math.Float64frombits(src.bits))
			} else {
				instr.AsF32const(math.Float32frombits(uint32(src.bits)))
			}
			invs[k] = instr.Insert(builder).Return()
		}
		invs[k] = toVec(invs[k])
	}
	for i, l := range p.loads {
		if p.stride(l.addr) != 0 {
			continue
		}
		switch k := int(p.partner[i]); {
		case k < 0:
			inBounds(invLoOf(i), i64(e))
		case k != i:
			// The same element as its partner, which is in bounds, unless its address wraps where the
			// partner's does not: it adds less than an element to it, which cannot.
			if d := int32(l.addr.konst - p.loads[k].addr.konst); d < 0 || uint64(d) >= e {
				skipIf(invLoOf(i), invLoOf(k), ssa.IntegerCmpCondNotEqual)
			}
		}
	}
	progN := builder.AddVecProgram(prog)

	ptrs := make([]ssa.Value, len(streams))
	for i := range streams {
		ptrs[i] = bin(ssa.OpcodeIadd, memBase, streams[i].base)
	}

	// The vector part, in chunks with a preemption check between them.
	header, done := builder.AllocateBasicBlock(), builder.AllocateBasicBlock()
	doneSoFar := header.AddParam(builder, ssa.TypeI64)
	entryArgs := c.allocateVarLengthValues(2, i64(0))
	accs := make([]ssa.Value, len(p.reduces))
	for k, r := range p.reduces {
		accs[k] = header.AddParam(builder, vecSSAType(p.slotType[r.slot]))
		entryArgs = entryArgs.Append(builder.VarLengthPool(), l0[r.slot])
	}
	c.insertJumpToBlock(entryArgs, header)
	builder.SetCurrentBlock(header)
	if len(iotas) > 0 {
		done32 := builder.AllocateInstruction().AsIreduce(doneSoFar, ssa.TypeI32).Insert(builder).Return()
		for k, src := range invSrc {
			if src.kind == vecInvIotaBase {
				invs[k] = toVec(bin(ssa.OpcodeIadd, iotaBase[src.arg], mulK(done32, uint64(p.stride(iotas[src.arg])))))
			}
		}
	}
	left := bin(ssa.OpcodeIsub, elems, doneSoFar)
	chunk := i64(vecChunk)
	big := builder.AllocateInstruction().AsIcmp(left, chunk, ssa.IntegerCmpCondUnsignedGreaterThan).Insert(builder).Return()
	count := builder.AllocateInstruction().AsSelect(big, chunk, left).Insert(builder).Return()
	offset := mulK(doneSoFar, e)
	args := c.allocateVarLengthValues(len(streams) + 1 + len(invs))
	for _, ptr := range ptrs {
		args = args.Append(builder.VarLengthPool(), bin(ssa.OpcodeIadd, ptr, offset))
	}
	args = args.Append(builder.VarLengthPool(), count)
	for _, v := range invs {
		args = args.Append(builder.VarLengthPool(), v)
	}
	vm := builder.AllocateInstruction().AsVecMap(progN, args).Insert(builder)
	next := bin(ssa.OpcodeIadd, doneSoFar, count)
	backArgs := c.allocateVarLengthValues(2, next)
	first, rest := vm.Returns()
	for k, r := range p.reduces {
		total := first
		if k > 0 {
			total = rest[k-1]
		}
		accs[k] = bin(map[uint8]ssa.Opcode{
			vecOpAdd: ssa.OpcodeIadd, vecOpBand: ssa.OpcodeBand, vecOpBor: ssa.OpcodeBor, vecOpBxor: ssa.OpcodeBxor,
		}[r.op], accs[k], total)
		backArgs = backArgs.Append(builder.VarLengthPool(), accs[k])
	}
	c.emitPreemptCheck()
	more := builder.AllocateInstruction().AsIcmp(next, elems, ssa.IntegerCmpCondUnsignedLessThan).Insert(builder).Return()
	builder.AllocateInstruction().AsBrnz(more, backArgs, header).Insert(builder)
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
		v := builder.AllocateInstruction().AsIadd(l0[s], mulK(m32, uint64(p.step[s]))).Insert(builder).Return()
		builder.DefineVariableInCurrentBB(c.localVariable(p.slotLocal[s]), v)
	}
	for k, r := range p.reduces {
		builder.DefineVariableInCurrentBB(c.localVariable(p.slotLocal[r.slot]), accs[k])
	}
	c.insertJumpToBlock(ssa.ValuesNil, join)

	builder.SetCurrentBlock(skip)
	builder.Seal(skip)
	c.insertJumpToBlock(ssa.ValuesNil, join)

	builder.SetCurrentBlock(join)
	builder.Seal(join)
	return true
}

// vecMinIterations returns the fewest source iterations, a multiple of q, worth sending to the vector
// part: enough that what they save over the scalar loop pays for the checks and the setup, both counted
// roughly in instructions.
func vecMinIterations(p *vecPlan, q uint64, streams int) uint64 {
	fixed := uint64(40 + 6*streams)
	perIter := uint64(len(p.loads) + 2*len(p.stores) + 3)
	for _, nd := range p.nodes {
		if vecBinaryOp(nd.op) || vecUnaryOp(nd.op) {
			perIter++
		}
	}
	// The vector part does 64 bytes in a few instructions where the scalar loop does an iteration's
	// worth in perIter.
	saved := perIter - perIter*uint64(p.unroll)*uint64(p.elemSize)/vecGroupBytes
	if saved == 0 {
		saved = 1
	}
	n := (fixed + saved - 1) / saved
	return (n + q - 1) / q * q
}

// vecConstTrips returns t, as lowerVecMap computes it, for a condition on constants, and false when the
// loop does not end the way vectorizing it needs.
func vecConstTrips(op wasm.Opcode, a, b, dA uint32) (uint64, bool) {
	switch op {
	case wasm.OpcodeI32Ne:
		diff, step := b-a, dA
		if int32(dA) < 0 {
			diff, step = a-b, -dA
		}
		if diff%step != 0 {
			return 0, false
		}
		return uint64(diff / step), true
	case wasm.OpcodeI32LtS:
		x, y := int64(int32(a)), int64(int32(b))
		if x >= y || y+int64(dA)-1 > 1<<31-1 {
			return 0, false
		}
		return uint64((y - x + int64(dA) - 1) / int64(dA)), true
	default:
		x, y := uint64(a), uint64(b)
		if x >= y || y+uint64(dA)-1 > 1<<32-1 {
			return 0, false
		}
		return (y - x + uint64(dA) - 1) / uint64(dA), true
	}
}

// Where an invariant of a VecProgram comes from.
const (
	vecInvLoad     = iota // an element the loop loads every iteration: arg is the load
	vecInvLocal           // a local the loop does not write: arg is its slot
	vecInvAff             // an i32 the iteration does not change: arg indexes plan.invAffs
	vecInvConst           // a constant: bits
	vecInvIotaBase        // where an iota starts: arg indexes the iotas vecProgramOf returns
	vecInvIotaStep        // how far an iota moves per element
)

type vecInvSource struct {
	kind uint8
	arg  uint8
	bits uint64
}

// vecRoots returns the nodes whose values the program computes per element.
func vecRoots(p *vecPlan) []uint8 {
	if len(p.reduces) == 0 {
		return []uint8{p.stores[0].node}
	}
	roots := make([]uint8, len(p.reduces))
	for k, r := range p.reduces {
		roots[k] = r.node
	}
	return roots
}

// vecProgramOf turns the element computation of p into a VecProgram, its invariants' sources, the streams
// its pointers walk, the store's first, and the iotas it moves, each at the start of the loop and without
// its constant: an iota that differs from another by a constant is that one plus the constant.
func vecProgramOf(p *vecPlan) (*ssa.VecProgram, []vecInvSource, []vecStream, []vecAffine) {
	streams := vecStreamsOf(p)
	prog := &ssa.VecProgram{Elem: vecSSAType(p.elem), Spaced: p.spaced, Ptrs: len(streams), Store: -1}
	if len(p.reduces) == 0 {
		prog.Store = 0
	}
	var invSrc []vecInvSource
	var iotas []vecAffine
	var iotaOps []uint8
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
	isFloat := p.elem == wasm.ValueTypeF32 || p.elem == wasm.ValueTypeF64
	push := func(op ssa.VecOp) uint8 {
		prog.Ops = append(prog.Ops, op)
		return uint8(len(prog.Ops) - 1)
	}
	var emit func(n uint8) uint8
	emit = func(n uint8) uint8 {
		if o := opOf[n]; o >= 0 {
			return uint8(o)
		}
		nd := p.nodes[n]
		w := uint8(4)
		if nd.typ == wasm.ValueTypeI64 || nd.typ == wasm.ValueTypeF64 {
			w = 8
		}
		var op ssa.VecOp
		switch nd.op {
		case vecOpIota:
			a := p.invAffs[nd.arg]
			konst := a.konst
			a.konst = 0
			k := 0
			for k < len(iotas) && iotas[k] != a {
				k++
			}
			if k == len(iotas) {
				iotas = append(iotas, a)
				iotaOps = append(iotaOps, push(ssa.VecOp{
					Kind: ssa.VecOpIota, W: 4,
					Arg: addInv(vecInvSource{kind: vecInvIotaBase, arg: uint8(k)}),
					B:   addInv(vecInvSource{kind: vecInvIotaStep, arg: uint8(k)}),
				}))
			}
			if konst == 0 {
				opOf[n] = int(iotaOps[k])
				return iotaOps[k]
			}
			op = ssa.VecOp{Kind: ssa.VecOpAdd, A: iotaOps[k], B: addInv(vecInvSource{kind: vecInvConst, bits: uint64(konst)})}
			op.B = push(ssa.VecOp{Kind: ssa.VecOpInv, Arg: op.B, W: 4})
		case vecOpZext:
			// An and with an invariant, zero-extended into its lane, already is.
			if a := p.nodes[nd.a]; a.op == vecOpBand && (vecInvariant(p.nodes[a.a].op) || vecInvariant(p.nodes[a.b].op)) {
				o := emit(nd.a)
				prog.Ops[o].W = 8
				opOf[n] = int(o)
				return o
			}
			op = ssa.VecOp{Kind: ssa.VecOpZext, A: emit(nd.a)}
		case vecOpMulU:
			// The multiplication reads only the low half of each lane: an operand zero-extended needs not be.
			factor := func(m uint8) uint8 {
				if f := p.nodes[m]; f.op == vecOpZext {
					return emit(f.a)
				}
				return emit(m)
			}
			op = ssa.VecOp{Kind: ssa.VecOpMulU, A: factor(nd.a), B: factor(nd.b)}
		case vecOpLoad:
			if acc := p.loads[nd.arg]; p.stride(acc.addr) == 0 {
				op = ssa.VecOp{Kind: ssa.VecOpInv, Arg: addInv(vecInvSource{kind: vecInvLoad, arg: nd.arg})}
			} else {
				op = ssa.VecOp{Kind: ssa.VecOpLoad, Arg: uint8(streamOf(streams, acc))}
			}
		case vecOpInv:
			op = ssa.VecOp{Kind: ssa.VecOpInv, Arg: addInv(vecInvSource{kind: vecInvLocal, arg: nd.arg})}
		case vecOpInvAff:
			op = ssa.VecOp{Kind: ssa.VecOpInv, Arg: addInv(vecInvSource{kind: vecInvAff, arg: nd.arg})}
		case vecOpConst:
			op = ssa.VecOp{Kind: ssa.VecOpInv, Arg: addInv(vecInvSource{kind: vecInvConst, bits: nd.bits})}
		case vecOpNeg, vecOpAbs:
			a := emit(nd.a)
			// A sign flip and an absolute value are bit operations on the sign bit, as the scalar ones are.
			sign := uint64(1) << (8*p.elemSize - 1)
			mask, kind := sign, uint8(ssa.VecOpXor)
			if nd.op == vecOpAbs {
				mask, kind = sign-1, ssa.VecOpAnd
			}
			op = ssa.VecOp{Kind: kind, A: a, Arg: addInv(vecInvSource{kind: vecInvConst, bits: mask})}
		case vecOpSqrt:
			op = ssa.VecOp{Kind: ssa.VecOpSqrt, A: emit(nd.a)}
		case vecOpShl, vecOpShrU, vecOpShrS:
			kind := map[uint8]uint8{vecOpShl: ssa.VecOpShl, vecOpShrU: ssa.VecOpShrU, vecOpShrS: ssa.VecOpShrS}[nd.op]
			op = ssa.VecOp{Kind: kind, A: emit(nd.a), Arg: nd.arg}
		default:
			a, b := emit(nd.a), emit(nd.b)
			kind := map[uint8]uint8{
				vecOpAdd: ssa.VecOpAdd, vecOpSub: ssa.VecOpSub, vecOpMul: ssa.VecOpMul, vecOpDiv: ssa.VecOpDiv,
				vecOpBand: ssa.VecOpBand, vecOpBor: ssa.VecOpBor, vecOpBxor: ssa.VecOpBxor,
			}[nd.op]
			// An integer operation whose operands may trade places takes a moving load second, where a
			// backend can read it from memory. A float one keeps its order, which decides which NaN it
			// returns when both are.
			commutes := kind != ssa.VecOpSub && kind != ssa.VecOpDiv && !isFloat
			if commutes && prog.Ops[a].Kind == ssa.VecOpLoad && prog.Ops[b].Kind != ssa.VecOpLoad {
				a, b = b, a
			}
			op = ssa.VecOp{Kind: kind, A: a, B: b}
		}
		op.W = w
		opOf[n] = int(push(op))
		return uint8(opOf[n])
	}
	for _, r := range p.reduces {
		prog.Reduces = append(prog.Reduces, ssa.VecReduce{
			Op:   map[uint8]uint8{vecOpAdd: ssa.VecOpAdd, vecOpBand: ssa.VecOpBand, vecOpBor: ssa.VecOpBor, vecOpBxor: ssa.VecOpBxor}[r.op],
			Root: emit(r.node),
		})
	}
	if len(p.reduces) == 0 {
		emit(p.stores[0].node)
	}
	prog.Invs = len(invSrc)
	return prog, invSrc, streams, iotas
}

// vecInvariant reports whether a node of op is an invariant of the program.
func vecInvariant(op uint8) bool {
	return op == vecOpInv || op == vecOpInvAff || op == vecOpConst
}

// vecStreamsOf returns the moving accesses of the element computation, the store's first: the loads its
// value tree makes, one stream per distinct element walk. The other unrolled copies' accesses are these
// shifted by whole elements, so the same streams cover them.
func vecStreamsOf(p *vecPlan) []vecStream {
	var streams []vecStream
	if len(p.reduces) == 0 {
		streams = append(streams, vecStream{acc: p.stores[0], store: true})
	}
	var walk func(n uint8)
	walk = func(n uint8) {
		nd := p.nodes[n]
		switch {
		case nd.op == vecOpLoad:
			if l := p.loads[nd.arg]; p.stride(l.addr) != 0 && streamOf(streams, l) < 0 {
				streams = append(streams, vecStream{acc: l})
			}
		case vecBinaryOp(nd.op):
			walk(nd.a)
			walk(nd.b)
		case vecUnaryOp(nd.op):
			walk(nd.a)
		}
	}
	for _, n := range vecRoots(p) {
		walk(n)
	}
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

// sameElements reports whether two accesses with the same stride touch the same element every iteration,
// as long as neither address computation wraps, which lowerVecMap's guards check.
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

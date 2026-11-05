package ssa

import (
	"math/bits"
	"slices"
)

// LoopCounter is an integer parameter of a loop header that the loop's only back
// edge advances by a constant.
type LoopCounter struct {
	// Param is the header parameter.
	Param Value
	// Next is its value on the back edge, Param plus Step.
	Next Value
	// Step is what each iteration adds to it, modulo its width.
	Step uint64
	// Start is the value it enters the loop with.
	Start Value
	// BackEdge is the branch that continues the loop.
	BackEdge *Instruction
	// preheader is the block the loop is entered from, and header the loop's.
	preheader, header *basicBlock
}

// FindLoopCounter returns the counter of the loop headed by header, entered from
// preheader, if it has one: the header has exactly one back edge, and it passes
// Param+c or Param-c for one of the header's integer parameters. With a single
// back edge, every iteration that continues the loop takes it, so the counter
// moves by exactly Step each time. Every edge into the header but the preheader's
// is a back edge, even one that passes every parameter back unchanged: an iteration that takes it moves nothing,
// so the loop can run on with its counter standing still. Of several counters, it
// returns the one the back edge tests, if any: the one that says how long the loop
// runs.
//
// The header must be sealed, so that every value the loop carries is one of its
// parameters.
func (b *builder) FindLoopCounter(header, preheader BasicBlock) (lc LoopCounter, ok bool) {
	h := header.(*basicBlock)
	if !h.sealed {
		return
	}
	var entry, back *Instruction
	for _, p := range h.preds {
		switch {
		case BasicBlock(p.blk) == preheader:
			entry = p.branch
		case back != nil:
			return // a second back edge
		default:
			back = p.branch
		}
	}
	if entry == nil || back == nil || entry.opcode != OpcodeJump {
		return
	}
	switch back.opcode {
	case OpcodeJump, OpcodeBrz, OpcodeBrnz:
	default:
		return
	}
	params, backArgs, entryArgs := h.params.View(), back.vs.View(), entry.vs.View()
	if len(backArgs) != len(params) || len(entryArgs) != len(params) {
		return
	}
	var testedAt [6]Value
	tested := testedAt[:0]
	for _, t := range b.loopTests(h, back) {
		v := b.resolveAlias(t.cond)
		tested = append(tested, v)
		if cond := b.InstructionOfValue(v); cond != nil && cond.opcode == OpcodeIcmp {
			tested = append(tested, b.resolveAlias(cond.v), b.resolveAlias(cond.v2))
		}
	}
	for j, p := range params {
		if !p.Type().IsInt() {
			continue
		}
		next := b.resolveAlias(backArgs[j])
		def := b.InstructionOfValue(next)
		if def == nil {
			continue
		}
		var step uint64
		switch def.opcode {
		case OpcodeIadd:
			x, y := b.resolveAlias(def.v), b.resolveAlias(def.v2)
			if c, isConst := b.constantOf(y); isConst && x == p {
				step = c
			} else if c, isConst = b.constantOf(x); isConst && y == p {
				step = c
			}
		case OpcodeIsub:
			if c, isConst := b.constantOf(def.v2); isConst && b.resolveAlias(def.v) == p {
				step = -c
			}
		}
		if step &= widthMask(p.Type()); step == 0 {
			continue
		}
		c := LoopCounter{
			Param: p, Next: next, Step: step, Start: b.resolveAlias(entryArgs[j]), BackEdge: back,
			preheader: preheader.(*basicBlock), header: h,
		}
		for _, v := range tested {
			if v.Valid() && (v == p || v == next) {
				return c, true
			}
		}
		if !ok {
			lc, ok = c, true
		}
	}
	return
}

// TripBound returns how many times at most the loop counted by lc runs its body,
// when its test (see loopTest) compares the counter against a constant, or tests
// the counter itself against zero, and the counter starts from a constant. ok is false when that cannot be shown, including when the
// counter could wrap around past the bound. A loop that steps by one until its
// counter equals the bound may also start anywhere in a range (see startRange):
// the remainder loop of an unrolled one runs at most three times.
func (b *builder) TripBound(lc LoopCounter) (trips uint64, ok bool) {
	lo, hi, isRange := b.startRange(lc)
	if !isRange {
		return
	}
	for _, t := range b.loopTests(lc.header, lc.BackEdge) {
		if n, bounded := b.testTrips(lc, t, lo, hi); bounded && (!ok || n < trips) {
			trips, ok = n, true
		}
	}
	return
}

// testTrips is TripBound for the loop test t, the counter starting from lo to hi.
func (b *builder) testTrips(lc LoopCounter, t loopTest, lo, hi uint64) (trips uint64, ok bool) {
	x := b.resolveAlias(t.cond)
	var bound uint64
	cc := IntegerCmpCondNotEqual
	if x != lc.Next && x != lc.Param {
		cond := b.InstructionOfValue(x)
		if cond == nil || cond.opcode != OpcodeIcmp {
			return
		}
		var y Value
		var isConst bool
		x, y, cc = cond.IcmpData()
		x, y = b.resolveAlias(x), b.resolveAlias(y)
		if bound, isConst = b.constantOf(y); !isConst {
			if bound, isConst = b.constantOf(x); !isConst {
				return
			}
			x, cc = y, cc.Swapped()
		}
	}
	if !t.onTrue {
		cc = cc.Inverted()
	}

	typ := lc.Param.Type()
	mask := widthMask(typ)
	if lo != hi {
		return rangeTrips(lc, x, lo, hi, bound&mask, cc, mask)
	}
	start := lo
	// first is the counter value the condition sees in the first iteration.
	first := start & mask
	switch x {
	case lc.Next:
		first = (start + lc.Step) & mask
	case lc.Param:
	default:
		return
	}
	bound &= mask

	up := lc.Step < mask>>1+1 // a step below half the range moves up
	dist := lc.Step
	if !up {
		dist = (-lc.Step) & mask
	}
	// Order the values so that, for signed compares, the most negative is zero.
	if cc.Signed() {
		bias := mask>>1 + 1
		first, bound = (first+bias)&mask, (bound+bias)&mask
	}
	// limit is how many steps it takes to reach a value the condition rejects.
	var limit uint64
	switch {
	case cc == IntegerCmpCondNotEqual:
		var meets bool
		if limit, meets = stepsTo(first, bound, lc.Step, mask); !meets {
			return
		}
	case up && (cc == IntegerCmpCondUnsignedLessThan || cc == IntegerCmpCondSignedLessThan):
		if first >= bound {
			return 1, true
		}
		if bound > mask-dist+1 { // could step past the top and wrap
			return
		}
		limit = (bound - first + dist - 1) / dist
	case up && (cc == IntegerCmpCondUnsignedLessThanOrEqual || cc == IntegerCmpCondSignedLessThanOrEqual):
		if first > bound {
			return 1, true
		}
		if bound > mask-dist {
			return
		}
		limit = (bound - first + dist) / dist
	case !up && (cc == IntegerCmpCondUnsignedGreaterThan || cc == IntegerCmpCondSignedGreaterThan):
		if first <= bound {
			return 1, true
		}
		if bound < dist-1 {
			return
		}
		limit = (first - bound + dist - 1) / dist
	case !up && (cc == IntegerCmpCondUnsignedGreaterThanOrEqual || cc == IntegerCmpCondSignedGreaterThanOrEqual):
		if first < bound {
			return 1, true
		}
		if bound < dist {
			return
		}
		limit = (first - bound + dist) / dist
	default:
		return
	}
	if limit == mask { // the 2^64 case of ne would overflow below
		return
	}
	return limit + 1, true
}

// loopTest is a condition that decides whether a loop goes round again: it does
// only if the condition holds, where onTrue, or fails, where not.
type loopTest struct {
	cond   Value
	onTrue bool
}

// loopTests returns the tests of the loop headed by h, whose one back edge is
// back: back's, if it is conditional, or else those of the conditional branches
// out of the loop that every iteration going round passes. The loop's body is the
// blocks that reach back's block without passing h, and a branch out of it is one
// to a block outside, which never gets back to h. Every iteration passes the first
// branch of a block that dominates back's within the body. Bodies over
// loopBodyMaxBlocks blocks are not searched.
func (b *builder) loopTests(h *basicBlock, back *Instruction) []loopTest {
	tests := b.loopTestScratch[:0]
	defer func() { b.loopTestScratch = tests }()
	if back.opcode != OpcodeJump {
		return append(tests, loopTest{back.v, back.opcode == OpcodeBrnz})
	}
	var from *basicBlock
	for _, p := range h.preds {
		if p.branch == back {
			from = p.blk
		}
	}
	body, ok := b.reachers(h, from, nil, b.loopBodyScratch[:0])
	b.loopBodyScratch = body
	if !ok {
		return tests
	}
	for _, blk := range body {
		br := blk.currentInstr
		for br != nil && br.prev != nil && isBranch(br.prev) {
			br = br.prev
		}
		if br == nil || br.opcode != OpcodeBrz && br.opcode != OpcodeBrnz || slices.Contains(body, b.basicBlock(BasicBlockID(br.rValue))) {
			continue
		}
		if blk != h && blk != from {
			others, ok := b.reachers(h, from, blk, b.loopAvoidScratch[:0])
			b.loopAvoidScratch = others
			if !ok || slices.Contains(others, h) {
				continue
			}
		}
		tests = append(tests, loopTest{br.v, br.opcode == OpcodeBrz})
	}
	return tests
}

func isBranch(i *Instruction) bool {
	return i.opcode == OpcodeJump || i.opcode == OpcodeBrz || i.opcode == OpcodeBrnz
}

// loopBodyMaxBlocks is the most blocks of a loop's body loopTests searches.
const loopBodyMaxBlocks = 32

// reachers appends to dst from and the blocks that reach it without passing h or
// avoid, and h where one of them is reached from it, and returns them, or false
// where they are over loopBodyMaxBlocks.
func (b *builder) reachers(h, from, avoid *basicBlock, dst []*basicBlock) ([]*basicBlock, bool) {
	if from == nil {
		return dst, false
	}
	dst = append(dst, from)
	for i := 0; i < len(dst); i++ {
		if dst[i] == h {
			continue
		}
		for _, p := range dst[i].preds {
			switch {
			case p.blk == avoid || slices.Contains(dst, p.blk):
			case len(dst) == loopBodyMaxBlocks:
				return dst, false
			default:
				dst = append(dst, p.blk)
			}
		}
	}
	return dst, true
}

// rangeTrips is TripBound for a counter that starts anywhere from lo to hi: the
// loop must step by one, up or down, until the counter x tested equals bound, and
// from every start in the range reach it without wrapping around.
func rangeTrips(lc LoopCounter, x Value, lo, hi, bound uint64, cc IntegerCmpCond, mask uint64) (trips uint64, ok bool) {
	if cc != IntegerCmpCondNotEqual || lc.Step != 1 && lc.Step != mask {
		return
	}
	switch x {
	case lc.Next: // the first test sees the start plus one step
		if lc.Step == 1 && hi == mask || lc.Step == mask && lo == 0 {
			return
		}
		lo, hi = (lo+lc.Step)&mask, (hi+lc.Step)&mask
	case lc.Param:
	default:
		return
	}
	var limit uint64
	switch {
	case lc.Step == 1 && hi <= bound:
		limit = bound - lo
	case lc.Step == mask && lo >= bound:
		limit = hi - bound
	default:
		return
	}
	if limit == mask {
		return
	}
	return limit + 1, true
}

// startRange returns the least and greatest values the counter of lc may start
// from (see valueRange), the least raised to one when the loop is entered only
// past a branch on its being zero.
func (b *builder) startRange(lc LoopCounter) (lo, hi uint64, ok bool) {
	if lo, hi, ok = b.valueRange(lc.Start, lc.preheader, startRangeDepth); ok && lo == 0 && lo != hi && b.nonZeroOnEntry(lc) {
		lo = 1
	}
	return
}

// startRangeDepth is how many block parameters valueRange looks through.
const startRangeDepth = 4

// valueRange returns the least and greatest values v may take, unsigned: a
// constant, a value masked with a constant, or a parameter of blk or of a block it
// is reached from alone, which takes one of the values its predecessors pass it
// other than itself. Building SSA leaves such parameters for the locals a loop
// carries unchanged until a pass takes them out, after the frontend has tuned the
// loop. depth limits how many it looks through.
func (b *builder) valueRange(v Value, blk *basicBlock, depth int) (lo, hi uint64, ok bool) {
	v = b.resolveAlias(v)
	if c, isConst := b.constantOf(v); isConst {
		return c, c, true
	}
	if def := b.InstructionOfValue(v); def != nil {
		if def.opcode != OpcodeBand {
			return
		}
		m, isConst := b.constantOf(def.v2)
		if !isConst {
			if m, isConst = b.constantOf(def.v); !isConst {
				return
			}
		}
		return 0, m, true
	}
	if depth == 0 {
		return
	}
	for ; blk != nil; blk = blk.singlePred {
		j := slices.Index(blk.params.View(), v)
		if j < 0 {
			continue
		}
		lo, hi = ^uint64(0), 0
		for _, p := range blk.preds {
			arg := b.resolveAlias(p.branch.vs.View()[j])
			if arg == v {
				continue
			}
			l, h, isRange := b.valueRange(arg, p.blk, depth-1)
			if !isRange {
				return 0, 0, false
			}
			lo, hi = min(lo, l), max(hi, h)
		}
		return lo, hi, lo <= hi
	}
	return
}

// nonZeroOnEntry reports whether the loop of lc is entered only when its counter
// starts other than zero: its preheader's one predecessor gets there only by a
// branch that tests that, the start itself or its comparison with zero.
func (b *builder) nonZeroOnEntry(lc LoopCounter) bool {
	pre := lc.preheader
	if pre == nil || len(pre.preds) != 1 {
		return false
	}
	var c Value
	var whenNonZero bool // whether the edge to pre is taken when c is not zero
	switch br := pre.preds[0].branch; br.opcode {
	case OpcodeBrnz, OpcodeBrz:
		c, whenNonZero = br.v, br.opcode == OpcodeBrnz
	case OpcodeJump: // taken when the conditional branch before it is not
		cond := br.prev
		if cond == nil || cond.opcode != OpcodeBrnz && cond.opcode != OpcodeBrz || BasicBlockID(cond.rValue) == pre.id {
			return false
		}
		c, whenNonZero = cond.v, cond.opcode == OpcodeBrz
	default:
		return false
	}
	c = b.resolveAlias(c)
	if c == lc.Start {
		return whenNonZero
	}
	def := b.InstructionOfValue(c)
	if def == nil || def.opcode != OpcodeIcmp {
		return false
	}
	x, y, cc := def.IcmpData()
	x, y = b.resolveAlias(x), b.resolveAlias(y)
	if y == lc.Start {
		x, y = y, x
	}
	if zero, isConst := b.constantOf(y); x != lc.Start || !isConst || zero != 0 {
		return false
	}
	switch cc {
	case IntegerCmpCondEqual:
		return !whenNonZero
	case IntegerCmpCondNotEqual:
		return whenNonZero
	}
	return false
}

// MaskedCheckBits returns the mask a loop check can test the counter of lc
// against to run once every 2^w iterations: an odd step visits every residue, so
// after shifting out the step's trailing zeros, w bits of the counter are all zero
// exactly once in every 2^w arrivals from any start. It stays below bit 31, so
// that it is a sign-extended 32-bit immediate on amd64 at either width; zero
// means there is no such mask.
func MaskedCheckBits(lc LoopCounter, w int) uint64 {
	k := bits.TrailingZeros64(lc.Step)
	if w <= 0 || k+w > 31 {
		return 0
	}
	return (uint64(1)<<w - 1) << k
}

// MaskedCheckOnEntry reports whether a check testing the bits mask of the counter
// of lc runs in full the first time the loop reaches it: whether the counter
// starts with them all zero.
func (b *builder) MaskedCheckOnEntry(lc LoopCounter, mask uint64) bool {
	start, ok := b.constantOf(lc.Start)
	return ok && start&mask == 0
}

// stepsTo returns the fewest steps k for which first + k*step equals bound modulo
// the width that mask covers, or false when no k does: k*step is always a multiple
// of step's lowest set bit, so the distance must be too, and dividing it out
// leaves an odd step, which has an inverse.
func stepsTo(first, bound, step, mask uint64) (uint64, bool) {
	dist := (bound - first) & mask
	z := bits.TrailingZeros64(step)
	if dist&(1<<z-1) != 0 {
		return 0, false
	}
	odd := step >> z
	inv := odd // right in its low 3 bits, and each round below doubles that
	for range 5 {
		inv *= 2 - odd*inv
	}
	return (dist >> z) * inv & (mask >> z), true
}

func (b *builder) constantOf(v Value) (uint64, bool) {
	def := b.InstructionOfValue(b.resolveAlias(v))
	if def == nil || def.opcode != OpcodeIconst {
		return 0, false
	}
	return def.ConstantVal() & widthMask(def.typ), true
}

func widthMask(t Type) uint64 {
	if t.Bits() == 32 {
		return 0xffffffff
	}
	return ^uint64(0)
}

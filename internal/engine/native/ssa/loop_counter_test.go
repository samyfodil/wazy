package ssa

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// countedLoop builds
//
//	blk0: v = start; jump blk1(v)
//	blk1(p): next = p + step (or p - (-step) when sub); continue while next cc bound
//	blk2:
//
// with an i32 counter, and returns the builder, the header and the preheader.
type countedLoop struct {
	start, step, bound uint64
	cc                 IntegerCmpCond
	sub                bool   // compute next as p - (-step)
	cmpParam           bool   // compare p rather than next
	jumpBack           bool   // continue unconditionally instead of comparing
	secondBackEdge     bool   // add another back edge that also advances the counter
	rewrite            bool   // pass p*2 on the back edge, not p + step
	unknownStart       bool   // enter with a parameter of blk0 instead of a constant
	standStill         bool   // add a back edge that passes p back unchanged
	nonZero            bool   // branch back on the compared value itself, rather than comparing it
	startMask          uint64 // enter with a parameter masked with this instead of a constant
	guard              bool   // enter only past a branch away on the start being zero
	guardCmp           bool   // ... which compares it with zero rather than testing it
	untested           bool   // add an earlier counter that the back edge does not test
	joined             bool   // pass the start through a parameter whose other predecessor passes it back
	typ                Type
}

func (l countedLoop) build() (*builder, BasicBlock, BasicBlock) {
	if l.typ == 0 {
		l.typ = TypeI32
	}
	b := NewBuilder().(*builder)
	pre, header, exit := b.AllocateBasicBlock(), b.AllocateBasicBlock(), b.AllocateBasicBlock()
	iconst := func(v uint64) Value {
		if l.typ == TypeI32 {
			return b.AllocateInstruction().AsIconst32(uint32(v)).Insert(b).Return()
		}
		return b.AllocateInstruction().AsIconst64(v).Insert(b).Return()
	}

	entry := pre
	if l.guard || l.joined {
		entry = b.AllocateBasicBlock()
	}
	b.SetCurrentBlock(entry)
	start := entry.AddParam(b, l.typ)
	switch {
	case l.startMask != 0:
		start = b.AllocateInstruction().AsBand(start, iconst(l.startMask)).Insert(b).Return()
	case !l.unknownStart:
		start = iconst(l.start)
	}
	if l.joined {
		join, self := b.AllocateBasicBlock(), b.AllocateBasicBlock()
		jp := join.AddParam(b, l.typ)
		b.AllocateInstruction().AsJump(ValuesNil.Append(&b.varLengthPool, start), join).Insert(b)
		b.Seal(entry)
		b.SetCurrentBlock(self)
		b.AllocateInstruction().AsJump(ValuesNil.Append(&b.varLengthPool, jp), join).Insert(b)
		b.Seal(self)
		b.Seal(join)
		entry, start = join, jp
		b.SetCurrentBlock(join)
		if !l.guard {
			b.AllocateInstruction().AsJump(ValuesNil, pre).Insert(b)
			b.SetCurrentBlock(pre)
		}
	}
	if l.guard {
		if l.guardCmp {
			isZero := b.AllocateInstruction().AsIcmp(start, iconst(0), IntegerCmpCondEqual).Insert(b).Return()
			b.AllocateInstruction().AsBrnz(isZero, ValuesNil, exit).Insert(b)
		} else {
			brz := b.AllocateInstruction()
			brz.AsBrz(start, ValuesNil, exit)
			b.InsertInstruction(brz)
		}
		b.AllocateInstruction().AsJump(ValuesNil, pre).Insert(b)
		b.Seal(entry)
		b.SetCurrentBlock(pre)
	}
	entryArgs := ValuesNil
	if l.untested {
		entryArgs = entryArgs.Append(&b.varLengthPool, iconst(0))
	}
	b.AllocateInstruction().AsJump(entryArgs.Append(&b.varLengthPool, start), header).Insert(b)
	b.Seal(pre)

	b.SetCurrentBlock(header)
	var q Value
	if l.untested {
		q = header.AddParam(b, l.typ)
	}
	p := header.AddParam(b, l.typ)
	if l.standStill {
		b.AllocateInstruction().AsBrnz(iconst(1), ValuesNil.Append(&b.varLengthPool, p), header).Insert(b)
	}
	var next Value
	switch {
	case l.rewrite:
		next = b.AllocateInstruction().AsIadd(p, p).Insert(b).Return()
	case l.sub:
		next = b.AllocateInstruction().AsIsub(p, iconst(-l.step)).Insert(b).Return()
	default:
		next = b.AllocateInstruction().AsIadd(p, iconst(l.step)).Insert(b).Return()
	}
	if l.secondBackEdge {
		other := b.AllocateInstruction().AsIadd(p, iconst(l.step)).Insert(b).Return()
		brz := b.AllocateInstruction()
		brz.AsBrz(other, ValuesNil.Append(&b.varLengthPool, other), header)
		b.InsertInstruction(brz)
	}
	args := ValuesNil
	if l.untested {
		args = args.Append(&b.varLengthPool, b.AllocateInstruction().AsIadd(q, iconst(1)).Insert(b).Return())
	}
	args = args.Append(&b.varLengthPool, next)
	if l.jumpBack {
		b.AllocateInstruction().AsJump(args, header).Insert(b)
	} else {
		x := next
		if l.cmpParam {
			x = p
		}
		cond := x
		if !l.nonZero {
			cond = b.AllocateInstruction().AsIcmp(x, iconst(l.bound), l.cc).Insert(b).Return()
		}
		b.AllocateInstruction().AsBrnz(cond, args, header).Insert(b)
		b.AllocateInstruction().AsJump(ValuesNil, exit).Insert(b)
	}
	b.Seal(header)
	return b, header, pre
}

func TestFindLoopCounter_tested(t *testing.T) {
	b, h, pre := countedLoop{untested: true, step: 3, bound: 30, cc: IntegerCmpCondUnsignedLessThan}.build()
	lc, ok := b.FindLoopCounter(h, pre)
	require.True(t, ok)
	require.Equal(t, h.Param(1), lc.Param)
	require.Equal(t, uint64(3), lc.Step)
}

func TestFindLoopCounter(t *testing.T) {
	t.Run("increment", func(t *testing.T) {
		b, h, pre := countedLoop{step: 1, bound: 10, cc: IntegerCmpCondUnsignedLessThan}.build()
		lc, ok := b.FindLoopCounter(h, pre)
		require.True(t, ok)
		require.Equal(t, uint64(1), lc.Step)
		require.Equal(t, h.Param(0), lc.Param)
	})
	t.Run("decrement by sub", func(t *testing.T) {
		b, h, pre := countedLoop{step: 0xffffffff, sub: true, jumpBack: true}.build()
		lc, ok := b.FindLoopCounter(h, pre)
		require.True(t, ok)
		require.Equal(t, uint64(0xffffffff), lc.Step)
	})
	t.Run("rewritten", func(t *testing.T) {
		b, h, pre := countedLoop{rewrite: true, jumpBack: true}.build()
		_, ok := b.FindLoopCounter(h, pre)
		require.False(t, ok)
	})
	t.Run("two back edges", func(t *testing.T) {
		b, h, pre := countedLoop{step: 1, jumpBack: true, secondBackEdge: true}.build()
		_, ok := b.FindLoopCounter(h, pre)
		require.False(t, ok)
	})
	t.Run("a back edge that moves nothing", func(t *testing.T) {
		// A continue that skips the increment: the loop can run on forever
		// while its counter stands still, so it has none.
		b, h, pre := countedLoop{step: 1, bound: 10, cc: IntegerCmpCondUnsignedLessThan, standStill: true}.build()
		_, ok := b.FindLoopCounter(h, pre)
		require.False(t, ok)
	})
	t.Run("zero step", func(t *testing.T) {
		b, h, pre := countedLoop{step: 0, jumpBack: true}.build()
		_, ok := b.FindLoopCounter(h, pre)
		require.False(t, ok)
	})
}

func TestTripBound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		loop  countedLoop
		trips uint64
		ok    bool
	}{
		{name: "next < 10 from 0", loop: countedLoop{start: 0, step: 1, bound: 10, cc: IntegerCmpCondUnsignedLessThan}, trips: 10, ok: true},
		{name: "p < 10 from 0", loop: countedLoop{start: 0, step: 1, bound: 10, cc: IntegerCmpCondUnsignedLessThan, cmpParam: true}, trips: 11, ok: true},
		{name: "next <= 10 from 0", loop: countedLoop{start: 0, step: 1, bound: 10, cc: IntegerCmpCondSignedLessThanOrEqual}, trips: 11, ok: true},
		{name: "step 3 below 10", loop: countedLoop{start: 0, step: 3, bound: 10, cc: IntegerCmpCondUnsignedLessThan}, trips: 4, ok: true},
		{name: "already past", loop: countedLoop{start: 20, step: 1, bound: 10, cc: IntegerCmpCondUnsignedLessThan}, trips: 1, ok: true},
		{name: "signed from negative", loop: countedLoop{start: 0xfffffffb, step: 1, bound: 5, cc: IntegerCmpCondSignedLessThan}, trips: 10, ok: true},
		{name: "down to 0", loop: countedLoop{start: 10, step: 0xffffffff, bound: 0, cc: IntegerCmpCondUnsignedGreaterThan}, trips: 10, ok: true},
		{name: "down by sub", loop: countedLoop{start: 10, step: 0xffffffff, sub: true, bound: 0, cc: IntegerCmpCondSignedGreaterThan}, trips: 10, ok: true},
		{name: "ne by one", loop: countedLoop{start: 0, step: 1, bound: 7, cc: IntegerCmpCondNotEqual}, trips: 7, ok: true},
		{name: "ne by two", loop: countedLoop{start: 0, step: 2, bound: 16, cc: IntegerCmpCondNotEqual}, trips: 8, ok: true},
		{name: "ne by two past an odd bound", loop: countedLoop{start: 0, step: 2, bound: 7, cc: IntegerCmpCondNotEqual}},
		{name: "ne by three around the top", loop: countedLoop{start: 0, step: 3, bound: 1, cc: IntegerCmpCondNotEqual}, trips: 0xaaaaaaab, ok: true},
		{name: "ne down by four", loop: countedLoop{start: 40, step: 0xfffffffc, bound: 0, cc: IntegerCmpCondNotEqual}, trips: 10, ok: true},
		{name: "ne down by one", loop: countedLoop{start: 10, step: 0xffffffff, bound: 3, cc: IntegerCmpCondNotEqual}, trips: 7, ok: true},
		{name: "ne by one all the way round", loop: countedLoop{typ: TypeI64, start: 0, step: 1, bound: 0, cc: IntegerCmpCondNotEqual}},
		{name: "up by two to zero, branching on next", loop: countedLoop{start: 0xffffffec, step: 2, nonZero: true}, trips: 10, ok: true},
		{name: "down to zero, branching on p", loop: countedLoop{start: 5, step: 0xffffffff, nonZero: true, cmpParam: true}, trips: 6, ok: true},
		{name: "by two to zero from odd, branching on next", loop: countedLoop{start: 7, step: 2, nonZero: true}},
		{name: "remainder loop", loop: countedLoop{startMask: 3, guard: true, step: 0xffffffff, nonZero: true}, trips: 3, ok: true},
		{name: "remainder loop, guarded by a compare", loop: countedLoop{startMask: 3, guard: true, guardCmp: true, step: 0xffffffff, nonZero: true}, trips: 3, ok: true},
		{name: "remainder loop with no guard", loop: countedLoop{startMask: 3, step: 0xffffffff, nonZero: true}},
		{name: "remainder loop past a join", loop: countedLoop{startMask: 3, joined: true, guard: true, guardCmp: true, step: 0xffffffff, nonZero: true}, trips: 3, ok: true},
		{name: "constant start past a join", loop: countedLoop{start: 0, joined: true, step: 1, bound: 10, cc: IntegerCmpCondUnsignedLessThan}, trips: 10, ok: true},
		{name: "remainder loop testing p with no guard", loop: countedLoop{startMask: 3, step: 0xffffffff, nonZero: true, cmpParam: true}, trips: 4, ok: true},
		{name: "masked start up to the bound", loop: countedLoop{startMask: 7, step: 1, bound: 8, cc: IntegerCmpCondNotEqual}, trips: 8, ok: true},
		{name: "masked start past the bound", loop: countedLoop{startMask: 15, step: 1, bound: 8, cc: IntegerCmpCondNotEqual}},
		{name: "masked start by two", loop: countedLoop{startMask: 3, step: 2, bound: 8, cc: IntegerCmpCondNotEqual}},
		{name: "masked start below a bound", loop: countedLoop{startMask: 3, step: 1, bound: 8, cc: IntegerCmpCondUnsignedLessThan}},
		{name: "step away from bound", loop: countedLoop{start: 0, step: 0xffffffff, bound: 10, cc: IntegerCmpCondSignedLessThan}},
		{name: "could wrap past the bound", loop: countedLoop{start: 0, step: 2, bound: 0xffffffff, cc: IntegerCmpCondUnsignedLessThan}},
		{name: "unknown start", loop: countedLoop{unknownStart: true, step: 1, bound: 10, cc: IntegerCmpCondUnsignedLessThan}},
		{name: "unconditional", loop: countedLoop{step: 1, jumpBack: true}},
		{name: "i64", loop: countedLoop{typ: TypeI64, start: 0, step: 1, bound: 100, cc: IntegerCmpCondUnsignedLessThan}, trips: 100, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, h, pre := tc.loop.build()
			lc, ok := b.FindLoopCounter(h, pre)
			require.True(t, ok)
			trips, ok := b.TripBound(lc)
			require.Equal(t, tc.ok, ok)
			if ok {
				require.Equal(t, tc.trips, trips)
			}
		})
	}
}

func TestMaskedCheckBits(t *testing.T) {
	b, h, pre := countedLoop{step: 1, jumpBack: true}.build()
	lc, ok := b.FindLoopCounter(h, pre)
	require.True(t, ok)
	require.Equal(t, uint64(0xfff), MaskedCheckBits(lc, 12))
	require.Equal(t, uint64(0), MaskedCheckBits(lc, 0))

	// An even step shifts the mask past its trailing zeros, so that what it tests
	// still cycles through every value.
	b, h, pre = countedLoop{step: 8, jumpBack: true}.build()
	lc, ok = b.FindLoopCounter(h, pre)
	require.True(t, ok)
	require.Equal(t, uint64(0xfff<<3), MaskedCheckBits(lc, 12))

	// Too wide to stay below bit 31.
	b, h, pre = countedLoop{step: 1 << 20, jumpBack: true}.build()
	lc, ok = b.FindLoopCounter(h, pre)
	require.True(t, ok)
	require.Equal(t, uint64(0), MaskedCheckBits(lc, 12))
}

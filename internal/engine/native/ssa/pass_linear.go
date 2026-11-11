package ssa

import "math/bits"

// passLinearFormOpt rewrites an integer expression of adds, subtractions, and multiplications and left
// shifts by constants as the sum of its leaves, each times one coefficient, and a constant, where that
// takes fewer multiplications, or as many and fewer other instructions. Arithmetic modulo 2^n is a
// commutative ring, so the sum is exactly what the expression computes. A tree is the instructions of a
// block that only one other of it takes, and also, scaled into the coefficient of what they scale, the
// multiplications and shifts by constants that others take too: ((a*-11 + b*32)*3 + a*21) * 2 is
// a*-24 + b*192, one multiplication where there were four. Producers leave such sums of products of
// constants, which distributing constants over sums would make longer in general.
func passLinearFormOpt(b *builder) {
	// Only multiplications make a rewrite worth its while, and at least two make one possible.
	if b.imuls < 2 {
		return
	}
	muls := 0
	for _, blk := range b.reversePostOrderedBasicBlocks {
		for cur := blk.rootInstr; cur != nil && muls < 2; cur = cur.next {
			if cur.opcode != OpcodeImul {
				continue
			}
			if _, k, ok := b.linearScale(cur); ok && !isPow2(k&widthMask(cur.rValue.Type())) {
				muls++
			}
		}
	}
	if muls < 2 {
		return
	}

	b.linearStamp++
	stamp := b.linearStamp
	if n := int(b.nextValueID); len(b.linearMark) < n {
		b.linearMark = append(b.linearMark, make([]linearMark, n-len(b.linearMark))...)
	}
	mark := b.linearMark
	blockStamp := b.linearBlockStamp
	for _, blk := range b.reversePostOrderedBasicBlocks {
		blockStamp++
		for cur := blk.rootInstr; cur != nil; cur = cur.next {
			linear := isLinear(b, cur)
			if linear {
				mark[cur.rValue.ID()] = linearMark{stamp: stamp, block: blockStamp}
			}
			v1, v2, v3, vs := cur.Args()
			for _, v := range [3]Value{v1, v2, v3} {
				if !v.Valid() {
					continue
				}
				if m := &mark[b.resolveAlias(v).ID()]; m.stamp == stamp {
					m.uses++
					m.linearUser = linear && m.block == blockStamp
				}
			}
			for _, v := range vs {
				if m := &mark[b.resolveAlias(v).ID()]; m.stamp == stamp {
					m.uses++
				}
			}
		}
	}
	b.linearBlockStamp = blockStamp

	for _, blk := range b.reversePostOrderedBasicBlocks {
		for cur := blk.rootInstr; cur != nil; cur = cur.next {
			if !isLinear(b, cur) {
				continue
			}
			if m := mark[cur.rValue.ID()]; m.stamp == stamp && !(m.uses == 1 && m.linearUser) {
				b.rewriteLinear(blk, cur)
			}
		}
	}
}

// linearMark is passLinearFormOpt's per value scratch: a linear instruction's stamp, its block's, how many
// instructions take it, and whether the last of them is a linear one of its block, which, where it is
// the only one, makes it part of that one's tree.
type linearMark struct {
	stamp, block, uses int32
	linearUser         bool
}

// linearTreeMax is the most instructions passLinearFormOpt takes into one tree.
const linearTreeMax = 64

// linearTerm is a leaf of a linear tree and its coefficient.
type linearTerm struct {
	v    Value
	coef uint64
}

// isLinear reports whether instr is an integer add, subtraction, or multiplication or left shift by a
// constant.
func isLinear(b *builder, instr *Instruction) bool {
	switch instr.opcode {
	case OpcodeIadd, OpcodeIsub:
		return instr.rValue.Type().IsInt()
	}
	_, _, ok := b.linearScale(instr)
	return ok
}

// linearScale returns what instr multiplies, and by what, where it is an integer multiplication or left
// shift by a constant.
func (b *builder) linearScale(instr *Instruction) (x Value, k uint64, ok bool) {
	switch instr.opcode {
	case OpcodeImul:
		if c, isConst := b.constantOf(instr.v2); isConst {
			return instr.v, c, true
		}
		if c, isConst := b.constantOf(instr.v); isConst {
			return instr.v2, c, true
		}
	case OpcodeIshl:
		if c, isConst := b.constantOf(instr.v2); isConst {
			return instr.v, 1 << (c & uint64(instr.rValue.Type().Bits()-1)), true
		}
	}
	return
}

// rewriteLinear rewrites the tree rooted at root, an instruction of blk, where passLinearFormOpt would.
func (b *builder) rewriteLinear(blk *basicBlock, root *Instruction) {
	typ := root.rValue.Type()
	mask := widthMask(typ)
	terms := b.linearTerms[:0]
	var konst uint64
	muls, others, nodes := 0, 0, 0
	stamp := b.linearStamp
	mark := b.linearMark
	// inTree reports whether v is computed by an instruction of the tree, which goes with it.
	inTree := func(v Value) bool {
		if int(v.ID()) >= len(mark) {
			return false
		}
		m := mark[v.ID()]
		return m.stamp == stamp && m.uses == 1 && m.linearUser
	}
	var walk func(v Value, coef uint64, top bool) bool
	walk = func(v Value, coef uint64, top bool) bool {
		v = b.resolveAlias(v)
		if c, isConst := b.constantOf(v); isConst {
			konst += c * coef
			return true
		}
		instr := b.InstructionOfValue(v)
		if !top && !inTree(v) {
			// What another instruction takes too stays, but its scaling of a value can still go into that
			// value's coefficient.
			if instr != nil {
				if x, k, ok := b.linearScale(instr); ok && !b.isConstantOf(x) {
					v, coef = b.resolveAlias(x), coef*k
				}
			}
			for i := range terms {
				if terms[i].v == v {
					terms[i].coef += coef
					return true
				}
			}
			terms = append(terms, linearTerm{v, coef})
			return true
		}
		if nodes++; nodes > linearTreeMax {
			return false
		}
		switch instr.opcode {
		case OpcodeIadd:
			others++
			return walk(instr.v, coef, false) && walk(instr.v2, coef, false)
		case OpcodeIsub:
			others++
			return walk(instr.v, coef, false) && walk(instr.v2, -coef, false)
		}
		x, k, _ := b.linearScale(instr)
		if instr.opcode == OpcodeImul && !isPow2(k&mask) {
			muls++
		} else {
			others++
		}
		return walk(x, coef*k, false)
	}
	ok := walk(root.rValue, 1, true)
	b.linearTerms = terms
	if !ok {
		return
	}

	konst &= mask
	// A term whose coefficient is minus one or minus a power of two is subtracted, and any other added:
	// the first of those as it is, unless there are none, when the constant or zero leads.
	newMuls, newOthers, added, subtracted := 0, 0, 0, 0
	for i := range terms {
		c := terms[i].coef & mask
		terms[i].coef = c
		switch {
		case c == 0:
		case subtracts(c, mask):
			if subtracted++; -c&mask != 1 {
				newOthers++
			}
		default:
			if added++; c != 1 && isPow2(c) {
				newOthers++
			} else if c != 1 {
				newMuls++
			}
		}
	}
	newOthers += max(added-1, 0) + subtracted
	if konst != 0 && added+subtracted > 0 {
		newOthers++
	} else if konst == 0 && added == 0 && subtracted > 0 {
		newOthers++ // 0 - x
	}
	if newMuls > muls || newMuls == muls && newOthers >= others {
		return
	}

	konstOf := func(c uint64) Value {
		instr := b.AllocateInstruction()
		if typ == TypeI32 {
			instr.AsIconst32(uint32(c))
		} else {
			instr.AsIconst64(c)
		}
		return b.insertBefore(blk, root, instr)
	}
	scale := func(x Value, c uint64) Value {
		switch {
		case c == 1:
			return x
		case isPow2(c):
			return b.insertBefore(blk, root, b.AllocateInstruction().AsIshl(x, konstOf(uint64(bits.TrailingZeros64(c)))))
		}
		return b.insertBefore(blk, root, b.AllocateInstruction().AsImul(x, konstOf(c)))
	}
	acc := ValueInvalid
	for _, t := range terms {
		if t.coef == 0 || subtracts(t.coef, mask) {
			continue
		}
		if v := scale(t.v, t.coef); acc.Valid() {
			acc = b.insertBefore(blk, root, b.AllocateInstruction().AsIadd(acc, v))
		} else {
			acc = v
		}
	}
	if !acc.Valid() {
		acc, konst = konstOf(konst), 0
	}
	for _, t := range terms {
		if t.coef != 0 && subtracts(t.coef, mask) {
			acc = b.insertBefore(blk, root, b.AllocateInstruction().AsIsub(acc, scale(t.v, -t.coef&mask)))
		}
	}
	if konst != 0 {
		acc = b.insertBefore(blk, root, b.AllocateInstruction().AsIadd(acc, konstOf(konst)))
	}
	b.alias(root.rValue, acc)
}

// subtracts reports whether a term of coefficient c, of a width of mask, is subtracted: c is minus one or
// minus a power of two.
func subtracts(c, mask uint64) bool {
	return isPow2(-c&mask) && !isPow2(c)
}

func (b *builder) isConstantOf(v Value) bool {
	_, ok := b.constantOf(v)
	return ok
}

func isPow2(c uint64) bool {
	return c != 0 && c&(c-1) == 0
}

package frontend

import "github.com/samyfodil/wazy/internal/engine/native/ssa"

// WebAssembly has no 64x64->128 multiply, so producers build one from 32-bit halves. With
// xl, xh = x&M, x>>32 (likewise for y) and t = xl*yl, compiler-rt's __multi3 and Go's
// math/bits.Mul64 both compute
//
//	u  = t>>32 + xh*yl
//	v  = u&M + xl*yh
//	hi = xh*yh + u>>32 + v>>32
//	lo = t&M | v<<32
//
// where the two cross products may swap roles and every sum may come in either order. No step
// overflows, so hi is exactly the high half of x*y and lo exactly its low half. fuseWideMulAdd
// and fuseWideMulOr recognize these sums so the backend emits one multiply instead of four plus
// a dozen shifts, masks and adds.

const mask32 = 0xffffffff

func (c *Compiler) defOf(v ssa.Value, op ssa.Opcode) *ssa.Instruction {
	if instr := c.ssaBuilder.InstructionOfValue(v); instr != nil && instr.Opcode() == op {
		return instr
	}
	return nil
}

func (c *Compiler) isIconst(v ssa.Value, k uint64) bool {
	instr := c.defOf(v, ssa.OpcodeIconst)
	return instr != nil && instr.ConstantVal() == k
}

// hi32 returns x when v is x>>32.
func (c *Compiler) hi32(v ssa.Value) (ssa.Value, bool) {
	if instr := c.defOf(v, ssa.OpcodeUshr); instr != nil {
		if x, amt := instr.Arg2(); c.isIconst(amt, 32) {
			return x, true
		}
	}
	return ssa.ValueInvalid, false
}

// lo32 returns x when v is x&M.
func (c *Compiler) lo32(v ssa.Value) (ssa.Value, bool) {
	if instr := c.defOf(v, ssa.OpcodeBand); instr != nil {
		x, y := instr.Arg2()
		if c.isIconst(y, mask32) {
			return x, true
		}
		if c.isIconst(x, mask32) {
			return y, true
		}
	}
	return ssa.ValueInvalid, false
}

// mulOf returns the operands of v when v is a product of f(a) and g(b), in either order.
func (c *Compiler) mulOf(v ssa.Value, f, g func(ssa.Value) (ssa.Value, bool)) (a, b ssa.Value, ok bool) {
	instr := c.defOf(v, ssa.OpcodeImul)
	if instr == nil {
		return
	}
	p, q := instr.Arg2()
	if a, ok = f(p); ok {
		if b, ok = g(q); ok {
			return
		}
	}
	if a, ok = f(q); ok {
		b, ok = g(p)
	}
	return
}

func (c *Compiler) addOf(v ssa.Value) (x, y ssa.Value, ok bool) {
	if instr := c.defOf(v, ssa.OpcodeIadd); instr != nil {
		x, y = instr.Arg2()
		return x, y, true
	}
	return
}

// wideMulCarries returns x and y when u and v are the two carry sums of the high half of x*y,
// as in the comment above.
func (c *Compiler) wideMulCarries(u, v ssa.Value) (x, y ssa.Value, ok bool) {
	v1, v2, ok := c.addOf(v)
	if !ok {
		return
	}
	cross2 := v2
	if w, ok := c.lo32(v1); !ok || w != u {
		if w, ok := c.lo32(v2); !ok || w != u {
			return x, y, false
		}
		cross2 = v1
	}
	u1, u2, ok := c.addOf(u)
	if !ok {
		return
	}
	for _, pair := range [2][2]ssa.Value{{u1, u2}, {u2, u1}} {
		t, ok := c.hi32(pair[0])
		if !ok {
			continue
		}
		if x, y, ok = c.mulOf(t, c.lo32, c.lo32); !ok {
			continue
		}
		cross1 := pair[1]
		if c.isCross(cross1, x, y) && c.isCross(cross2, y, x) || c.isCross(cross1, y, x) && c.isCross(cross2, x, y) {
			return x, y, true
		}
	}
	return x, y, false
}

// isCross reports whether v is (a&M)*(b>>32).
func (c *Compiler) isCross(v, a, b ssa.Value) bool {
	l, h, ok := c.mulOf(v, c.lo32, c.hi32)
	return ok && l == a && h == b
}

// addTerms appends the summands of v, looking through nested adds.
func (c *Compiler) addTerms(v ssa.Value, terms []ssa.Value) []ssa.Value {
	if x, y, ok := c.addOf(v); ok && len(terms) < 8 {
		return c.addTerms(y, c.addTerms(x, terms))
	}
	return append(terms, v)
}

// fuseWideMulAdd returns x+y rebuilt around a single Umulhi when the sum contains the high half
// of a 64x64->128 product spelled out as above.
func (c *Compiler) fuseWideMulAdd(x, y ssa.Value) (ssa.Value, bool) {
	if x.Type() != ssa.TypeI64 {
		return ssa.ValueInvalid, false
	}
	if _, ok := c.hi32(y); !ok {
		if _, ok = c.hi32(x); !ok {
			return ssa.ValueInvalid, false
		}
	}
	terms := c.addTerms(y, c.addTerms(x, c.wideMulTerms[:0]))
	c.wideMulTerms = terms
	for i, ti := range terms {
		u, ok := c.hi32(ti)
		if !ok {
			continue
		}
		for j, tj := range terms {
			if j == i {
				continue
			}
			v, ok := c.hi32(tj)
			if !ok {
				continue
			}
			a, b, ok := c.wideMulCarries(u, v)
			if !ok {
				continue
			}
			for k, tk := range terms {
				if k == i || k == j {
					continue
				}
				h1, h2, ok := c.mulOf(tk, c.hi32, c.hi32)
				if !ok || !(h1 == a && h2 == b || h1 == b && h2 == a) {
					continue
				}
				builder := c.ssaBuilder
				sum := builder.AllocateInstruction().AsUmulhi(a, b).Insert(builder).Return()
				for l, tl := range terms {
					if l != i && l != j && l != k {
						sum = builder.AllocateInstruction().AsIadd(sum, tl).Insert(builder).Return()
					}
				}
				return sum, true
			}
		}
	}
	return ssa.ValueInvalid, false
}

// fuseWideMulOr returns x*y when x|y is the low half of x*y spelled out as above.
func (c *Compiler) fuseWideMulOr(x, y ssa.Value) (ssa.Value, bool) {
	if x.Type() != ssa.TypeI64 {
		return ssa.ValueInvalid, false
	}
	for _, pair := range [2][2]ssa.Value{{x, y}, {y, x}} {
		t, ok := c.lo32(pair[0])
		if !ok {
			continue
		}
		shl := c.defOf(pair[1], ssa.OpcodeIshl)
		if shl == nil {
			continue
		}
		v, amt := shl.Arg2()
		if !c.isIconst(amt, 32) {
			continue
		}
		v1, v2, ok := c.addOf(v)
		if !ok {
			continue
		}
		u, ok := c.lo32(v1)
		if !ok {
			if u, ok = c.lo32(v2); !ok {
				continue
			}
		}
		a, b, ok := c.wideMulCarries(u, v)
		if !ok {
			continue
		}
		if l1, l2, ok := c.mulOf(t, c.lo32, c.lo32); !ok || !(l1 == a && l2 == b || l1 == b && l2 == a) {
			continue
		}
		builder := c.ssaBuilder
		return builder.AllocateInstruction().AsImul(a, b).Insert(builder).Return(), true
	}
	return ssa.ValueInvalid, false
}

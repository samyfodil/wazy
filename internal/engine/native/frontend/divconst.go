package frontend

import (
	"math/bits"

	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/wasm"
)

// lowerDivByConst lowers an integer division or remainder whose divisor is a constant other than zero
// and, for a signed one, minus one, to shifts and multiplications: a divide instruction takes tens of
// cycles. Neither divisor it leaves alone can trap here, so the result is exact with no checks. It
// reports false for any other divisor.
func (c *Compiler) lowerDivByConst(op wasm.Opcode, x, y ssa.Value) (ssa.Value, bool) {
	instr := c.ssaBuilder.InstructionOfValue(y)
	if instr == nil || instr.Opcode() != ssa.OpcodeIconst {
		return ssa.ValueInvalid, false
	}
	is64 := y.Type() == ssa.TypeI64
	d := instr.ConstantVal()
	signed := op == wasm.OpcodeI32DivS || op == wasm.OpcodeI64DivS || op == wasm.OpcodeI32RemS || op == wasm.OpcodeI64RemS
	rem := op == wasm.OpcodeI32RemU || op == wasm.OpcodeI64RemU || op == wasm.OpcodeI32RemS || op == wasm.OpcodeI64RemS
	width := uint64(32)
	if is64 {
		width = 64
	} else {
		d = uint64(uint32(d))
	}
	sd := int64(d)
	if !is64 {
		sd = int64(int32(d))
	}
	if d == 0 || signed && sd == -1 {
		return ssa.ValueInvalid, false
	}
	l := &divLowering{c: c, is64: is64, width: width}
	if !signed {
		if rem {
			return l.urem(x, d), true
		}
		return l.udiv(x, d), true
	}
	ad := uint64(sd)
	if sd < 0 {
		ad = -ad
	}
	if !is64 {
		ad = uint64(uint32(ad))
	}
	if ad&(ad-1) == 0 {
		return l.sPow2(x, ad, sd < 0, rem), true
	}
	if !is64 {
		q := l.sdiv32(x, int32(sd))
		if rem {
			return l.sub(x, l.mul(q, l.k(d))), true
		}
		return q, true
	}
	// |x| divided by |d|, unsigned, then the signs: a quotient is negative when exactly one of them is,
	// and a remainder takes the dividend's sign.
	s := l.sshr(x, width-1)
	a := l.sub(l.xor(x, s), s)
	if rem {
		r := l.urem(a, ad)
		return l.sub(l.xor(r, s), s), true
	}
	q := l.udiv(a, ad)
	t := s
	if sd < 0 {
		t = l.xor(s, l.k(^uint64(0)))
	}
	return l.sub(l.xor(q, t), t), true
}

type divLowering struct {
	c     *Compiler
	is64  bool
	width uint64
}

func (l *divLowering) b() ssa.Builder { return l.c.ssaBuilder }

func (l *divLowering) k(v uint64) ssa.Value {
	if l.is64 {
		return l.b().AllocateInstruction().AsIconst64(v).Insert(l.b()).Return()
	}
	return l.b().AllocateInstruction().AsIconst32(uint32(v)).Insert(l.b()).Return()
}

func (l *divLowering) k64(v uint64) ssa.Value {
	return l.b().AllocateInstruction().AsIconst64(v).Insert(l.b()).Return()
}

func (l *divLowering) sub(x, y ssa.Value) ssa.Value {
	return l.b().AllocateInstruction().AsIsub(x, y).Insert(l.b()).Return()
}

func (l *divLowering) add(x, y ssa.Value) ssa.Value {
	return l.b().AllocateInstruction().AsIadd(x, y).Insert(l.b()).Return()
}

func (l *divLowering) mul(x, y ssa.Value) ssa.Value {
	return l.b().AllocateInstruction().AsImul(x, y).Insert(l.b()).Return()
}

func (l *divLowering) xor(x, y ssa.Value) ssa.Value {
	i := l.b().AllocateInstruction()
	i.AsBxor(x, y)
	return i.Insert(l.b()).Return()
}

func (l *divLowering) and(x, y ssa.Value) ssa.Value {
	return l.b().AllocateInstruction().AsBand(x, y).Insert(l.b()).Return()
}

func (l *divLowering) shamt(x ssa.Value, n uint64) ssa.Value {
	if x.Type() == ssa.TypeI64 {
		return l.k64(n)
	}
	return l.b().AllocateInstruction().AsIconst32(uint32(n)).Insert(l.b()).Return()
}

func (l *divLowering) ushr(x ssa.Value, n uint64) ssa.Value {
	return l.b().AllocateInstruction().AsUshr(x, l.shamt(x, n)).Insert(l.b()).Return()
}

func (l *divLowering) sshr(x ssa.Value, n uint64) ssa.Value {
	return l.b().AllocateInstruction().AsSshr(x, l.shamt(x, n)).Insert(l.b()).Return()
}

func (l *divLowering) umulhi(x, y ssa.Value) ssa.Value {
	return l.b().AllocateInstruction().AsUmulhi(x, y).Insert(l.b()).Return()
}

func (l *divLowering) zext(x ssa.Value) ssa.Value {
	return l.b().AllocateInstruction().AsUExtend(x, 32, 64).Insert(l.b()).Return()
}

func (l *divLowering) wrap(x ssa.Value) ssa.Value {
	return l.b().AllocateInstruction().AsIreduce(x, ssa.TypeI32).Insert(l.b()).Return()
}

// udiv divides x, unsigned, by d, which is not zero.
func (l *divLowering) udiv(x ssa.Value, d uint64) ssa.Value {
	switch {
	case d == 1:
		return x
	case d&(d-1) == 0:
		return l.ushr(x, uint64(bits.TrailingZeros64(d)))
	case !l.is64:
		// For a 32-bit x and M = ⌈2^64/d⌉, ⌊x/d⌋ is the high half of x·M (Lemire, Kaser and Kurz,
		// "Faster Remainder by Direct Computation", 2019: exact as 64 ≥ 32 + ⌈log2 d⌉).
		return l.wrap(l.umulhi(l.zext(x), l.k64(^uint64(0)/d+1)))
	case d > 1<<63:
		// The quotient is one when x reaches d and zero otherwise.
		return l.b().AllocateInstruction().AsUExtend(
			l.b().AllocateInstruction().AsIcmp(x, l.k(d), ssa.IntegerCmpCondUnsignedGreaterThanOrEqual).Insert(l.b()).Return(),
			32, 64).Insert(l.b()).Return()
	}
	// Granlund and Montgomery's round-up method: with s = ⌈log2 d⌉ and m = ⌊2^(64+s)/d⌋ - 2^64 + 1,
	// t = mulhi(m, x) and the quotient is (t + (x-t)/2) >> (s-1).
	s := uint64(bits.Len64(d - 1))
	quo, _ := bits.Div64(1<<s-d, 0, d)
	t := l.umulhi(x, l.k64(quo+1))
	return l.ushr(l.add(t, l.ushr(l.sub(x, t), 1)), s-1)
}

// urem is x modulo d, unsigned, d not zero.
func (l *divLowering) urem(x ssa.Value, d uint64) ssa.Value {
	switch {
	case d == 1:
		return l.k(0)
	case d&(d-1) == 0:
		return l.and(x, l.k(d-1))
	case !l.is64:
		// The low half of x·M is the fraction x/d scaled by 2^64; times d, its high half is x mod d
		// (Lemire, Kaser and Kurz, 2019).
		frac := l.mul(l.zext(x), l.k64(^uint64(0)/d+1))
		return l.wrap(l.umulhi(frac, l.k64(d)))
	}
	return l.sub(x, l.mul(l.udiv(x, d), l.k(d)))
}

// sdiv32 divides the i32 x, signed, by d, which is neither 0, nor ±1, nor ± a power of two: the high
// half of x times d's magic number, corrected by x when their signs disagree, shifted, and one more
// when negative to round towards zero (Warren, "Hacker's Delight", 10-4).
func (l *divLowering) sdiv32(x ssa.Value, d int32) ssa.Value {
	m, sh := magicS32(d)
	wide := l.b().AllocateInstruction().AsSExtend(x, 32, 64).Insert(l.b()).Return()
	p := l.mul(wide, l.k64(uint64(int64(m))))
	var q ssa.Value
	if d > 0 && m < 0 || d < 0 && m > 0 {
		hi := l.wrap(l.sshr(p, 32))
		if d > 0 {
			hi = l.add(hi, x)
		} else {
			hi = l.sub(hi, x)
		}
		q = hi
		if sh > 0 {
			q = l.sshr(hi, uint64(sh))
		}
	} else {
		q = l.wrap(l.sshr(p, 32+uint64(sh)))
	}
	return l.add(q, l.ushr(q, 31))
}

// magicS32 returns the magic number and shift of signed 32-bit division by d, |d| at least 2 (Warren,
// "Hacker's Delight", figure 10-1).
func magicS32(d int32) (int32, uint) {
	const two31 = uint32(1) << 31
	ad := uint32(d)
	if d < 0 {
		ad = -ad
	}
	t := two31 + uint32(d)>>31
	anc := t - 1 - t%ad
	p := 31
	q1, r1 := two31/anc, two31-two31/anc*anc
	q2, r2 := two31/ad, two31-two31/ad*ad
	for {
		p++
		q1, r1 = 2*q1, 2*r1
		if r1 >= anc {
			q1, r1 = q1+1, r1-anc
		}
		q2, r2 = 2*q2, 2*r2
		if r2 >= ad {
			q2, r2 = q2+1, r2-ad
		}
		delta := ad - r2
		if !(q1 < delta || q1 == delta && r1 == 0) {
			break
		}
	}
	m := int32(q2 + 1)
	if d < 0 {
		m = -m
	}
	return m, uint(p - 32)
}

// sPow2 divides x, signed, by ±ad, a power of two: shifting rounds towards minus infinity, so a
// negative x is biased by ad-1 first to round towards zero instead.
func (l *divLowering) sPow2(x ssa.Value, ad uint64, neg, rem bool) ssa.Value {
	n := uint64(bits.TrailingZeros64(ad))
	if n == 0 { // |d| is one.
		if rem {
			return l.k(0)
		}
		if neg {
			return l.sub(l.k(0), x)
		}
		return x
	}
	bias := l.ushr(l.sshr(x, l.width-1), l.width-n)
	biased := l.add(x, bias)
	if rem {
		return l.sub(l.and(biased, l.k(ad-1)), bias)
	}
	q := l.sshr(biased, n)
	if neg {
		return l.sub(l.k(0), q)
	}
	return q
}

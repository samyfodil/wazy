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

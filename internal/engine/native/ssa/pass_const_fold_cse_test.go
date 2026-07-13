package ssa

import (
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestBuilder_passConstFoldAndCSE exercises passConstFoldAndCSE directly. Each case builds a
// small function by hand, runs the pass followed by passDeadCodeEliminationOpt (exactly as
// runPreBlockLayoutPasses does), and asserts the resulting textual IR. Running DCE afterward
// means aliases created by the pass are fully resolved and dead instructions are gone, which is
// what makes the "after" text below directly comparable to what real callers would see.
func TestBuilder_passConstFoldAndCSE(t *testing.T) {
	for _, tc := range []struct {
		name          string
		setup         func(b *builder) (verifier func(t *testing.T))
		before, after string
	}{
		{
			// Regression guard for the i32 wraparound boundary: 0xffffffff + 1 must wrap to 0,
			// not overflow into the upper 32 bits of the uint64 that backs the constant.
			name: "fold: i32 Iadd wraps at the 2^32 boundary",
			setup: func(b *builder) func(*testing.T) {
				entry := b.AllocateBasicBlock()
				b.SetCurrentBlock(entry)

				x := b.AllocateInstruction().AsIconst32(0xffffffff).Insert(b).Return()
				y := b.AllocateInstruction().AsIconst32(1).Insert(b).Return()
				sum := b.AllocateInstruction().AsIadd(x, y).Insert(b).Return()

				ret := b.AllocateInstruction()
				args := b.varLengthPool.Allocate(1)
				args = args.Append(&b.varLengthPool, sum)
				ret.AsReturn(args)
				b.InsertInstruction(ret)
				return nil
			},
			before: `
blk0: ()
	v0:i32 = Iconst_32 0xffffffff
	v1:i32 = Iconst_32 0x1
	v2:i32 = Iadd v0, v1
	Return v2
`,
			after: `
blk0: ()
	v3:i32 = Iconst_32 0x0
	Return v3
`,
		},
		{
			name: "fold: i64 Iadd",
			setup: func(b *builder) func(*testing.T) {
				entry := b.AllocateBasicBlock()
				b.SetCurrentBlock(entry)

				x := b.AllocateInstruction().AsIconst64(10).Insert(b).Return()
				y := b.AllocateInstruction().AsIconst64(20).Insert(b).Return()
				sum := b.AllocateInstruction().AsIadd(x, y).Insert(b).Return()

				ret := b.AllocateInstruction()
				args := b.varLengthPool.Allocate(1)
				args = args.Append(&b.varLengthPool, sum)
				ret.AsReturn(args)
				b.InsertInstruction(ret)
				return nil
			},
			before: `
blk0: ()
	v0:i64 = Iconst_64 0xa
	v1:i64 = Iconst_64 0x14
	v2:i64 = Iadd v0, v1
	Return v2
`,
			after: `
blk0: ()
	v3:i64 = Iconst_64 0x1e
	Return v3
`,
		},
		{
			// The shift amount must be masked modulo the bit width (64 here) *before* folding.
			// A naive `1 << 65` would either be a Go compile-time-shift-of-constant issue or
			// (for a runtime uint64 shift) produce 0, whereas wasm/the correct masked semantics
			// give 65 & 63 == 1, hence 1 << 1 == 2.
			name: "fold: Ishl masks the shift amount modulo the bit width",
			setup: func(b *builder) func(*testing.T) {
				entry := b.AllocateBasicBlock()
				b.SetCurrentBlock(entry)

				x := b.AllocateInstruction().AsIconst64(1).Insert(b).Return()
				amt := b.AllocateInstruction().AsIconst64(65).Insert(b).Return()
				shl := b.AllocateInstruction().AsIshl(x, amt).Insert(b).Return()

				ret := b.AllocateInstruction()
				args := b.varLengthPool.Allocate(1)
				args = args.Append(&b.varLengthPool, shl)
				ret.AsReturn(args)
				b.InsertInstruction(ret)
				return nil
			},
			before: `
blk0: ()
	v0:i64 = Iconst_64 0x1
	v1:i64 = Iconst_64 0x41
	v2:i64 = Ishl v0, v1
	Return v2
`,
			after: `
blk0: ()
	v3:i64 = Iconst_64 0x2
	Return v3
`,
		},
		{
			// -1 (0xffffffff as i32 bits) is signed-less-than 1, but not unsigned-less-than 1.
			// This checks that the fold interprets the i32 bit pattern as signed for lt_s.
			name: "fold: Icmp lt_s treats the i32 bit pattern as signed",
			setup: func(b *builder) func(*testing.T) {
				entry := b.AllocateBasicBlock()
				b.SetCurrentBlock(entry)

				x := b.AllocateInstruction().AsIconst32(0xffffffff).Insert(b).Return()
				y := b.AllocateInstruction().AsIconst32(1).Insert(b).Return()
				cmp := b.AllocateInstruction().AsIcmp(x, y, IntegerCmpCondSignedLessThan).Insert(b).Return()

				ret := b.AllocateInstruction()
				args := b.varLengthPool.Allocate(1)
				args = args.Append(&b.varLengthPool, cmp)
				ret.AsReturn(args)
				b.InsertInstruction(ret)
				return nil
			},
			before: `
blk0: ()
	v0:i32 = Iconst_32 0xffffffff
	v1:i32 = Iconst_32 0x1
	v2:i32 = Icmp lt_s, v0, v1
	Return v2
`,
			after: `
blk0: ()
	v3:i32 = Iconst_32 0x1
	Return v3
`,
		},
		{
			// Exercises every algebraic identity that aliases directly back to one of the
			// original operands (i.e. none of these need to materialize a fresh constant):
			// x+0, 0+x, x-0, x*1, 1*x, x&x, x|x, x|0, x^0. All nine should collapse to the
			// block's own parameter.
			name: "identities: collapse to the surviving operand",
			setup: func(b *builder) func(*testing.T) {
				entry := b.AllocateBasicBlock()
				x := entry.AddParam(b, TypeI32)
				b.SetCurrentBlock(entry)

				zero := b.AllocateInstruction().AsIconst32(0).Insert(b).Return()
				one := b.AllocateInstruction().AsIconst32(1).Insert(b).Return()

				addZero := b.AllocateInstruction().AsIadd(x, zero).Insert(b).Return()
				zeroAdd := b.AllocateInstruction().AsIadd(zero, x).Insert(b).Return()
				subZero := b.AllocateInstruction().AsIsub(x, zero).Insert(b).Return()
				mulOne := b.AllocateInstruction().AsImul(x, one).Insert(b).Return()
				oneMul := b.AllocateInstruction().AsImul(one, x).Insert(b).Return()
				andSelf := b.AllocateInstruction().AsBand(x, x).Insert(b).Return()

				orSelfInst := b.AllocateInstruction()
				orSelfInst.AsBor(x, x)
				orSelfInst.Insert(b)
				orSelf := orSelfInst.Return()

				orZeroInst := b.AllocateInstruction()
				orZeroInst.AsBor(x, zero)
				orZeroInst.Insert(b)
				orZero := orZeroInst.Return()

				xorZeroInst := b.AllocateInstruction()
				xorZeroInst.AsBxor(x, zero)
				xorZeroInst.Insert(b)
				xorZero := xorZeroInst.Return()

				ret := b.AllocateInstruction()
				args := b.varLengthPool.Allocate(9)
				for _, v := range []Value{addZero, zeroAdd, subZero, mulOne, oneMul, andSelf, orSelf, orZero, xorZero} {
					args = args.Append(&b.varLengthPool, v)
				}
				ret.AsReturn(args)
				b.InsertInstruction(ret)
				return nil
			},
			before: `
blk0: (v0:i32)
	v1:i32 = Iconst_32 0x0
	v2:i32 = Iconst_32 0x1
	v3:i32 = Iadd v0, v1
	v4:i32 = Iadd v1, v0
	v5:i32 = Isub v0, v1
	v6:i32 = Imul v0, v2
	v7:i32 = Imul v2, v0
	v8:i32 = Band v0, v0
	v9:i32 = Bor v0, v0
	v10:i32 = Bor v0, v1
	v11:i32 = Bxor v0, v1
	Return v3, v4, v5, v6, v7, v8, v9, v10, v11
`,
			after: `
blk0: (v0:i32)
	Return v0, v0, v0, v0, v0, v0, v0, v0, v0
`,
		},
		{
			name: "identities: x*0 materializes a fresh i32 zero",
			setup: func(b *builder) func(*testing.T) {
				entry := b.AllocateBasicBlock()
				x := entry.AddParam(b, TypeI32)
				b.SetCurrentBlock(entry)

				zero := b.AllocateInstruction().AsIconst32(0).Insert(b).Return()
				mulZero := b.AllocateInstruction().AsImul(x, zero).Insert(b).Return()

				ret := b.AllocateInstruction()
				args := b.varLengthPool.Allocate(1)
				args = args.Append(&b.varLengthPool, mulZero)
				ret.AsReturn(args)
				b.InsertInstruction(ret)
				return nil
			},
			before: `
blk0: (v0:i32)
	v1:i32 = Iconst_32 0x0
	v2:i32 = Imul v0, v1
	Return v2
`,
			after: `
blk0: (v0:i32)
	v3:i32 = Iconst_32 0x0
	Return v3
`,
		},
		{
			name: "identities: x&0 materializes a fresh i64 zero",
			setup: func(b *builder) func(*testing.T) {
				entry := b.AllocateBasicBlock()
				x := entry.AddParam(b, TypeI64)
				b.SetCurrentBlock(entry)

				zero := b.AllocateInstruction().AsIconst64(0).Insert(b).Return()
				andZero := b.AllocateInstruction().AsBand(x, zero).Insert(b).Return()

				ret := b.AllocateInstruction()
				args := b.varLengthPool.Allocate(1)
				args = args.Append(&b.varLengthPool, andZero)
				ret.AsReturn(args)
				b.InsertInstruction(ret)
				return nil
			},
			before: `
blk0: (v0:i64)
	v1:i64 = Iconst_64 0x0
	v2:i64 = Band v0, v1
	Return v2
`,
			after: `
blk0: (v0:i64)
	v3:i64 = Iconst_64 0x0
	Return v3
`,
		},
		{
			name: "CSE: identical Iadd within a block collapses to the first",
			setup: func(b *builder) func(*testing.T) {
				entry := b.AllocateBasicBlock()
				p0 := entry.AddParam(b, TypeI32)
				p1 := entry.AddParam(b, TypeI32)
				b.SetCurrentBlock(entry)

				add1 := b.AllocateInstruction().AsIadd(p0, p1).Insert(b).Return()
				add2 := b.AllocateInstruction().AsIadd(p0, p1).Insert(b).Return()

				ret := b.AllocateInstruction()
				args := b.varLengthPool.Allocate(2)
				args = args.Append(&b.varLengthPool, add1)
				args = args.Append(&b.varLengthPool, add2)
				ret.AsReturn(args)
				b.InsertInstruction(ret)
				return nil
			},
			before: `
blk0: (v0:i32, v1:i32)
	v2:i32 = Iadd v0, v1
	v3:i32 = Iadd v0, v1
	Return v2, v3
`,
			after: `
blk0: (v0:i32, v1:i32)
	v2:i32 = Iadd v0, v1
	Return v2, v2
`,
		},
		{
			// CSE must canonicalize commutative operands so that `a op b` and `b op a` are
			// recognized as the same computation.
			name: "CSE: commutative Iadd collapses regardless of operand order",
			setup: func(b *builder) func(*testing.T) {
				entry := b.AllocateBasicBlock()
				p0 := entry.AddParam(b, TypeI32)
				p1 := entry.AddParam(b, TypeI32)
				b.SetCurrentBlock(entry)

				add1 := b.AllocateInstruction().AsIadd(p0, p1).Insert(b).Return()
				add2 := b.AllocateInstruction().AsIadd(p1, p0).Insert(b).Return()

				ret := b.AllocateInstruction()
				args := b.varLengthPool.Allocate(2)
				args = args.Append(&b.varLengthPool, add1)
				args = args.Append(&b.varLengthPool, add2)
				ret.AsReturn(args)
				b.InsertInstruction(ret)
				return nil
			},
			before: `
blk0: (v0:i32, v1:i32)
	v2:i32 = Iadd v0, v1
	v3:i32 = Iadd v1, v0
	Return v2, v3
`,
			after: `
blk0: (v0:i32, v1:i32)
	v2:i32 = Iadd v0, v1
	Return v2, v2
`,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			b := NewBuilder().(*builder)
			verifier := tc.setup(b)
			require.Equal(t, tc.before, b.Format())
			passConstFoldAndCSE(b)
			if verifier != nil {
				verifier(t)
			}
			passDeadCodeEliminationOpt(b)
			require.Equal(t, tc.after, b.Format())
		})
	}
}

// countOpcode returns how many instructions with the given opcode remain live in the function
// (walking every block's instruction list in its current order). Used by the guard tests below
// instead of exact-IR-text assertions so they stay robust to value-numbering churn.
func countOpcode(b *builder, op Opcode) int {
	n := 0
	for blk := b.blockIteratorBegin(); blk != nil; blk = b.blockIteratorNext() {
		for cur := blk.rootInstr; cur != nil; cur = cur.next {
			if cur.opcode == op {
				n++
			}
		}
	}
	return n
}

// TestBuilder_passConstFoldAndCSE_exitIfTrueGuard is the adversarial regression suite for the
// single sharp edge in this pass: an OpcodeIcmp that directly feeds an OpcodeExitIfTrueWithCode
// must survive both constant folding and CSE untouched, because both amd64's and arm64's
// lowerExitIfTrueWithCode fuse it via backend.Compiler.MatchInstr and hard-panic (no fallback)
// unless the condition is still an Icmp with RefCount < 2. See exitIfTrueGuardedIcmps.
//
// These build the exact IR shapes the frontend emits for trap checks (div-by-zero,
// memory-out-of-bounds) and prove the guard holds. Without the guard, each of these would
// compile-time panic in the backend on real input the spectest corpus does not always cover.
func TestBuilder_passConstFoldAndCSE_exitIfTrueGuard(t *testing.T) {
	// run applies exactly the pass sequence runPreBlockLayoutPasses uses around this pass:
	// constant-fold/CSE, then DCE to resolve aliases and drop the now-dead instructions.
	run := func(b *builder) {
		passConstFoldAndCSE(b)
		passDeadCodeEliminationOpt(b)
	}

	t.Run("const-operand Icmp feeding ExitIfTrueWithCode is NOT folded", func(t *testing.T) {
		// Mirrors an integer division-by-zero check whose divisor is a constant: the Icmp has two
		// constant operands, so tryConstFold would collapse it to an Iconst -- except that it feeds
		// ExitIfTrueWithCode and must stay an Icmp.
		b := NewBuilder().(*builder)
		entry := b.AllocateBasicBlock()
		execCtx := entry.AddParam(b, TypeI64)
		b.SetCurrentBlock(entry)

		five := b.AllocateInstruction().AsIconst32(5).Insert(b).Return()
		zero := b.AllocateInstruction().AsIconst32(0).Insert(b).Return()
		cmp := b.AllocateInstruction().AsIcmp(five, zero, IntegerCmpCondEqual).Insert(b).Return()
		b.AllocateInstruction().AsExitIfTrueWithCode(execCtx, cmp, nativeapi.ExitCodeIntegerDivisionByZero).Insert(b)

		run(b)

		require.Equal(t, 1, countOpcode(b, OpcodeIcmp)) // Icmp survived: still lowerable.
		require.Equal(t, 1, countOpcode(b, OpcodeExitIfTrueWithCode))
	})

	t.Run("a NON-guarded const Icmp is still folded even beside a guarded one", func(t *testing.T) {
		// Sanity that the guard is scoped, not global: an identical constant Icmp that feeds a plain
		// Return (not ExitIfTrue) must still fold away, while the guarded copy stays an Icmp.
		b := NewBuilder().(*builder)
		entry := b.AllocateBasicBlock()
		execCtx := entry.AddParam(b, TypeI64)
		b.SetCurrentBlock(entry)

		five := b.AllocateInstruction().AsIconst32(5).Insert(b).Return()
		zero := b.AllocateInstruction().AsIconst32(0).Insert(b).Return()
		guardedCmp := b.AllocateInstruction().AsIcmp(five, zero, IntegerCmpCondEqual).Insert(b).Return()
		b.AllocateInstruction().AsExitIfTrueWithCode(execCtx, guardedCmp, nativeapi.ExitCodeIntegerDivisionByZero).Insert(b)

		returnedCmp := b.AllocateInstruction().AsIcmp(five, zero, IntegerCmpCondEqual).Insert(b).Return()
		ret := b.AllocateInstruction()
		args := b.varLengthPool.Allocate(1)
		args = args.Append(&b.varLengthPool, returnedCmp)
		ret.AsReturn(args)
		b.InsertInstruction(ret)

		run(b)

		// Only the guarded Icmp remains; the returned one folded to an Iconst.
		require.Equal(t, 1, countOpcode(b, OpcodeIcmp))
	})

	t.Run("two identical Icmps each feeding ExitIfTrueWithCode are NOT CSE-merged", func(t *testing.T) {
		// This is the generalized shape of the original bug: two structurally identical trap-check
		// Icmps (as the frontend emits for two redundant memory bounds checks in one block). CSE
		// would merge the second into the first, pushing the survivor's RefCount to 2 and defeating
		// the fusion at the SECOND ExitIfTrueWithCode. The guard must keep both Icmps distinct.
		b := NewBuilder().(*builder)
		entry := b.AllocateBasicBlock()
		execCtx := entry.AddParam(b, TypeI64)
		x := entry.AddParam(b, TypeI32)
		y := entry.AddParam(b, TypeI32)
		b.SetCurrentBlock(entry)

		cmp1 := b.AllocateInstruction().AsIcmp(x, y, IntegerCmpCondUnsignedLessThan).Insert(b).Return()
		b.AllocateInstruction().AsExitIfTrueWithCode(execCtx, cmp1, nativeapi.ExitCodeMemoryOutOfBounds).Insert(b)
		cmp2 := b.AllocateInstruction().AsIcmp(x, y, IntegerCmpCondUnsignedLessThan).Insert(b).Return()
		b.AllocateInstruction().AsExitIfTrueWithCode(execCtx, cmp2, nativeapi.ExitCodeMemoryOutOfBounds).Insert(b)

		run(b)

		require.Equal(t, 2, countOpcode(b, OpcodeIcmp)) // both survived, neither merged away.
		require.Equal(t, 2, countOpcode(b, OpcodeExitIfTrueWithCode))
	})

	t.Run("operands of a guarded Icmp are still constant-folded", func(t *testing.T) {
		// Folding the Icmp itself is forbidden, but folding its *operands* is both safe and
		// beneficial: (2+3) collapses to a single Iconst that the Icmp then references. The Icmp
		// stays an Icmp (RefCount 1) and remains fusable.
		b := NewBuilder().(*builder)
		entry := b.AllocateBasicBlock()
		execCtx := entry.AddParam(b, TypeI64)
		x := entry.AddParam(b, TypeI32) // non-constant, so the Icmp is not fully foldable.
		b.SetCurrentBlock(entry)

		two := b.AllocateInstruction().AsIconst32(2).Insert(b).Return()
		three := b.AllocateInstruction().AsIconst32(3).Insert(b).Return()
		sum := b.AllocateInstruction().AsIadd(two, three).Insert(b).Return()
		cmp := b.AllocateInstruction().AsIcmp(x, sum, IntegerCmpCondUnsignedLessThan).Insert(b).Return()
		b.AllocateInstruction().AsExitIfTrueWithCode(execCtx, cmp, nativeapi.ExitCodeMemoryOutOfBounds).Insert(b)

		run(b)

		require.Equal(t, 1, countOpcode(b, OpcodeIcmp)) // Icmp preserved.
		require.Equal(t, 0, countOpcode(b, OpcodeIadd)) // 2+3 folded away.
	})
}

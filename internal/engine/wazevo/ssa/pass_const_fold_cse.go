package ssa

import "math/bits"

// passConstFoldAndCSE implements two classic, cheap, high-value SSA optimizations that were
// previously entirely absent from the pipeline (see the TODO that used to live in
// runPreBlockLayoutPasses): constant folding (plus a handful of algebraic identities), and local
// (per-block) common subexpression elimination via value numbering.
//
// Both sub-passes work purely by aliasing: they never delete an Instruction themselves. Instead
// they call b.alias(deadValue, survivingValue) so that every use of `deadValue` transparently
// resolves to `survivingValue`. passDeadCodeEliminationOpt, which runs right after this pass in
// runPreBlockLayoutPasses, then physically removes the now-unreferenced (and side-effect free)
// instructions. This mirrors exactly how passNopInstElimination and passRedundantPhiEliminationOpt
// already behave.
//
// Correctness (SSA dominance): aliasing a value to a newly-created constant, or to a
// value that already existed, must preserve the property that the definition of the surviving
// value dominates every use of the value it replaces. This pass guarantees that by construction:
//
//   - Constant folding and algebraic identities never move or reuse a value across blocks: any
//     freshly created constant instruction is spliced into the *same* basic block, immediately
//     before the instruction it replaces. Since block-level dominance is determined solely by
//     which block a value's defining instruction lives in (not its position within the block),
//     placing the new instruction earlier in the same block guarantees it dominates exactly
//     everything that the original instruction dominated.
//   - CSE only ever aliases to a value defined *earlier in the same block*. Because a block is a
//     straight-line sequence of instructions, "earlier in program order within the same block"
//     always dominates "later in program order within the same block". CSE state is reset for
//     each block, so there's no cross-block reasoning that could go wrong.
//
// Scope (deliberately conservative -- see wazevoapi/... OPTIMIZATIONS.md item C4):
//   - Constant folding covers the integer arithmetic/bitwise/shift/rotate opcodes plus Icmp, on
//     both i32 and i64, mirroring wasm wrapping and shift-amount-masking semantics exactly. It does
//     NOT fold Sdiv/Udiv/Srem/Urem, which can trap and therefore must stay reachable at runtime.
//   - Algebraic identities are limited to the well-known cheap set (x+0, x*1, x*0, x&0, x|0, x^0,
//     x&x, x|x). x<<0 (and friends) is intentionally NOT re-implemented here: passNopInstElimination
//     (which runs immediately before this pass) already handles the "shift/rotate by a constant
//     that's 0 mod bit-width" case.
//   - CSE is restricted to a hand-picked whitelist of pure, non-trapping, non-memory-touching
//     opcodes (see csePureOpcodes below). In particular it deliberately excludes Load/Store/Call
//     and the trapping div/rem ops: proving two loads observe the same value requires alias
//     analysis this pass does not do, and getting the purity set wrong here would be a miscompile,
//     not just a missed optimization.
//
// One subtlety earned the hard way (caught by the full spectest suite, not by reasoning about SSA
// alone): OpcodeIcmp instructions that directly feed an OpcodeExitIfTrueWithCode (the trap-check
// idiom the frontend emits for e.g. integer division-by-zero and out-of-bounds checks) must be
// left completely untouched, both by folding and by CSE. Both amd64's and arm64's
// lowerExitIfTrueWithCode fuse such an Icmp directly into a single compare-and-trap machine
// sequence, and that fusion requires (via backend.Compiler.MatchInstr) that the Icmp still be an
// Icmp *and* have exactly one reference -- there is no fallback path; it's a hard panic otherwise.
// Constant-folding such an Icmp into an Iconst changes its opcode, and CSE-merging it with an
// identical Icmp elsewhere pushes its reference count to two: either one defeats the fusion.
// Ordinary conditional branches (Brz/Brnz) have no such constraint -- both backends fall back to
// materializing the condition into a register and testing it -- so this guard is scoped
// specifically to ExitIfTrueWithCode's condition operand. See exitIfTrueGuardedIcmps.
func passConstFoldAndCSE(b *builder) {
	guarded := exitIfTrueGuardedIcmps(b)

	if b.cseCache == nil {
		b.cseCache = make(map[cseKey]Value)
	}
	cache := b.cseCache

	for blk := b.blockIteratorBegin(); blk != nil; blk = b.blockIteratorNext() {
		clear(cache)

		for cur := blk.rootInstr; cur != nil; cur = cur.next {
			// Cheap up-front filter: every opcode this pass can fold, simplify, or CSE is in
			// csePureOpcode, so anything else (the common case -- Load/Store/Call/branches/...)
			// is skipped before paying for alias resolution or any of the checks below.
			if !csePureOpcode[cur.opcode] {
				continue
			}

			// Resolve away any aliases created by earlier passes (or by this pass on an earlier
			// instruction in this same block) so that every check below observes canonical values.
			b.resolveArgumentAlias(cur)

			if cur.opcode == OpcodeIcmp && guarded[cur] {
				continue
			}

			if tryConstFold(b, blk, cur) {
				continue
			}
			if tryAlgebraicIdentity(b, blk, cur) {
				continue
			}
			tryCSE(b, cur, cache)
		}
	}
}

// exitIfTrueGuardedIcmps scans the whole function for OpcodeExitIfTrueWithCode instructions and
// returns the set of Icmp instructions that directly define their condition operand (after
// resolving aliases). See the note on passConstFoldAndCSE above for why these must be excluded
// from folding and CSE.
func exitIfTrueGuardedIcmps(b *builder) map[*Instruction]bool {
	var guarded map[*Instruction]bool
	for blk := b.blockIteratorBegin(); blk != nil; blk = b.blockIteratorNext() {
		for cur := blk.rootInstr; cur != nil; cur = cur.next {
			if cur.opcode != OpcodeExitIfTrueWithCode {
				continue
			}
			_, cond, _ := cur.ExitIfTrueWithCodeData()
			cond = b.resolveAlias(cond)
			def := b.InstructionOfValue(cond)
			if def == nil || def.opcode != OpcodeIcmp {
				continue
			}
			if guarded == nil {
				guarded = make(map[*Instruction]bool)
			}
			guarded[def] = true
		}
	}
	return guarded
}

// insertPureBefore splices a freshly allocated, side-effect-free instruction into `blk`
// immediately before `before`, and assigns its return value. This is the pass-time equivalent of
// Builder.InsertInstruction: that method is unusable here because it always appends at the
// block's `currentInstr` cursor (the position used while the frontend originally built the
// function), which has nothing to do with the arbitrary mid-block position we need once the
// function is fully built.
//
// Placing `newInstr` immediately before `before` in the same block is what makes this safe: it
// occupies a strictly earlier position in the same block, so it dominates everything `before`
// dominated (see the dominance discussion on passConstFoldAndCSE above).
func (b *builder) insertPureBefore(blk *basicBlock, before, newInstr *Instruction) {
	prev := before.prev
	newInstr.prev = prev
	newInstr.next = before
	before.prev = newInstr
	if prev != nil {
		prev.next = newInstr
	} else {
		blk.rootInstr = newInstr
	}

	r := b.allocateValue(newInstr.typ)
	newInstr.rValue = r.setInstructionID(newInstr.id)
}

// constIconst returns the OpcodeIconst instruction that defines `v`, or nil if `v` is not (known
// to be) an integer constant. `v` is assumed to already be alias-resolved.
func constIconst(b *builder, v Value) *Instruction {
	if !v.Valid() {
		return nil
	}
	def := b.InstructionOfValue(v)
	if def == nil || def.opcode != OpcodeIconst {
		return nil
	}
	return def
}

// intConstValue is a convenience wrapper around constIconst that directly returns the raw
// constant bit pattern (as stored by AsIconst32/AsIconst64) when `v` is a known integer constant.
func intConstValue(b *builder, v Value) (uint64, bool) {
	def := constIconst(b, v)
	if def == nil {
		return 0, false
	}
	return def.ConstantVal(), true
}

// tryConstFold attempts to replace `cur` with a freshly-created constant when all of its operands
// are themselves constants. On success, it inserts the new constant right before `cur` in `blk`
// and aliases cur's result to it, then returns true.
func tryConstFold(b *builder, blk *basicBlock, cur *Instruction) bool {
	switch cur.opcode {
	case OpcodeIadd, OpcodeIsub, OpcodeImul, OpcodeBand, OpcodeBor, OpcodeBxor,
		OpcodeIshl, OpcodeUshr, OpcodeSshr, OpcodeRotl, OpcodeRotr:
		xv, xok := intConstValue(b, cur.v)
		if !xok {
			return false
		}
		yv, yok := intConstValue(b, cur.v2)
		if !yok {
			return false
		}
		is32 := cur.typ == TypeI32
		result := foldIntBinary(cur.opcode, is32, xv, yv)

		newConst := b.AllocateInstruction()
		if is32 {
			newConst.AsIconst32(uint32(result))
		} else {
			newConst.AsIconst64(result)
		}
		b.insertPureBefore(blk, cur, newConst)
		b.alias(cur.Return(), newConst.Return())
		return true

	case OpcodeIcmp:
		x, y, cond := cur.IcmpData()
		xv, xok := intConstValue(b, x)
		if !xok {
			return false
		}
		yv, yok := intConstValue(b, y)
		if !yok {
			return false
		}
		is32 := x.Type() == TypeI32
		result := foldIcmp(cond, is32, xv, yv)

		newConst := b.AllocateInstruction()
		newConst.AsIconst32(uint32(result))
		b.insertPureBefore(blk, cur, newConst)
		b.alias(cur.Return(), newConst.Return())
		return true
	}
	return false
}

// foldIntBinary computes the result of a binary integer opcode given two constant operands,
// mirroring wasm's wrapping-arithmetic and shift/rotate-amount-masking semantics exactly:
// i32 operations wrap modulo 2^32, and shift/rotate amounts are masked modulo the bit width
// (31 for i32, 63 for i64) before being applied.
func foldIntBinary(op Opcode, is32 bool, xv, yv uint64) uint64 {
	switch op {
	case OpcodeIadd:
		r := xv + yv
		if is32 {
			r = uint64(uint32(r))
		}
		return r
	case OpcodeIsub:
		r := xv - yv
		if is32 {
			r = uint64(uint32(r))
		}
		return r
	case OpcodeImul:
		r := xv * yv
		if is32 {
			r = uint64(uint32(r))
		}
		return r
	case OpcodeBand:
		r := xv & yv
		if is32 {
			r = uint64(uint32(r))
		}
		return r
	case OpcodeBor:
		r := xv | yv
		if is32 {
			r = uint64(uint32(r))
		}
		return r
	case OpcodeBxor:
		r := xv ^ yv
		if is32 {
			r = uint64(uint32(r))
		}
		return r
	case OpcodeIshl:
		if is32 {
			amt := uint32(yv) & 31
			return uint64(uint32(xv) << amt)
		}
		amt := yv & 63
		return xv << amt
	case OpcodeUshr:
		if is32 {
			amt := uint32(yv) & 31
			return uint64(uint32(xv) >> amt)
		}
		amt := yv & 63
		return xv >> amt
	case OpcodeSshr:
		if is32 {
			amt := uint32(yv) & 31
			return uint64(uint32(int32(uint32(xv)) >> amt))
		}
		amt := yv & 63
		return uint64(int64(xv) >> amt)
	case OpcodeRotl:
		if is32 {
			amt := int(uint32(yv) & 31)
			return uint64(bits.RotateLeft32(uint32(xv), amt))
		}
		amt := int(yv & 63)
		return bits.RotateLeft64(xv, amt)
	case OpcodeRotr:
		if is32 {
			amt := int(uint32(yv) & 31)
			return uint64(bits.RotateLeft32(uint32(xv), -amt))
		}
		amt := int(yv & 63)
		return bits.RotateLeft64(xv, -amt)
	default:
		panic("BUG: unexpected opcode in foldIntBinary: " + op.String())
	}
}

// foldIcmp evaluates an integer comparison over two constant operands of the given width,
// returning 1 for true and 0 for false, matching the i32 result Icmp always produces.
func foldIcmp(cond IntegerCmpCond, is32 bool, xv, yv uint64) uint64 {
	var result bool
	if is32 {
		xu, yu := uint32(xv), uint32(yv)
		xs, ys := int32(xu), int32(yu)
		switch cond {
		case IntegerCmpCondEqual:
			result = xu == yu
		case IntegerCmpCondNotEqual:
			result = xu != yu
		case IntegerCmpCondSignedLessThan:
			result = xs < ys
		case IntegerCmpCondSignedGreaterThanOrEqual:
			result = xs >= ys
		case IntegerCmpCondSignedGreaterThan:
			result = xs > ys
		case IntegerCmpCondSignedLessThanOrEqual:
			result = xs <= ys
		case IntegerCmpCondUnsignedLessThan:
			result = xu < yu
		case IntegerCmpCondUnsignedGreaterThanOrEqual:
			result = xu >= yu
		case IntegerCmpCondUnsignedGreaterThan:
			result = xu > yu
		case IntegerCmpCondUnsignedLessThanOrEqual:
			result = xu <= yu
		default:
			panic("BUG: unexpected IntegerCmpCond in foldIcmp: " + cond.String())
		}
	} else {
		xs, ys := int64(xv), int64(yv)
		switch cond {
		case IntegerCmpCondEqual:
			result = xv == yv
		case IntegerCmpCondNotEqual:
			result = xv != yv
		case IntegerCmpCondSignedLessThan:
			result = xs < ys
		case IntegerCmpCondSignedGreaterThanOrEqual:
			result = xs >= ys
		case IntegerCmpCondSignedGreaterThan:
			result = xs > ys
		case IntegerCmpCondSignedLessThanOrEqual:
			result = xs <= ys
		case IntegerCmpCondUnsignedLessThan:
			result = xv < yv
		case IntegerCmpCondUnsignedGreaterThanOrEqual:
			result = xv >= yv
		case IntegerCmpCondUnsignedGreaterThan:
			result = xv > yv
		case IntegerCmpCondUnsignedLessThanOrEqual:
			result = xv <= yv
		default:
			panic("BUG: unexpected IntegerCmpCond in foldIcmp: " + cond.String())
		}
	}
	if result {
		return 1
	}
	return 0
}

// tryAlgebraicIdentity rewrites `cur` when only one operand is a known constant and the
// combination is a well-known algebraic identity (x+0, x*1, x*0, x&0, x|0, x^0, x&x, x|x). On a
// match, it aliases cur's result to the surviving operand (or to a freshly-inserted zero
// constant), and returns true. Note: x<<0 and friends are deliberately not handled here --
// passNopInstElimination already covers "shift/rotate by an immediate that's 0 mod bit-width",
// and re-implementing it here would just be redundant work on every compile.
func tryAlgebraicIdentity(b *builder, blk *basicBlock, cur *Instruction) bool {
	switch cur.opcode {
	case OpcodeIadd:
		x, y := cur.v, cur.v2
		if yv, ok := intConstValue(b, y); ok && yv == 0 {
			b.alias(cur.Return(), x)
			return true
		}
		if xv, ok := intConstValue(b, x); ok && xv == 0 {
			b.alias(cur.Return(), y)
			return true
		}
	case OpcodeIsub:
		// Only x-0 -> x is an identity; 0-x is negation, not x.
		x, y := cur.v, cur.v2
		if yv, ok := intConstValue(b, y); ok && yv == 0 {
			b.alias(cur.Return(), x)
			return true
		}
	case OpcodeImul:
		x, y := cur.v, cur.v2
		if yv, ok := intConstValue(b, y); ok {
			switch yv {
			case 1:
				b.alias(cur.Return(), x)
				return true
			case 0:
				aliasToZero(b, blk, cur)
				return true
			}
		}
		if xv, ok := intConstValue(b, x); ok {
			switch xv {
			case 1:
				b.alias(cur.Return(), y)
				return true
			case 0:
				aliasToZero(b, blk, cur)
				return true
			}
		}
	case OpcodeBand:
		x, y := cur.v, cur.v2
		if x == y {
			b.alias(cur.Return(), x)
			return true
		}
		if yv, ok := intConstValue(b, y); ok && yv == 0 {
			aliasToZero(b, blk, cur)
			return true
		}
		if xv, ok := intConstValue(b, x); ok && xv == 0 {
			aliasToZero(b, blk, cur)
			return true
		}
	case OpcodeBor:
		x, y := cur.v, cur.v2
		if x == y {
			b.alias(cur.Return(), x)
			return true
		}
		if yv, ok := intConstValue(b, y); ok && yv == 0 {
			b.alias(cur.Return(), x)
			return true
		}
		if xv, ok := intConstValue(b, x); ok && xv == 0 {
			b.alias(cur.Return(), y)
			return true
		}
	case OpcodeBxor:
		x, y := cur.v, cur.v2
		if yv, ok := intConstValue(b, y); ok && yv == 0 {
			b.alias(cur.Return(), x)
			return true
		}
		if xv, ok := intConstValue(b, x); ok && xv == 0 {
			b.alias(cur.Return(), y)
			return true
		}
	}
	return false
}

// aliasToZero aliases cur's result to a freshly-inserted zero constant of cur's type, spliced
// into blk immediately before cur (see insertPureBefore for why that placement is safe).
func aliasToZero(b *builder, blk *basicBlock, cur *Instruction) {
	zero := b.AllocateInstruction()
	if cur.typ == TypeI32 {
		zero.AsIconst32(0)
	} else {
		zero.AsIconst64(0)
	}
	b.insertPureBefore(blk, cur, zero)
	b.alias(cur.Return(), zero.Return())
}

// cseKey is the value-numbering key used by the local CSE pass: two instructions with equal keys
// are guaranteed to compute the same result, given that they belong to the whitelist of pure
// opcodes enforced by cseKeyOf.
type cseKey struct {
	op         Opcode
	v1, v2, v3 Value
	u1, u2     uint64
	typ        Type
}

// csePureOpcode is the whitelist of opcodes eligible for CSE, indexed directly by Opcode for an
// O(1) array lookup instead of a map hash+probe -- this is checked once per instruction on every
// single function compile, so avoiding map overhead here measurably affects compile time. It also
// doubles as passConstFoldAndCSE's up-front "is this instruction even worth looking at" filter:
// every opcode that tryConstFold or tryAlgebraicIdentity can rewrite is a subset of this whitelist,
// so instructions outside it (the common case: Load/Store/Call/branches/...) are skipped before
// paying for alias resolution or any of the fold/identity/CSE checks.
//
// Every opcode here is sideEffectNone (see instructionSideEffects): it cannot trap, cannot be
// observably reordered, and its result depends only on its operands and immediate fields. Notably
// absent: Load/Store/Call/Div/Rem, and any float/vector op -- CSE-ing those safely would need
// either alias analysis (loads) or careful handling of extra encoded state (e.g. VecLane) that
// this MVP does not attempt. Getting this list wrong is a miscompile, not just a missed
// optimization, so it is kept intentionally small.
var csePureOpcode = func() (t [opcodeEnd]bool) {
	for _, op := range [...]Opcode{
		OpcodeIconst, OpcodeF32const, OpcodeF64const,
		OpcodeIadd, OpcodeIsub, OpcodeImul,
		OpcodeBand, OpcodeBor, OpcodeBxor,
		OpcodeRotl, OpcodeRotr, OpcodeIshl, OpcodeSshr, OpcodeUshr,
		OpcodeIcmp,
		OpcodeSExtend, OpcodeUExtend, OpcodeIreduce,
		OpcodeClz, OpcodeCtz, OpcodePopcnt,
		OpcodeBitcast,
	} {
		t[op] = true
	}
	return
}()

// cseCommutative lists the (whitelisted) opcodes whose two operands can be freely swapped, so
// that e.g. `iadd a, b` and `iadd b, a` are recognized as the same computation.
func cseCommutative(op Opcode) bool {
	switch op {
	case OpcodeIadd, OpcodeImul, OpcodeBand, OpcodeBor, OpcodeBxor:
		return true
	}
	return false
}

// cseKeyOf builds the value-numbering key for `cur`, returning ok=false if `cur`'s opcode isn't
// in the CSE whitelist, or if it has variable-length arguments (none of the whitelisted opcodes
// are expected to, but this is checked defensively rather than assumed).
func cseKeyOf(cur *Instruction) (key cseKey, ok bool) {
	if !csePureOpcode[cur.opcode] {
		return cseKey{}, false
	}
	if len(cur.vs.View()) != 0 {
		return cseKey{}, false
	}
	v1, v2 := cur.v, cur.v2
	if cseCommutative(cur.opcode) && v1 > v2 {
		v1, v2 = v2, v1
	}
	return cseKey{op: cur.opcode, v1: v1, v2: v2, v3: cur.v3, u1: cur.u1, u2: cur.u2, typ: cur.typ}, true
}

// tryCSE looks up `cur` in the per-block value-numbering `table`. If an earlier instruction in
// this block already computed the same value, cur's result is aliased to it. Otherwise `cur` is
// recorded in `table` as the canonical instruction for its key.
func tryCSE(b *builder, cur *Instruction, table map[cseKey]Value) {
	key, ok := cseKeyOf(cur)
	if !ok {
		return
	}
	if existing, found := table[key]; found {
		b.alias(cur.Return(), existing)
		return
	}
	table[key] = cur.Return()
}

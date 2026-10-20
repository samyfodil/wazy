package frontend

import (
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

// wasm SIMD on a machine with no vector unit.
//
// A v128 is two 64-bit words, and every SIMD operation can be expressed on that
// pair with ordinary integer and floating-point instructions. Where the hardware
// has no vector unit, the vector opcodes are lowered that way instead of into
// ssa.TypeV128 values -- so no v128 value is ever created, the register allocator
// never sees its third register file, and the backend takes no part in it.
//
// riscv64 only, and emulateSIMD is a constant false everywhere else so none of
// this reaches those targets: the vector extension is optional on RISC-V and most
// shipping silicon lacks it, while SIMD is part of CoreFeaturesV2 and so of wazy's
// default -- which made SIMD withhold the compiler from *every* module on such a
// machine, including the overwhelming majority containing no v128 at all. amd64 is
// not offered a compiler without SSE4.1 and arm64 always has NEON, so neither can
// be in this position.
//
// The expansion lives in the frontend rather than in a later pass because the
// frontend is what owns the places a v128 can appear: the operand stack, block
// parameters, locals, and signatures. Splitting a value into two there is
// bookkeeping; splitting it after the IR is built would mean rewriting phis and
// call sites.

// pushV128 puts a v128 on the operand stack as its two words, low first.
//
// The stack is a flat list of values, so a v128 simply occupies two entries. Every
// count taken from a signature must therefore come from the expanded form rather
// than from the wasm arity, or the two disagree about how deep the stack is.
func (c *Compiler) pushV128(lo, hi ssa.Value) {
	state := c.state()
	state.push(lo)
	state.push(hi)
}

// popV128 takes the two words of a v128 off the operand stack.
func (c *Compiler) popV128() (lo, hi ssa.Value) {
	state := c.state()
	hi = state.pop()
	lo = state.pop()
	return
}

// v128LoadWords reads a v128 as two 64-bit loads from an address the caller has
// already bounds-checked for the full sixteen bytes.
func (c *Compiler) v128LoadWords(addr ssa.Value, disp uint32) (lo, hi ssa.Value) {
	builder := c.ssaBuilder
	loI := builder.AllocateInstruction()
	loI.AsLoad(addr, disp, ssa.TypeI64)
	builder.InsertInstruction(loI)
	hiI := builder.AllocateInstruction()
	hiI.AsLoad(addr, disp+8, ssa.TypeI64)
	builder.InsertInstruction(hiI)
	return loI.Return(), hiI.Return()
}

// v128StoreWords writes a v128 as two 64-bit stores.
func (c *Compiler) v128StoreWords(lo, hi, addr ssa.Value, disp uint32) {
	builder := c.ssaBuilder
	builder.AllocateInstruction().AsStore(ssa.OpcodeStore, lo, addr, disp).Insert(builder)
	builder.AllocateInstruction().AsStore(ssa.OpcodeStore, hi, addr, disp+8).Insert(builder)
}

// v128Binary applies op to both words of two v128 operands, which is the whole
// lowering for any operation that treats the vector as 128 undifferentiated bits or
// as two independent 64-bit lanes.
func (c *Compiler) v128Binary(op func(x, y ssa.Value) ssa.Value) {
	y0, y1 := c.popV128()
	x0, x1 := c.popV128()
	c.pushV128(op(x0, y0), op(x1, y1))
}

// The scalar word operations the lane lowerings are built from. Methods rather than
// closures at each site so the lowering reads as the operation it is.

func (c *Compiler) scalarBand(x, y ssa.Value) ssa.Value {
	return c.ssaBuilder.AllocateInstruction().AsBand(x, y).Insert(c.ssaBuilder).Return()
}

func (c *Compiler) scalarBor(x, y ssa.Value) ssa.Value {
	i := c.ssaBuilder.AllocateInstruction()
	i.AsBor(x, y) // AsBor and AsBxor return nothing, unlike AsBand
	c.ssaBuilder.InsertInstruction(i)
	return i.Return()
}

func (c *Compiler) scalarBxor(x, y ssa.Value) ssa.Value {
	i := c.ssaBuilder.AllocateInstruction()
	i.AsBxor(x, y)
	c.ssaBuilder.InsertInstruction(i)
	return i.Return()
}

// scalarBnot is xor with all ones: the SSA has no scalar bitwise-not.
func (c *Compiler) scalarBnot(x ssa.Value) ssa.Value {
	ones := c.ssaBuilder.AllocateInstruction().AsIconst64(^uint64(0)).Insert(c.ssaBuilder).Return()
	return c.scalarBxor(x, ones)
}

// scalarBandnot is x & ^y, matching v128.andnot's operand order.
func (c *Compiler) scalarBandnot(x, y ssa.Value) ssa.Value {
	return c.scalarBand(x, c.scalarBnot(y))
}

func (c *Compiler) scalarIadd(x, y ssa.Value) ssa.Value {
	return c.ssaBuilder.AllocateInstruction().AsIadd(x, y).Insert(c.ssaBuilder).Return()
}

func (c *Compiler) scalarIsub(x, y ssa.Value) ssa.Value {
	return c.ssaBuilder.AllocateInstruction().AsIsub(x, y).Insert(c.ssaBuilder).Return()
}

package frontend

import (
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// inlineLeafMaxBody is the largest function body, in bytes, inlineLeaf copies into a caller.
const inlineLeafMaxBody = 160

const (
	leafUnknown byte = iota
	leafYes
	leafNo
)

// isInlinableLeaf reports whether a call to the function fnIndex can be lowered as its body: a small
// local function of straight-line code, which makes no call and has no control flow, so the body
// needs nothing from the frame a call would give it.
func (c *Compiler) isInlinableLeaf(fnIndex uint32) bool {
	if !c.inlineLeaves || c.needListener || c.needSourceOffsetInfo || fnIndex < c.m.ImportFunctionCount {
		return false
	}
	i := fnIndex - c.m.ImportFunctionCount
	if int(i) >= len(c.m.CodeSection) {
		return false
	}
	if len(c.leafKinds) != len(c.m.CodeSection) {
		c.leafKinds = make([]byte, len(c.m.CodeSection))
	}
	if c.leafKinds[i] == leafUnknown {
		c.leafKinds[i] = leafNo
		if code := &c.m.CodeSection[i]; code.GoFunc == nil && straightLineBody(code.Body) {
			c.leafKinds[i] = leafYes
		}
	}
	return c.leafKinds[i] == leafYes
}

// straightLineBody reports whether body is at most inlineLeafMaxBody bytes of local, constant,
// non-trapping numeric, single-memory load and store, drop, select and global.get instructions,
// closed by its one end. The only trap such a body can raise is an out-of-bounds memory access.
func straightLineBody(body []byte) bool {
	if len(body) > inlineLeafMaxBody {
		return false
	}
	for pc := 0; pc < len(body); pc++ {
		op := body[pc]
		var n uint64
		var err error
		switch {
		case op == wasm.OpcodeEnd:
			return pc == len(body)-1
		case op == wasm.OpcodeLocalGet, op == wasm.OpcodeLocalSet, op == wasm.OpcodeLocalTee, op == wasm.OpcodeGlobalGet:
			_, n, err = leb128.LoadUint32(body[pc+1:])
		case op == wasm.OpcodeI32Const:
			_, n, err = leb128.LoadInt32(body[pc+1:])
		case op == wasm.OpcodeI64Const:
			_, n, err = leb128.LoadInt64(body[pc+1:])
		case op == wasm.OpcodeF32Const:
			n = 4
		case op == wasm.OpcodeF64Const:
			n = 8
		case wasm.OpcodeI32Load <= op && op <= wasm.OpcodeI64Store32:
			var align uint32
			if align, n, err = leb128.LoadUint32(body[pc+1:]); err != nil || align&0x40 != 0 {
				return false // a memory index follows: multi-memory.
			}
			var m uint64
			_, m, err = leb128.LoadUint64(body[pc+1+int(n):])
			n += m
		case op == wasm.OpcodeDrop, op == wasm.OpcodeSelect:
		case wasm.OpcodeI32DivS <= op && op <= wasm.OpcodeI32RemU, wasm.OpcodeI64DivS <= op && op <= wasm.OpcodeI64RemU,
			wasm.OpcodeI32TruncF32S <= op && op <= wasm.OpcodeI32TruncF64U, wasm.OpcodeI64TruncF32S <= op && op <= wasm.OpcodeI64TruncF64U:
			// Their traps are raised by the backend, which knows nothing of inlining: see memoryOutOfBoundsCode.
			return false
		case wasm.OpcodeI32Eqz <= op && op <= wasm.OpcodeI64Extend32S:
		default:
			return false
		}
		if err != nil {
			return false
		}
		pc += int(n)
	}
	return false
}

// inlineLeaf lowers a call to the function fnIndex, which isInlinableLeaf accepted, as its body: the
// arguments on the stack become the callee's parameters, and its results are left in their place.
func (c *Compiler) inlineLeaf(fnIndex uint32) {
	i := fnIndex - c.m.ImportFunctionCount
	code := &c.m.CodeSection[i]
	typ := &c.m.TypeSection[c.m.FunctionSection[i]]
	builder := c.ssaBuilder
	state := c.state()

	body, pc, locals := c.wasmFunctionBody, state.pc, c.wasmLocalToVariable
	c.wasmLocalToVariable = c.inlineLocals[:0]
	args := state.values[len(state.values)-len(typ.Params):]
	for j, t := range typ.Params {
		v := builder.DeclareVariable(WasmTypeToSSAType(t))
		builder.DefineVariableInCurrentBB(v, args[j])
		c.setWasmLocalVariable(wasm.Index(j), v)
	}
	state.values = state.values[:len(state.values)-len(typ.Params)]
	for j, t := range code.LocalTypes {
		st := WasmTypeToSSAType(t)
		v := builder.DeclareVariable(st)
		builder.DefineVariableInCurrentBB(v, zeroOf(builder, st))
		c.setWasmLocalVariable(wasm.Index(len(typ.Params)+j), v)
	}

	c.wasmFunctionBody = code.Body
	c.inlinedCallee = i + 1
	for state.pc = 0; state.pc < len(code.Body)-1; {
		c.lowerCurrentOpcode()
	}
	c.inlinedCallee = 0

	c.inlineLocals = c.wasmLocalToVariable
	c.wasmFunctionBody, state.pc, c.wasmLocalToVariable = body, pc, locals
}

func zeroOf(b ssa.Builder, t ssa.Type) ssa.Value {
	instr := b.AllocateInstruction()
	switch t {
	case ssa.TypeI32:
		instr.AsIconst32(0)
	case ssa.TypeI64:
		instr.AsIconst64(0)
	case ssa.TypeF32:
		instr.AsF32const(0)
	case ssa.TypeF64:
		instr.AsF64const(0)
	default:
		instr.AsVconst(0, 0)
	}
	return instr.Insert(b).Return()
}

// memoryOutOfBoundsCode is the exit code of a failed memory bounds check. Inside a body inlineLeaf is
// lowering it carries the callee, so the trap's backtrace can still show the frame a call would have
// had: see callEngine.addInlinedFrame.
func (c *Compiler) memoryOutOfBoundsCode() nativeapi.ExitCode {
	return nativeapi.ExitCodeMemoryOutOfBounds | nativeapi.ExitCode(c.inlinedCallee)<<8
}

// foldInlined simplifies an add, or or mul of a constant 0 or 1 inside a body inlineLeaf is lowering.
// Producers fold those themselves, so nothing else looks for them, but an inlined body meets its
// arguments as constants: compiler-rt's __multi3 multiplies by a high word C code passes as 0.
func (c *Compiler) foldInlined(op wasm.Opcode, x, y ssa.Value) (ssa.Value, bool) {
	if c.inlinedCallee == 0 {
		return ssa.ValueInvalid, false
	}
	isConst := func(v ssa.Value, k uint64) bool {
		instr := c.ssaBuilder.InstructionOfValue(v)
		return instr != nil && instr.Opcode() == ssa.OpcodeIconst && instr.ConstantVal() == k
	}
	switch op {
	case wasm.OpcodeI32Add, wasm.OpcodeI64Add, wasm.OpcodeI32Or, wasm.OpcodeI64Or:
		if isConst(x, 0) {
			return y, true
		}
		if isConst(y, 0) {
			return x, true
		}
	case wasm.OpcodeI32Mul, wasm.OpcodeI64Mul:
		switch {
		case isConst(x, 0):
			return x, true
		case isConst(y, 0):
			return y, true
		case isConst(x, 1):
			return y, true
		case isConst(y, 1):
			return x, true
		}
	}
	return ssa.ValueInvalid, false
}

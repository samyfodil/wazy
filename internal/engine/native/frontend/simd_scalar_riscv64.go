package frontend

import (
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/wasm"
)

// emulateSIMD reports whether the vector opcodes are lowered to scalar pairs.
//
// A property of the machine, not of a configuration: the same answer for every
// module in the process, and a module compiled one way must never be served from
// cache to a runtime expecting the other.
//
// A var here and a constant false everywhere else, so that on every other
// architecture the scalar lowering compiles out entirely rather than becoming a
// branch those targets have to carry.
var emulateSIMD = platform.RiscV64EmulatesSIMD()

// fillStoreBytes is the store width memory.fill's loops are built around.
//
// memory.fill is bulk-memory, not SIMD -- a module that never enables the SIMD
// feature reaches that lowering -- but a 16-byte store still needs a vector unit,
// and on riscv64 that unit is the optional V extension. Where it is absent the
// wide form compiled to instructions the CPU traps on, so the width narrows to a
// 64-bit word. Nowhere else can this happen: amd64 has no compiler at all without
// SSE4.1, and arm64 always has NEON, so both keep the constant.
var fillStoreBytes = func() uint32 {
	if platform.RiscV64EmulatesSIMD() {
		return 8
	}
	return 16
}()

// memoryFillMainLoopBytes is how much the main loop writes per iteration: four
// stores. Fills shorter than this skip it entirely.
var memoryFillMainLoopBytes = 4 * fillStoreBytes

// simdEmulated is the ledger of vector opcodes lowered without a vector unit.
//
// Consulted before the switch in lower.go runs, so an opcode that has no scalar
// lowering yet refuses the module loudly instead of falling through to a path that
// would emit vector instructions the CPU cannot execute. Membership here is the
// coverage number.
var simdEmulated = map[wasm.OpcodeVec]bool{
	wasm.OpcodeVecV128Const:  true,
	wasm.OpcodeVecV128Load:   true,
	wasm.OpcodeVecV128Store:  true,
	wasm.OpcodeVecV128Not:    true,
	wasm.OpcodeVecV128And:    true,
	wasm.OpcodeVecV128Or:     true,
	wasm.OpcodeVecV128Xor:    true,
	wasm.OpcodeVecV128AndNot: true,
	wasm.OpcodeVecI64x2Add:   true,
	wasm.OpcodeVecI64x2Sub:   true,
}

// requireEmulatedVecOp refuses a module whose vector opcode has no scalar lowering
// yet, before that opcode is lowered.
//
// The alternative is emitting a vector instruction the CPU cannot execute, which
// is an illegal instruction at run time with no stack trace worth reading. The
// engine's compile path recovers the panic into an error naming the module, so the
// module is rejected and the host survives; a partial ledger can therefore never
// become a SIGILL.
func (c *Compiler) requireEmulatedVecOp(vecOp wasm.OpcodeVec, unreachable bool) {
	if emulateSIMD && !unreachable && !simdEmulated[vecOp] {
		panic("TODO: no scalar lowering yet for " + wasm.VectorInstructionName(vecOp) +
			" on a CPU without a vector unit")
	}
}

// withSIMDEmulation forces the mode and returns a function restoring it, so a test
// can pin which lowering it is describing instead of inheriting the host's CPU.
func withSIMDEmulation(on bool) func() {
	prev := emulateSIMD
	emulateSIMD = on
	return func() { emulateSIMD = prev }
}

//go:build !riscv64

package frontend

import "github.com/samyfodil/wazy/internal/wasm"

// Only riscv64 can have a compiler and no vector unit at the same time: amd64 is
// not offered one without SSE4.1, and arm64 always has NEON. So the scalar v128
// lowering is a compile-time impossibility here, and saying so as a constant is
// what keeps it out of these targets entirely -- every `if emulateSIMD` in the
// vector lowering folds away rather than becoming a branch and a load they would
// carry for an architecture they are not.
const emulateSIMD = false

// The widths memory.fill's loops are built around, as they were before riscv64:
// constants, so the loop bounds fold.
const (
	fillStoreBytes          uint32 = 16
	memoryFillMainLoopBytes        = 4 * fillStoreBytes
)

// requireEmulatedVecOp has nothing to refuse: every vector opcode lowers to a
// vector instruction here, and the machine can execute it. Empty, so it inlines to
// nothing -- the ledger it consults on riscv64 does not exist on this build.
func (c *Compiler) requireEmulatedVecOp(wasm.OpcodeVec, bool) {}

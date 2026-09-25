package frontend

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestSIMDEmulatedLedgerNamesRealOpcodes keeps the ledger from claiming coverage
// it does not have: a mistyped constant would sit in the map suppressing the
// refusal for an opcode that has no scalar lowering, which is the one failure mode
// that turns into a SIGILL instead of an error.
func TestSIMDEmulatedLedgerNamesRealOpcodes(t *testing.T) {
	require.False(t, len(simdEmulated) == 0, "no vector opcode claims a scalar lowering")
	for op := range simdEmulated {
		name := wasm.VectorInstructionName(op)
		require.False(t, name == "", "opcode %#x is in the ledger but has no name", op)
	}
	t.Logf("scalar lowerings: %d of the vector opcodes", len(simdEmulated))
}

// pinVectorLowering forces the vector lowering for a test whose expectations
// describe it, instead of letting the host's CPU decide: on a riscv64 without the
// vector extension the frontend lowers v128 to scalar pairs, and goldens written
// against vector instructions would describe the wrong thing.
func pinVectorLowering() func() { return withSIMDEmulation(false) }

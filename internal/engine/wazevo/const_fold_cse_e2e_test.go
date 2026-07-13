package wazevo_test

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// redundantTrapChecksWasm is a hand-authored module (compiled from wat) that exercises the
// exitIfTrueGuardedIcmps guard through the *entire* compiler pipeline, not just the SSA pass.
//
//	(module
//	  (memory 1)
//	  (func (export "redun") (param i32 i32) (result i32)
//	    (i32.add
//	      (i32.load offset=0 (i32.add (local.get 0) (local.get 1)))
//	      (i32.load offset=0 (i32.add (local.get 0) (local.get 1)))))
//	  (func (export "divc") (param i32) (result i32)
//	    (i32.div_u (local.get 0) (i32.const 7))))
//
// "redun" recomputes the same address expression twice in one block. The frontend does NOT
// coalesce the two loads (its known-safe-bound elision is keyed on the base-address value ID,
// which differs between the two distinct Iadd defs), so two structurally identical
// memory-out-of-bounds trap checks reach the pass. Local CSE would merge the duplicated
// Iadd/UExtend/ceil and then the two trap Icmps, pushing the survivor's RefCount to 2 -- which
// makes the SECOND ExitIfTrueWithCode's lowerExitIfTrueWithCode fusion (MatchInstr requires
// RefCount < 2, no fallback) hard-panic. "divc" divides by a constant, so its div-by-zero trap
// Icmp has a constant operand that constant folding would collapse to an Iconst, breaking the
// same fusion a different way. Both must compile and run correctly.
var redundantTrapChecksWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x0c, 0x02, 0x60,
	0x02, 0x7f, 0x7f, 0x01, 0x7f, 0x60, 0x01, 0x7f, 0x01, 0x7f, 0x03, 0x03,
	0x02, 0x00, 0x01, 0x05, 0x03, 0x01, 0x00, 0x01, 0x07, 0x10, 0x02, 0x05,
	0x72, 0x65, 0x64, 0x75, 0x6e, 0x00, 0x00, 0x04, 0x64, 0x69, 0x76, 0x63,
	0x00, 0x01, 0x0a, 0x1d, 0x02, 0x13, 0x00, 0x20, 0x00, 0x20, 0x01, 0x6a,
	0x28, 0x02, 0x00, 0x20, 0x00, 0x20, 0x01, 0x6a, 0x28, 0x02, 0x00, 0x6a,
	0x0b, 0x07, 0x00, 0x20, 0x00, 0x41, 0x07, 0x6e, 0x0b,
}

// TestConstFoldCSE_TrapCheckFusionE2E is the end-to-end regression for the passConstFoldAndCSE
// backend-fusion surface: it compiles and runs the module above and asserts both correct results
// and, implicitly, that the backend does not panic while fusing the trap-check Icmps. Without the
// guard, CompileModule panics on "redun" (RefCount==2 after CSE) and/or "divc" (folded Icmp).
func TestConstFoldCSE_TrapCheckFusionE2E(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)

	compiled, err := r.CompileModule(ctx, redundantTrapChecksWasm)
	require.NoError(t, err)

	mod, err := r.InstantiateModule(ctx, compiled, wazy.NewModuleConfig().WithName("m"))
	require.NoError(t, err)

	// Write a known 32-bit value at memory offset (base=8, no local offset) so both loads read it.
	mem := mod.Memory()
	require.True(t, mem.WriteUint32Le(8, 0x11223344))

	// "redun": base(=4) + offset(=4) => address 8, loaded twice and summed.
	redun := mod.ExportedFunction("redun")
	res, err := redun.Call(ctx, 4, 4)
	require.NoError(t, err)
	require.Equal(t, uint64(uint32(0x11223344)*2), res[0])

	// A genuinely out-of-bounds address must still trap (the guard preserves the check, it does
	// not delete it).
	_, err = redun.Call(ctx, 0x7fffffff, 0x7fffffff)
	require.Error(t, err)

	// "divc": normal division by the constant 7, and the div-by-zero path unaffected.
	divc := mod.ExportedFunction("divc")
	res, err = divc.Call(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, uint64(100/7), res[0])
}

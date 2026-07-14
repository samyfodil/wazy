package native_test

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// c27TrapWasm exercises C27 (dedup of per-site trap-island exec-context saves)
// end-to-end. sum4 does four dynamic loads in one straight-line block, so the
// four bounds-check sites share a machine block: C27 keeps the first ctx-save and
// removes the other three. The correctness risk is that a trap at one of the
// *removed* sites must still recover the exec-context (from the surviving save's
// slot) and report memory_out_of_bounds — not a stale/garbage trap or a crash.
//
//	(module (memory 1)
//	  (func (export "sum4") (param $p i32) (result i32)
//	    load[p] + load[p+4] + load[p+8] + load[p+12]))
var c27TrapWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x06, 0x01, 0x60,
	0x01, 0x7f, 0x01, 0x7f, 0x03, 0x02, 0x01, 0x00, 0x05, 0x03, 0x01, 0x00,
	0x01, 0x07, 0x08, 0x01, 0x04, 0x73, 0x75, 0x6d, 0x34, 0x00, 0x00, 0x0a,
	0x24, 0x01, 0x22, 0x00, 0x20, 0x00, 0x28, 0x02, 0x00, 0x20, 0x00, 0x41,
	0x04, 0x6a, 0x28, 0x02, 0x00, 0x6a, 0x20, 0x00, 0x41, 0x08, 0x6a, 0x28,
	0x02, 0x00, 0x20, 0x00, 0x41, 0x0c, 0x6a, 0x28, 0x02, 0x00, 0x6a, 0x6a,
	0x0b,
}

func TestC27TrapAfterDedupE2E(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)

	compiled, err := r.CompileModule(ctx, c27TrapWasm)
	require.NoError(t, err)
	mod, err := r.InstantiateModule(ctx, compiled, wazy.NewModuleConfig().WithName("m"))
	require.NoError(t, err)

	mem := mod.Memory()
	for i := uint32(0); i < 0x100; i += 4 {
		require.True(t, mem.WriteUint32Le(0xFF00+i, i)) // 0xFF00..0xFFFC valid
	}
	sum4 := mod.ExportedFunction("sum4")

	// In-bounds: p=0xFF00 -> loads at 0xFF00,0xFF04,0xFF08,0xFF0C = 0+4+8+12 = 24.
	res, err := sum4.Call(ctx, 0xFF00)
	require.NoError(t, err)
	require.Equal(t, uint64(24), res[0])

	// p=0xFFFC: first load (0xFFFC, the KEPT save) is in-bounds; the second load
	// (0x10000) is out of bounds and its ctx-save was DEDUP'd. The trap must still
	// fire correctly via the shared island reading the surviving save's slot.
	_, err = sum4.Call(ctx, 0xFFFC)
	require.Error(t, err)
	require.Contains(t, err.Error(), "out of bounds memory access")
}

package native_test

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// c27TrapWasm exercises shared trap-island exec-context recovery end-to-end.
// sum4 does four dynamic loads in one straight-line block, so four bounds-check
// sites share a machine block. On amd64 the exec-context the island needs is
// written ONCE to the reserved ctx slot in the prologue (see needsCtxSlot /
// setupPrologue), not re-saved at each site. The correctness risk is that a trap
// at any of these sites — here the second load — must recover the exec-context
// from the reserved slot and report memory_out_of_bounds, not a stale/garbage
// trap or a crash.
//
// History: this originally guarded the C27 per-site-save dedup pass; that pass
// (and its per-site saves) was replaced by the reserved-slot scheme, so the
// dedup miscompile class it protected against no longer exists by construction.
// The test is kept as a regression guard for the reserved-slot recovery path.
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

func TestSharedTrapIslandCtxRecoveryE2E(t *testing.T) {
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

	// p=0xFFFC: first load (0xFFFC) is in-bounds; the second load (0x10000) is
	// out of bounds. The trap must fire correctly via the shared island reading
	// execCtx from the reserved prologue slot.
	_, err = sum4.Call(ctx, 0xFFFC)
	require.Error(t, err)
	require.Contains(t, err.Error(), "out of bounds memory access")
}

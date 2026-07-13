package native_test

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// maskedBoundsWasm exercises range-based bounds-check elision (B) end-to-end:
//
//	(module
//	  (memory 1)
//	  (func (export "masked")  (param i32) (result i32)
//	    (i32.load (i32.and (local.get 0) (i32.const 0xff))))      ;; <=255 < 64KiB => elided
//	  (func (export "bigmask") (param i32) (result i32)
//	    (i32.load (i32.and (local.get 0) (i32.const 0x3ffff)))))  ;; up to 256KiB => NOT elided
//
// "masked" is provably in-bounds (mask 0xff + 4 <= the 1-page minimum) so the
// frontend drops its bounds check; it must still load the correct value.
// "bigmask" can address past the minimum, so the check is kept and a genuinely
// out-of-bounds masked address must still trap.
var maskedBoundsWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x06, 0x01, 0x60,
	0x01, 0x7f, 0x01, 0x7f, 0x03, 0x03, 0x02, 0x00, 0x00, 0x05, 0x03, 0x01,
	0x00, 0x01, 0x07, 0x14, 0x02, 0x06, 0x6d, 0x61, 0x73, 0x6b, 0x65, 0x64,
	0x00, 0x00, 0x07, 0x62, 0x69, 0x67, 0x6d, 0x61, 0x73, 0x6b, 0x00, 0x01,
	0x0a, 0x1a, 0x02, 0x0b, 0x00, 0x20, 0x00, 0x41, 0xff, 0x01, 0x71, 0x28,
	0x02, 0x00, 0x0b, 0x0c, 0x00, 0x20, 0x00, 0x41, 0xff, 0xff, 0x0f, 0x71,
	0x28, 0x02, 0x00, 0x0b,
}

func TestMaskedBoundsCheckElisionE2E(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)

	compiled, err := r.CompileModule(ctx, maskedBoundsWasm)
	require.NoError(t, err)
	mod, err := r.InstantiateModule(ctx, compiled, wazy.NewModuleConfig().WithName("m"))
	require.NoError(t, err)

	mem := mod.Memory()
	require.True(t, mem.WriteUint32Le(0x40, 0xcafef00d))

	// masked: 0x40 & 0xff == 0x40; the bounds check is elided but the load must
	// still read the value written at address 0x40.
	masked := mod.ExportedFunction("masked")
	res, err := masked.Call(ctx, 0x40)
	require.NoError(t, err)
	require.Equal(t, uint64(0xcafef00d), res[0])

	// bigmask: an address whose masked value lands past the 1-page minimum must
	// still trap (the check is NOT elided for this mask).
	bigmask := mod.ExportedFunction("bigmask")
	_, err = bigmask.Call(ctx, 0x3fffc) // & 0x3ffff == 0x3fffc, well past 64KiB
	require.Error(t, err)

	// bigmask within bounds still works.
	res, err = bigmask.Call(ctx, 0x40)
	require.NoError(t, err)
	require.Equal(t, uint64(0xcafef00d), res[0])
}

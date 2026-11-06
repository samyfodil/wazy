package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/atomic_trap_order.wasm
var atomicTrapOrderWasm []byte

//go:embed testdata/atomic_trap_order64.wasm
var atomicTrapOrder64Wasm []byte

// TestAtomicTrapOrder checks that the compiler traps as the interpreter does on atomic accesses in
// bounds or not, aligned or not: misaligned first, then out of bounds.
func TestAtomicTrapOrder(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	for _, tc := range []struct {
		bin   []byte
		fns   []string
		addrs []uint64
	}{
		{
			bin: atomicTrapOrderWasm, fns: []string{"load", "load64", "rmw", "store16", "notify", "wait"},
			addrs: []uint64{0, 1, 2, 4, 65528, 65531, 65532, 65533, 65535, 65536, 65537, 0xfffffffd},
		},
		{
			bin: atomicTrapOrder64Wasm, fns: []string{"load", "rmw"},
			addrs: []uint64{0, 1, 2, 5, 65530, 65531, 1 << 40, ^uint64(0) - 8, ^uint64(0) - 5, ^uint64(0) - 2, ^uint64(0)},
		},
	} {
		mods := map[bool]api.Module{}
		for _, interp := range []bool{false, true} {
			config := wazy.NewRuntimeConfigCompiler()
			if interp {
				config = wazy.NewRuntimeConfigInterpreter()
			}
			r := wazy.NewRuntimeWithConfig(ctx, config.WithCoreFeatures(api.CoreFeaturesV3|experimental.CoreFeaturesThreads))
			defer r.Close(ctx)
			m, err := r.Instantiate(ctx, tc.bin)
			require.NoError(t, err)
			mods[interp] = m
		}
		for _, fn := range tc.fns {
			for _, p := range tc.addrs {
				var errs [2]string
				for i, interp := range []bool{false, true} {
					if _, err := mods[interp].ExportedFunction(fn).Call(ctx, p); err != nil {
						errs[i] = err.Error()
					}
				}
				require.Equal(t, errs[1], errs[0], "%s(%d)", fn, p)
			}
		}
	}
}

package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
	binaryformat "github.com/samyfodil/wazy/internal/wasm/binary"
)

var (
	//go:embed testdata/branch_hints.wasm
	branchHintsWasm []byte
	//go:embed testdata/branch_hints_inverted.wasm
	branchHintsInvertedWasm []byte
)

// TestBranchHints runs functions whose if and br_if instructions carry branch hints, once with the hints right
// and once with every one of them wrong. Hints move blocks around, so each layout has to compute what the
// unhinted function does.
func TestBranchHints(t *testing.T) {
	for _, bin := range [][]byte{branchHintsWasm, branchHintsInvertedWasm} {
		m, err := binaryformat.DecodeModule(bin, api.CoreFeaturesV2, wasm.MemoryLimitPages, false, 0, false, false)
		require.NoError(t, err)
		hints := 0
		for i := range m.CodeSection {
			hints += len(m.CodeSection[i].BranchHints)
		}
		require.Equal(t, 7, hints, "every hint should land on its if or br_if")
	}

	sum := func(n uint32) uint32 {
		var i, s uint32
		for i < n {
			i++
			if s += i; s > 1000 {
				break
			}
		}
		return s
	}
	rareloop := func(n uint32) uint32 {
		var c uint32
		for n > 100 {
			c, n = c+1, n-1
		}
		return n + c*1000
	}
	rejoin := func(x uint32) uint32 {
		switch {
		case x&3 == 3:
			return x
		case x&1 == 1:
			return x + 107
		default:
			return x + 102
		}
	}

	configs := map[string]wazy.RuntimeConfig{"interpreter": wazy.NewRuntimeConfigInterpreter()}
	if platform.CompilerSupported() {
		configs["compiler"] = wazy.NewRuntimeConfigCompiler()
	}
	for name, config := range configs {
		for variant, bin := range map[string][]byte{"right": branchHintsWasm, "wrong": branchHintsInvertedWasm} {
			t.Run(name+"/"+variant, func(t *testing.T) {
				ctx := context.Background()
				r := wazy.NewRuntimeWithConfig(ctx, config)
				defer r.Close(ctx)
				mod, err := r.Instantiate(ctx, bin)
				require.NoError(t, err)

				call := func(fn string, x uint32) uint32 {
					res, err := mod.ExportedFunction(fn).Call(ctx, uint64(x))
					require.NoError(t, err)
					return uint32(res[0])
				}
				for _, x := range []uint32{0, 1, 2, 3, 7, 44, 45, 99, 100, 101, 150, 1000, 1 << 20} {
					require.Equal(t, map[bool]uint32{true: 10, false: 20}[x != 0], call("ifelse", x), "ifelse(%d)", x)
					require.Equal(t, map[bool]uint32{true: 6, false: 1}[x != 0], call("ifnoelse", x), "ifnoelse(%d)", x)
					require.Equal(t, sum(x), call("sum", x), "sum(%d)", x)
					require.Equal(t, rareloop(x), call("rareloop", x), "rareloop(%d)", x)
					require.Equal(t, rejoin(x), call("rejoin", x), "rejoin(%d)", x)
				}
			})
		}
	}
}

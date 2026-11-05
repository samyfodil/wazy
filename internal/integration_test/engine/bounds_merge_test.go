package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/bounds_merge.wasm
var boundsMergeWasm []byte

// TestBoundsMerge runs loads from two places in a block, whose bounds checks arm64 makes one, against the
// interpreter, with either address or both out of bounds, and compares the memory a trap leaves.
func TestBoundsMerge(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	addrs := []uint32{0, 4, 8, 65528, 65531, 65532, 65533, 65535, 65536, 0x7fffffff, 0xfffffff0, 0xffffffff}
	for _, fn := range []string{"two", "computed", "chase", "store", "four", "long", "shared"} {
		for _, a := range addrs {
			for _, b := range addrs {
				var results [2][]uint64
				var errs [2]error
				var mems [2][]byte
				for i, interp := range []bool{false, true} {
					config := wazy.NewRuntimeConfigCompiler()
					if interp {
						config = wazy.NewRuntimeConfigInterpreter()
					}
					r := wazy.NewRuntimeWithConfig(ctx, config)
					m, err := r.Instantiate(ctx, boundsMergeWasm)
					require.NoError(t, err)
					results[i], errs[i] = m.ExportedFunction(fn).Call(ctx, api.EncodeU32(a), api.EncodeU32(b))
					mem, _ := m.Memory().Read(0, 32)
					mems[i] = append([]byte(nil), mem...)
					require.NoError(t, r.Close(ctx))
				}
				require.Equal(t, errs[1] == nil, errs[0] == nil, "%s(%#x, %#x): %v vs %v", fn, a, b, errs[0], errs[1])
				if errs[1] != nil {
					require.Equal(t, errs[1].Error(), errs[0].Error(), "%s(%#x, %#x)", fn, a, b)
				}
				require.Equal(t, results[1], results[0], "%s(%#x, %#x)", fn, a, b)
				require.Equal(t, mems[1], mems[0], "%s(%#x, %#x) memory", fn, a, b)
			}
		}
	}
}

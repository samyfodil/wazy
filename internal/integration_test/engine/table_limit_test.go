package adhoc

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

var (
	//go:embed testdata/table_limit.wasm
	tableLimitWasm []byte
	//go:embed testdata/table_limit_min.wasm
	tableLimitMinWasm []byte
)

// TestTableLimitElements checks WithTableLimitElements end to end on each engine: table.grow stops at the limit
// and reports failure as -1, and a module whose declared tables add up to more than the limit does not
// instantiate.
func TestTableLimitElements(t *testing.T) {
	configs := map[string]wazy.RuntimeConfig{"interpreter": wazy.NewRuntimeConfigInterpreter()}
	if platform.CompilerSupported() {
		configs["compiler"] = wazy.NewRuntimeConfigCompiler()
	}
	for name, config := range configs {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			t.Run("table.grow stops at the limit", func(t *testing.T) {
				r := wazy.NewRuntimeWithConfig(ctx, config.WithTableLimitElements(5))
				defer r.Close(ctx)
				mod, err := r.Instantiate(ctx, tableLimitWasm)
				require.NoError(t, err)

				grow, size := mod.ExportedFunction("grow"), mod.ExportedFunction("size")
				res, err := grow.Call(ctx, 5)
				require.NoError(t, err)
				require.Equal(t, uint64(0), res[0])
				res, err = grow.Call(ctx, 1)
				require.NoError(t, err)
				require.Equal(t, uint64(0xffff_ffff), res[0])
				res, err = size.Call(ctx)
				require.NoError(t, err)
				require.Equal(t, uint64(5), res[0])
			})

			t.Run("table.grow stops at the default limit", func(t *testing.T) {
				r := wazy.NewRuntimeWithConfig(ctx, config)
				defer r.Close(ctx)
				mod, err := r.Instantiate(ctx, tableLimitWasm)
				require.NoError(t, err)

				res, err := mod.ExportedFunction("grow").Call(ctx, 10_000_001)
				require.NoError(t, err)
				require.Equal(t, uint64(0xffff_ffff), res[0])
			})

			t.Run("declared tables over the limit", func(t *testing.T) {
				r := wazy.NewRuntimeWithConfig(ctx, config.WithTableLimitElements(11))
				defer r.Close(ctx)
				_, err := r.Instantiate(ctx, tableLimitMinWasm)
				require.EqualError(t, err, "total table minimum across 2 tables (12 entries) exceeds 11")
			})

			t.Run("declared tables within the limit", func(t *testing.T) {
				r := wazy.NewRuntimeWithConfig(ctx, config.WithTableLimitElements(12))
				defer r.Close(ctx)
				_, err := r.Instantiate(ctx, tableLimitMinWasm)
				require.NoError(t, err)
			})
		})
	}
}

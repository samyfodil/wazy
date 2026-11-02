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
	//go:embed testdata/exported_table.wasm
	exportedTableWasm []byte
	//go:embed testdata/exported_table_reexport.wasm
	exportedTableReexportWasm []byte
)

// TestExportedTable checks api.Module.ExportedTable against the guest's own view of the same tables: what the host
// writes the guest reads, what the guest writes the host reads, and a table the host grows is the size the guest
// sees, on each engine.
func TestExportedTable(t *testing.T) {
	configs := map[string]wazy.RuntimeConfig{"interpreter": wazy.NewRuntimeConfigInterpreter()}
	if platform.CompilerSupported() {
		configs["compiler"] = wazy.NewRuntimeConfigCompiler()
	}
	for name, config := range configs {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntimeWithConfig(ctx, config)
			defer r.Close(ctx)
			mod, err := r.InstantiateWithConfig(ctx, exportedTableWasm, wazy.NewModuleConfig().WithName("tables"))
			require.NoError(t, err)
			get, set, size, call := mod.ExportedFunction("get"), mod.ExportedFunction("set"),
				mod.ExportedFunction("size"), mod.ExportedFunction("call")

			require.Nil(t, mod.ExportedTable("missing"))
			require.Nil(t, mod.ExportedTable("get"))

			externs := mod.ExportedTable("externs")
			require.Equal(t, uint32(2), externs.Size())

			t.Run("the host writes, the guest reads", func(t *testing.T) {
				const ref = uint64(0xdead_beef)
				require.True(t, externs.Set(1, ref))
				res, err := get.Call(ctx, 1)
				require.NoError(t, err)
				require.Equal(t, ref, res[0])
				require.False(t, externs.Set(2, ref))
			})

			t.Run("the guest writes, the host reads", func(t *testing.T) {
				_, err := set.Call(ctx, 0, 42)
				require.NoError(t, err)
				v, ok := externs.Get(0)
				require.True(t, ok)
				require.Equal(t, uint64(42), v)
				_, ok = externs.Get(2)
				require.False(t, ok)
			})

			t.Run("the host grows, the guest sees it", func(t *testing.T) {
				prev, ok := externs.Grow(3, 7)
				require.True(t, ok)
				require.Equal(t, uint32(2), prev)
				res, err := size.Call(ctx)
				require.NoError(t, err)
				require.Equal(t, uint64(5), res[0])
				res, err = get.Call(ctx, 4)
				require.NoError(t, err)
				require.Equal(t, uint64(7), res[0])

				// The declared maximum is 10.
				_, ok = externs.Grow(6, 0)
				require.False(t, ok)
				require.Equal(t, uint32(5), externs.Size())
			})

			t.Run("a re-exported table is the same table", func(t *testing.T) {
				again, err := r.Instantiate(ctx, exportedTableReexportWasm)
				require.NoError(t, err)
				table := again.ExportedTable("again")
				require.Equal(t, externs.Size(), table.Size())
				require.True(t, table.Set(3, 99))
				v, ok := externs.Get(3)
				require.True(t, ok)
				require.Equal(t, uint64(99), v)
			})

			t.Run("a function table", func(t *testing.T) {
				funcs := mod.ExportedTable("funcs")
				fn := funcs.Function(0)
				require.NotNil(t, fn)
				res, err := fn.Call(ctx, 2, 3)
				require.NoError(t, err)
				require.Equal(t, uint64(5), res[0])

				require.Nil(t, funcs.Function(1)) // null
				require.Nil(t, funcs.Function(2)) // out of range
				require.Nil(t, externs.Function(0))

				// Function references are not read or written as values.
				_, ok := funcs.Get(0)
				require.False(t, ok)
				require.False(t, funcs.Set(1, 1))
				_, ok = funcs.Grow(1, 1)
				require.False(t, ok)

				prev, ok := funcs.Grow(1, 0)
				require.True(t, ok)
				require.Equal(t, uint32(2), prev)
				res, err = call.Call(ctx, 0, 4, 5)
				require.NoError(t, err)
				require.Equal(t, uint64(9), res[0])
			})
		})
	}
}

package wazy

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

func identityConfigs(t *testing.T) map[string]RuntimeConfig {
	configs := map[string]RuntimeConfig{"interpreter": NewRuntimeConfigInterpreter()}
	if platform.CompilerSupported() {
		configs["compiler"] = NewRuntimeConfigCompiler()
	}
	for name, c := range configs {
		configs[name] = c.WithCoreFeatures(api.CoreFeaturesV3)
	}
	return configs
}

// definer exports f, a function of type () -> (), and r, which returns ref.func f.
var definer = []byte{
	0, 'a', 's', 'm', 1, 0, 0, 0,
	1, 8, 2, 0x60, 0, 0, 0x60, 0, 1, 0x70, // types: () -> (), () -> funcref
	3, 3, 2, 0, 1, // functions
	7, 9, 2, 1, 'f', 0, 0, 1, 'r', 0, 1, // exports f and r
	10, 9, 2, 2, 0, 0x0b, 4, 0, 0xd2, 0, 0x0b, // f: nothing; r: ref.func 0
}

// importer imports f from "definer", re-exports it as f2, and exports r, which returns ref.func of the import.
var importer = []byte{
	0, 'a', 's', 'm', 1, 0, 0, 0,
	1, 8, 2, 0x60, 0, 0, 0x60, 0, 1, 0x70,
	2, 13, 1, 7, 'd', 'e', 'f', 'i', 'n', 'e', 'r', 1, 'f', 0, 0, // import definer.f
	3, 2, 1, 1,
	7, 10, 2, 2, 'f', '2', 0, 0, 1, 'r', 0, 1,
	10, 6, 1, 4, 0, 0xd2, 0, 0x0b, // r: ref.func 0 (the import)
}

// A function has one funcref however many modules import it: ref.func of an import gives the reference
// the defining module gives, so a host comparing references sees the same function.
func TestFuncref_importSharesTheDefinersReference(t *testing.T) {
	ctx := context.Background()
	for name, config := range identityConfigs(t) {
		t.Run(name, func(t *testing.T) {
			r := NewRuntimeWithConfig(ctx, config)
			defer r.Close(ctx)
			def, err := r.InstantiateWithConfig(ctx, definer, NewModuleConfig().WithName("definer"))
			require.NoError(t, err)
			imp, err := r.InstantiateWithConfig(ctx, importer, NewModuleConfig().WithName("importer"))
			require.NoError(t, err)
			want, err := def.ExportedFunction("r").Call(ctx)
			require.NoError(t, err)
			got, err := imp.ExportedFunction("r").Call(ctx)
			require.NoError(t, err)
			require.NotEqual(t, uint64(0), want[0])
			require.Equal(t, want[0], got[0])
		})
	}
}

// externGlobal exports g, an immutable externref global initialized to extern.convert_any (ref.i31 5).
var externGlobal = []byte{
	0, 'a', 's', 'm', 1, 0, 0, 0,
	6, 10, 1, 0x6f, 0, 0x41, 5, 0xfb, 28, 0xfb, 27, 0x0b,
	7, 5, 1, 1, 'g', 3, 0,
}

// extern.convert_any in a constant expression keeps its operand: only the static type changes, so the
// global holds the i31 made external, not null.
func TestConstExpr_externConvertAnyKeepsTheValue(t *testing.T) {
	ctx := context.Background()
	for name, config := range identityConfigs(t) {
		t.Run(name, func(t *testing.T) {
			r := NewRuntimeWithConfig(ctx, config)
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, externGlobal)
			require.NoError(t, err)
			g := mod.ExportedGlobal("g").Get()
			require.NotEqual(t, uint64(0), g)
		})
	}
}

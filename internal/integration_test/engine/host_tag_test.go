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

var (
	//go:embed testdata/host_tag_thrower.wasm
	hostTagThrowerWasm []byte
	//go:embed testdata/host_tag_catcher.wasm
	hostTagCatcherWasm []byte
	//go:embed testdata/host_tag_mismatch.wasm
	hostTagMismatchWasm []byte
)

// TestHostTag checks a tag defined by a host module: one guest throws it and another catches it, which only
// works if both imports resolve to the same tag, and an import declaring another type does not link.
func TestHostTag(t *testing.T) {
	configs := map[string]wazy.RuntimeConfig{"interpreter": wazy.NewRuntimeConfigInterpreter()}
	if platform.CompilerSupported() {
		configs["compiler"] = wazy.NewRuntimeConfigCompiler()
	}
	for name, config := range configs {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntimeWithConfig(ctx, config.WithCoreFeatures(api.CoreFeaturesV3))
			defer r.Close(ctx)

			_, err := wazy.HostProc0(r.NewHostModuleBuilder("env").NewFunctionBuilder(), func(context.Context, api.Module) {}).
				Export("unused").
				ExportTag("__cpp_exception", api.ValueTypeI32).
				Instantiate(ctx)
			require.NoError(t, err)

			_, err = r.InstantiateWithConfig(ctx, hostTagThrowerWasm, wazy.NewModuleConfig().WithName("thrower"))
			require.NoError(t, err)
			catcher, err := r.Instantiate(ctx, hostTagCatcherWasm)
			require.NoError(t, err)

			res, err := catcher.ExportedFunction("catch").Call(ctx, 42)
			require.NoError(t, err)
			require.Equal(t, uint64(42), res[0])

			_, err = r.Instantiate(ctx, hostTagMismatchWasm)
			require.Error(t, err)
			require.Contains(t, err.Error(), "tag type mismatch")
		})
	}
}

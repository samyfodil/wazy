package experimental_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

func TestImportResolver(t *testing.T) {
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	for i := 0; i < 5; i++ {
		var callCount int
		start := func(ctx context.Context) {
			callCount++
		}
		modImport, err := r.NewHostModuleBuilder(fmt.Sprintf("env%d", i)).
			NewFunctionBuilder().WithGoFunction(api.GoFunc(func(ctx context.Context, _ []uint64) { start(ctx) }), nil, nil).Export("start").
			Compile(ctx)
		require.NoError(t, err)
		// Anonymous module, it will be resolved by the import resolver.
		instanceImport, err := r.InstantiateModule(ctx, modImport, wazy.NewModuleConfig().WithName(""))
		require.NoError(t, err)

		resolveImport := func(name string) api.Module {
			if name == "env" {
				return instanceImport
			}
			return nil
		}

		// Set the import resolver in the context.
		ctx = experimental.WithImportResolver(context.Background(), resolveImport)

		one := uint32(1)
		binary := binaryencoding.EncodeModule(&wasm.Module{
			TypeSection:     []wasm.FunctionType{{}},
			ImportSection:   []wasm.Import{{Module: "env", Name: "start", Type: wasm.ExternTypeFunc, DescFunc: 0}},
			FunctionSection: []wasm.Index{0},
			CodeSection: []wasm.Code{
				{Body: []byte{wasm.OpcodeCall, 0, wasm.OpcodeEnd}}, // Call the imported env.start.
			},
			StartSection: &one,
		})

		modMain, err := r.CompileModule(ctx, binary)
		require.NoError(t, err)

		_, err = r.InstantiateModule(ctx, modMain, wazy.NewModuleConfig())
		require.NoError(t, err)
		require.Equal(t, 1, callCount)
	}
}

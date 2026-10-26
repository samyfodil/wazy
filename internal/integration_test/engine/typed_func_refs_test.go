package adhoc

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
	"github.com/samyfodil/wazy/internal/wasmruntime"
)

func typedFuncRefsConfigs() []struct {
	name   string
	config wazy.RuntimeConfig
} {
	configs := []struct {
		name   string
		config wazy.RuntimeConfig
	}{
		{"interpreter", wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(
			api.CoreFeaturesV2 | api.CoreFeatureTypedFunctionReferences,
		)},
	}
	if platform.CompilerSupported() {
		configs = append(configs, struct {
			name   string
			config wazy.RuntimeConfig
		}{"compiler", wazy.NewRuntimeConfigCompiler().WithCoreFeatures(
			api.CoreFeaturesV2 | api.CoreFeatureTypedFunctionReferences,
		)})
	}
	return configs
}

// TestCallRefTrapOnNull verifies that call_ref on a null reference traps
// with ErrRuntimeNullReference.
func TestCallRefTrapOnNull(t *testing.T) {
	// (module
	//   (type $t (func (result i32)))
	//   (func (export "test") (result i32)
	//     ref.null $t
	//     call_ref $t
	//   )
	// )
	mod := &wasm.Module{
		TypeSection: []wasm.FunctionType{
			{Results: []wasm.ValueType{wasm.ValueTypeI32}, ResultNumInUint64: 1},
		},
		FunctionSection: []wasm.Index{0},
		CodeSection: []wasm.Code{
			{Body: []byte{
				wasm.OpcodeRefNull, 0x00, // ref.null type_index=0
				wasm.OpcodeCallRef, 0x00, // call_ref type_index=0
				wasm.OpcodeEnd,
			}},
		},
		ExportSection: []wasm.Export{{Name: "test", Type: wasm.ExternTypeFunc, Index: 0}},
	}

	for _, tc := range typedFuncRefsConfigs() {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntimeWithConfig(ctx, tc.config)
			defer r.Close(ctx)

			buf := binaryencoding.EncodeModule(mod)
			inst, err := r.InstantiateWithConfig(ctx, buf, wazy.NewModuleConfig())
			require.NoError(t, err)

			_, err = inst.ExportedFunction("test").Call(ctx)
			require.ErrorIs(t, err, wasmruntime.ErrRuntimeNullReference)
		})
	}
}

// TestRefAsNonNullTrapOnNull verifies that ref.as_non_null on a null funcref
// traps with ErrRuntimeNullReference.
func TestRefAsNonNullTrapOnNull(t *testing.T) {
	// (module
	//   (type $t (func))
	//   (func (export "test")
	//     ref.null func
	//     ref.as_non_null
	//     drop
	//   )
	// )
	mod := &wasm.Module{
		TypeSection:     []wasm.FunctionType{{}},
		FunctionSection: []wasm.Index{0},
		CodeSection: []wasm.Code{
			{Body: []byte{
				wasm.OpcodeRefNull, wasm.ValueTypeFuncref.Kind(), // ref.null func
				wasm.OpcodeRefAsNonNull, // trap if null
				wasm.OpcodeDrop,
				wasm.OpcodeEnd,
			}},
		},
		ExportSection: []wasm.Export{{Name: "test", Type: wasm.ExternTypeFunc, Index: 0}},
	}

	for _, tc := range typedFuncRefsConfigs() {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntimeWithConfig(ctx, tc.config)
			defer r.Close(ctx)

			buf := binaryencoding.EncodeModule(mod)
			inst, err := r.InstantiateWithConfig(ctx, buf, wazy.NewModuleConfig())
			require.NoError(t, err)

			_, err = inst.ExportedFunction("test").Call(ctx)
			require.ErrorIs(t, err, wasmruntime.ErrRuntimeNullReference)
		})
	}
}

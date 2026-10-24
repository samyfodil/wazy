package spectest

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/testing/upstream"
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

// TestCallRefWithConcreteRefLocals reproduces the call_ref.wast "run" function
// which uses (local (ref null $ii)) concrete ref locals, sets them with ref.func,
// and calls through them with call_ref.
func TestCallRefWithConcreteRefLocals(t *testing.T) {
	buf := upstream.ReadFile(t, testcases, "testdata/call_ref.0.wasm", "build.spectest.typed_function_references")

	configs := []struct {
		name   string
		config wazy.RuntimeConfig
	}{
		{"interpreter", wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(
			api.CoreFeaturesV2 | api.CoreFeatureTypedFunctionReferences | api.CoreFeatureTailCall,
		)},
	}
	if platform.CompilerSupported() {
		configs = append(configs, struct {
			name   string
			config wazy.RuntimeConfig
		}{"compiler", wazy.NewRuntimeConfigCompiler().WithCoreFeatures(
			api.CoreFeaturesV2 | api.CoreFeatureTypedFunctionReferences | api.CoreFeatureTailCall,
		)})
	}

	for _, tc := range configs {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntimeWithConfig(ctx, tc.config)
			defer r.Close(ctx)

			inst, err := r.InstantiateWithConfig(ctx, buf, wazy.NewModuleConfig())
			require.NoError(t, err)

			fn := inst.ExportedFunction("run")
			require.NotNil(t, fn)

			results, err := fn.Call(ctx, 0)
			require.NoError(t, err)
			require.Equal(t, uint64(0), results[0])

			results, err = fn.Call(ctx, 3)
			require.NoError(t, err)
			expected := uint64(0xfffffff7) // -9 as i32
			require.Equal(t, expected, results[0])
		})
	}
}

// TestBrOnNull verifies that br_on_null branches when the ref is null
// and falls through (pushing the ref) when non-null.
func TestBrOnNull(t *testing.T) {
	buf := upstream.ReadFile(t, testcases, "testdata/br_on_null.0.wasm", "build.spectest.typed_function_references")

	for _, tc := range typedFuncRefsConfigs() {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntimeWithConfig(ctx, tc.config)
			defer r.Close(ctx)

			inst, err := r.InstantiateWithConfig(ctx, buf, wazy.NewModuleConfig())
			require.NoError(t, err)

			// null ref → branches, returns -1
			fn := inst.ExportedFunction("nullable-null")
			require.NotNil(t, fn)
			results, err := fn.Call(ctx)
			require.NoError(t, err)
			require.Equal(t, uint64(0xffffffff), results[0]) // -1 as i32

			// non-null ref → falls through, calls $f, returns 7
			fn = inst.ExportedFunction("nullable-f")
			require.NotNil(t, fn)
			results, err = fn.Call(ctx)
			require.NoError(t, err)
			require.Equal(t, uint64(7), results[0])
		})
	}
}

// TestBrOnNonNull verifies that br_on_non_null branches (carrying the ref)
// when the ref is non-null, and falls through when null.
func TestBrOnNonNull(t *testing.T) {
	// (module
	//   (type $t (func (result i32)))
	//   (func $f (type $t) (i32.const 42))
	//   (func (export "null") (result i32)
	//     (block $l (result i32)
	//       ref.null 0
	//       br_on_non_null $l   ;; null → fall through (no ref pushed)
	//       i32.const 1
	//     )
	//   )
	//   ... see below for "non_null"
	// )

	// For br_on_non_null targeting a block that expects (ref $t) on branch,
	// we need the block's type signature to accept the ref. Using a type index
	// for the block type is complex in binary encoding.
	// Instead, test via a pre-compiled spec wasm.
	buf := upstream.ReadFile(t, testcases, "testdata/br_on_non_null.0.wasm", "build.spectest.typed_function_references")

	for _, tc := range typedFuncRefsConfigs() {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntimeWithConfig(ctx, tc.config)
			defer r.Close(ctx)

			inst, err := r.InstantiateWithConfig(ctx, buf, wazy.NewModuleConfig())
			require.NoError(t, err)

			// null ref → falls through, returns -1
			fn := inst.ExportedFunction("nullable-null")
			require.NotNil(t, fn)
			results, err := fn.Call(ctx)
			require.NoError(t, err)
			require.Equal(t, uint64(0xffffffff), results[0]) // -1 as i32

			// non-null ref → branches carrying ref, calls $f, returns 7
			fn = inst.ExportedFunction("nullable-f")
			require.NotNil(t, fn)
			results, err = fn.Call(ctx)
			require.NoError(t, err)
			require.Equal(t, uint64(7), results[0])
		})
	}
}

package spectest

import (
	"context"
	"embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/integration_test/spectest"
)

//go:embed testdata/*.wasm
//go:embed testdata/*.json
var testcases embed.FS

const enabledFeatures = api.CoreFeaturesV2 | api.CoreFeatureTypedFunctionReferences | api.CoreFeatureTailCall

func TestCompiler(t *testing.T) {
	// Not CompilerSupported(), which asks about CoreFeaturesV2 and so about SIMD:
	// this corpus does not use v128, and on hardware with no vector unit the
	// suite would skip itself over a feature it never touches. See
	// spectest.CompilerFeatures.
	features, ok := spectest.CompilerFeatures(t, testcases, enabledFeatures)
	if !ok {
		t.Skip()
	}
	spectest.Run(t, testcases, context.Background(), wazy.NewRuntimeConfigCompiler().WithCoreFeatures(features))
}

func TestInterpreter(t *testing.T) {
	spectest.Run(t, testcases, context.Background(), wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(enabledFeatures))
}

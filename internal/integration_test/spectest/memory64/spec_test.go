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

// The memory64 branch of the specification's core suite assumes the other
// proposals merged into WebAssembly 3.0 alongside it: imports.wast declares
// several memories in one module and imports a tag.
const enabledFeatures = api.CoreFeaturesV2 | api.CoreFeatureMemory64 |
	api.CoreFeatureMultiMemory | api.CoreFeatureExceptionHandling

func TestCompiler(t *testing.T) {
	// Not CompilerSupported(), which asks about CoreFeaturesV2 and so about SIMD:
	// this corpus does not use v128, and on hardware with no vector unit the
	// suite would skip itself over a feature it never touches. See
	// spectest.CompilerFeatures.
	features, ok := spectest.CompilerFeatures(t, testcases, enabledFeatures)
	if !ok {
		t.Skip()
	}
	spectest.Run(t, testcases, context.Background(),
		wazy.NewRuntimeConfigCompiler().WithCoreFeatures(features), spectest.WithMemory64HostModule())
}

func TestInterpreter(t *testing.T) {
	spectest.Run(t, testcases, context.Background(),
		wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(enabledFeatures), spectest.WithMemory64HostModule())
}

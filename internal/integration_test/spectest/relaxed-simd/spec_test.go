package spectest

import (
	"context"
	"embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/integration_test/spectest"
	"github.com/samyfodil/wazy/internal/platform"
)

//go:embed testdata/*.wasm
//go:embed testdata/*.json
var testcases embed.FS

const enabledFeatures = api.CoreFeaturesV2 | api.CoreFeatureRelaxedSIMD

func TestCompiler(t *testing.T) {
	// Relaxed SIMD is built on SIMD: every module here executes v128, so this
	// asks for the whole set rather than the V2 default.
	if !platform.CompilerSupports(enabledFeatures) {
		t.Skip()
	}
	spectest.Run(t, testcases, context.Background(), wazy.NewRuntimeConfigCompiler().WithCoreFeatures(enabledFeatures))
}

func TestInterpreter(t *testing.T) {
	spectest.Run(t, testcases, context.Background(), wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(enabledFeatures))
}

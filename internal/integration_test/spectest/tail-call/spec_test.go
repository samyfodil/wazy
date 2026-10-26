package spectest

import (
	"context"
	"embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/integration_test/spectest"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/upstream"
)

//go:embed all:testdata
var testcases embed.FS

const enabledFeatures = api.CoreFeaturesV2 | api.CoreFeatureTailCall

func TestCompiler(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	upstream.Require(t, testcases, "testdata/*.json", "build.spectest.tail_call")
	spectest.Run(t, testcases, context.Background(), wazy.NewRuntimeConfigCompiler().WithCoreFeatures(enabledFeatures))
}

func TestInterpreter(t *testing.T) {
	upstream.Require(t, testcases, "testdata/*.json", "build.spectest.tail_call")
	spectest.Run(t, testcases, context.Background(), wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(enabledFeatures))
}

package v2

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/integration_test/spectest"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/upstream"
)

const enabledFeatures = api.CoreFeaturesV2

func TestCompiler(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	upstream.Require(t, Testcases, "testdata/*.json", "build.spectest.v2")
	spectest.Run(t, Testcases, context.Background(), wazy.NewRuntimeConfigCompiler().WithCoreFeatures(enabledFeatures))
}

func TestInterpreter(t *testing.T) {
	upstream.Require(t, Testcases, "testdata/*.json", "build.spectest.v2")
	spectest.Run(t, Testcases, context.Background(), wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(enabledFeatures))
}

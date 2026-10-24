package v1

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/integration_test/spectest"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/upstream"
)

func TestCompiler(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	upstream.Require(t, Testcases, "testdata/*.json", "build.spectest.v1")
	spectest.Run(t, Testcases, context.Background(), wazy.NewRuntimeConfigCompiler().WithCoreFeatures(api.CoreFeaturesV1))
}

func TestInterpreter(t *testing.T) {
	upstream.Require(t, Testcases, "testdata/*.json", "build.spectest.v1")
	spectest.Run(t, Testcases, context.Background(), wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(api.CoreFeaturesV1))
}

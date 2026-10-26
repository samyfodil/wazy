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

// The memory64 branch of the specification's core suite assumes the other
// proposals merged into WebAssembly 3.0 alongside it: imports.wast declares
// several memories in one module and imports a tag.
const enabledFeatures = api.CoreFeaturesV2 | api.CoreFeatureMemory64 |
	api.CoreFeatureMultiMemory | api.CoreFeatureExceptionHandling

func TestCompiler(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	upstream.Require(t, testcases, "testdata/*.json", "build.spectest.memory64")
	spectest.Run(t, testcases, context.Background(),
		wazy.NewRuntimeConfigCompiler().WithCoreFeatures(enabledFeatures), spectest.WithMemory64HostModule())
}

func TestInterpreter(t *testing.T) {
	upstream.Require(t, testcases, "testdata/*.json", "build.spectest.memory64")
	spectest.Run(t, testcases, context.Background(),
		wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(enabledFeatures), spectest.WithMemory64HostModule())
}

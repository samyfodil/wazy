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

// The feature set the GC proposal's own branch assumes: 2.0 plus the three proposals it builds on. Not the
// whole of 3.0 -- that branch forked before multi-memory and memory64 landed, and its binary.wast, memory.wast
// and imports.wast still require the encodings those two relax (a plain zero where multi-memory reads a memory
// index). Those files are covered with their later encodings by the multi-memory and memory64 suites.
const enabledFeatures = api.CoreFeaturesV2 |
	api.CoreFeatureTailCall |
	api.CoreFeatureExtendedConst |
	api.CoreFeatureTypedFunctionReferences |
	api.CoreFeatureGC

func TestCompiler(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	spectest.Run(t, testcases, context.Background(), wazy.NewRuntimeConfigCompiler().WithCoreFeatures(enabledFeatures))
}

func TestInterpreter(t *testing.T) {
	spectest.Run(t, testcases, context.Background(), wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(enabledFeatures))
}

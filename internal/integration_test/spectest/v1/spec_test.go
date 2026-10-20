package v1

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/integration_test/spectest"
	"github.com/samyfodil/wazy/internal/platform"
)

func TestCompiler(t *testing.T) {
	// Ask about the feature set this suite actually runs with, not the
	// default. platform.CompilerSupported() reports on CoreFeaturesV2, which
	// includes SIMD; a backend that compiles V1 but not V2 -- riscv64 today --
	// would skip this entire suite despite being perfectly able to run it.
	if !platform.CompilerSupports(api.CoreFeaturesV1) {
		t.Skip()
	}
	spectest.Run(t, Testcases, context.Background(), wazy.NewRuntimeConfigCompiler().WithCoreFeatures(api.CoreFeaturesV1))
}

func TestInterpreter(t *testing.T) {
	spectest.Run(t, Testcases, context.Background(), wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(api.CoreFeaturesV1))
}

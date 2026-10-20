package v2

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/integration_test/spectest"
	"github.com/samyfodil/wazy/internal/platform"
)

const enabledFeatures = api.CoreFeaturesV2

func TestCompiler(t *testing.T) {
	// This corpus needs the vector extension and cannot have SIMD cleared the way
	// the proposal suites can: simd_select.wast is v128 value types with no vector
	// opcode at all, so it validates with SIMD disabled and then executes vector
	// instructions. The emulated riscv64 job is where these run.
	if !platform.CompilerSupports(enabledFeatures) {
		t.Skip()
	}
	spectest.Run(t, Testcases, context.Background(), wazy.NewRuntimeConfigCompiler().WithCoreFeatures(enabledFeatures))
}

func TestInterpreter(t *testing.T) {
	spectest.Run(t, Testcases, context.Background(), wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(enabledFeatures))
}

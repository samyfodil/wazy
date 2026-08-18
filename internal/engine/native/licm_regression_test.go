package native_test

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// Each of these puts a compiler-generated trap check inside a loop whose
// condition is loop-invariant. Hoisting such a check's comparison out of the
// loop leaves the trap behind in another instruction group, and lowering an
// ExitIfTrueWithCode whose Icmp is no longer group-matchable panics outright:
//
//	panic: TODO: OpcodeExitIfTrueWithCode must come after Icmp at the moment
//
// The arm64 backend is the one that panics, and none of the spec suites cover
// this shape on either backend, so these modules exist to compile.
//
// Two separate rules in the pass keep this safe today: it refuses comparisons,
// and it refuses constants, which is what every one of these checks compares
// against. Either alone is enough, so this test only fails if both go away --
// which is the point of keeping it, since making constants hoistable again is
// an obvious future change and on its own it reintroduces the panic.
var (
	// A load through a loop-invariant address: the bounds check is invariant.
	//go:embed testdata/licmtrap.wasm
	licmTrapWasm []byte
	// A division by a loop-invariant divisor: the zero check is invariant.
	//go:embed testdata/licmdiv.wasm
	licmDivWasm []byte
	// ref.as_non_null on a loop-invariant funcref: the null check is invariant,
	// and it compares against a constant rather than a loaded value, which is
	// what makes this one reachable where the other two are not.
	//go:embed testdata/licmref.wasm
	licmRefWasm []byte
)

func TestLoopInvariantTrapChecksCompile(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	for _, tc := range []struct {
		name     string
		bin      []byte
		features api.CoreFeatures
	}{
		{"invariant bounds check", licmTrapWasm, api.CoreFeaturesV2},
		{"invariant divisor check", licmDivWasm, api.CoreFeaturesV2},
		{"invariant null check", licmRefWasm, api.CoreFeaturesV2 | api.CoreFeatureTypedFunctionReferences},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithCoreFeatures(tc.features))
			defer r.Close(ctx)
			_, err := r.CompileModule(ctx, tc.bin)
			require.NoError(t, err)
		})
	}
}

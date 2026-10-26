// Package spectest covers the seams between the proposals WebAssembly 3.0 folded in.
//
// Every one of them landed with its own branch of the core suite, and those branches test a proposal
// against 2.0, not against each other -- so the crossings are what nobody's conformance suite reaches.
//
// The wasmtime-*.wast cases are Wasmtime's own tests, from its tests/misc_testsuite: an
// implementation's hand-written extras, not the specification's suite. They already cover several of
// these: GC arrays built from data and element segments, GC objects holding a v128, memory.copy between
// a 32-bit and a 64-bit memory, and constant expressions that allocate. The wazy-*.wast cases are
// this repository's, for the crossings those leave: a GC array reading a segment in a module whose
// memories are 64-bit or plural, relaxed vector instructions reading and writing a v128 held in a
// struct field or an array element, the two constant-expression forms that take a vector operand,
// and the rest of the memory instructions in a module holding both memory widths at once.
//
// Both kinds run through the same harness the downloaded suites use. Only the wazy-* cases are
// committed; `make build.spectest.v3_interaction` fetches the Wasmtime ones at a pinned commit and
// rebuilds every .json.
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

//go:embed testdata/*.wasm
//go:embed testdata/*.json
var testcases embed.FS

func TestCompiler(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	spectest.Run(t, testcases, context.Background(),
		wazy.NewRuntimeConfigCompiler().WithCoreFeatures(api.CoreFeaturesV3), spectest.WithMemory64HostModule())
}

func TestInterpreter(t *testing.T) {
	spectest.Run(t, testcases, context.Background(),
		wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(api.CoreFeaturesV3), spectest.WithMemory64HostModule())
}

// TestWasmtimeCases fails or skips on its own when the Wasmtime cases have not been fetched, since the
// two tests above still run the committed wazy-* cases without them.
func TestWasmtimeCases(t *testing.T) {
	upstream.Require(t, testcases, "testdata/wasmtime-*.json", "build.spectest.v3_interaction")
}

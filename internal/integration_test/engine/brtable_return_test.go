package adhoc

import (
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/nodiff"
)

//go:embed testdata/brtable_return.wasm
var brTableReturnWasm []byte

// TestBrTableReturn checks that a br_table of a few labels returning from its function calls the
// function listener as a return does, logging the same calls and returns as the interpreter.
func TestBrTableReturn(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	nodiff.RequireNoDiffT(t, brTableReturnWasm, true, true)
}

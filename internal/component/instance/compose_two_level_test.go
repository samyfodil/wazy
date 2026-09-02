package instance

import (
	_ "embed"
	"testing"
)

// A composed component used as the PROVIDER of a further composition -- the
// output of `wac compose` fed straight back into `wasm-tools compose`. See the
// .wat sources for the annotated shape; in one line:
//
//	level 1: (instance $p (instantiate $Provider)) + (alias export $p "X") + (export "X")
//	level 2: (instance $w (instantiate $Wrapper))  + (alias export $w "X")
//	         + (instance $c (instantiate $Consumer (with "X" (instance $g2))))
//
//go:embed testdata/compose_two_level_resource.wasm
var composeTwoLevelResourceWasm []byte

func TestComposeTwoLevelCarriesResourceIdentity(t *testing.T) {
	const want = 7*10 + 1
	if got := composedRun(t, composeTwoLevelResourceWasm, "run"); got != want {
		t.Fatalf("run = %d, want %d (%d would mean the handles crossed but the provider's destructor never ran)", got, want, 7*10)
	}
}

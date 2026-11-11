package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
)

//go:embed testdata/async/outer_alias_future.wasm
var outerAliasFutureWasm []byte

// future.drop-readable sizes its element through a type an imported instance aliases out of
// the component, as every WASI 0.3 stream's future<result<_, error-code>> does (issue #102).
func TestInstantiate_FutureElementThroughOuterAlias(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	in, err := Instantiate(ctx, r, outerAliasFutureWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	in.Close(ctx) //nolint:errcheck // test cleanup
}

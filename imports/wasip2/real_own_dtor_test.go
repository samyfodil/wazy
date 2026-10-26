package wasip2

import (
	"context"
	"embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/internal/testing/upstream"
)

// ownDtorFixture is module 20 of the component-model repository's
// test/wasmtime/resources.wast, which `make build.component_testdata` fetches
// and builds: a single component defining two resources (one with a
// destructor) whose core `start` section creates them, asserts dense handle
// numbering (1, 2, 3), drops them,
// and asserts a drop count + last-dropped rep that only the DESTRUCTOR maintains
// (it increments a counter in a sibling core instance). So instantiating it at
// all proves: (a) the reference Table's free list -- a dropped handle index is
// reused, and (b) canon resource.drop runs the guest's OWN destructor, which
// wazy previously did only for a host-initiated DropResource. The destructor is
// registered before core modules instantiate (a `start` may drop mid-graph) and
// resolved lazily.
func ownDtorFixture(t *testing.T) []byte {
	t.Helper()
	return upstream.ReadFile(t, componentModelFS, "testdata/component-model/resources.20.wasm", "build.component_testdata")
}

//go:embed all:testdata/component-model
var componentModelFS embed.FS

func TestRealOwnResourceDtorOnDrop(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	// The whole test lives in the component's core `start`, which traps
	// (unreachable) on any wrong handle index, drop count, or last-drop rep.
	// A successful instantiation therefore means every assertion held.
	inst, err := component.Instantiate(ctx, r, ownDtorFixture(t), WithWASI(WASIConfig{})...)
	if err != nil {
		t.Fatalf("instantiate (start-section self-test failed): %v", err)
	}
	inst.Close(ctx)
}

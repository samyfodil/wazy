package instance

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
)

//go:embed testdata/async/lift_async_callback.wasm
var liftAsyncCallbackWasm []byte

// TestAsyncLiftFailsLoud pins the Phase 0 boundary: a component whose export is
// an async (callback) canon lift decodes fine but has no runtime yet, so
// binding it must fail loud rather than silently binding it as a synchronous
// lift. Phase 1's callback-lift routing replaces the guard in
// bindFuncExportGraph. Fixture is real wasm-tools 1.253 output (see
// binary/testdata/async/lift_async_callback.wat).
func TestAsyncLiftFailsLoud(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	_, err := Instantiate(ctx, r, liftAsyncCallbackWasm)
	if err == nil {
		t.Fatal("expected async canon lift to fail loud at bind, got nil")
	}
	if !strings.Contains(err.Error(), "async canon lift is not yet supported") {
		t.Fatalf("want async-not-supported error, got: %v", err)
	}
}

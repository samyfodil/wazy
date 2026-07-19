package component_test

import (
	"context"
	_ "embed"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

// await_import.wasm's "run-async" export awaits an async import "get" (()->u32).
//
//go:embed testdata/await_import.wasm
var awaitImportWasm []byte

// TestCallAsync_ExternalImport drives the whole public surface end to end: an
// async import registered with component.WithAsyncImport completes from another
// goroutine, and component.Instance.CallAsync + PendingCall.Await pick up the
// result -- the flow blocking Call cannot express.
func TestCallAsync_ExternalImport(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	release := make(chan struct{})
	getImport := component.WithAsyncImport("get", "",
		func(_ context.Context, _ []component.Value, call *component.AsyncCall) error {
			go func() {
				<-release // simulate real I/O completing later, off this goroutine
				call.Resolve([]component.Value{uint32(42)})
			}()
			return nil
		},
		nil, // no params
		[]component.TypeDesc{component.PrimitiveDesc{Prim: "u32"}}, // -> u32
	)

	inst, err := component.Instantiate(ctx, r, awaitImportWasm, getImport)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	p, err := inst.CallAsync(ctx, "run-async")
	if err != nil {
		t.Fatalf("CallAsync: %v", err)
	}

	select {
	case <-p.Done():
		t.Fatal("resolved before the external import completed")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	res, err := p.Await(ctx)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if len(res) != 1 || res[0].(uint32) != 42 {
		t.Fatalf("Await = %v, want [42]", res)
	}
}

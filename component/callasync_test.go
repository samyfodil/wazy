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

// TestCallAsync_AwaitCancellableAndNot pins that Await returns the same result
// whether or not its context can be cancelled.
//
// Await installs its cancellation watcher only when ctx.Done() is non-nil; a
// Background context can never be done, so it takes the no-watcher path. The
// two arms must agree.
func TestCallAsync_AwaitCancellableAndNot(t *testing.T) {
	run := func(t *testing.T, awaitCtx func() (context.Context, context.CancelFunc)) []component.Value {
		t.Helper()
		ctx := context.Background()
		r := wazy.NewRuntime(ctx)
		defer r.Close(ctx)

		release := make(chan struct{})
		getImport := component.WithAsyncImport("get", "",
			func(_ context.Context, _ []component.Value, call *component.AsyncCall) error {
				go func() {
					<-release
					call.Resolve([]component.Value{uint32(42)})
				}()
				return nil
			},
			nil,
			[]component.TypeDesc{component.PrimitiveDesc{Prim: "u32"}},
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
		close(release)

		ac, cancel := awaitCtx()
		defer cancel()
		res, err := p.Await(ac)
		if err != nil {
			t.Fatalf("Await: %v", err)
		}
		return res
	}

	// Background: Done() is nil, so Await takes the no-watcher path.
	notCancellable := run(t, func() (context.Context, context.CancelFunc) {
		return context.Background(), func() {}
	})
	// WithCancel: Done() is non-nil, so the watcher is installed.
	cancellable := run(t, func() (context.Context, context.CancelFunc) {
		return context.WithCancel(context.Background())
	})

	if len(notCancellable) != 1 || notCancellable[0].(uint32) != 42 {
		t.Fatalf("Await(Background) = %v, want [42]", notCancellable)
	}
	if len(cancellable) != 1 || cancellable[0].(uint32) != 42 {
		t.Fatalf("Await(WithCancel) = %v, want [42]", cancellable)
	}
}

// TestCallAsync_AwaitCancelStillWakes pins that the watcher is still installed,
// and still wakes a blocked Await, when the context really can be cancelled.
// Without it a cancelled Await would sit on the cond until some unrelated
// completion happened to broadcast.
func TestCallAsync_AwaitCancelStillWakes(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	getImport := component.WithAsyncImport("get", "",
		func(_ context.Context, _ []component.Value, call *component.AsyncCall) error {
			return nil // never resolves: only cancellation can end this Await
		},
		nil,
		[]component.TypeDesc{component.PrimitiveDesc{Prim: "u32"}},
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

	actx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := p.Await(actx)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Await returned a nil error, want the cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Await did not return after its context was cancelled: the watcher never woke it")
	}
}

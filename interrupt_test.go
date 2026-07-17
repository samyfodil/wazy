package wazy_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// requireCompiler skips a compiler-codegen-specific test on a GOARCH with no
// compiler backend (anything but amd64/arm64), where NewRuntimeConfigCompiler
// would panic "unsupported architecture" (isa_other.go) on the first compile.
func requireCompiler(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip("compiler engine not supported on this GOARCH")
	}
}

// infLoopWasm exports "loop_forever" = (loop (br 0)): a tight native loop with
// no host calls, so its only scheduler/GC yield point is the loop-header
// interrupt check emitted under WithCloseOnContextDone.
var infLoopWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x04, 0x01, 0x60, 0x00, 0x00, 0x03, 0x02,
	0x01, 0x00, 0x07, 0x10, 0x01, 0x0c, 0x6c, 0x6f, 0x6f, 0x70, 0x5f, 0x66, 0x6f, 0x72, 0x65, 0x76,
	0x65, 0x72, 0x00, 0x00, 0x0a, 0x09, 0x01, 0x07, 0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x0b,
}

// spinWasm exports "spin" (param i64 n) (result i64): counts n down to 0 in a
// tight loop and returns 0. Used to validate the counter-based loop lowering
// produces correct results.
var spinWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x06, 0x01, 0x60, 0x01, 0x7e, 0x01, 0x7e,
	0x03, 0x02, 0x01, 0x00, 0x07, 0x08, 0x01, 0x04, 0x73, 0x70, 0x69, 0x6e, 0x00, 0x00, 0x0a, 0x1a,
	0x01, 0x18, 0x00, 0x02, 0x40, 0x03, 0x40, 0x20, 0x00, 0x50, 0x0d, 0x01, 0x20, 0x00, 0x42, 0x01,
	0x7d, 0x21, 0x00, 0x0c, 0x00, 0x0b, 0x0b, 0x20, 0x00, 0x0b,
}

// TestInterruptCheck_GCNotBlockedAndInterrupts is the load-bearing test: under
// WithCloseOnContextDone with wazy's non-zero default interval, a tight
// infinite loop (a) still yields often enough that Go's GC stop-the-world is
// not blocked, and (b) is terminated when the context deadline fires. Without
// the interrupt check a tight loop blocks GC for its entire duration.
func TestInterruptCheck_GCNotBlockedAndInterrupts(t *testing.T) {
	ctx := context.Background()
	requireCompiler(t)
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithCloseOnContextDone(true))
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, infLoopWasm)
	require.NoError(t, err)
	fn := mod.ExportedFunction("loop_forever")

	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, e := fn.Call(callCtx)
		errCh <- e
	}()

	time.Sleep(300 * time.Millisecond) // ensure the goroutine is inside the loop

	start := time.Now()
	runtime.GC()
	gcDur := time.Since(start)
	require.True(t, gcDur < 500*time.Millisecond,
		"runtime.GC() took %v: the loop-header yield point was lost", gcDur)

	select {
	case e := <-errCh:
		require.Error(t, e) // context deadline exceeded / module closed
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not terminate on context cancellation")
	}
}

// TestInterruptCheck_CounterPathCorrect validates that the counter-based loop
// lowering (used under WithCloseOnContextDone with a non-zero interval) still
// computes correct loop results.
func TestInterruptCheck_CounterPathCorrect(t *testing.T) {
	ctx := context.Background()
	requireCompiler(t)
	for _, interval := range []uint64{0, 1, 8, 64} {
		r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithCloseOnContextDone(true))
		cctx := wazy.WithInterruptCheckInterval(ctx, interval)
		compiled, err := r.CompileModule(cctx, spinWasm)
		require.NoError(t, err)
		mod, err := r.InstantiateModule(cctx, compiled, wazy.NewModuleConfig())
		require.NoError(t, err)
		res, err := mod.ExportedFunction("spin").Call(ctx, 5000)
		require.NoError(t, err)
		require.Equal(t, uint64(0), res[0])
		r.Close(ctx)
	}
}

// TestInterruptCheck_Validation checks that a non-power-of-two interval is
// rejected and legal intervals compile (distinct intervals are distinct
// compiled variants, enabling re-lower-by-recompile).
func TestInterruptCheck_Validation(t *testing.T) {
	ctx := context.Background()
	requireCompiler(t)
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithCloseOnContextDone(true))
	defer r.Close(ctx)

	_, err := r.CompileModule(wazy.WithInterruptCheckInterval(ctx, 3), infLoopWasm)
	require.Error(t, err)
	require.Contains(t, err.Error(), "power of two")

	for _, n := range []uint64{0, 1, 8, 64, 1024} {
		_, err := r.CompileModule(wazy.WithInterruptCheckInterval(ctx, n), infLoopWasm)
		require.NoError(t, err)
	}
}

package http_handler

import (
	"errors"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestNewMiddleware_instantiateError covers the guest failing to instantiate:
// it imports a function the host module doesn't export, which links only at
// instantiation.
func TestNewMiddleware_instantiateError(t *testing.T) {
	mw, err := NewMiddleware(testCtx,
		newGuestModule(guestParts{importsHTTPHandler: true, unknownHostImport: true}),
		UnimplementedHost{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "wasm: error instantiating guest")
	require.Nil(t, mw)
}

// TestHandleRequest_guestTrap covers a guest that traps in handle_request:
// the error surfaces to the caller, and the request state is released.
func TestHandleRequest_guestTrap(t *testing.T) {
	mw, err := NewMiddleware(testCtx,
		newGuestModule(guestParts{trapHandleRequest: true}), UnimplementedHost{})
	require.NoError(t, err)
	defer mw.Close(testCtx)

	_, ctxNext, err := mw.HandleRequest(testCtx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unreachable")
	require.Zero(t, ctxNext)
}

// TestHandleResponse_guestTrap covers the same in handle_response.
func TestHandleResponse_guestTrap(t *testing.T) {
	mw, err := NewMiddleware(testCtx,
		newGuestModule(guestParts{trapHandleResponse: true}), UnimplementedHost{})
	require.NoError(t, err)
	defer mw.Close(testCtx)

	outCtx, ctxNext, err := mw.HandleRequest(testCtx)
	require.NoError(t, err)
	require.Equal(t, CtxNext(1), ctxNext)

	err = mw.HandleResponse(outCtx, 0, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unreachable")
}

// TestHandleResponse_hostError covers the isError parameter: a host error is
// passed to the guest as 1, so it can release request resources.
func TestHandleResponse_hostError(t *testing.T) {
	mw, err := NewMiddleware(testCtx, newGuestModule(guestParts{}), UnimplementedHost{})
	require.NoError(t, err)
	defer mw.Close(testCtx)

	outCtx, _, err := mw.HandleRequest(testCtx)
	require.NoError(t, err)
	require.NoError(t, mw.HandleResponse(outCtx, 42, errors.New("next failed")))
}

// TestGuestPool covers reuse: the second request takes the instance the first
// returned to the pool rather than instantiating another.
func TestGuestPool(t *testing.T) {
	mw, err := NewMiddleware(testCtx, newGuestModule(guestParts{}), UnimplementedHost{})
	require.NoError(t, err)
	defer mw.Close(testCtx)

	m := mw.(*middleware)
	require.Equal(t, uint64(1), m.instanceCounter.Load()) // the eager one

	const requests = 3
	for range requests {
		outCtx, _, err := mw.HandleRequest(testCtx)
		require.NoError(t, err)
		require.NoError(t, mw.HandleResponse(outCtx, 0, nil))
	}

	// Instances are pooled, so serial requests don't each need one. How many
	// survive is up to sync.Pool - it may drop entries at any GC, and does
	// under -race - so this asserts the bound, not an exact count.
	instances := m.instanceCounter.Load()
	require.True(t, instances <= requests+1,
		"instantiated %d guests for %d serial requests", instances, requests)
}

// TestEmptyValuesWriteNothing covers the "nothing to write" arm shared by the
// get_XXX functions: a zero-length value reports zero and touches no memory.
func TestEmptyValuesWriteNothing(t *testing.T) {
	f := newFixture(t, 0, false)
	f.h.method = ""
	f.m.guestConfig = nil

	stack := []uint64{1 << 30, 8} // an offset outside memory
	f.m.getMethod(f.ctx, f.mod, stack)
	require.Zero(t, stack[0])

	stack = []uint64{1 << 30, 8}
	f.m.getConfig(f.ctx, f.mod, stack)
	require.Zero(t, stack[0])
}

// ExampleConsoleLogger shows the logger guests get when the host passes
// WithLogger(ConsoleLogger{}).
func ExampleConsoleLogger() {
	l := ConsoleLogger{}
	l.Log(testCtx, LogLevelInfo, "logged")
	l.Log(testCtx, LogLevelDebug, "dropped: below info")

	// Output:
	// logged
}

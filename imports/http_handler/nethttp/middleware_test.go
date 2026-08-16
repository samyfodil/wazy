package nethttp

import (
	"context"
	_ "embed"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/imports/http_handler"
	"github.com/samyfodil/wazy/internal/testing/require"
)

var testCtx = context.Background()

// binPanicOnHandleRequest is http-wasm's own fixture for a guest that traps.
//
//go:embed testdata/panic_on_handle_request.wasm
var binPanicOnHandleRequest []byte

//go:embed testdata/router.wasm
var binRouter []byte

// TestNewMiddleware_error surfaces a guest that fails the contract, rather
// than failing later per-request.
func TestNewMiddleware_error(t *testing.T) {
	mw, err := NewMiddleware(testCtx, []byte("not wasm"))
	require.Error(t, err)
	require.Nil(t, mw)
}

// TestServeHTTP_guestError covers the trap path: a guest that panics gets a
// 500 with the reason, and the next handler is never called.
func TestServeHTTP_guestError(t *testing.T) {
	mw, err := NewMiddleware(testCtx, binPanicOnHandleRequest)
	require.NoError(t, err)
	defer mw.Close(testCtx)

	var nextCalled bool
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true })

	w := httptest.NewRecorder()
	mw.NewHandler(testCtx, next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	require.Equal(t, 500, w.Code)
	require.Contains(t, w.Body.String(), "wasm error: unreachable")
	require.Contains(t, w.Body.String(), "panic_on_handle_request.handle_request")
	require.False(t, nextCalled)
}

// TestServeHTTP_nextPanics covers the recover in handleNext: a panic from the
// next handler becomes the error the guest sees in handle_response.
func TestServeHTTP_nextPanics(t *testing.T) {
	mw, err := NewMiddleware(testCtx, binRouter)
	require.NoError(t, err)
	defer mw.Close(testCtx)

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("next exploded")
	})

	w := httptest.NewRecorder()
	// /host/a routes to next, unlike the paths the guest answers itself.
	mw.NewHandler(testCtx, next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/host/a", nil))

	// The guest's handle_response ran without error, so nothing was written.
	require.Equal(t, 200, w.Code)
}

// TestBufferingRequestBody covers the wrapper that lets a downstream handler
// read a body the guest already consumed.
func TestBufferingRequestBody(t *testing.T) {
	b := &bufferingRequestBody{delegate: io.NopCloser(strings.NewReader("hello"))}

	buf := make([]byte, 8)
	n, err := b.Read(buf)
	require.Equal(t, 5, n)
	require.NoError(t, err)

	// At EOF with bytes read, the wrapper keeps them.
	n, err = b.Read(buf)
	require.Zero(t, n)
	require.Equal(t, io.EOF, err)
	require.NoError(t, b.Close())

	// A nil delegate closes cleanly.
	require.NoError(t, (&bufferingRequestBody{}).Close())
}

// TestBufferingResponseWriter covers deferring the status and body, which is
// what FeatureBufferResponse buys a guest.
func TestBufferingResponseWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &bufferingResponseWriter{delegate: rec}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(201)
	n, err := w.Write([]byte("body"))
	require.NoError(t, err)
	require.Equal(t, 4, n)

	// Nothing reached the delegate yet.
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "", rec.Body.String())

	w.release()
	require.Equal(t, 201, rec.Code)
	require.Equal(t, "body", rec.Body.String())
	require.Equal(t, "text/plain", rec.Header().Get("Content-Type"))

	// Releasing with nothing buffered writes nothing.
	rec2 := httptest.NewRecorder()
	(&bufferingResponseWriter{delegate: rec2}).release()
	require.Equal(t, 200, rec2.Code)
	require.Equal(t, "", rec2.Body.String())
}

// TestHost_EnableFeatures covers the init-time call, when there is no request
// state to attach features to.
func TestHost_EnableFeatures(t *testing.T) {
	features := host{}.EnableFeatures(testCtx, http_handler.FeatureTrailers)
	require.Equal(t, http_handler.FeatureTrailers, features)
}

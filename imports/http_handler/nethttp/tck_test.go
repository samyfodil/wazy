package nethttp

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/samyfodil/wazy/imports/http_handler/nethttp/internal/tck"
)

// TestTCK is the acceptance test for the whole port: the http-wasm
// Technology Compatibility Kit, driving its own guest binary over real HTTP
// against this middleware. It covers get/set method, URI, protocol version,
// header names and values, request body reads and the source address.
func TestTCK(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		http2    bool
		expected string
	}{
		{http2: false, expected: "HTTP/1.1"},
		{http2: true, expected: "HTTP/2.0"},
	}

	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			// Everything here is scoped to this subtest: tck.Run marks it
			// parallel, so it resumes after TestTCK itself returns, and a
			// middleware closed in the parent would already be gone.
			mw, err := NewMiddleware(ctx, tck.GuestWASM)
			if err != nil {
				t.Fatal(err)
			}
			defer mw.Close(ctx)

			// The TCK guest sits in front of the backend handler it expects.
			h := mw.NewHandler(ctx, tck.BackendHandler())

			ts := httptest.NewUnstartedServer(h)
			if tc.http2 {
				ts.EnableHTTP2 = true
				ts.StartTLS()
			} else {
				ts.Start()
			}
			defer ts.Close()

			tck.Run(t, ts.Client(), ts.URL)
		})
	}
}

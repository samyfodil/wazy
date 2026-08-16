// Package tck is the http-wasm Technology Compatibility Kit, vendored from
// github.com/http-wasm/http-wasm-host-go (Apache 2.0): its guest binary
// (tck.wasm), the backend handler that guest expects, and the assertions that
// exercise the ABI over real HTTP.
//
// It is vendored rather than imported because importing it would add a
// dependency on the wazero-based host it ships with, and wazy has none. The
// only edit is the import of the Func* constants, repointed at
// imports/http_handler.
//
// A host implementation passes the ABI when these assertions pass against it.
package tck

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
)

// BackendHandler is a http.Handler implementing the logic expected by the TCK.
// It serves to echo back information from the request to the response for
// checking expectations.
func BackendHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-httpwasm-next-method", r.Method)
		w.Header().Set("x-httpwasm-next-uri", r.RequestURI)
		for k, vs := range r.Header {
			for i, v := range vs {
				w.Header().Add(fmt.Sprintf("x-httpwasm-next-header-%s-%d", k, i), v)
			}
		}
	})
}

// StartBackend starts a httptest.Server at the given address implementing BackendHandler.
func StartBackend(addr string) *httptest.Server {
	s := httptest.NewUnstartedServer(BackendHandler())
	if addr != "" {
		s.Listener.Close()
		l, err := net.Listen("tcp", addr)
		if err != nil {
			panic(err)
		}
		s.Listener = l
	}
	s.Start()
	return s
}

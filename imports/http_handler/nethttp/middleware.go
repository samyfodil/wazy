// Package nethttp wraps an http-wasm guest as a net/http.Handler, so a guest
// can act as HTTP middleware in front of an existing handler.
//
// This is the integration point: Traefik's and dapr's WebAssembly middleware
// call NewMiddleware, then NewHandler with the next handler in the chain.
//
//	mw, err := nethttp.NewMiddleware(ctx, guestWasm)
//	if err != nil {
//		return err
//	}
//	defer mw.Close(ctx)
//	http.ListenAndServe(":8080", mw.NewHandler(ctx, next))
//
// # Attribution
//
// This is a port of github.com/http-wasm/http-wasm-host-go's
// handler/nethttp package (Apache 2.0). That package has no runtime imports,
// so the port is its logic verbatim over the http_handler package here.
package nethttp

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/imports/http_handler"
)

// compile-time checks to ensure interfaces are implemented.
var (
	_ http.Handler = (*guest)(nil)
	_ Middleware   = (*middleware)(nil)
)

// Middleware is a factory of net/http handlers implemented in Wasm.
type Middleware interface {
	// NewHandler creates an HTTP server handler implemented by a WebAssembly
	// module. The returned handler will not invoke next when it constructs a
	// response in guest wasm, for reasons such as an authorization failure or
	// serving from cache.
	//
	// ## Notes
	//   - Each handler is independent, so they don't share memory.
	//   - Handlers returned are not safe for concurrent use.
	NewHandler(ctx context.Context, next http.Handler) http.Handler

	api.Closer
}

type middleware struct {
	m http_handler.Middleware
}

// NewMiddleware compiles guest and returns a factory of net/http handlers.
func NewMiddleware(ctx context.Context, guest []byte, options ...http_handler.Option) (Middleware, error) {
	m, err := http_handler.NewMiddleware(ctx, guest, host{}, options...)
	if err != nil {
		return nil, err
	}

	return &middleware{m: m}, nil
}

// requestStateKey is a context.Context value associated with a requestState
// pointer to the current request.
type requestStateKey struct{}

type requestState struct {
	w        http.ResponseWriter
	r        *http.Request
	next     http.Handler
	features http_handler.Features
}

func newRequestState(w http.ResponseWriter, r *http.Request, g *guest) *requestState {
	s := &requestState{w: w, r: r, next: g.next}
	s.enableFeatures(g.features)
	return s
}

func (s *requestState) enableFeatures(features http_handler.Features) {
	s.features = s.features.WithEnabled(features)
	if features.IsEnabled(http_handler.FeatureBufferRequest) {
		s.r.Body = &bufferingRequestBody{delegate: s.r.Body}
	}
	if s.features.IsEnabled(http_handler.FeatureBufferResponse) {
		if _, ok := s.w.(*bufferingResponseWriter); !ok { // don't double-wrap
			s.w = &bufferingResponseWriter{delegate: s.w}
		}
	}
}

func (s *requestState) handleNext() (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if e, ok := recovered.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", recovered)
			}
		}
	}()

	// If we intercepted the request body for any reason, reset it before
	// calling downstream.
	if br, ok := s.r.Body.(*bufferingRequestBody); ok {
		if br.buffer.Len() == 0 {
			s.r.Body = br.delegate
		} else {
			br.Close() // nolint
			s.r.Body = io.NopCloser(&br.buffer)
		}
	}
	s.next.ServeHTTP(s.w, s.r)
	return
}

func requestStateFromContext(ctx context.Context) *requestState {
	return ctx.Value(requestStateKey{}).(*requestState)
}

// NewHandler implements the same method as documented on Middleware.
func (w *middleware) NewHandler(_ context.Context, next http.Handler) http.Handler {
	return &guest{
		handleRequest:  w.m.HandleRequest,
		handleResponse: w.m.HandleResponse,
		next:           next,
		features:       w.m.Features(),
	}
}

// Close implements the same method as documented on Middleware.
func (w *middleware) Close(ctx context.Context) error {
	return w.m.Close(ctx)
}

type guest struct {
	handleRequest  func(ctx context.Context) (outCtx context.Context, ctxNext http_handler.CtxNext, err error)
	handleResponse func(ctx context.Context, reqCtx uint32, err error) error
	next           http.Handler
	features       http_handler.Features
}

// ServeHTTP implements http.Handler
func (g *guest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The guest Wasm actually handles the request. As it may call host
	// functions, we add context parameters of the current request.
	s := newRequestState(w, r, g)
	ctx := context.WithValue(r.Context(), requestStateKey{}, s)
	outCtx, ctxNext, requestErr := g.handleRequest(ctx)
	if requestErr != nil {
		handleErr(w, requestErr)
	}

	// If buffering was enabled, ensure it flushes.
	if bw, ok := s.w.(*bufferingResponseWriter); ok {
		defer bw.release()
	}

	// Returning zero means the guest wants to break the handler chain, and
	// handle the response directly.
	if uint32(ctxNext) == 0 {
		return
	}

	// Otherwise, the host calls the next handler.
	err := s.handleNext()

	// Finally, call the guest with the response or error
	if err = g.handleResponse(outCtx, uint32(ctxNext>>32), err); err != nil {
		panic(err)
	}
}

func handleErr(w http.ResponseWriter, requestErr error) {
	w.WriteHeader(500)
	w.Write([]byte(requestErr.Error())) // nolint
}

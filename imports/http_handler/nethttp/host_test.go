package nethttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/imports/http_handler"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// hostFixture is a request state as ServeHTTP builds one, so the host methods
// can be driven without a guest. The guests prove the wiring; this proves the
// translation between the ABI and net/http, including trailers, which no
// example guest touches.
type hostFixture struct {
	ctx context.Context
	s   *requestState
	rec *httptest.ResponseRecorder
}

func newHostFixture(t *testing.T, r *http.Request, buffered bool) *hostFixture {
	t.Helper()
	rec := httptest.NewRecorder()

	s := &requestState{w: rec, r: r}
	if buffered {
		s.enableFeatures(http_handler.FeatureBufferResponse)
	}
	return &hostFixture{ctx: context.WithValue(testCtx, requestStateKey{}, s), s: s, rec: rec}
}

func TestHost_methodAndProtocol(t *testing.T) {
	f := newHostFixture(t, httptest.NewRequest(http.MethodGet, "/", nil), false)

	require.Equal(t, http.MethodGet, host{}.GetMethod(f.ctx))
	host{}.SetMethod(f.ctx, http.MethodPost)
	require.Equal(t, http.MethodPost, host{}.GetMethod(f.ctx))

	require.Equal(t, "HTTP/1.1", host{}.GetProtocolVersion(f.ctx))
	require.Equal(t, "192.0.2.1:1234", host{}.GetSourceAddr(f.ctx))
}

func TestHost_getURI(t *testing.T) {
	tests := []struct {
		name, target, expected string
	}{
		{"path", "/a/b", "/a/b"},
		{"query", "/a?b=c", "/a?b=c"},
		{"escaping", "/a%20b", "/a%20b"},
		{"empty query", "/a?", "/a?"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newHostFixture(t, httptest.NewRequest(http.MethodGet, tc.target, nil), false)
			require.Equal(t, tc.expected, host{}.GetURI(f.ctx))
		})
	}

	// A request with no path at all reports "/".
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.URL.Path = ""
	f := newHostFixture(t, r, false)
	require.Equal(t, "/", host{}.GetURI(f.ctx))
}

func TestHost_setURI(t *testing.T) {
	tests := []struct {
		name, uri, expected string
	}{
		{"path", "/a/b", "/a/b"},
		{"query", "/a?b=c", "/a?b=c"},
		{"empty means root", "", "/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newHostFixture(t, httptest.NewRequest(http.MethodGet, "/original", nil), false)
			host{}.SetURI(f.ctx, tc.uri)
			require.Equal(t, tc.expected, host{}.GetURI(f.ctx))
		})
	}

	t.Run("unparseable", func(t *testing.T) {
		f := newHostFixture(t, httptest.NewRequest(http.MethodGet, "/", nil), false)
		err := require.CapturePanic(func() { host{}.SetURI(f.ctx, "not a uri") })
		require.Error(t, err)
	})
}

func TestHost_requestHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept", "text/plain")
	f := newHostFixture(t, r, false)

	// Host is special-cased: it is a field on the request, not a header.
	require.Equal(t, []string{"Accept", "Host"}, host{}.GetRequestHeaderNames(f.ctx))
	require.Equal(t, []string{"example.com"}, host{}.GetRequestHeaderValues(f.ctx, "Host"))
	require.Equal(t, []string{"text/plain"}, host{}.GetRequestHeaderValues(f.ctx, "Accept"))

	host{}.SetRequestHeaderValue(f.ctx, "Accept", "application/json")
	require.Equal(t, []string{"application/json"}, host{}.GetRequestHeaderValues(f.ctx, "Accept"))

	host{}.AddRequestHeaderValue(f.ctx, "Accept", "text/csv")
	require.Equal(t, []string{"application/json", "text/csv"}, host{}.GetRequestHeaderValues(f.ctx, "Accept"))

	host{}.RemoveRequestHeader(f.ctx, "Accept")
	require.Nil(t, host{}.GetRequestHeaderValues(f.ctx, "Accept"))
}

// TestHost_requestHeaderNames_empty covers a request with neither a Host nor
// headers, and one carrying only trailers.
func TestHost_requestHeaderNames_empty(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = ""
	r.Header = http.Header{}
	f := newHostFixture(t, r, false)
	require.Nil(t, host{}.GetRequestHeaderNames(f.ctx))

	r.Header.Set(http.TrailerPrefix+"Grpc-Status", "1")
	require.Nil(t, host{}.GetRequestHeaderNames(f.ctx))
}

func TestHost_responseHeaders(t *testing.T) {
	f := newHostFixture(t, httptest.NewRequest(http.MethodGet, "/", nil), false)
	require.Nil(t, host{}.GetResponseHeaderNames(f.ctx))

	host{}.SetResponseHeaderValue(f.ctx, "Content-Type", "text/plain")
	require.Equal(t, []string{"Content-Type"}, host{}.GetResponseHeaderNames(f.ctx))
	require.Equal(t, []string{"text/plain"}, host{}.GetResponseHeaderValues(f.ctx, "Content-Type"))

	host{}.AddResponseHeaderValue(f.ctx, "Set-Cookie", "a=b")
	host{}.AddResponseHeaderValue(f.ctx, "Set-Cookie", "c=d")
	require.Equal(t, []string{"a=b", "c=d"}, host{}.GetResponseHeaderValues(f.ctx, "Set-Cookie"))

	host{}.RemoveResponseHeader(f.ctx, "Set-Cookie")
	require.Equal(t, []string{"Content-Type"}, host{}.GetResponseHeaderNames(f.ctx))

	// A response carrying only trailers has no header names.
	host{}.RemoveResponseHeader(f.ctx, "Content-Type")
	host{}.SetResponseTrailerValue(f.ctx, "Grpc-Status", "1")
	require.Nil(t, host{}.GetResponseHeaderNames(f.ctx))
}

// TestHost_trailers covers both directions. net/http carries trailers as
// headers under http.TrailerPrefix, which is what the ABI's trailer kinds map
// onto.
func TestHost_trailers(t *testing.T) {
	f := newHostFixture(t, httptest.NewRequest(http.MethodGet, "/", nil), false)

	require.Nil(t, host{}.GetRequestTrailerNames(f.ctx))
	require.Nil(t, host{}.GetResponseTrailerNames(f.ctx))

	host{}.SetRequestTrailerValue(f.ctx, "Grpc-Status", "1")
	host{}.AddResponseTrailerValue(f.ctx, "Grpc-Message", "ok")

	require.Equal(t, []string{"Grpc-Message", "Grpc-Status"}, host{}.GetRequestTrailerNames(f.ctx))
	require.Equal(t, []string{"Grpc-Message", "Grpc-Status"}, host{}.GetResponseTrailerNames(f.ctx))
	require.Equal(t, []string{"1"}, host{}.GetRequestTrailerValues(f.ctx, "Grpc-Status"))
	require.Equal(t, []string{"ok"}, host{}.GetResponseTrailerValues(f.ctx, "Grpc-Message"))

	// The header itself is what the client sees on the wire.
	require.Equal(t, "1", f.rec.Header().Get(http.TrailerPrefix+"Grpc-Status"))

	host{}.RemoveRequestTrailer(f.ctx, "Grpc-Status")
	host{}.RemoveResponseTrailer(f.ctx, "Grpc-Message")
	require.Nil(t, host{}.GetResponseTrailerNames(f.ctx))
}

func TestHost_statusCode(t *testing.T) {
	t.Run("buffered", func(t *testing.T) {
		f := newHostFixture(t, httptest.NewRequest(http.MethodGet, "/", nil), true)

		// Nothing written yet: the default.
		require.Equal(t, uint32(200), host{}.GetStatusCode(f.ctx))

		host{}.SetStatusCode(f.ctx, 404)
		require.Equal(t, uint32(404), host{}.GetStatusCode(f.ctx))
		require.Equal(t, 200, f.rec.Code) // deferred until release
	})

	t.Run("unbuffered writes through", func(t *testing.T) {
		f := newHostFixture(t, httptest.NewRequest(http.MethodGet, "/", nil), false)
		host{}.SetStatusCode(f.ctx, 404)
		require.Equal(t, 404, f.rec.Code)
	})
}

func TestHost_bodies(t *testing.T) {
	f := newHostFixture(t, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("request")), true)

	body, err := io.ReadAll(host{}.RequestBodyReader(f.ctx))
	require.NoError(t, err)
	require.Equal(t, "request", string(body))

	// Writing the request body replaces it for the next handler.
	n, err := host{}.RequestBodyWriter(f.ctx).Write([]byte("replaced"))
	require.NoError(t, err)
	require.Equal(t, 8, n)
	body, err = io.ReadAll(f.s.r.Body)
	require.NoError(t, err)
	require.Equal(t, "replaced", string(body))

	// The buffered response body round-trips through the writer and reader.
	_, err = host{}.ResponseBodyWriter(f.ctx).Write([]byte("response"))
	require.NoError(t, err)
	body, err = io.ReadAll(host{}.ResponseBodyReader(f.ctx))
	require.NoError(t, err)
	require.Equal(t, "response", string(body))

	// Unbuffered, the writer is the ResponseWriter itself.
	plain := newHostFixture(t, httptest.NewRequest(http.MethodGet, "/", nil), false)
	_, err = host{}.ResponseBodyWriter(plain.ctx).Write([]byte("direct"))
	require.NoError(t, err)
	require.Equal(t, "direct", plain.rec.Body.String())
}

// TestHost_enableFeaturesPerRequest covers features arriving mid-request:
// buffering must wrap the request body and response writer once each.
func TestHost_enableFeaturesPerRequest(t *testing.T) {
	f := newHostFixture(t, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body")), false)

	host{}.EnableFeatures(f.ctx, http_handler.FeatureBufferRequest|http_handler.FeatureBufferResponse)
	_, isBufferedBody := f.s.r.Body.(*bufferingRequestBody)
	require.True(t, isBufferedBody)
	first, isBufferedWriter := f.s.w.(*bufferingResponseWriter)
	require.True(t, isBufferedWriter)

	// Enabling again must not double-wrap.
	host{}.EnableFeatures(f.ctx, http_handler.FeatureBufferResponse)
	second := f.s.w.(*bufferingResponseWriter)
	require.Equal(t, first, second)
}

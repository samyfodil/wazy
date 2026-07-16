package instance

import (
	"context"
	_ "embed"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
)

// real_http_trailers.component.wasm is a genuine rustc wasm32-wasip2
// wasi:http/proxy component (built with the wasi 0.14 crate) whose
// incoming-handler writes a body and then finishes the outgoing-body WITH a
// trailer:
//
//	let out = body.write().unwrap();
//	out.blocking_write_and_flush(b"hello-with-trailer").unwrap();
//	drop(out);
//	let trailers = Fields::new();
//	trailers.set("x-checksum", &[b"abc123".to_vec()]).unwrap();
//	OutgoingBody::finish(body, Some(trailers)).unwrap();
//
// This exercises wasi:http/types outgoing-body.finish(this, Some(trailers)) --
// the response-trailer path that was previously fail-loud. (wasmtime's own
// HTTP/1.1 serve bridge does not re-emit wasi trailers on the wire, so the
// assertion is behavioral: wazy must ACCEPT the trailer call and surface the
// trailer, rather than trap.)
//
//go:embed testdata/real_http_trailers.component.wasm
var realHTTPTrailersWasm []byte

// TestRealHTTPTrailers proves the response-trailer path: the guest's
// outgoing-body.finish(Some(trailers)) succeeds (no trap) and wazy surfaces the
// trailer through net/http's server-side trailer protocol, captured by the
// httptest recorder.
func TestRealHTTPTrailers(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realHTTPTrailersWasm, WithWASI(WASIConfig{EnableHTTP: true})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	inst.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got, want := rec.Body.String(), "hello-with-trailer"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	// The trailer the guest set must be surfaced (proves the finish(Some(...))
	// path ran and the fields were read back through the ABI, not dropped).
	res := rec.Result()
	if got := res.Trailer.Get("x-checksum"); got != "abc123" {
		t.Fatalf("trailer x-checksum = %q, want %q (full trailers: %v)", got, "abc123", res.Trailer)
	}
}

// real_http_reqtrailers.component.wasm is a genuine rustc wasi:http/proxy guest
// that reads its REQUEST trailers: it consumes the incoming-body, drains the
// stream, then IncomingBody::finish -> future-trailers -> get, and echoes the
// x-req-trailer trailer value into the response body ("reqtrailer=<value>", or
// "reqtrailer=none" when absent). This exercises the request-trailer read path:
// incoming-body.finish, future-trailers.subscribe, future-trailers.get.
//
//go:embed testdata/real_http_reqtrailers.component.wasm
var realHTTPReqTrailersWasm []byte

// TestRealHTTPRequestTrailers proves the read path: a request carrying a
// trailer is delivered to the guest through future-trailers.get, and the guest
// echoes it back. Two distinct trailer values prove real data flow (not a
// constant); a request with no trailer yields "none".
func TestRealHTTPRequestTrailers(t *testing.T) {
	cases := []struct {
		name    string
		trailer http.Header
		want    string
	}{
		{name: "value_trX", trailer: http.Header{"X-Req-Trailer": {"trX"}}, want: "reqtrailer=trX"},
		{name: "value_other", trailer: http.Header{"X-Req-Trailer": {"different-99"}}, want: "reqtrailer=different-99"},
		{name: "absent", trailer: nil, want: "reqtrailer=none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)

			inst, err := Instantiate(ctx, r, realHTTPReqTrailersWasm, WithWASI(WASIConfig{EnableHTTP: true})...)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			defer inst.Close(ctx)

			req := httptest.NewRequest("POST", "/", strings.NewReader("hello"))
			req.Trailer = tc.trailer
			rec := httptest.NewRecorder()
			inst.ServeHTTP(rec, req)

			if rec.Code != 200 {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Body.String(); got != tc.want {
				t.Fatalf("body = %q, want %q", got, tc.want)
			}
		})
	}
}

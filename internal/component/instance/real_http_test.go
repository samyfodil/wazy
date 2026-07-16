package instance

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	_ "embed"

	"github.com/samyfodil/wazy"
)

//go:embed testdata/real_http_incoming.component.wasm
var realHTTPIncomingWasm []byte

// TestRealHTTP_IncomingHandler runs a real rustc wasm32-wasip2 component that
// exports wasi:http/incoming-handler: on each request it reads the method and
// path-with-query off the incoming-request and writes
// "method=Method::<M> path=<pathQ>\n" as a text/plain 200 response. The
// expected bodies are the exact output `wasmtime serve -S cli` produced for the
// same fixture (differential golden -- same discipline as TestConformance),
// verifying wazy's wasi:http/types ABI end to end: the exported handle is
// called with synthesized incoming-request + response-outparam resources, the
// guest's method()/path-with-query()/fields/outgoing-response/outgoing-body
// calls are serviced, its body write lands through the shared output-stream
// path, and response-outparam.set captures the response.
func TestRealHTTP_IncomingHandler(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realHTTPIncomingWasm, WithWASI(WASIConfig{EnableHTTP: true})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	cases := []struct {
		method, path, wantBody string
	}{
		{"GET", "/hello?x=1", "method=Method::Get path=/hello?x=1\n"},
		{"GET", "/", "method=Method::Get path=/\n"},
		{"POST", "/a/b/c", "method=Method::Post path=/a/b/c\n"},
		{"DELETE", "/x?y=2&z=3", "method=Method::Delete path=/x?y=2&z=3\n"},
		{"PUT", "/p", "method=Method::Put path=/p\n"},
		{"PATCH", "/patch", "method=Method::Patch path=/patch\n"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			u, err := url.Parse(tc.path)
			if err != nil {
				t.Fatalf("parse path: %v", err)
			}
			status, hdr, body, err := inst.serveHTTP(ctx, tc.method, u, http.Header{}, nil)
			if err != nil {
				t.Fatalf("serveHTTP: %v", err)
			}
			if status != 200 {
				t.Errorf("status = %d, want 200", status)
			}
			if ct := hdr.Get("content-type"); ct != "text/plain" {
				t.Errorf("content-type = %q, want text/plain", ct)
			}
			if string(body) != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// TestRealHTTP_ServeHTTP proves the public net/http.Handler surface: the same
// guest, driven through (*Instance).ServeHTTP against an httptest recorder,
// produces the expected status/header/body.
func TestRealHTTP_ServeHTTP(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realHTTPIncomingWasm, WithWASI(WASIConfig{EnableHTTP: true})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	req := httptest.NewRequest("GET", "/greet?name=wazy", nil)
	rec := httptest.NewRecorder()
	inst.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("content-type"); ct != "text/plain" {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	if got, want := rec.Body.String(), "method=Method::Get path=/greet?name=wazy\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestRealHTTP_NotEnabled proves ServeHTTP fails loud on an instance created
// without EnableHTTP, rather than silently mis-serving.
func TestRealHTTP_NotEnabled(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realHTTPIncomingWasm, WithWASI(WASIConfig{})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	u, _ := url.Parse("/x")
	_, _, _, err = inst.serveHTTP(ctx, "GET", u, http.Header{}, nil)
	if err == nil || !strings.Contains(err.Error(), "EnableHTTP") {
		t.Fatalf("expected an EnableHTTP error, got %v", err)
	}
}

//go:embed testdata/real_http_outgoing.component.wasm
var realHTTPOutgoingWasm []byte

// backendRoundTripper serves the same fixed body the real backend returned when
// the golden was captured under `wasmtime serve -S cli -S inherit-network`
// (127.0.0.1:8912 -> "hello-from-backend\n"), so wazy's outbound path is
// exercised hermetically -- no real socket, deterministic, matching the golden.
type backendRoundTripper struct{ t *testing.T }

func (b backendRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "127.0.0.1:8912" || r.URL.Path != "/backend" || r.Method != "GET" {
		b.t.Errorf("unexpected outbound request: %s %s", r.Method, r.URL)
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader("hello-from-backend\n")),
	}, nil
}

// TestRealHTTP_OutgoingHandler runs a real rustc wasm32-wasip2 component that,
// on each request, makes an OUTBOUND HTTP GET via wasi:http/outgoing-handler
// and echoes the fetched body back. The expected body is exactly what
// `wasmtime serve -S cli -S inherit-network` produced for the same fixture
// against the same backend (differential golden). This verifies the client-side
// ABI end to end: outgoing-request build-up (set-method/scheme/authority/
// path), outgoing-handler.handle -> http.Client, future-incoming-response
// subscribe/get, incoming-response.consume, incoming-body.stream, and the
// reused input-stream blocking-read.
func TestRealHTTP_OutgoingHandler(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	client := &http.Client{Transport: backendRoundTripper{t: t}}
	inst, err := Instantiate(ctx, r, realHTTPOutgoingWasm, WithWASI(WASIConfig{EnableHTTP: true, HTTPClient: client})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	status, _, body, err := inst.serveHTTP(ctx, "GET", mustURL("/trigger"), http.Header{}, nil)
	if err != nil {
		t.Fatalf("serveHTTP: %v", err)
	}
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if string(body) != "hello-from-backend\n" {
		t.Errorf("body = %q, want %q", body, "hello-from-backend\n")
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

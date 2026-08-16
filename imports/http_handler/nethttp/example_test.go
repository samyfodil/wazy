package nethttp_test

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/imports/http_handler"
	nethttp "github.com/samyfodil/wazy/imports/http_handler/nethttp"
)

// The guests below are http-wasm's own example binaries, taken from
// github.com/http-wasm/http-wasm-host-go (Apache 2.0). They were built
// elsewhere against the http-wasm SDK, so they fail if this implementation
// drifts from the ABI rather than agreeing with a matching mistake. The
// expected output of each example is upstream's, unchanged.
var (
	//go:embed testdata/auth.wasm
	binExampleAuth []byte

	//go:embed testdata/wasi.wasm
	binExampleWASI []byte

	//go:embed testdata/log.wasm
	binExampleLog []byte

	//go:embed testdata/router.wasm
	binExampleRouter []byte

	//go:embed testdata/redact.wasm
	binExampleRedact []byte
)

var (
	requestBody  = "{\"hello\": \"panda\"}"
	responseBody = "{\"hello\": \"world\"}"

	serveJSON = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		w.Write([]byte(responseBody)) // nolint
	})

	servePath = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Content-Type", "text/plain")
		w.Write([]byte(r.URL.Path)) // nolint
	})
)

// Example_auth shows a guest that terminates unauthorized requests itself,
// never calling the next handler.
func Example_auth() {
	ctx := context.Background()

	mw, err := nethttp.NewMiddleware(ctx, binExampleAuth)
	if err != nil {
		log.Panicln(err)
	}
	defer mw.Close(ctx)

	// Wrap the real handler with an interceptor implemented in WebAssembly.
	ts := httptest.NewServer(mw.NewHandler(ctx, serveJSON))
	defer ts.Close()

	// Invoke some requests, only one of which should pass.
	headers := []http.Header{
		{"NotAuthorization": {"1"}},
		{"Authorization": {""}},
		{"Authorization": {"Basic QWxhZGRpbjpvcGVuIHNlc2FtZQ=="}},
		{"Authorization": {"0"}},
	}

	for _, header := range headers {
		req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
		if err != nil {
			log.Panicln(err)
		}
		req.Header = header

		resp, err := ts.Client().Do(req)
		if err != nil {
			log.Panicln(err)
		}
		resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			fmt.Println("OK")
		case http.StatusUnauthorized:
			fmt.Println("Unauthorized")
		default:
			log.Panicln("unexpected status code", resp.StatusCode)
		}
		if auth, ok := resp.Header["Www-Authenticate"]; ok {
			fmt.Println("Www-Authenticate:", auth[0])
		}
	}

	// Output:
	// Unauthorized
	// Www-Authenticate: Basic realm="test"
	// Unauthorized
	// OK
	// Unauthorized
}

// Example_wasi shows a guest that also imports WASI: NewMiddleware detects
// that and instantiates wazy's imports/wasi_snapshot_preview1 for it. The
// guest prints the request and response, including trailers, to stdout.
func Example_wasi() {
	ctx := context.Background()
	moduleConfig := wazy.NewModuleConfig().WithStdout(os.Stdout)

	mw, err := nethttp.NewMiddleware(ctx, binExampleWASI,
		http_handler.WithModuleConfig(moduleConfig))
	if err != nil {
		log.Panicln(err)
	}
	defer mw.Close(ctx)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Set-Cookie", "a=b") // example of multiple headers
		w.Header().Add("Set-Cookie", "c=d")
		w.Header().Set("Date", "Tue, 15 Nov 1994 08:12:31 GMT")

		// Use chunked encoding so we can set a test trailer
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("Trailer", "grpc-status")
		w.Header().Set(http.TrailerPrefix+"grpc-status", "1")
		w.Write([]byte(`{"hello": "world"}`)) // nolint
	})

	ts := httptest.NewServer(mw.NewHandler(ctx, next))
	defer ts.Close()

	req, err := http.NewRequest("POST", ts.URL, strings.NewReader(requestBody))
	if err != nil {
		log.Panicln(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Host = "localhost"
	resp, err := ts.Client().Do(req)
	if err != nil {
		log.Panicln(err)
	}
	defer resp.Body.Close()

	// Output:
	// POST / HTTP/1.1
	// accept-encoding: gzip
	// content-length: 18
	// content-type: application/json
	// host: localhost
	// user-agent: Go-http-client/1.1
	//
	// {"hello": "panda"}
	//
	// HTTP/1.1 200
	// content-type: application/json
	// date: Tue, 15 Nov 1994 08:12:31 GMT
	// set-cookie: a=b
	// set-cookie: c=d
	// trailer: grpc-status
	// transfer-encoding: chunked
	//
	// {"hello": "world"}
	// grpc-status: 1
}

// Example_log shows a guest logging through the host's Logger.
func Example_log() {
	ctx := context.Background()

	mw, err := nethttp.NewMiddleware(ctx, binExampleLog,
		http_handler.WithLogger(http_handler.ConsoleLogger{}))
	if err != nil {
		log.Panicln(err)
	}
	defer mw.Close(ctx)

	ts := httptest.NewServer(mw.NewHandler(ctx, serveJSON))
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL)
	if err != nil {
		log.Panicln(err)
	}
	defer resp.Body.Close()

	// Output:
	// hello world
}

// Example_router shows a guest that rewrites the URI, or answers directly.
func Example_router() {
	ctx := context.Background()

	mw, err := nethttp.NewMiddleware(ctx, binExampleRouter)
	if err != nil {
		log.Panicln(err)
	}
	defer mw.Close(ctx)

	ts := httptest.NewServer(mw.NewHandler(ctx, servePath))
	defer ts.Close()

	paths := []string{
		"",
		"nothosst",
		"host/a",
	}

	for _, p := range paths {
		url := fmt.Sprintf("%s/%s", ts.URL, p)
		resp, err := ts.Client().Get(url)
		if err != nil {
			log.Panicln(err)
		}
		defer resp.Body.Close()
		content, _ := io.ReadAll(resp.Body)
		fmt.Println(string(content))
	}

	// Output:
	// hello world
	// hello world
	// /a
}

// Example_redact shows guest config plus buffered request and response
// bodies: the guest rewrites a secret out of both directions.
func Example_redact() {
	ctx := context.Background()

	secret := "open sesame"
	mw, err := nethttp.NewMiddleware(ctx, binExampleRedact,
		http_handler.WithGuestConfig([]byte(secret)))
	if err != nil {
		log.Panicln(err)
	}
	defer mw.Close(ctx)

	var body string
	serveBody := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content, _ := io.ReadAll(r.Body)
		fmt.Println(string(content))
		r.Header.Set("Content-Type", "text/plain")
		w.Write([]byte(body)) // nolint
	})

	ts := httptest.NewServer(mw.NewHandler(ctx, serveBody))
	defer ts.Close()

	bodies := []string{
		secret,
		"hello world",
		fmt.Sprintf("hello %s world", secret),
	}

	for _, b := range bodies {
		body = b

		resp, err := ts.Client().Post(ts.URL, "text/plain", strings.NewReader(body))
		if err != nil {
			log.Panicln(err)
		}
		defer resp.Body.Close()
		content, _ := io.ReadAll(resp.Body)
		fmt.Println(string(content))
	}

	// Output:
	// ###########
	// ###########
	// hello world
	// hello world
	// hello ########### world
	// hello ########### world
}

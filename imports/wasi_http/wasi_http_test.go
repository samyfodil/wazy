package wasi_http

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// httpGuest is dapr's own wasm test fixture, byte for byte:
// bindings/wasm/testdata/http/main.wasm from dapr/components-contrib
// (Apache 2.0). It is a Go guest built against dev-wasm's wasi-http client
// bindings that GETs os.Args[1] and prints the status and body. Keeping
// dapr's binary rather than building our own is the point: it is compiled
// against wasi-go's ABI, so it fails if this package drifts from it.
//
//go:embed testdata/http/main.wasm
var httpGuest []byte

const guestOutput = "Status: 200\nBody: \nhello from the host\n"

// newGuestServer returns a server whose body the fixture prints.
func newGuestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello from the host")
	}))
	t.Cleanup(ts.Close)
	return ts
}

// runGuest instantiates the fixture, which runs it, and returns its stdout.
func runGuest(ctx context.Context, t *testing.T, r wazy.Runtime, compiled wazy.CompiledModule, name, url string) string {
	t.Helper()
	var stdout bytes.Buffer
	config := wazy.NewModuleConfig().WithName(name).WithArgs("http", url).WithStdout(&stdout)
	mod, err := r.InstantiateModule(ctx, compiled, config)
	require.NoError(t, err)
	require.NoError(t, mod.Close(ctx))
	return stdout.String()
}

// TestGuest is the acceptance test: dapr's fixture, unmodified, driven end to
// end through this package's host functions.
func TestGuest(t *testing.T) {
	ts := newGuestServer(t)
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	closer, err := Instantiate(ctx, r)
	require.NoError(t, err)
	defer closer.Close(ctx)

	compiled, err := r.CompileModule(ctx, httpGuest)
	require.NoError(t, err)

	require.Equal(t, guestOutput, runGuest(ctx, t, r, compiled, "http-1", ts.URL))
}

// TestGuest_concurrent mirrors dapr's TestEnsureConcurrency: many instances of
// one compiled module, sharing one set of handle tables, in flight at once. A
// handle allocated by one instance must never be visible to another.
func TestGuest_concurrent(t *testing.T) {
	ts := newGuestServer(t)
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	closer, err := Instantiate(ctx, r)
	require.NoError(t, err)
	defer closer.Close(ctx)

	compiled, err := r.CompileModule(ctx, httpGuest)
	require.NoError(t, err)

	const instances = 8
	var wg sync.WaitGroup
	outputs := make([]string, instances)
	for i := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outputs[i] = runGuest(ctx, t, r, compiled, "http-"+strconv.Itoa(i), ts.URL)
		}()
	}
	wg.Wait()

	for i, out := range outputs {
		require.Equal(t, guestOutput, out, "instance %d", i)
	}
}

// TestGuest_serverStatus proves the status and body the guest prints are the
// server's, not defaults that would pass whatever the server returned.
func TestGuest_serverStatus(t *testing.T) {
	ctx := context.Background()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, "short and stout")
	}))
	defer ts.Close()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	closer, err := Instantiate(ctx, r)
	require.NoError(t, err)
	defer closer.Close(ctx)

	compiled, err := r.CompileModule(ctx, httpGuest)
	require.NoError(t, err)

	require.Equal(t, "Status: 418\nBody: \nshort and stout\n", runGuest(ctx, t, r, compiled, "http-teapot", ts.URL))
}

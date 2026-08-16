package wasmedge_test

import (
	"bytes"
	"context"
	_ "embed"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/imports/wasmedge"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// sockGuestV1Wasm is the same exercise built against the V1 table: sock_accept
// without fdflags, address getters that report the family separately, and bare
// addresses rather than the 128-byte V2 layout. Source: testdata/sockguest_v1.rs.
//
//go:embed testdata/sockguest_v1.wasm
var sockGuestV1Wasm []byte

func runGuestV1(t *testing.T, args ...string) string {
	t.Helper()
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })

	_, err := wasmedge.Instantiate(ctx, r, wasmedge.V1)
	require.NoError(t, err)

	var stdout bytes.Buffer
	config := wazy.NewModuleConfig().
		WithArgs(append([]string{"sockguest"}, args...)...).
		WithStdout(&stdout).
		WithStderr(&stdout)

	mod, err := r.InstantiateModule(ctx, mustCompile(t, r, sockGuestV1Wasm), config)
	require.NoError(t, err)
	require.NoError(t, mod.Close(ctx))
	return stdout.String()
}

// TestGuestV1_client covers the V1 wire format end to end: a bare four-byte
// address on the way in, and the family reported as its own result on the way
// back out.
func TestGuestV1_client(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	served := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			served <- "accept: " + err.Error()
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			served <- "read: " + err.Error()
			return
		}
		served <- string(buf[:n])
		_, _ = conn.Write([]byte("pong"))
	}()

	p := port(t, ln.Addr())
	out := runGuestV1(t, "client", strconv.Itoa(p))

	require.Equal(t, "ping", <-served)
	require.Contains(t, out, "sent 4")
	require.Contains(t, out, "recv pong")
	// An IPv4 address in the wider buffer stays four bytes and reports type 4,
	// rather than being widened to a v4-mapped v6 address.
	require.Contains(t, out, "local 127.0.0.1 type=4 port_set=true")
	require.Contains(t, out, "peer 127.0.0.1 type=4 port="+strconv.Itoa(p))
}

// TestGuestV1_server covers V1's two-parameter sock_accept, which is the one
// function whose signature differs from standard WASI.
func TestGuestV1_server(t *testing.T) {
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	_, err := wasmedge.Instantiate(ctx, r, wasmedge.V1)
	require.NoError(t, err)

	compiled := mustCompile(t, r, sockGuestV1Wasm)

	stdout := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		config := wazy.NewModuleConfig().
			WithArgs("sockguest", "server").
			WithStdout(stdout).
			WithStderr(stdout)
		mod, err := r.InstantiateModule(ctx, compiled, config)
		if err != nil {
			done <- err
			return
		}
		done <- mod.Close(ctx)
	}()

	guestPort := waitForPort(t, stdout)

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(guestPort)), 5*time.Second)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)

	reply := make([]byte, 64)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	n, err := conn.Read(reply)
	require.NoError(t, err)
	require.Equal(t, "pong", string(reply[:n]))

	require.NoError(t, <-done)
	require.Contains(t, stdout.String(), "server got ping")
}

// TestDetectV1 covers telling the versions apart by signature, since they
// share every function name.
func TestDetectV1(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	compiled := mustCompile(t, r, sockGuestV1Wasm)
	require.Equal(t, wasmedge.V1, wasmedge.Detect(compiled.ImportedFunctions()))
	require.Equal(t, "v1", wasmedge.V1.String())
}

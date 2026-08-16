package wasmedge_test

import (
	"bytes"
	"context"
	_ "embed"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/imports/wasmedge"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// sockGuestWasm is a Rust guest built against the extension through
// hand-declared imports, so it depends on no SDK -- only on the wire contract
// a WasmEdge-targeted guest is compiled against. Source: testdata/sockguest.rs,
// built with `cargo build --release --target wasm32-wasip1`.
//
// It deliberately mixes the extension with standard WASI: it creates and
// connects sockets with sock_open/sock_connect, then does its I/O with the
// standard sock_send/sock_recv, which is how a real guest behaves.
//
//go:embed testdata/sockguest.wasm
var sockGuestWasm []byte

// runGuest runs the guest with the given arguments and returns its stdout.
func runGuest(t *testing.T, args ...string) string {
	t.Helper()
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })

	// One call gives the guest standard WASI plus the V2 sockets table.
	_, err := wasmedge.Instantiate(ctx, r, wasmedge.V2)
	require.NoError(t, err)

	var stdout bytes.Buffer
	config := wazy.NewModuleConfig().
		WithArgs(append([]string{"sockguest"}, args...)...).
		WithStdout(&stdout).
		WithStderr(&stdout)

	mod, err := r.InstantiateModule(ctx, mustCompile(t, r, sockGuestWasm), config)
	require.NoError(t, err)
	require.NoError(t, mod.Close(ctx))
	return stdout.String()
}

func mustCompile(t *testing.T, r wazy.Runtime, bin []byte) wazy.CompiledModule {
	t.Helper()
	c, err := r.CompileModule(context.Background(), bin)
	require.NoError(t, err)
	return c
}

// TestGuest_client is the acceptance test for the outbound path: the guest
// opens a socket, connects to a real listener, writes, reads the reply, and
// reads back the local address the connection was assigned.
func TestGuest_client(t *testing.T) {
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

	out := runGuest(t, "client", strconv.Itoa(port(t, ln.Addr())))

	require.Equal(t, "ping", <-served, "the host listener should have received the guest's bytes")
	require.Contains(t, out, "sent 4")
	require.Contains(t, out, "recv pong")
	// The address getter reports the loopback address and a real ephemeral port.
	require.Contains(t, out, "local 127.0.0.1 port_set=true")
}

// TestGuest_server covers the inbound path: the guest binds, listens, reports
// the port it was given, accepts, and echoes. sock_accept has to work on a
// listener the guest created, which the standard WASI function cannot do.
func TestGuest_server(t *testing.T) {
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	_, err := wasmedge.Instantiate(ctx, r, wasmedge.V2)
	require.NoError(t, err)

	compiled := mustCompile(t, r, sockGuestWasm)

	// The guest prints its port, then blocks in accept, so it has to run
	// alongside the dialer.
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

// TestGuest_udp covers the datagram path, which uses the extension's own
// send/receive rather than the standard stream functions.
func TestGuest_udp(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer pc.Close()

	received := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			received <- "read: " + err.Error()
			return
		}
		received <- string(buf[:n])
		_, _ = pc.WriteTo([]byte("pong"), addr)
	}()

	out := runGuest(t, "udp", strconv.Itoa(port(t, pc.LocalAddr())))

	require.Equal(t, "ping", <-received)
	require.Contains(t, out, "sent 4")
	// The source address of the reply round-trips back through the V2 layout.
	require.Contains(t, out, "recv pong from 127.0.0.1 port_set=true")
}

// TestDetect covers picking the version from a module's imports, which is how
// an embedder decides what to export without asking the user.
func TestDetect(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	compiled := mustCompile(t, r, sockGuestWasm)
	require.Equal(t, wasmedge.V2, wasmedge.Detect(compiled.ImportedFunctions()))
	require.Equal(t, "v2", wasmedge.Detect(compiled.ImportedFunctions()).String())
}

// TestDetect_none covers a guest that needs no extension: a plain WASI module
// must not be given the socket functions.
func TestDetect_none(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	require.Equal(t, wasmedge.None, wasmedge.Detect(nil))
	require.Equal(t, "none", wasmedge.None.String())
}

func port(t *testing.T, addr net.Addr) int {
	t.Helper()
	switch a := addr.(type) {
	case *net.TCPAddr:
		return a.Port
	case *net.UDPAddr:
		return a.Port
	}
	t.Fatalf("unexpected address %v", addr)
	return 0
}

// waitForPort reads the "listening <port>" line the server guest prints.
func waitForPort(t *testing.T, out *syncBuffer) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(out.String(), "\n") {
			if rest, ok := strings.CutPrefix(line, "listening "); ok {
				p, err := strconv.Atoi(strings.TrimSpace(rest))
				require.NoError(t, err)
				return p
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("guest never reported a port; output was:\n%s", out.String())
	return 0
}

// TestGuest_peerAddr covers the peer address getter, which reports the
// endpoint the guest connected to.
func TestGuest_peerAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			time.Sleep(50 * time.Millisecond)
		}
	}()

	p := port(t, ln.Addr())
	out := runGuest(t, "peer", strconv.Itoa(p))
	require.Contains(t, out, "peer 127.0.0.1 port="+strconv.Itoa(p))
}

// TestGuest_sockOpts covers the option surface: an option the backend can
// serve, one it can answer without a raw socket, and one it must refuse.
func TestGuest_sockOpts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			time.Sleep(50 * time.Millisecond)
		}
	}()

	out := runGuest(t, "opts", strconv.Itoa(port(t, ln.Addr())))

	// SO_TYPE answers Stream, and SO_LINGER is refused with ENOTSUP (58)
	// rather than being silently accepted.
	require.Contains(t, out, "sotype 2")
	require.Contains(t, out, "linger errno 58")
}

// TestGuest_addrInfo covers sock_getaddrinfo, whose results go into a linked
// list the guest allocated. The guest sets the sockaddr length to 14, as
// WasmEdge's own library does while allocating 26 bytes, so the host's
// correction for that is exercised too.
func TestGuest_addrInfo(t *testing.T) {
	out := runGuest(t, "addrinfo")

	require.Contains(t, out, "addrinfo n=1")
	require.Contains(t, out, "family=1") // AF_INET, WasmEdge's numbering
	require.Contains(t, out, "port=80")  // resolved from the service
	require.Contains(t, out, "addr=127.0.0.1")
}

package instance

import (
	"bytes"
	"context"
	_ "embed"
	"net"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
)

// real_tcp.component.wasm is a genuine rustc wasm32-wasip2 component built
// from:
//
//	let mut stream = TcpStream::connect(&addr).expect("connect failed");
//	stream.write_all(msg.as_bytes()).expect("write failed");
//	let mut buf = Vec::new();
//	stream.read_to_end(&mut buf).expect("read failed");
//	print!("{}", String::from_utf8_lossy(&buf));
//
// (addr and msg come from argv[1]/argv[2] -- see WASIConfig.Args below).
// Confirmed to work end-to-end under a real `wasmtime run -S
// inherit-network` against a scratch Go TCP server before any of wazy's own
// wasi:sockets host implementation existed (see wasi_sockets.go's package
// doc), proving std::net genuinely functions on wasm32-wasip2 and that this
// fixture's compiled glue is a legitimate reference for what a real guest
// calls. TestRealTCP is this milestone's proof: the same component, run
// entirely through wazy's own host (no wasmtime involved), really connects
// over a real net.Conn to a real Go TCP server this test starts, writes
// real guest-computed bytes, and reads back the server's real reply.
//
//go:embed testdata/real_tcp.component.wasm
var realTCPWasm []byte

// runFixedReplyServer starts a TCP server on 127.0.0.1:0 that accepts
// exactly one connection, reads whatever the client sends in a single Read
// call (sufficient here: real_tcp's guest sends its whole payload in one
// write_all, which loopback TCP delivers in one Read for payloads this
// small -- see this file's package doc), and writes back prefix+received
// before closing the connection. Closing is required for the guest to ever
// return: std::io::Read::read_to_end blocks until the peer (this server)
// closes its side. Returns the listener address to connect to and a
// channel that receives exactly what the server read, once the one
// expected connection has been handled.
func runFixedReplyServer(t *testing.T, prefix string) (addr string, received <-chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	ch := make(chan []byte, 1)
	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			close(ch)
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Errorf("SetDeadline: %v", err)
		}
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		got := append([]byte(nil), buf[:n]...)
		ch <- got
		if _, err := conn.Write([]byte(prefix + string(got))); err != nil {
			t.Errorf("server write: %v", err)
		}
	}()
	return ln.Addr().String(), ch
}

// TestRealTCP is the milestone's behavioral proof (network output isn't
// golden-diffable against wasmtime, so correctness is asserted against a
// fixed, deterministic Go server's own behavior instead -- see this
// package's task doc). Two sub-tests exchange genuinely different payloads
// and expect genuinely different server replies, proving this is real
// socket data flow through wazy's own wasi:sockets/wasi:io/poll host
// implementation (wasi_sockets.go), not a hardcoded response.
func TestRealTCP(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		prefix  string
	}{
		{name: "first", payload: "hello from wazy guest", prefix: "ECHO:"},
		{name: "second_different_data", payload: "a totally different payload, 12345!", prefix: "REPLY-"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			addr, received := runFixedReplyServer(t, tc.prefix)

			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)

			var stdout, stderr bytes.Buffer
			inst, err := Instantiate(ctx, r, realTCPWasm, WithWASI(WASIConfig{
				Stdout:   &stdout,
				Stderr:   &stderr,
				AllowTCP: true,
				Args:     []string{addr, tc.payload},
			})...)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			defer inst.Close(ctx)

			if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
				t.Fatalf("Call run(): %v (stdout so far: %q, stderr: %q)", err, stdout.String(), stderr.String())
			}

			var gotServer []byte
			select {
			case gotServer = <-received:
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for the server to report what it received")
			}
			if string(gotServer) != tc.payload {
				t.Fatalf("server received %q, want %q (the guest's actual sent bytes)", gotServer, tc.payload)
			}

			wantStdout := tc.prefix + tc.payload
			if stdout.String() != wantStdout {
				t.Fatalf("guest stdout = %q, want %q (the server's actual reply bytes, echoed by the guest -- stderr: %q)", stdout.String(), wantStdout, stderr.String())
			}
		})
	}
}

// TestRealTCP_DisallowedFailsLoud confirms WASIConfig.AllowTCP's gating: with
// it left false (the default), wasi:sockets/wasi:io/poll stay entirely
// unregistered, so the graph engine's own automatic trap-stub fallback fails
// the call loud, naming the specific WASI iface+func the guest first
// reached -- exactly the existing documented behavior for any WASI surface
// this package doesn't implement (see wasi.go's package doc), now verified
// for sockets specifically now that a real implementation exists to gate.
func TestRealTCP_DisallowedFailsLoud(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var stdout, stderr bytes.Buffer
	inst, err := Instantiate(ctx, r, realTCPWasm, WithWASI(WASIConfig{
		Stdout: &stdout,
		Stderr: &stderr,
		Args:   []string{"127.0.0.1:1", "unused"},
	})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "wasi:cli/run@0.2.3#run")
	if err == nil {
		t.Fatal("Call run(): expected an error (sockets disallowed), got nil")
	}
	t.Logf("run() failed as expected: %v", err)
	requireErrContains(t, err, "not implemented (trap stub)")
}

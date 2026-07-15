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

// real_udp.component.wasm is a genuine rustc wasm32-wasip2 component built
// from:
//
//	let sock = UdpSocket::bind("127.0.0.1:0").expect("bind failed");
//	sock.send_to(payload.as_bytes(), server_addr).expect("send_to failed");
//	let mut buf = [0u8; 4096];
//	let (n, _src) = sock.recv_from(&mut buf).expect("recv_from failed");
//	print!("{}", String::from_utf8_lossy(&buf[..n]));
//
// (server_addr and payload come from argv[1]/argv[2] -- see WASIConfig.Args
// below). Confirmed to work end-to-end under a real `wasmtime run -S
// inherit-network` against a scratch Go UDP server before any of wazy's own
// wasi:sockets/udp host implementation existed (see wasi_sockets.go's
// "wasi:sockets/udp" section doc), proving std::net::UdpSocket genuinely
// functions on wasm32-wasip2 (exactly as std::net::TcpStream already does,
// see real_tcp_test.go) and that this fixture's compiled glue is a
// legitimate reference for what a real UDP guest calls. TestRealUDP is this
// milestone's proof: the same component, run entirely through wazy's own
// host (no wasmtime involved), really binds a UDP socket, sends a real
// datagram over a real net.PacketConn to a real Go UDP server this test
// starts, and receives back the server's real reply datagram.
//
//go:embed testdata/real_udp.component.wasm
var realUDPWasm []byte

// runFixedReplyUDPServer starts a UDP "server" on 127.0.0.1:0 that reads
// exactly one datagram (sufficient here: real_udp's guest sends its whole
// payload in one send_to call, which loopback UDP delivers as one datagram
// for payloads this small -- see this file's package doc) and replies with
// prefix+received to whichever address it came from. Returns the socket's
// address to send to and a channel that receives exactly what the server
// read, once the one expected datagram has been handled.
func runFixedReplyUDPServer(t *testing.T, prefix string) (addr string, received <-chan []byte) {
	t.Helper()
	udpAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.ResolveUDPAddr: %v", err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		t.Fatalf("net.ListenUDP: %v", err)
	}
	ch := make(chan []byte, 1)
	go func() {
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Errorf("SetDeadline: %v", err)
		}
		buf := make([]byte, 4096)
		n, raddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			close(ch)
			return
		}
		got := append([]byte(nil), buf[:n]...)
		ch <- got
		if _, err := conn.WriteToUDP([]byte(prefix+string(got)), raddr); err != nil {
			t.Errorf("server write: %v", err)
		}
	}()
	return conn.LocalAddr().String(), ch
}

// TestRealUDP is the milestone's behavioral proof (network output isn't
// golden-diffable against wasmtime, so correctness is asserted against a
// fixed, deterministic Go server's own behavior instead -- mirrors
// TestRealTCP's own doc). Two sub-tests exchange genuinely different
// payloads and expect genuinely different server replies, proving this is
// real datagram data flow through wazy's own wasi:sockets/udp + wasi:io/poll
// host implementation (wasi_sockets.go), not a hardcoded response.
func TestRealUDP(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		prefix  string
	}{
		{name: "first", payload: "hello from wazy udp guest", prefix: "ECHO:"},
		{name: "second_different_data", payload: "a totally different datagram, 67890!", prefix: "REPLY-"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			addr, received := runFixedReplyUDPServer(t, tc.prefix)

			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)

			var stdout, stderr bytes.Buffer
			inst, err := Instantiate(ctx, r, realUDPWasm, WithWASI(WASIConfig{
				Stdout:   &stdout,
				Stderr:   &stderr,
				AllowUDP: true,
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
				t.Fatalf("server received %q, want %q (the guest's actual sent datagram)", gotServer, tc.payload)
			}

			wantStdout := tc.prefix + tc.payload
			if stdout.String() != wantStdout {
				t.Fatalf("guest stdout = %q, want %q (the server's actual reply datagram, echoed by the guest -- stderr: %q)", stdout.String(), wantStdout, stderr.String())
			}
		})
	}
}

// TestRealUDP_DisallowedFailsLoud confirms WASIConfig.AllowUDP's gating: with
// it left false (the default), wasi:sockets/udp stays entirely unregistered,
// so the graph engine's own automatic trap-stub fallback fails the call
// loud, naming the specific WASI iface+func the guest first reached --
// mirrors TestRealTCP_DisallowedFailsLoud's identical proof for AllowTCP.
func TestRealUDP_DisallowedFailsLoud(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var stdout, stderr bytes.Buffer
	inst, err := Instantiate(ctx, r, realUDPWasm, WithWASI(WASIConfig{
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
		t.Fatal("Call run(): expected an error (UDP sockets disallowed), got nil")
	}
	t.Logf("run() failed as expected: %v", err)
	requireErrContains(t, err, "not implemented (trap stub)")
}

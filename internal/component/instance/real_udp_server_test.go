package instance

import (
	"bytes"
	"context"
	_ "embed"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
)

// real_udp_server.component.wasm is a genuine rustc wasm32-wasip2 component
// built from:
//
//	let sock = UdpSocket::bind("127.0.0.1:0").expect("bind");
//	println!("listening on {}", sock.local_addr().unwrap());
//	let (n, src) = sock.recv_from(&mut buf).expect("recv_from");
//	sock.send_to(to_uppercase(&buf[..n]).as_bytes(), src).expect("send_to");
//
// It exercises the UDP SERVER path: bind + local-address + receive-from-anyone
// + send-to-sender. Confirmed under `wasmtime run -S inherit-network`.
//
//go:embed testdata/real_udp_server.component.wasm
var realUDPServerWasm []byte

// TestRealUDPServer proves the UDP bind-and-serve path through wazy's own host:
// a real guest binds a UDP socket, a Go client sends it a datagram, and the
// guest echoes it back uppercased to the sender. Two payloads prove real data
// flow. The injected ListenPacket reports the guest's bound ephemeral :0 port
// so the client knows where to send; the guest runs in a goroutine because
// recv_from blocks inside run().
func TestRealUDPServer(t *testing.T) {
	for _, tc := range []struct{ payload, want string }{
		{"hello udp", "HELLO UDP"},
		{"MixedCase!42", "MIXEDCASE!42"},
	} {
		t.Run(tc.payload, func(t *testing.T) {
			ctx := context.Background()

			addrCh := make(chan string, 1)
			listenPacket := func(network, address string) (net.PacketConn, error) {
				pc, err := net.ListenPacket(network, address)
				if err == nil {
					addrCh <- pc.LocalAddr().String()
				}
				return pc, err
			}

			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)

			var stdout, stderr bytes.Buffer
			inst, err := Instantiate(ctx, r, realUDPServerWasm, WithWASI(WASIConfig{
				Stdout:       &stdout,
				Stderr:       &stderr,
				AllowUDP:     true,
				ListenPacket: listenPacket,
			})...)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			defer inst.Close(ctx)

			runErr := make(chan error, 1)
			go func() {
				_, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run")
				runErr <- err
			}()

			var addr string
			select {
			case addr = <-addrCh:
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for the guest to bind")
			}

			conn, err := net.Dial("udp", addr)
			if err != nil {
				t.Fatalf("dial udp %s: %v", addr, err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write([]byte(tc.payload)); err != nil {
				t.Fatalf("send datagram: %v", err)
			}
			reply := make([]byte, 512)
			n, err := conn.Read(reply)
			if err != nil {
				t.Fatalf("read echo: %v", err)
			}
			if got := string(reply[:n]); got != tc.want {
				t.Fatalf("echo = %q, want %q (stdout: %q, stderr: %q)", got, tc.want, stdout.String(), stderr.String())
			}

			select {
			case err := <-runErr:
				if err != nil {
					t.Fatalf("Call run(): %v (stdout: %q, stderr: %q)", err, stdout.String(), stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for run() to return")
			}
			if out := stdout.String(); !strings.Contains(out, "listening on 127.0.0.1:") {
				t.Fatalf("stdout = %q, want it to report the bound address", out)
			}
		})
	}
}

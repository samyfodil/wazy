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

// real_tcp_listen.component.wasm is a genuine rustc wasm32-wasip2 component
// built from:
//
//	let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
//	let addr = listener.local_addr().expect("local_addr");
//	println!("listening on {}", addr);
//	let (mut stream, peer) = listener.accept().expect("accept");
//	println!("accepted from {}", peer);
//	let mut buf = [0u8; 512];
//	let n = stream.read(&mut buf).unwrap_or(0);
//	let up = String::from_utf8_lossy(&buf[..n]).to_uppercase();
//	stream.write_all(up.as_bytes());
//
// Confirmed to work end-to-end under a real `wasmtime run -S inherit-network`
// (bind→accept→echo-uppercase) before wazy's server-side wasi:sockets host
// existed, proving std::net::TcpListener genuinely functions on wasm32-wasip2
// and that this fixture is a legitimate reference for what a real listening
// guest calls (create-tcp-socket → start/finish-bind → start/finish-listen →
// local-address → accept → remote-address → stream read/write).
//
//go:embed testdata/real_tcp_listen.component.wasm
var realTCPListenWasm []byte

// TestRealTCPListen is the server-side milestone's behavioral proof (the
// mirror image of TestRealTCP): a real guest LISTENS through wazy's own
// wasi:sockets host, this test connects to it as a client, and the guest
// echoes back the payload uppercased. Two sub-tests send genuinely different
// payloads and expect the matching uppercased reply, proving real socket data
// flow (not a hardcoded response). The guest blocks in accept() inside run(),
// so run() executes in a goroutine while the test dials the address the
// injected Listen hook reports.
func TestRealTCPListen(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "first", payload: "hello world", want: "HELLO WORLD"},
		{name: "second_different_data", payload: "MixedCase 123!", want: "MIXEDCASE 123!"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			// The Listen hook wraps a real net.Listen but reports the bound
			// address (the guest binds to :0, so only the host knows the
			// ephemeral port) so the test can connect as the client.
			addrCh := make(chan string, 1)
			listen := func(network, address string) (net.Listener, error) {
				ln, err := net.Listen(network, address)
				if err == nil {
					addrCh <- ln.Addr().String()
				}
				return ln, err
			}

			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)

			var stdout, stderr bytes.Buffer
			inst, err := Instantiate(ctx, r, realTCPListenWasm, WithWASI(WASIConfig{
				Stdout:   &stdout,
				Stderr:   &stderr,
				AllowTCP: true,
				Listen:   listen,
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
				t.Fatal("timed out waiting for the guest to bind (Listen hook never fired)")
			}

			conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
			if err != nil {
				t.Fatalf("dialing the guest at %s: %v", addr, err)
			}
			if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatalf("SetDeadline: %v", err)
			}
			if _, err := conn.Write([]byte(tc.payload)); err != nil {
				t.Fatalf("writing payload to guest: %v", err)
			}
			// Half-close so the guest's single read sees the whole payload and
			// then EOF; without this a larger read could block.
			if tcp, ok := conn.(*net.TCPConn); ok {
				if err := tcp.CloseWrite(); err != nil {
					t.Fatalf("CloseWrite: %v", err)
				}
			}
			reply := make([]byte, 512)
			n, _ := conn.Read(reply)
			conn.Close()
			if got := string(reply[:n]); got != tc.want {
				t.Fatalf("guest echoed %q, want %q (stdout: %q, stderr: %q)", got, tc.want, stdout.String(), stderr.String())
			}

			select {
			case err := <-runErr:
				if err != nil {
					t.Fatalf("Call run(): %v (stdout: %q, stderr: %q)", err, stdout.String(), stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for run() to return after the exchange")
			}

			// The guest's own stdout proves it went through the real
			// bind→local-address→accept→remote-address path (the port is the
			// ephemeral one the host chose, so only the prefixes are asserted).
			if out := stdout.String(); !strings.Contains(out, "listening on 127.0.0.1:") || !strings.Contains(out, "accepted from 127.0.0.1:") {
				t.Fatalf("guest stdout = %q, want it to report listening + accepted (stderr: %q)", out, stderr.String())
			}
		})
	}
}

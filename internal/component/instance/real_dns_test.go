package instance

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
)

// real_dns.component.wasm is a genuine rustc wasm32-wasip2 component built from:
//
//	let (host, port) = (argv[1], argv[2]);
//	for a in (host, port).to_socket_addrs()? { println!("resolved={}", a.ip()); n += 1; }
//	println!("count={}", n);
//
// std::net::ToSocketAddrs drives wasi:sockets/ip-name-lookup.resolve-addresses
// + resolve-address-stream.resolve-next-address. Confirmed under
// `wasmtime run -S allow-ip-name-lookup` to print resolved=127.0.0.1 / count=1
// for localhost before wazy's ip-name-lookup host existed.
//
//go:embed testdata/real_dns.component.wasm
var realDNSWasm []byte

// TestRealDNS proves wazy's wasi:sockets/ip-name-lookup host. A ResolveIP hook
// returns a fixed set of IPs so the guest's printed addresses are
// deterministically assertable without touching real DNS -- proving the
// resolved addresses flow from the host resolver through the ABI, not a
// constant (two cases return different IP sets and expect matching output).
func TestRealDNS(t *testing.T) {
	cases := []struct {
		name string
		ips  []net.IP
		want []string
	}{
		{
			name: "two_ipv4",
			ips:  []net.IP{net.IPv4(93, 184, 216, 34), net.IPv4(10, 0, 0, 1)},
			want: []string{"resolved=93.184.216.34", "resolved=10.0.0.1", "count=2"},
		},
		{
			name: "ipv4_and_ipv6",
			ips:  []net.IP{net.IPv4(1, 2, 3, 4), net.ParseIP("2606:2800:220:1:248:1893:25c8:1946")},
			want: []string{"resolved=1.2.3.4", "resolved=2606:2800:220:1:248:1893:25c8:1946", "count=2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)

			var stdout, stderr bytes.Buffer
			inst, err := Instantiate(ctx, r, realDNSWasm, WithWASI(WASIConfig{
				Stdout:   &stdout,
				Stderr:   &stderr,
				AllowTCP: true,
				Args:     []string{"example.test", "80"},
				ResolveIP: func(ctx context.Context, name string) ([]net.IP, error) {
					if name != "example.test" {
						return nil, errors.New("unexpected host: " + name)
					}
					return tc.ips, nil
				},
			})...)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			defer inst.Close(ctx)

			if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
				t.Fatalf("Call run(): %v (stdout: %q, stderr: %q)", err, stdout.String(), stderr.String())
			}
			for _, w := range tc.want {
				if !strings.Contains(stdout.String(), w) {
					t.Fatalf("guest stdout = %q, want it to contain %q", stdout.String(), w)
				}
			}
		})
	}
}

// TestRealDNS_LookupFailure proves the Err path: a resolver error makes
// resolve-addresses report error-code::name-unresolvable, which Rust surfaces
// as a failed to_socket_addrs (the guest prints error=... and count=0).
func TestRealDNS_LookupFailure(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var stdout, stderr bytes.Buffer
	inst, err := Instantiate(ctx, r, realDNSWasm, WithWASI(WASIConfig{
		Stdout:   &stdout,
		Stderr:   &stderr,
		AllowTCP: true,
		Args:     []string{"nope.invalid", "80"},
		ResolveIP: func(ctx context.Context, name string) ([]net.IP, error) {
			return nil, errors.New("no such host")
		},
	})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
		t.Fatalf("Call run(): %v (stdout: %q, stderr: %q)", err, stdout.String(), stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "count=0") {
		t.Fatalf("guest stdout = %q, want count=0 on lookup failure", out)
	}
}

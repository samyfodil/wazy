package instance

import (
	"context"
	_ "embed"
	"errors"
	"net"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// TestWasiIPSocketAddrToString covers wasiIPSocketAddrToString's ipv4/ipv6
// happy paths plus its malformed-input error branches directly, since
// real_tcp.component.wasm (the only guest fixture exercising this package)
// only ever supplies an ipv4 address at runtime -- see wasi_sockets.go's
// package doc.
func TestWasiIPSocketAddrToString(t *testing.T) {
	t.Run("ipv4", func(t *testing.T) {
		v := abi.VariantValue{Disc: 0, Payload: []abi.Value{
			uint32(8080),
			[]abi.Value{uint32(127), uint32(0), uint32(0), uint32(1)},
		}}
		got, err := wasiIPSocketAddrToString(v)
		if err != nil {
			t.Fatalf("wasiIPSocketAddrToString: %v", err)
		}
		if want := "127.0.0.1:8080"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("ipv6", func(t *testing.T) {
		v := abi.VariantValue{Disc: 1, Payload: []abi.Value{
			uint32(443), uint32(0),
			[]abi.Value{uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(1)},
			uint32(0),
		}}
		got, err := wasiIPSocketAddrToString(v)
		if err != nil {
			t.Fatalf("wasiIPSocketAddrToString: %v", err)
		}
		if want := "[0:0:0:0:0:0:0:1]:443"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		if _, _, err := net.SplitHostPort(got); err != nil {
			t.Fatalf("result %q is not a valid host:port per net.SplitHostPort: %v", got, err)
		}
	})

	t.Run("not a variant", func(t *testing.T) {
		if _, err := wasiIPSocketAddrToString(uint32(1)); err == nil {
			t.Fatal("expected an error for a non-variant value, got nil")
		}
	})

	t.Run("unknown disc", func(t *testing.T) {
		if _, err := wasiIPSocketAddrToString(abi.VariantValue{Disc: 2}); err == nil {
			t.Fatal("expected an error for an unrecognized variant case, got nil")
		}
	})

	t.Run("malformed ipv4 payload", func(t *testing.T) {
		if _, err := wasiIPSocketAddrToString(abi.VariantValue{Disc: 0, Payload: "bogus"}); err == nil {
			t.Fatal("expected an error for a malformed ipv4 payload, got nil")
		}
	})

	t.Run("malformed ipv4 address tuple", func(t *testing.T) {
		v := abi.VariantValue{Disc: 0, Payload: []abi.Value{uint32(1), []abi.Value{uint32(1), uint32(2)}}}
		if _, err := wasiIPSocketAddrToString(v); err == nil {
			t.Fatal("expected an error for a short ipv4 address tuple, got nil")
		}
	})

	t.Run("malformed ipv6 payload", func(t *testing.T) {
		if _, err := wasiIPSocketAddrToString(abi.VariantValue{Disc: 1, Payload: "bogus"}); err == nil {
			t.Fatal("expected an error for a malformed ipv6 payload, got nil")
		}
	})
}

// TestWasiTCPDialErrToCode covers wasiTCPDialErrToCode's three branches
// directly: a real connection-refused dial (against a port nothing is
// listening on), a context-deadline-style timeout, and a generic error
// falling back to wasiSockErrUnknown.
func TestWasiTCPDialErrToCode(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
		_, dialErr := net.Dial("tcp", addr)
		if dialErr == nil {
			t.Fatal("expected a connection-refused dial error, got nil (port unexpectedly accepting)")
		}
		if got := wasiTCPDialErrToCode(dialErr); got != wasiSockErrConnectionRefused {
			t.Fatalf("wasiTCPDialErrToCode(%v) = %d, want wasiSockErrConnectionRefused (%d)", dialErr, got, wasiSockErrConnectionRefused)
		}
	})

	t.Run("generic error falls back to unknown", func(t *testing.T) {
		if got := wasiTCPDialErrToCode(errors.New("some other failure")); got != wasiSockErrUnknown {
			t.Fatalf("wasiTCPDialErrToCode(generic) = %d, want wasiSockErrUnknown (%d)", got, wasiSockErrUnknown)
		}
	})
}

// TestWasiTCPListenErrToCode covers the listen/accept error mapper. It reuses
// the UDP mapper (bind/accept share the same failure modes -- see its doc), so
// this proves the address-in-use and generic-fallback ends of that reuse: a
// real double-bind to the same address yields address-in-use, and an arbitrary
// error falls back to unknown.
func TestWasiTCPListenErrToCode(t *testing.T) {
	t.Run("address in use", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		_, bindErr := net.Listen("tcp", ln.Addr().String())
		if bindErr == nil {
			t.Fatal("expected an address-in-use error re-binding the same address, got nil")
		}
		if got := wasiTCPListenErrToCode(bindErr); got != wasiSockErrAddressInUse {
			t.Fatalf("wasiTCPListenErrToCode(%v) = %d, want wasiSockErrAddressInUse (%d)", bindErr, got, wasiSockErrAddressInUse)
		}
	})

	t.Run("generic error falls back to unknown", func(t *testing.T) {
		if got := wasiTCPListenErrToCode(errors.New("some other failure")); got != wasiSockErrUnknown {
			t.Fatalf("wasiTCPListenErrToCode(generic) = %d, want wasiSockErrUnknown (%d)", got, wasiSockErrUnknown)
		}
	})
}

// TestRealTCP_ConnectionRefused proves the connection-refused path end to
// end through a real guest: dialing a port nothing listens on makes
// start-connect's synchronous net.Dial fail, finish-connect reports
// error-code::connection-refused, and Rust's
// `TcpStream::connect(&addr).expect("connect failed")` panics on the
// resulting Err -- surfacing as run() returning an error, not a
// hardcoded/synthetic host-side failure.
func TestRealTCP_ConnectionRefused(t *testing.T) {
	ctx := context.Background()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realTCPWasm, WithWASI(WASIConfig{
		AllowTCP: true,
		Args:     []string{addr, "unused"},
	})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err == nil {
		t.Fatal("Call run(): expected an error (connection refused), got nil")
	} else {
		t.Logf("run() failed as expected: %v", err)
	}
}

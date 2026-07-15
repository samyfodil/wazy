package instance

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/samyfodil/wazy/internal/component/abi"
)

// TestWasiIPSocketAddrFromUDPAddr covers wasiIPSocketAddrFromUDPAddr's ipv4
// and ipv6 branches directly, since real_udp.component.wasm (the only guest
// fixture exercising this package's UDP path) only ever binds/receives over
// ipv4 loopback at runtime -- mirrors TestWasiIPSocketAddrToString's
// identical rationale for the reverse conversion.
func TestWasiIPSocketAddrFromUDPAddr(t *testing.T) {
	t.Run("ipv4", func(t *testing.T) {
		addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4242}
		v, err := wasiIPSocketAddrFromUDPAddr(addr)
		if err != nil {
			t.Fatalf("wasiIPSocketAddrFromUDPAddr: %v", err)
		}
		vv, ok := v.(abi.VariantValue)
		if !ok || vv.Disc != 0 {
			t.Fatalf("got %#v, want an ipv4 (disc 0) variant", v)
		}
		rec, ok := vv.Payload.([]abi.Value)
		if !ok || len(rec) != 2 {
			t.Fatalf("payload = %#v, want a 2-field record", vv.Payload)
		}
		if rec[0] != uint32(4242) {
			t.Fatalf("port = %#v, want 4242", rec[0])
		}
		octets, ok := rec[1].([]abi.Value)
		if !ok || len(octets) != 4 {
			t.Fatalf("address = %#v, want a 4-tuple", rec[1])
		}
		want := []abi.Value{uint32(127), uint32(0), uint32(0), uint32(1)}
		for i := range want {
			if octets[i] != want[i] {
				t.Fatalf("address[%d] = %#v, want %#v", i, octets[i], want[i])
			}
		}

		// round-trip through wasiIPSocketAddrToString, proving both
		// directions agree on the same wire shape.
		s, err := wasiIPSocketAddrToString(v)
		if err != nil {
			t.Fatalf("wasiIPSocketAddrToString(round-trip): %v", err)
		}
		if want := "127.0.0.1:4242"; s != want {
			t.Fatalf("round-trip = %q, want %q", s, want)
		}
	})

	t.Run("ipv6", func(t *testing.T) {
		addr := &net.UDPAddr{IP: net.ParseIP("::1"), Port: 9999}
		v, err := wasiIPSocketAddrFromUDPAddr(addr)
		if err != nil {
			t.Fatalf("wasiIPSocketAddrFromUDPAddr: %v", err)
		}
		vv, ok := v.(abi.VariantValue)
		if !ok || vv.Disc != 1 {
			t.Fatalf("got %#v, want an ipv6 (disc 1) variant", v)
		}
		s, err := wasiIPSocketAddrToString(v)
		if err != nil {
			t.Fatalf("wasiIPSocketAddrToString(round-trip): %v", err)
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			t.Fatalf("round-trip %q is not a valid host:port: %v", s, err)
		}
	})

	t.Run("invalid IP", func(t *testing.T) {
		addr := &net.UDPAddr{IP: net.IP{1, 2, 3}, Port: 1}
		if _, err := wasiIPSocketAddrFromUDPAddr(addr); err == nil {
			t.Fatal("expected an error for a malformed IP, got nil")
		}
	})
}

// TestWasiUDPErrToCode covers wasiUDPErrToCode's branches directly: a real
// address-in-use bind failure (two listeners on the same address), and a
// generic error falling back to wasiSockErrUnknown -- mirrors
// TestWasiTCPDialErrToCode's identical rationale.
func TestWasiUDPErrToCode(t *testing.T) {
	t.Run("address in use", func(t *testing.T) {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		_, bindErr := net.ListenPacket("udp", pc.LocalAddr().String())
		if bindErr == nil {
			t.Fatal("expected an address-in-use bind error, got nil")
		}
		if got := wasiUDPErrToCode(bindErr); got != wasiSockErrAddressInUse {
			t.Fatalf("wasiUDPErrToCode(%v) = %d, want wasiSockErrAddressInUse (%d)", bindErr, got, wasiSockErrAddressInUse)
		}
	})

	t.Run("generic error falls back to unknown", func(t *testing.T) {
		if got := wasiUDPErrToCode(errors.New("some other failure")); got != wasiSockErrUnknown {
			t.Fatalf("wasiUDPErrToCode(generic) = %d, want wasiSockErrUnknown (%d)", got, wasiSockErrUnknown)
		}
	})
}

// TestIncomingDatagramStreamReceive covers incomingDatagramStream.receive
// directly: a zero max-results immediate empty result, a real datagram
// delivered from an unconnected stream, a real datagram filtered out by a
// connected stream's remote mismatch (proving the discard-and-keep-waiting
// loop in receive's own doc), and a closed-socket error path.
func TestIncomingDatagramStreamReceive(t *testing.T) {
	t.Run("max-results zero returns immediately", func(t *testing.T) {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		s := &incomingDatagramStream{pconn: pc}
		got, err := s.receive(0)
		if err != nil {
			t.Fatalf("receive(0): %v", err)
		}
		rv, ok := got[0].(abi.ResultValue)
		if !ok || rv.IsErr {
			t.Fatalf("receive(0) = %#v, want Ok", got[0])
		}
		list, ok := rv.Payload.([]abi.Value)
		if !ok || len(list) != 0 {
			t.Fatalf("receive(0) payload = %#v, want an empty list", rv.Payload)
		}
	})

	t.Run("real datagram, unconnected", func(t *testing.T) {
		serverPC, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer serverPC.Close()
		clientPC, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer clientPC.Close()

		if _, err := clientPC.WriteTo([]byte("payload"), serverPC.LocalAddr()); err != nil {
			t.Fatal(err)
		}

		s := &incomingDatagramStream{pconn: serverPC}
		got, err := s.receive(1)
		if err != nil {
			t.Fatalf("receive(1): %v", err)
		}
		rv, ok := got[0].(abi.ResultValue)
		if !ok || rv.IsErr {
			t.Fatalf("receive(1) = %#v, want Ok", got[0])
		}
		list, ok := rv.Payload.([]abi.Value)
		if !ok || len(list) != 1 {
			t.Fatalf("receive(1) payload = %#v, want a 1-element list", rv.Payload)
		}
		rec, ok := list[0].([]abi.Value)
		if !ok || len(rec) != 2 {
			t.Fatalf("datagram = %#v, want a 2-field record", list[0])
		}
		data, err := wasiBytesFromList(rec[0])
		if err != nil {
			t.Fatalf("datagram.data: %v", err)
		}
		if string(data) != "payload" {
			t.Fatalf("datagram.data = %q, want %q", data, "payload")
		}
	})

	t.Run("filters to connected peer, discarding others", func(t *testing.T) {
		serverPC, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer serverPC.Close()

		stranger, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer stranger.Close()
		peer, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer peer.Close()

		if _, err := stranger.WriteTo([]byte("from stranger"), serverPC.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		if _, err := peer.WriteTo([]byte("from peer"), serverPC.LocalAddr()); err != nil {
			t.Fatal(err)
		}

		peerAddr, err := net.ResolveUDPAddr("udp", peer.LocalAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		s := &incomingDatagramStream{pconn: serverPC, remote: peerAddr}
		got, err := s.receive(1)
		if err != nil {
			t.Fatalf("receive(1): %v", err)
		}
		rv := got[0].(abi.ResultValue)
		list := rv.Payload.([]abi.Value)
		rec := list[0].([]abi.Value)
		data, err := wasiBytesFromList(rec[0])
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "from peer" {
			t.Fatalf("datagram.data = %q, want %q (the stranger's datagram should have been discarded)", data, "from peer")
		}
	})

	t.Run("closed socket reports an error", func(t *testing.T) {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if err := pc.Close(); err != nil {
			t.Fatal(err)
		}
		s := &incomingDatagramStream{pconn: pc}
		got, err := s.receive(1)
		if err != nil {
			t.Fatalf("receive(1): %v", err)
		}
		rv, ok := got[0].(abi.ResultValue)
		if !ok || !rv.IsErr {
			t.Fatalf("receive(1) on a closed socket = %#v, want an Err", got[0])
		}
	})
}

// TestOutgoingDatagramStreamSend covers outgoingDatagramStream.send
// directly: an explicit per-datagram remote-address, a connected stream's
// default remote-address (datagram omits its own), an empty datagram list
// (Ok(0), per send's own doc), a malformed record, and the "no remote and
// unconnected" invalid-argument-shaped failure.
func TestOutgoingDatagramStreamSend(t *testing.T) {
	newPeer := func(t *testing.T) (net.PacketConn, *net.UDPAddr) {
		t.Helper()
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pc.Close() })
		addr, err := net.ResolveUDPAddr("udp", pc.LocalAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		return pc, addr
	}

	t.Run("explicit remote-address per datagram", func(t *testing.T) {
		clientPC, _ := newPeer(t)
		serverPC, serverAddr := newPeer(t)

		remoteVal, err := wasiIPSocketAddrFromUDPAddr(serverAddr)
		if err != nil {
			t.Fatal(err)
		}
		s := &outgoingDatagramStream{pconn: clientPC}
		datagrams := []abi.Value{
			[]abi.Value{wasiListFromBytes([]byte("hi")), remoteVal},
		}
		got, err := s.send(datagrams)
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		rv, ok := got[0].(abi.ResultValue)
		if !ok || rv.IsErr {
			t.Fatalf("send = %#v, want Ok", got[0])
		}
		if rv.Payload != uint64(1) {
			t.Fatalf("send count = %#v, want 1", rv.Payload)
		}

		buf := make([]byte, 16)
		if err := serverPC.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		n, _, err := serverPC.ReadFrom(buf)
		if err != nil {
			t.Fatalf("server ReadFrom: %v", err)
		}
		if string(buf[:n]) != "hi" {
			t.Fatalf("server received %q, want %q", buf[:n], "hi")
		}
	})

	t.Run("default remote-address from connected stream", func(t *testing.T) {
		clientPC, _ := newPeer(t)
		serverPC, serverAddr := newPeer(t)

		s := &outgoingDatagramStream{pconn: clientPC, remote: serverAddr}
		datagrams := []abi.Value{
			[]abi.Value{wasiListFromBytes([]byte("bye")), nil},
		}
		got, err := s.send(datagrams)
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		rv := got[0].(abi.ResultValue)
		if rv.IsErr || rv.Payload != uint64(1) {
			t.Fatalf("send = %#v, want Ok(1)", got[0])
		}

		buf := make([]byte, 16)
		if err := serverPC.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		n, _, err := serverPC.ReadFrom(buf)
		if err != nil {
			t.Fatalf("server ReadFrom: %v", err)
		}
		if string(buf[:n]) != "bye" {
			t.Fatalf("server received %q, want %q", buf[:n], "bye")
		}
	})

	t.Run("empty datagram list", func(t *testing.T) {
		pc, _ := newPeer(t)
		s := &outgoingDatagramStream{pconn: pc}
		got, err := s.send(nil)
		if err != nil {
			t.Fatalf("send(nil): %v", err)
		}
		rv := got[0].(abi.ResultValue)
		if rv.IsErr || rv.Payload != uint64(0) {
			t.Fatalf("send(nil) = %#v, want Ok(0)", got[0])
		}
	})

	t.Run("malformed record", func(t *testing.T) {
		pc, _ := newPeer(t)
		s := &outgoingDatagramStream{pconn: pc}
		if _, err := s.send([]abi.Value{uint32(1)}); err == nil {
			t.Fatal("expected an error for a malformed datagram record, got nil")
		}
	})

	t.Run("no remote-address and unconnected", func(t *testing.T) {
		pc, _ := newPeer(t)
		s := &outgoingDatagramStream{pconn: pc}
		datagrams := []abi.Value{
			[]abi.Value{wasiListFromBytes([]byte("x")), nil},
		}
		got, err := s.send(datagrams)
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		rv, ok := got[0].(abi.ResultValue)
		if !ok || !rv.IsErr {
			t.Fatalf("send with no remote-address and no default = %#v, want an Err", got[0])
		}
	})
}

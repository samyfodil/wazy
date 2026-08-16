package wasmedge

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasip1"
	"github.com/samyfodil/wazy/sys"
)

// These drive the socket backend directly, for the arms a guest cannot easily
// reach: the families and socket types the wire supports, and every failure
// path. The real guests in the other tests prove the wiring.

func TestNewSocket(t *testing.T) {
	tests := []struct {
		name             string
		family, sockType int32
		expectedErrno    wasip1.Errno
		expectedNetwork  string
	}{
		{name: "tcp4", family: addressFamilyInet4, sockType: socketTypeStream, expectedNetwork: "tcp4"},
		{name: "tcp6", family: addressFamilyInet6, sockType: socketTypeStream, expectedNetwork: "tcp6"},
		{name: "udp4", family: addressFamilyInet4, sockType: socketTypeDatagram, expectedNetwork: "udp4"},
		{name: "udp6", family: addressFamilyInet6, sockType: socketTypeDatagram, expectedNetwork: "udp6"},
		{name: "unix", family: addressFamilyUnix, sockType: socketTypeStream, expectedNetwork: "unix"},
		{name: "unixgram", family: addressFamilyUnix, sockType: socketTypeDatagram, expectedNetwork: "unixgram"},
		{
			// An unspecified family defaults to IPv4, which is what a guest
			// that leaves it zero expects.
			name: "unspecified family", family: addressFamilyUnspec, sockType: socketTypeStream,
			expectedNetwork: "tcp4",
		},
		{
			// Likewise an unspecified type is a stream.
			name: "any type", family: addressFamilyInet4, sockType: socketTypeAny,
			expectedNetwork: "tcp4",
		},
		{name: "bad family", family: 99, sockType: socketTypeStream, expectedErrno: wasip1.ErrnoAfnosupport},
		{name: "bad type", family: addressFamilyInet4, sockType: 99, expectedErrno: wasip1.ErrnoInval},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, errno := newSocket(tc.family, tc.sockType)
			if tc.expectedErrno != wasip1.ErrnoSuccess {
				require.Equal(t, tc.expectedErrno, errno)
				require.Nil(t, s)
				return
			}
			require.Equal(t, wasip1.ErrnoSuccess, errno)
			require.Equal(t, tc.expectedNetwork, s.network())
		})
	}
}

func TestSocket_tcpRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 8)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(buf[:n])
	}()

	s, errno := newSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()

	addr := ln.Addr().(*net.TCPAddr)
	require.Equal(t, wasip1.ErrnoSuccess, s.connect("127.0.0.1", addr.Port))

	// Connecting twice is a guest error, not a silent re-dial.
	require.Equal(t, wasip1.ErrnoIsconn, s.connect("127.0.0.1", addr.Port))

	n, ferrno := s.Write([]byte("hi"))
	require.EqualErrno(t, 0, ferrno)
	require.Equal(t, 2, n)

	buf := make([]byte, 8)
	n, ferrno = s.Read(buf)
	require.EqualErrno(t, 0, ferrno)
	require.Equal(t, "hi", string(buf[:n]))

	// Both address getters report the connection.
	host, port, errno := s.localAddr()
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	require.Equal(t, "127.0.0.1", host)
	require.NotEqual(t, 0, port)

	host, port, errno = s.peerAddr()
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	require.Equal(t, "127.0.0.1", host)
	require.Equal(t, addr.Port, port)
}

func TestSocket_listenAccept(t *testing.T) {
	s, errno := newSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()

	require.Equal(t, wasip1.ErrnoSuccess, s.bind("127.0.0.1", 0))
	require.Equal(t, wasip1.ErrnoSuccess, s.listen(8))
	// Listening twice is a guest error.
	require.Equal(t, wasip1.ErrnoInval, s.listen(8))

	_, port, errno := s.localAddr()
	require.Equal(t, wasip1.ErrnoSuccess, errno)

	go func() {
		c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
		if err == nil {
			_, _ = c.Write([]byte("x"))
			time.Sleep(50 * time.Millisecond)
			c.Close()
		}
	}()

	conn, errno := s.accept()
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer conn.Close()

	buf := make([]byte, 4)
	n, ferrno := conn.Read(buf)
	require.EqualErrno(t, 0, ferrno)
	require.Equal(t, "x", string(buf[:n]))

	// The accepted connection has a peer; the listener does not.
	_, _, errno = conn.peerAddr()
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	_, _, errno = s.peerAddr()
	require.Equal(t, wasip1.ErrnoNotconn, errno)
}

// TestSocket_acceptNonblock covers the emulation of a non-blocking accept:
// with nothing pending it must report EAGAIN rather than block.
func TestSocket_acceptNonblock(t *testing.T) {
	s, errno := newSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()

	require.Equal(t, wasip1.ErrnoSuccess, s.bind("127.0.0.1", 0))
	require.Equal(t, wasip1.ErrnoSuccess, s.listen(1))
	require.EqualErrno(t, 0, s.SetNonblock(true))
	require.True(t, s.IsNonblock())

	_, errno = s.accept()
	require.Equal(t, wasip1.ErrnoAgain, errno)
}

// TestSocket_readNonblock covers the same for reads on a connected socket.
func TestSocket_readNonblock(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			time.Sleep(time.Second)
			c.Close()
		}
	}()

	s, errno := newSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()
	require.Equal(t, wasip1.ErrnoSuccess, s.connect("127.0.0.1", ln.Addr().(*net.TCPAddr).Port))

	require.EqualErrno(t, 0, s.SetNonblock(true))
	_, ferrno := s.Read(make([]byte, 8))
	require.EqualErrno(t, sys.EAGAIN, ferrno)
}

func TestSocket_udp(t *testing.T) {
	peer, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer peer.Close()

	go func() {
		buf := make([]byte, 16)
		n, addr, err := peer.ReadFrom(buf)
		if err == nil {
			_, _ = peer.WriteTo(buf[:n], addr)
		}
	}()

	s, errno := newSocket(addressFamilyInet4, socketTypeDatagram)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()

	// bind opens the socket immediately for datagrams, unlike a stream.
	require.Equal(t, wasip1.ErrnoSuccess, s.bind("127.0.0.1", 0))
	require.Equal(t, wasip1.ErrnoInval, s.bind("127.0.0.1", 0)) // already bound

	port := peer.LocalAddr().(*net.UDPAddr).Port
	n, errno := s.sendTo([]byte("ping"), "127.0.0.1", port)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	require.Equal(t, 4, n)

	buf := make([]byte, 16)
	n, host, srcPort, errno := s.recvFrom(buf)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	require.Equal(t, "ping", string(buf[:n]))
	require.Equal(t, "127.0.0.1", host)
	require.Equal(t, port, srcPort)

	// A datagram socket cannot listen.
	require.Equal(t, wasip1.ErrnoNotsup, s.listen(1))
}

// TestSocket_unix covers the AF_UNIX path, which only V2 addresses can carry.
func TestSocket_unix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sock")

	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer ln.Close()

	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			buf := make([]byte, 8)
			n, _ := c.Read(buf)
			_, _ = c.Write(buf[:n])
		}
	}()

	s, errno := newSocket(addressFamilyUnix, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()

	// The port is ignored for a unix socket: the path is the address.
	require.Equal(t, wasip1.ErrnoSuccess, s.connect(path, 0))

	_, ferrno := s.Write([]byte("hi"))
	require.EqualErrno(t, 0, ferrno)
	buf := make([]byte, 8)
	n, ferrno := s.Read(buf)
	require.EqualErrno(t, 0, ferrno)
	require.Equal(t, "hi", string(buf[:n]))

	host, _, errno := s.peerAddr()
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	require.Equal(t, path, host)
}

// TestSocket_beforeConnect covers every operation that needs an established
// socket, on one that has none.
func TestSocket_beforeConnect(t *testing.T) {
	s, errno := newSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()

	_, _, errno = s.localAddr()
	require.Equal(t, wasip1.ErrnoNotconn, errno)
	_, _, errno = s.peerAddr()
	require.Equal(t, wasip1.ErrnoNotconn, errno)
	_, _, _, errno = s.recvFrom(make([]byte, 4))
	require.Equal(t, wasip1.ErrnoNotconn, errno)
	require.Equal(t, wasip1.ErrnoNotconn, s.setOpt(levelSocket, optSendBufferSize, 1024))

	_, ferrno := s.Read(make([]byte, 4))
	require.EqualErrno(t, sys.EBADF, ferrno)
	_, ferrno = s.Write([]byte("x"))
	require.EqualErrno(t, sys.EBADF, ferrno)

	// Accepting on a socket that never listened is a guest error.
	_, errno = s.accept()
	require.Equal(t, wasip1.ErrnoInval, errno)
}

// TestSocket_afterClose covers the whole surface reporting EBADF once closed,
// rather than panicking on a nil connection.
func TestSocket_afterClose(t *testing.T) {
	s, errno := newSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)

	require.EqualErrno(t, 0, s.Close())
	require.EqualErrno(t, 0, s.Close()) // idempotent

	require.Equal(t, wasip1.ErrnoBadf, s.bind("127.0.0.1", 0))
	require.Equal(t, wasip1.ErrnoBadf, s.listen(1))
	require.Equal(t, wasip1.ErrnoBadf, s.connect("127.0.0.1", 1))
	require.Equal(t, wasip1.ErrnoBadf, s.setOpt(levelSocket, optKeepAlive, 1))
	_, errno = s.accept()
	require.Equal(t, wasip1.ErrnoBadf, errno)
	_, errno = s.getOpt(levelSocket, optQuerySocketType)
	require.Equal(t, wasip1.ErrnoBadf, errno)
	_, _, _, errno = s.recvFrom(make([]byte, 4))
	require.Equal(t, wasip1.ErrnoBadf, errno)
	_, errno = s.sendTo([]byte("x"), "127.0.0.1", 1)
	require.Equal(t, wasip1.ErrnoBadf, errno)
	require.EqualErrno(t, sys.EBADF, s.Shutdown(0))

	_, ferrno := s.Read(make([]byte, 1))
	require.EqualErrno(t, sys.EBADF, ferrno)
}

func TestSocket_options(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			time.Sleep(100 * time.Millisecond)
			c.Close()
		}
	}()

	s, errno := newSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()
	require.Equal(t, wasip1.ErrnoSuccess, s.connect("127.0.0.1", ln.Addr().(*net.TCPAddr).Port))

	// Supported through net.
	require.Equal(t, wasip1.ErrnoSuccess, s.setOpt(levelTCP, optTCPNoDelay, 1))
	require.Equal(t, wasip1.ErrnoSuccess, s.setOpt(levelSocket, optKeepAlive, 1))
	require.Equal(t, wasip1.ErrnoSuccess, s.setOpt(levelSocket, optSendBufferSize, 4096))
	require.Equal(t, wasip1.ErrnoSuccess, s.setOpt(levelSocket, optRecvBufferSize, 4096))
	// Accepted and ignored, since net sets it on listeners itself.
	require.Equal(t, wasip1.ErrnoSuccess, s.setOpt(levelSocket, optReuseAddress, 1))

	// Not reachable through net: refused rather than silently accepted.
	require.Equal(t, wasip1.ErrnoNotsup, s.setOpt(levelSocket, optLinger, 0))
	require.Equal(t, wasip1.ErrnoNotsup, s.setOpt(levelSocket, optRecvTimeout, 0))
	require.Equal(t, wasip1.ErrnoNotsup, s.setOpt(levelSocket, optBindToDevice, 0))
	require.Equal(t, wasip1.ErrnoNotsup, s.setOpt(levelTCP, 99, 0))
	require.Equal(t, wasip1.ErrnoNotsup, s.setOpt(99, 0, 0))

	// Queries answerable without a raw socket.
	v, errno := s.getOpt(levelSocket, optQuerySocketType)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	require.Equal(t, socketTypeStream, v)
	v, errno = s.getOpt(levelSocket, optQueryAcceptConnections)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	require.Equal(t, int32(0), v) // connected, not listening
	_, errno = s.getOpt(levelSocket, optQuerySocketError)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	_, errno = s.getOpt(levelSocket, optLinger)
	require.Equal(t, wasip1.ErrnoNotsup, errno)
}

// TestSocket_shutdown covers the half-close directions.
func TestSocket_shutdown(t *testing.T) {
	for _, tc := range []struct {
		name string
		how  int
	}{
		{name: "read", how: 0},
		{name: "write", how: 1},
		{name: "both", how: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer ln.Close()
			go func() {
				if c, err := ln.Accept(); err == nil {
					time.Sleep(100 * time.Millisecond)
					c.Close()
				}
			}()

			s, errno := newSocket(addressFamilyInet4, socketTypeStream)
			require.Equal(t, wasip1.ErrnoSuccess, errno)
			defer s.Close()
			require.Equal(t, wasip1.ErrnoSuccess, s.connect("127.0.0.1", ln.Addr().(*net.TCPAddr).Port))

			require.EqualErrno(t, 0, s.Shutdown(tc.how))
		})
	}
}

// TestSocket_connectRefused covers a failure the guest is expected to branch
// on: it must surface as ECONNREFUSED, not a generic I/O error.
func TestSocket_connectRefused(t *testing.T) {
	// Bind then close, so the port is almost certainly unused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	s, errno := newSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()

	errno = s.connect("127.0.0.1", port)
	require.Equal(t, wasip1.ErrnoConnrefused, errno)
}

// TestSocket_fileSurface covers what the descriptor table and the standard
// WASI functions ask of a socket.
func TestSocket_fileSurface(t *testing.T) {
	s, errno := newSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()

	st, ferrno := s.Stat()
	require.EqualErrno(t, 0, ferrno)
	require.True(t, st.Mode&os.ModeSocket != 0)

	isDir, ferrno := s.IsDir()
	require.EqualErrno(t, sys.ENOTDIR, ferrno)
	require.False(t, isDir)

	// Zero-length reads and writes short-circuit before touching the socket.
	n, ferrno := s.Read(nil)
	require.EqualErrno(t, 0, ferrno)
	require.Equal(t, 0, n)
	n, ferrno = s.Write(nil)
	require.EqualErrno(t, 0, ferrno)
	require.Equal(t, 0, n)

	// wazy's own socket files report ENOSYS from Poll; so does this one.
	_, ferrno = s.Poll(0, 0)
	require.EqualErrno(t, sys.ENOSYS, ferrno)

	// Only a plain receive is expressible through net.
	_, ferrno = s.Recvfrom(make([]byte, 1), 2)
	require.EqualErrno(t, sys.ENOTSUP, ferrno)
}

func itoa(i int) string {
	return net.JoinHostPort("", "")[:0] + func() string {
		b := make([]byte, 0, 8)
		if i == 0 {
			return "0"
		}
		for i > 0 {
			b = append([]byte{byte('0' + i%10)}, b...)
			i /= 10
		}
		return string(b)
	}()
}

// TestSocket_acceptAdapter covers the socketapi.TCPSock adapter, which is what
// wazy's own socket plumbing calls rather than the extension's accept.
func TestSocket_acceptAdapter(t *testing.T) {
	s, errno := newSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()
	require.Equal(t, wasip1.ErrnoSuccess, s.bind("127.0.0.1", 0))
	require.Equal(t, wasip1.ErrnoSuccess, s.listen(1))

	_, port, errno := s.localAddr()
	require.Equal(t, wasip1.ErrnoSuccess, errno)

	go func() {
		if c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port))); err == nil {
			time.Sleep(50 * time.Millisecond)
			c.Close()
		}
	}()

	conn, ferrno := s.Accept()
	require.EqualErrno(t, 0, ferrno)
	require.NotNil(t, conn)
	require.EqualErrno(t, 0, conn.Close())

	// On a socket that never listened, the adapter reports a bad descriptor
	// rather than returning a nil connection with no error.
	fresh, errno := newSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer fresh.Close()
	conn, ferrno = fresh.Accept()
	require.EqualErrno(t, sys.EBADF, ferrno)
	require.Nil(t, conn)
}

// TestSocket_v1IPv6Address covers a V1 address getter with an IPv6 socket,
// where the 16-byte buffer holds the whole address and the family comes back
// as type 6.
func TestSocket_v1IPv6Address(t *testing.T) {
	s, errno := newSocket(addressFamilyInet6, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()

	require.Equal(t, wasip1.ErrnoSuccess, s.bind("::1", 0))
	require.Equal(t, wasip1.ErrnoSuccess, s.listen(1))

	host, port, errno := s.localAddr()
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	require.Equal(t, "::1", host)
	require.NotEqual(t, 0, port)
}

// TestSocket_datagramPaths covers the arms of sendTo and recvFrom a plain
// bound-then-exchange test never reaches: a connected socket, an unbound one,
// and each failure.
func TestSocket_datagramPaths(t *testing.T) {
	t.Run("send before bind opens a socket", func(t *testing.T) {
		peer, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		defer peer.Close()

		s, errno := newSocket(addressFamilyInet4, socketTypeDatagram)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		defer s.Close()

		// No bind: the send has to open a socket on an ephemeral port itself.
		n, errno := s.sendTo([]byte("ping"), "127.0.0.1", peer.LocalAddr().(*net.UDPAddr).Port)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		require.Equal(t, 4, n)

		buf := make([]byte, 8)
		require.NoError(t, peer.SetReadDeadline(time.Now().Add(5*time.Second)))
		n, _, err = peer.ReadFrom(buf)
		require.NoError(t, err)
		require.Equal(t, "ping", string(buf[:n]))
	})

	t.Run("connected socket ignores the destination", func(t *testing.T) {
		// sendto(2) on a connected socket sends to the peer, so a bogus
		// destination is not an error.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer ln.Close()

		got := make(chan string, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			buf := make([]byte, 8)
			n, _ := c.Read(buf)
			got <- string(buf[:n])
			_, _ = c.Write([]byte("back"))
		}()

		s, errno := newSocket(addressFamilyInet4, socketTypeStream)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		defer s.Close()
		require.Equal(t, wasip1.ErrnoSuccess, s.connect("127.0.0.1", ln.Addr().(*net.TCPAddr).Port))

		n, errno := s.sendTo([]byte("ping"), "192.0.2.1", 9)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		require.Equal(t, 4, n)
		require.Equal(t, "ping", <-got)

		// Likewise a receive on a connected socket reports the known peer
		// rather than requiring a datagram source.
		buf := make([]byte, 8)
		n, host, port, errno := s.recvFrom(buf)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		require.Equal(t, "back", string(buf[:n]))
		require.Equal(t, "127.0.0.1", host)
		require.Equal(t, ln.Addr().(*net.TCPAddr).Port, port)
	})

	t.Run("receive before bind is not connected", func(t *testing.T) {
		s, errno := newSocket(addressFamilyInet4, socketTypeDatagram)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		defer s.Close()

		_, _, _, errno = s.recvFrom(make([]byte, 8))
		require.Equal(t, wasip1.ErrnoNotconn, errno)
	})

	t.Run("non-blocking receive", func(t *testing.T) {
		s, errno := newSocket(addressFamilyInet4, socketTypeDatagram)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		defer s.Close()
		require.Equal(t, wasip1.ErrnoSuccess, s.bind("127.0.0.1", 0))
		require.EqualErrno(t, 0, s.SetNonblock(true))

		_, _, _, errno = s.recvFrom(make([]byte, 8))
		require.Equal(t, wasip1.ErrnoAgain, errno)
	})

	t.Run("unresolvable destination", func(t *testing.T) {
		s, errno := newSocket(addressFamilyInet4, socketTypeDatagram)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		defer s.Close()
		require.Equal(t, wasip1.ErrnoSuccess, s.bind("127.0.0.1", 0))

		_, errno = s.sendTo([]byte("x"), "no.such.host.invalid", 80)
		require.NotEqual(t, wasip1.ErrnoSuccess, errno)
	})
}

// TestSocket_afterClose covers every operation on a closed socket reporting a
// bad descriptor rather than panicking on a nil connection.
func TestSocket_closedOperations(t *testing.T) {
	s, errno := newSocket(addressFamilyInet4, socketTypeDatagram)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	require.Equal(t, wasip1.ErrnoSuccess, s.bind("127.0.0.1", 0))
	require.EqualErrno(t, 0, s.Close())
	// Closing twice is not an error, since a guest may do it on cleanup.
	require.EqualErrno(t, 0, s.Close())

	_, errno = s.sendTo([]byte("x"), "127.0.0.1", 80)
	require.Equal(t, wasip1.ErrnoBadf, errno)

	_, _, _, errno = s.recvFrom(make([]byte, 8))
	require.Equal(t, wasip1.ErrnoBadf, errno)

	_, ferrno := s.Read(make([]byte, 8))
	require.EqualErrno(t, sys.EBADF, ferrno)

	_, ferrno = s.Write([]byte("x"))
	require.EqualErrno(t, sys.EBADF, ferrno)

	require.Equal(t, wasip1.ErrnoBadf, s.connect("127.0.0.1", 80))
	require.Equal(t, wasip1.ErrnoBadf, s.listen(1))
}

// TestSocket_recvfromFlags covers the MSG_PEEK-style flags the standard
// sock_recv path passes down, which the net backend cannot express.
func TestSocket_recvfromFlags(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			_, _ = c.Write([]byte("hi"))
			time.Sleep(50 * time.Millisecond)
		}
	}()

	s, errno := newSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, errno)
	defer s.Close()
	require.Equal(t, wasip1.ErrnoSuccess, s.connect("127.0.0.1", ln.Addr().(*net.TCPAddr).Port))

	// A peek cannot be emulated, so it is refused rather than silently
	// consuming the bytes.
	_, ferrno := s.Recvfrom(make([]byte, 8), 2)
	require.EqualErrno(t, sys.ENOTSUP, ferrno)

	buf := make([]byte, 8)
	n, ferrno := s.Recvfrom(buf, 0)
	require.EqualErrno(t, 0, ferrno)
	require.Equal(t, "hi", string(buf[:n]))
}

// TestSocket_connectFailures covers the connect arms other than a successful
// dial.
func TestSocket_connectFailures(t *testing.T) {
	t.Run("unresolvable", func(t *testing.T) {
		s, errno := newSocket(addressFamilyInet4, socketTypeStream)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		defer s.Close()
		require.NotEqual(t, wasip1.ErrnoSuccess, s.connect("no.such.host.invalid", 80))
	})

	t.Run("refused", func(t *testing.T) {
		// A port nothing is listening on: reserve one and release it.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := ln.Addr().(*net.TCPAddr).Port
		require.NoError(t, ln.Close())

		s, errno := newSocket(addressFamilyInet4, socketTypeStream)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		defer s.Close()
		require.Equal(t, wasip1.ErrnoConnrefused, s.connect("127.0.0.1", port))
	})

	// A guest that binds before connecting is choosing its source address, so
	// the dial has to honour the bind rather than ignore it.
	t.Run("bind then connect keeps the source port", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer ln.Close()

		peers := make(chan string, 1)
		go func() {
			if c, err := ln.Accept(); err == nil {
				defer c.Close()
				peers <- c.RemoteAddr().String()
				time.Sleep(50 * time.Millisecond)
			}
		}()

		// Reserve a source port, then release it so the bind can take it.
		reserve, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		srcPort := reserve.Addr().(*net.TCPAddr).Port
		require.NoError(t, reserve.Close())

		s, errno := newSocket(addressFamilyInet4, socketTypeStream)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		defer s.Close()
		require.Equal(t, wasip1.ErrnoSuccess, s.bind("127.0.0.1", srcPort))
		require.Equal(t, wasip1.ErrnoSuccess, s.connect("127.0.0.1", ln.Addr().(*net.TCPAddr).Port))

		require.Equal(t, net.JoinHostPort("127.0.0.1", itoa(srcPort)), <-peers)
	})
}

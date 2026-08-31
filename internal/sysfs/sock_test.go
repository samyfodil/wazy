package sysfs

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/sys"
)

func TestTcpConnFile_Write(t *testing.T) {
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listen.Close()

	tcpAddr, err := net.ResolveTCPAddr("tcp", listen.Addr().String())
	require.NoError(t, err)
	tcp, err := net.DialTCP("tcp", nil, tcpAddr)
	require.NoError(t, err)
	defer tcp.Close() //nolint

	file := newTcpConn(tcp)
	errno := sys.Errno(0)
	// Ensure we don't interrupt until we get a non-zero errno,
	// and we retry on EAGAIN (i.e. when nonblocking is true).
	for {
		_, errno = file.Write([]byte("wazero"))
		if errno != sys.EAGAIN {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.Zero(t, errno)

	conn, err := listen.Accept()
	require.NoError(t, err)
	defer conn.Close()

	bytes := make([]byte, 4)

	n, err := conn.Read(bytes)
	require.NoError(t, err)
	require.NotEqual(t, 0, n)

	require.Equal(t, "waze", string(bytes))
}

func TestTcpConnFile_Read(t *testing.T) {
	// Test #1: Read from a TCP connection with default synchrony
	// (i.e., without explicitly setting the non-blocking flag).
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listen.Close()

	tcpAddr, err := net.ResolveTCPAddr("tcp", listen.Addr().String())
	require.NoError(t, err)
	tcp, err := net.DialTCP("tcp", nil, tcpAddr)
	require.NoError(t, err)
	defer tcp.Close() //nolint

	n, err := tcp.Write([]byte("wazero"))
	require.NoError(t, err)
	require.NotEqual(t, 0, n)

	conn, err := listen.Accept()
	require.NoError(t, err)
	defer conn.Close()

	bytes := make([]byte, 4)

	require.NoError(t, err)
	errno := sys.Errno(0)
	file := newTcpConn(conn.(*net.TCPConn))
	// Ensure we don't interrupt until we get a non-zero errno,
	// and we retry on EAGAIN (i.e. when nonblocking is true).
	for {
		_, errno = file.Read(bytes)
		if errno != sys.EAGAIN {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.Zero(t, errno)
	require.NoError(t, err)
	require.Equal(t, "waze", string(bytes))

	// Test #2: Read from a TCP connection asynchronously (i.e., with
	// the non-blocking flag set explicitly).
	tcpAddr2, err := net.ResolveTCPAddr("tcp", listen.Addr().String())
	require.NoError(t, err)
	tcp2, err := net.DialTCP("tcp", nil, tcpAddr2)
	require.NoError(t, err)
	defer tcp.Close() //nolint

	// Use a goroutine to asynchronously write to the TCP connection
	// with a delay that is visible to the test.
	go func() {
		time.Sleep(200 * time.Millisecond)
		n2, err := tcp2.Write([]byte("wazero"))
		require.NoError(t, err)
		require.NotEqual(t, 0, n2)
	}()

	conn2, err := listen.Accept()
	require.NoError(t, err)
	defer conn.Close()

	bytes2 := make([]byte, 4)

	require.NoError(t, err)
	errno2 := sys.Errno(0)
	file2 := newTcpConn(conn2.(*net.TCPConn))
	errno2 = file2.(*tcpConnFile).SetNonblock(true)
	require.Zero(t, errno2)

	// Ensure we start by getting EAGAIN.
	_, errno2 = file2.Read(bytes2)
	require.Equal(t, sys.EAGAIN, errno2)

	// Ensure we don't interrupt until we get a non-zero errno,
	// and we retry on EAGAIN (i.e. when nonblocking is true).
	for {
		_, errno2 = file2.Read(bytes2)
		if errno2 != sys.EAGAIN {
			break
		}
	}
	require.Zero(t, errno2)
	require.NoError(t, err)
	require.Equal(t, "waze", string(bytes2))
}

// TestTcpConnFile_ReadPeekWrite covers the syscall state a connection shares
// between Read, Recvfrom and Write: each call must see its own buffer and
// result, and a closed connection must fail even when data is available.
func TestTcpConnFile_ReadPeekWrite(t *testing.T) {
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listen.Close()

	tcpAddr, err := net.ResolveTCPAddr("tcp", listen.Addr().String())
	require.NoError(t, err)
	tcp, err := net.DialTCP("tcp", nil, tcpAddr)
	require.NoError(t, err)
	defer tcp.Close() //nolint

	conn, err := listen.Accept()
	require.NoError(t, err)
	defer conn.Close()

	file := newTcpConn(tcp)
	require.Zero(t, file.(*tcpConnFile).SetNonblock(true))

	buf := make([]byte, 8)
	_, errno := file.Read(buf)
	require.Equal(t, sys.EAGAIN, errno)

	_, err = conn.Write([]byte("wazy"))
	require.NoError(t, err)

	// Peeking doesn't consume, so the following Read sees the same bytes.
	var n int
	for {
		n, errno = file.Recvfrom(buf, MSG_PEEK)
		if errno != sys.EAGAIN {
			break
		}
	}
	require.Zero(t, errno)
	require.Equal(t, "wazy", string(buf[:n]))

	n, errno = file.Read(buf)
	require.Zero(t, errno)
	require.Equal(t, "wazy", string(buf[:n]))

	n, errno = file.Write([]byte("wazy!"))
	require.Zero(t, errno)
	require.Equal(t, 5, n)

	peeked := make([]byte, 5)
	_, err = io.ReadFull(conn, peeked)
	require.NoError(t, err)
	require.Equal(t, "wazy!", string(peeked))

	// A closed file fails even when the read would otherwise have succeeded.
	_, err = conn.Write([]byte("wazy"))
	require.NoError(t, err)
	require.Zero(t, file.Close())
	_, errno = file.Read(buf)
	require.Equal(t, sys.EBADF, errno)
}

func TestTcpConnFile_Stat(t *testing.T) {
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listen.Close()

	tcpAddr, err := net.ResolveTCPAddr("tcp", listen.Addr().String())
	require.NoError(t, err)
	tcp, err := net.DialTCP("tcp", nil, tcpAddr)
	require.NoError(t, err)
	defer tcp.Close() //nolint

	conn, err := listen.Accept()
	require.NoError(t, err)
	defer conn.Close()

	file := newTcpConn(tcp)
	_, errno := file.Stat()
	require.Zero(t, errno, "Stat should not fail")
}

func TestTcpConnFile_SetNonblock(t *testing.T) {
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listen.Close()

	lf := newTCPListenerFile(listen.(*net.TCPListener))

	tcpAddr, err := net.ResolveTCPAddr("tcp", listen.Addr().String())
	require.NoError(t, err)
	tcp, err := net.DialTCP("tcp", nil, tcpAddr)
	require.NoError(t, err)
	defer tcp.Close() //nolint

	nblf := lf.(sys.PollableFile)
	errno := nblf.SetNonblock(true)
	require.EqualErrno(t, 0, errno)
	require.True(t, nblf.IsNonblock())

	conn, errno := lf.Accept()
	require.EqualErrno(t, 0, errno)
	defer conn.Close()

	file := newTcpConn(tcp).(sys.PollableFile)
	errno = file.SetNonblock(true)
	require.EqualErrno(t, 0, errno)
	require.True(t, file.IsNonblock())
}

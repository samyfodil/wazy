package sysfs

import (
	"net"
	"os"
	"syscall"

	socketapi "github.com/samyfodil/wazy/internal/sock"
	"github.com/samyfodil/wazy/sys"
)

// NewTCPListenerFile creates a socketapi.TCPSock for a given *net.TCPListener.
func NewTCPListenerFile(tl *net.TCPListener) socketapi.TCPSock {
	return newTCPListenerFile(tl)
}

// baseSockFile implements base behavior for all TCPSock, TCPConn files,
// regardless the platform.
type baseSockFile struct {
	sys.UnimplementedFile
}

var _ sys.File = (*baseSockFile)(nil)

// IsDir implements the same method as documented on File.IsDir
func (*baseSockFile) IsDir() (bool, sys.Errno) {
	// We need to override this method because WASI-libc prestats the FD
	// and the default impl returns ENOSYS otherwise.
	return false, 0
}

// Stat implements the same method as documented on File.Stat
func (f *baseSockFile) Stat() (fs sys.Stat_t, errno sys.Errno) {
	// The mode is not really important, but it should be neither a regular file nor a directory.
	fs.Mode = os.ModeIrregular
	return
}

// sockOp caches a socket's syscall.RawConn together with a closure bound to it
// once. RawConn.Control takes its callback through an interface call, so a
// closure built per read or write escapes, dragging the n and errno it captures
// to the heap with it; binding one closure per socket and passing arguments and
// results through these fields keeps the hot path allocation-free. Calls on one
// socket are serialized, like the rest of these files' mutable state.
type sockOp struct {
	conn  syscall.Conn
	rc    syscall.RawConn
	fn    func(fd uintptr, buf []byte) (int, sys.Errno)
	buf   []byte
	n     int
	errno sys.Errno
	op    func(fd uintptr)
}

func (s *sockOp) init(conn syscall.Conn) {
	s.conn = conn
	s.op = func(fd uintptr) { s.n, s.errno = s.fn(fd, s.buf) }
}

// call runs fn against the socket's file descriptor, which RawConn.Control
// keeps valid for the duration of the call. The errno from fn wins; otherwise
// Control's own error is surfaced.
func (s *sockOp) call(fn func(fd uintptr, buf []byte) (int, sys.Errno), buf []byte) (int, sys.Errno) {
	if s.rc == nil {
		rc, err := s.conn.SyscallConn()
		if err != nil {
			return 0, sys.UnwrapOSError(err)
		}
		s.rc = rc
	}
	s.fn, s.buf, s.n, s.errno = fn, buf, 0, 0
	controlErr := s.rc.Control(s.op)
	if s.errno != 0 {
		return s.n, s.errno
	}
	return s.n, sys.UnwrapOSError(controlErr)
}

var _ socketapi.TCPSock = (*tcpListenerFile)(nil)

type tcpListenerFile struct {
	baseSockFile
	sockOp

	tl       *net.TCPListener
	closed   bool
	nonblock bool
}

// newTCPListenerFile is a constructor for a socketapi.TCPSock.
//
// The current strategy is to wrap a net.TCPListener
// and invoking raw syscalls using syscallConnControl:
// this internal calls RawConn.Control(func(fd)), making sure
// that the underlying file descriptor is valid throughout
// the duration of the syscall.
func newDefaultTCPListenerFile(tl *net.TCPListener) socketapi.TCPSock {
	f := &tcpListenerFile{tl: tl}
	f.init(tl)
	return f
}

// Close implements the same method as documented on sys.File
func (f *tcpListenerFile) Close() sys.Errno {
	if !f.closed {
		return sys.UnwrapOSError(f.tl.Close())
	}
	return 0
}

// Addr is exposed for testing.
func (f *tcpListenerFile) Addr() *net.TCPAddr {
	return f.tl.Addr().(*net.TCPAddr)
}

// IsNonblock implements the same method as documented on sys.PollableFile
func (f *tcpListenerFile) IsNonblock() bool {
	return f.nonblock
}

// Poll implements the same method as documented on sys.Pollable
func (f *tcpListenerFile) Poll(flag sys.Pflag, timeoutMillis int32) (ready bool, errno sys.Errno) {
	return false, sys.ENOSYS
}

var _ socketapi.TCPConn = (*tcpConnFile)(nil)

type tcpConnFile struct {
	baseSockFile
	sockOp

	tc *net.TCPConn

	// nonblock is true when the underlying connection is flagged as non-blocking.
	// This ensures that reads and writes return sys.EAGAIN without blocking the caller.
	nonblock bool
	// closed is true when closed was called. This ensures proper sys.EBADF
	closed bool
}

func newTcpConn(tc *net.TCPConn) socketapi.TCPConn {
	f := &tcpConnFile{tc: tc}
	f.init(tc)
	return f
}

// Read implements the same method as documented on sys.File
func (f *tcpConnFile) Read(buf []byte) (n int, errno sys.Errno) {
	if len(buf) == 0 {
		return 0, 0 // Short-circuit 0-len reads.
	}
	if nonBlockingFileReadSupported && f.IsNonblock() {
		n, errno = f.call(readSocket, buf)
		// Validate even on success: the guest may have closed this file.
		return n, fileError(f, f.closed, errno)
	} else {
		n, errno = read(f.tc, buf)
	}
	if errno != 0 {
		// Defer validation overhead until we've already had an error.
		errno = fileError(f, f.closed, errno)
	}
	return
}

// Write implements the same method as documented on sys.File
func (f *tcpConnFile) Write(buf []byte) (n int, errno sys.Errno) {
	if nonBlockingFileWriteSupported && f.IsNonblock() {
		n, errno = f.call(writeSocket, buf)
		// Validate even on success: the guest may have closed this file.
		return n, fileError(f, f.closed, errno)
	} else {
		n, errno = write(f.tc, buf)
	}
	if errno != 0 {
		// Defer validation overhead until we've already had an error.
		errno = fileError(f, f.closed, errno)
	}
	return
}

// Recvfrom implements the same method as documented on socketapi.TCPConn
func (f *tcpConnFile) Recvfrom(p []byte, flags int) (n int, errno sys.Errno) {
	if flags != MSG_PEEK {
		errno = sys.EINVAL
		return
	}
	n, errno = f.call(peekSocket, p)
	return n, fileError(f, f.closed, errno)
}

// peekSocket is recvfrom with MSG_PEEK, as a top-level function so that
// Recvfrom passes it to call without allocating a closure.
func peekSocket(fd uintptr, buf []byte) (int, sys.Errno) {
	return recvfrom(fd, buf, MSG_PEEK)
}

// Close implements the same method as documented on sys.File
func (f *tcpConnFile) Close() sys.Errno {
	return f.close()
}

func (f *tcpConnFile) close() sys.Errno {
	if f.closed {
		return 0
	}
	f.closed = true
	return f.Shutdown(socketapi.SHUT_RDWR)
}

// SetNonblock implements the same method as documented on sys.PollableFile
func (f *tcpConnFile) SetNonblock(enabled bool) (errno sys.Errno) {
	f.nonblock = enabled
	_, errno = syscallConnControl(f.tc, func(fd uintptr) (int, sys.Errno) {
		return 0, sys.UnwrapOSError(setNonblockSocket(fd, enabled))
	})
	return
}

// IsNonblock implements the same method as documented on sys.PollableFile
func (f *tcpConnFile) IsNonblock() bool {
	return f.nonblock
}

// Poll implements the same method as documented on sys.Pollable
func (f *tcpConnFile) Poll(flag sys.Pflag, timeoutMillis int32) (ready bool, errno sys.Errno) {
	return false, sys.ENOSYS
}

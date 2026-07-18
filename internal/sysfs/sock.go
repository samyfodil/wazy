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

var _ socketapi.TCPSock = (*tcpListenerFile)(nil)

type tcpListenerFile struct {
	baseSockFile

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
	return &tcpListenerFile{tl: tl}
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

	tc *net.TCPConn

	// rc caches the connection's syscall.RawConn so per-read/write syscalls skip
	// a SyscallConn() allocation (W4). nil if SyscallConn failed at construction,
	// in which case control() falls back to syscallConnControl.
	rc syscall.RawConn

	// nonblock is true when the underlying connection is flagged as non-blocking.
	// This ensures that reads and writes return sys.EAGAIN without blocking the caller.
	nonblock bool
	// closed is true when closed was called. This ensures proper sys.EBADF
	closed bool
}

func newTcpConn(tc *net.TCPConn) socketapi.TCPConn {
	f := &tcpConnFile{tc: tc}
	// Cache RawConn once; RawConn.Control still guards fd validity per call.
	if rc, err := tc.SyscallConn(); err == nil {
		f.rc = rc
	}
	return f
}

// control runs op against the connection's fd, reusing the cached RawConn when
// available. op matches RawConn.Control's signature exactly (writing results via
// captured vars) so no adapter closure is allocated on top of it — one escaping
// closure per call instead of two (W4). RawConn.Control keeps the fd valid for
// the duration of the call. The returned Errno is Control's own error, which the
// caller should use only when op reported success (inner errno wins).
func (f *tcpConnFile) control(op func(fd uintptr)) sys.Errno {
	if f.rc != nil {
		return sys.UnwrapOSError(f.rc.Control(op))
	}
	rc, err := f.tc.SyscallConn()
	if err != nil {
		return sys.UnwrapOSError(err)
	}
	return sys.UnwrapOSError(rc.Control(op))
}

// Read implements the same method as documented on sys.File
func (f *tcpConnFile) Read(buf []byte) (n int, errno sys.Errno) {
	if len(buf) == 0 {
		return 0, 0 // Short-circuit 0-len reads.
	}
	if nonBlockingFileReadSupported && f.IsNonblock() {
		controlErr := f.control(func(fd uintptr) {
			var e sys.Errno
			n, e = readSocket(fd, buf)
			errno = fileError(f, f.closed, e)
		})
		if errno == 0 { // inner errno wins; else surface Control's own error
			errno = controlErr
		}
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
		controlErr := f.control(func(fd uintptr) {
			var e sys.Errno
			n, e = writeSocket(fd, buf)
			errno = fileError(f, f.closed, e)
		})
		if errno == 0 { // inner errno wins; else surface Control's own error
			errno = controlErr
		}
		return
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
	controlErr := f.control(func(fd uintptr) {
		var e sys.Errno
		n, e = recvfrom(fd, p, MSG_PEEK)
		errno = fileError(f, f.closed, e)
	})
	if errno == 0 { // inner errno wins; else surface Control's own error
		errno = controlErr
	}
	return
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

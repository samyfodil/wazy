//go:build (linux || darwin) && !tinygo

package sysfs

import (
	"net"
	"syscall"

	socketapi "github.com/samyfodil/wazy/internal/sock"
	"github.com/samyfodil/wazy/sys"
)

// MSG_PEEK is the constant syscall.MSG_PEEK
const MSG_PEEK = syscall.MSG_PEEK

func newTCPListenerFile(tl *net.TCPListener) socketapi.TCPSock {
	return newDefaultTCPListenerFile(tl)
}

func _pollSock(f *tcpListenerFile, flag sys.Pflag, timeoutMillis int32) (bool, sys.Errno) {
	// The function literal captures nothing, so it is a static value: no
	// closure is allocated per poll.
	n, errno := f.call(func(fd uintptr, _ []byte) (int, sys.Errno) {
		if ready, errno := poll(fd, sys.POLLIN, 0); !ready || errno != 0 {
			return -1, errno
		} else {
			return 0, errno
		}
	}, nil)
	return n >= 0, errno
}

func setNonblockSocket(fd uintptr, enabled bool) sys.Errno {
	return sys.UnwrapOSError(setNonblock(fd, enabled))
}

func readSocket(fd uintptr, buf []byte) (int, sys.Errno) {
	n, err := syscall.Read(int(fd), buf)
	return n, sys.UnwrapOSError(err)
}

func writeSocket(fd uintptr, buf []byte) (int, sys.Errno) {
	n, err := syscall.Write(int(fd), buf)
	return n, sys.UnwrapOSError(err)
}

func recvfrom(fd uintptr, buf []byte, flags int32) (n int, errno sys.Errno) {
	n, _, err := syscall.Recvfrom(int(fd), buf, int(flags))
	return n, sys.UnwrapOSError(err)
}

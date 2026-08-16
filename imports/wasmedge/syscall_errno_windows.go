package wasmedge

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/samyfodil/wazy/internal/wasip1"
)

// syscallToWasiErrno maps a socket error to the errno a guest branches on.
//
// Winsock reports its own errnos, which do not match the POSIX names of the
// same meaning -- a refused connection is WSAECONNREFUSED (10061), not
// ECONNREFUSED -- so a guest would otherwise see EIO on Windows for every
// condition it wants to tell apart.
func syscallToWasiErrno(err error) (wasip1.Errno, bool) {
	var se syscall.Errno
	if !errors.As(err, &se) {
		return 0, false
	}
	switch se {
	case windows.WSAECONNREFUSED:
		return wasip1.ErrnoConnrefused, true
	case windows.WSAECONNRESET:
		return wasip1.ErrnoConnreset, true
	case windows.WSAECONNABORTED:
		return wasip1.ErrnoConnaborted, true
	case windows.WSAEADDRINUSE:
		return wasip1.ErrnoAddrinuse, true
	case windows.WSAEADDRNOTAVAIL:
		return wasip1.ErrnoAddrnotavail, true
	case windows.WSAEHOSTUNREACH:
		return wasip1.ErrnoHostunreach, true
	case windows.WSAENETUNREACH:
		return wasip1.ErrnoNetunreach, true
	case windows.WSAESHUTDOWN:
		return wasip1.ErrnoPipe, true
	case windows.WSAEACCES:
		return wasip1.ErrnoAcces, true
	case windows.WSAEWOULDBLOCK:
		return wasip1.ErrnoAgain, true
	case windows.WSAEINVAL:
		return wasip1.ErrnoInval, true
	case syscall.EPIPE:
		return wasip1.ErrnoPipe, true
	}
	return 0, false
}

//go:build !(plan9 || aix || windows)

package wasmedge

import (
	"errors"
	"syscall"

	"github.com/samyfodil/wazy/internal/wasip1"
)

// syscallToWasiErrno maps a socket error to the errno a guest branches on.
// Rust's std turns ECONNREFUSED and friends into distinct error kinds, so this
// reports the specific condition rather than collapsing everything to EIO.
func syscallToWasiErrno(err error) (wasip1.Errno, bool) {
	var se syscall.Errno
	if !errors.As(err, &se) {
		return 0, false
	}
	switch se {
	case syscall.ECONNREFUSED:
		return wasip1.ErrnoConnrefused, true
	case syscall.ECONNRESET:
		return wasip1.ErrnoConnreset, true
	case syscall.ECONNABORTED:
		return wasip1.ErrnoConnaborted, true
	case syscall.EADDRINUSE:
		return wasip1.ErrnoAddrinuse, true
	case syscall.EADDRNOTAVAIL:
		return wasip1.ErrnoAddrnotavail, true
	case syscall.EHOSTUNREACH:
		return wasip1.ErrnoHostunreach, true
	case syscall.ENETUNREACH:
		return wasip1.ErrnoNetunreach, true
	case syscall.EPIPE:
		return wasip1.ErrnoPipe, true
	case syscall.EACCES:
		return wasip1.ErrnoAcces, true
	case syscall.EAGAIN:
		return wasip1.ErrnoAgain, true
	case syscall.EINVAL:
		return wasip1.ErrnoInval, true
	}
	return 0, false
}

//go:build plan9 || aix

package wasmedge

import "github.com/samyfodil/wazy/internal/wasip1"

// syscallToWasiErrno reports nothing on platforms whose syscall package does
// not define the socket errnos, so the caller falls back to EIO. Sockets are
// not reachable there in the first place -- this only keeps the package
// building, as sys.syscallToErrno does.
func syscallToWasiErrno(err error) (wasip1.Errno, bool) {
	return 0, false
}

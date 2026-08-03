//go:build unix

package sysfs

import (
	"syscall"

	"github.com/samyfodil/wazy/sys"
)

const (
	nonBlockingFileReadSupported  = true
	nonBlockingFileWriteSupported = true
)

func rmdir(path string) sys.Errno {
	err := syscall.Rmdir(path)
	// POSIX lets rmdir report a non-empty directory as EITHER ENOTEMPTY or
	// EEXIST, and platforms disagree: Linux and the BSDs pick ENOTEMPTY,
	// illumos picks EEXIST. Normalize to ENOTEMPTY so a guest sees the same
	// error wherever the host happens to run -- EEXIST has no other meaning
	// for rmdir, so nothing is lost by folding it. This mirrors unlink's own
	// EPERM -> EISDIR normalization (unlink_unix.go), which smooths the same
	// kind of POSIX-sanctioned divergence.
	if errno := sys.UnwrapOSError(err); errno == sys.EEXIST {
		return sys.ENOTEMPTY
	} else {
		return errno
	}
}

// readFd exposes syscall.Read.
func readFd(fd uintptr, buf []byte) (int, sys.Errno) {
	if len(buf) == 0 {
		return 0, 0 // Short-circuit 0-len reads.
	}
	n, err := syscall.Read(int(fd), buf)
	errno := sys.UnwrapOSError(err)
	return n, errno
}

// writeFd exposes syscall.Write.
func writeFd(fd uintptr, buf []byte) (int, sys.Errno) {
	if len(buf) == 0 {
		return 0, 0 // Short-circuit 0-len writes.
	}
	n, err := syscall.Write(int(fd), buf)
	errno := sys.UnwrapOSError(err)
	return n, errno
}

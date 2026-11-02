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

// pwriteFd exposes syscall.Pwrite.
func pwriteFd(fd uintptr, buf []byte, off int64) (int, sys.Errno) {
	if len(buf) == 0 {
		return 0, 0
	}
	n, err := syscall.Pwrite(int(fd), buf, off)
	return max(n, 0), sys.UnwrapOSError(err)
}

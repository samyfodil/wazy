//go:build !(unix || windows)

package sysfs

import (
	"github.com/samyfodil/wazy/sys"
)

const (
	nonBlockingFileReadSupported  = false
	nonBlockingFileWriteSupported = false
)

// readFd returns ENOSYS on unsupported platforms.
func readFd(fd uintptr, buf []byte) (int, sys.Errno) {
	return -1, sys.ENOSYS
}

// writeFd returns ENOSYS on unsupported platforms.
func writeFd(fd uintptr, buf []byte) (int, sys.Errno) {
	return -1, sys.ENOSYS
}

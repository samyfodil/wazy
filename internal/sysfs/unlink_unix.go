//go:build unix

package sysfs

import (
	"syscall"

	"github.com/samyfodil/wazy/experimental/sys"
)

func unlink(name string) (errno sys.Errno) {
	err := syscall.Unlink(name)
	if errno = sys.UnwrapOSError(err); errno == sys.EPERM {
		errno = sys.EISDIR
	}
	return errno
}

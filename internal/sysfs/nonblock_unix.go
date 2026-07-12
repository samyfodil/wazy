//go:build unix

package sysfs

import (
	"syscall"

	"github.com/samyfodil/wazy/experimental/sys"
)

func setNonblock(fd uintptr, enable bool) sys.Errno {
	return sys.UnwrapOSError(syscall.SetNonblock(int(fd), enable))
}

func isNonblock(f *osFile) bool {
	return f.flag&sys.O_NONBLOCK == sys.O_NONBLOCK
}

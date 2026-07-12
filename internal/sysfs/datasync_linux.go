package sysfs

import (
	"os"
	"syscall"

	"github.com/samyfodil/wazy/experimental/sys"
)

func datasync(f *os.File) sys.Errno {
	return sys.UnwrapOSError(syscall.Fdatasync(int(f.Fd())))
}

//go:build unix

package sysfs

import (
	"syscall"

	"github.com/samyfodil/wazy/sys"
)

func rename(from, to string) sys.Errno {
	if from == to {
		return 0
	}
	// EEXIST -> ENOTEMPTY for the same reason rmdir normalizes it
	// (file_unix.go): POSIX lets rename report "new names a non-empty
	// directory" as either one, and illumos picks EEXIST where Linux and the
	// BSDs pick ENOTEMPTY. That is the only case POSIX lists EEXIST for
	// rename, so the fold cannot swallow a different meaning.
	if errno := sys.UnwrapOSError(syscall.Rename(from, to)); errno == sys.EEXIST {
		return sys.ENOTEMPTY
	} else {
		return errno
	}
}

package sysfs

import (
	"io/fs"
	"os"
	"syscall"

	"github.com/samyfodil/wazy/sys"
)

// lutimensIn sets the times of name itself: a symlink's through a handle on
// the link.
func lutimensIn(r *os.Root, name string, atim, mtim int64) sys.Errno {
	if info, err := r.Lstat(name); err != nil {
		return rootErrno(err)
	} else if info.Mode()&fs.ModeSymlink == 0 {
		return rootErrno(r.Chtimes(name, omittableTime(atim), omittableTime(mtim)))
	}
	h, err := openLinkIn(r, name, fileWriteAttributes)
	if err != nil {
		return rootErrno(err)
	}
	defer syscall.CloseHandle(h)
	return sys.UnwrapOSError(futimens(uintptr(h), atim, mtim))
}

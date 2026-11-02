//go:build !linux && !darwin && !windows

package sysfs

import (
	"io/fs"
	"os"

	"github.com/samyfodil/wazy/sys"
)

// lutimensIn sets the times of name itself, which it can only do where name is
// not a symlink.
func lutimensIn(r *os.Root, name string, atim, mtim int64) sys.Errno {
	if info, err := r.Lstat(name); err != nil {
		return rootErrno(err)
	} else if info.Mode()&fs.ModeSymlink != 0 {
		return sys.ENOSYS
	}
	return rootErrno(r.Chtimes(name, omittableTime(atim), omittableTime(mtim)))
}

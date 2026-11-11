//go:build linux || darwin

package sysfs

import (
	"os"
	"path"
	"syscall"

	"github.com/samyfodil/wazy/sys"
)

// lutimensIn sets the times of name itself, through the directory holding it,
// which the root resolves: the last element is then a name in that directory
// that nothing follows.
func lutimensIn(r *os.Root, name string, atim, mtim int64) sys.Errno {
	dir, leaf := path.Split(name)
	if leaf == "" || leaf == "." || leaf == ".." {
		// A directory, whose times are the same followed or not.
		return rootErrno(r.Chtimes(name, omittableTime(atim), omittableTime(mtim)))
	}
	if dir == "" {
		dir = "."
	}
	parent, err := r.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return rootErrno(err)
	}
	defer parent.Close()
	var times [2]syscall.Timespec
	if !timesToTimespecs(atim, mtim, &times) {
		return 0
	}
	return utimensatNoFollow(int(parent.Fd()), leaf, &times)
}

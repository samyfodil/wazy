package sysfs

import (
	"fmt"
	"io/fs"
	"path"
	"runtime"

	"github.com/samyfodil/wazy/sys"
)

type AdaptFS struct {
	FS fs.FS
}

// String implements fmt.Stringer
func (a *AdaptFS) String() string {
	return fmt.Sprintf("%v", a.FS)
}

// OpenFile implements the same method as documented on sys.FS
func (a *AdaptFS) OpenFile(path string, flag sys.Oflag, perm fs.FileMode) (sys.File, sys.Errno) {
	return OpenFSFile(a.FS, cleanPath(path), flag, perm)
}

// Lstat implements the same method as documented on sys.FS
func (a *AdaptFS) Lstat(path string) (sys.Stat_t, sys.Errno) {
	// At this time, we make the assumption sys.FS instances do not support
	// symbolic links, therefore Lstat is the same as Stat. This is obviously
	// not true, but until FS.FS has a solid story for how to handle symlinks,
	// we are better off not making a decision that would be difficult to
	// revert later on.
	//
	// For further discussions on the topic, see:
	// https://github.com/golang/go/issues/49580
	return a.Stat(path)
}

// Stat implements the same method as documented on sys.FS
func (a *AdaptFS) Stat(path string) (sys.Stat_t, sys.Errno) {
	// ponytail: fs.StatFS avoids the open+fstat+close round trip (W2). Skipped on
	// Windows, where NewStat_t(info) can't fill Dev/Ino from a plain FileInfo —
	// only the handle-based open path (GetFileInformationByHandle) has them.
	// runtime.GOOS is a compile-time constant, so this branch folds away off Windows.
	if statFS, ok := a.FS.(fs.StatFS); ok && runtime.GOOS != "windows" {
		info, err := statFS.Stat(cleanPath(path))
		if errno := sys.UnwrapOSError(err); errno != 0 {
			return sys.Stat_t{}, errno
		}
		return sys.NewStat_t(info), 0
	}
	f, errno := a.OpenFile(path, sys.O_RDONLY, 0)
	if errno != 0 {
		return sys.Stat_t{}, errno
	}
	defer f.Close()
	return f.Stat()
}

// Readlink implements the same method as documented on sys.FS
func (a *AdaptFS) Readlink(string) (string, sys.Errno) {
	return "", sys.ENOSYS
}

// Mkdir implements the same method as documented on sys.FS
func (a *AdaptFS) Mkdir(string, fs.FileMode) sys.Errno {
	return sys.ENOSYS
}

// Chmod implements the same method as documented on sys.FS
func (a *AdaptFS) Chmod(string, fs.FileMode) sys.Errno {
	return sys.ENOSYS
}

// Rename implements the same method as documented on sys.FS
func (a *AdaptFS) Rename(string, string) sys.Errno {
	return sys.ENOSYS
}

// Rmdir implements the same method as documented on sys.FS
func (a *AdaptFS) Rmdir(string) sys.Errno {
	return sys.ENOSYS
}

// Link implements the same method as documented on sys.FS
func (a *AdaptFS) Link(string, string) sys.Errno {
	return sys.ENOSYS
}

// Symlink implements the same method as documented on sys.FS
func (a *AdaptFS) Symlink(string, string) sys.Errno {
	return sys.ENOSYS
}

// Unlink implements the same method as documented on sys.FS
func (a *AdaptFS) Unlink(string) sys.Errno {
	return sys.ENOSYS
}

// Utimens implements the same method as documented on sys.FS
func (a *AdaptFS) Utimens(string, int64, int64) sys.Errno {
	return sys.ENOSYS
}

func cleanPath(name string) string {
	if len(name) == 0 {
		return name
	}
	// fs.ValidFile cannot be rooted (start with '/')
	cleaned := name
	if name[0] == '/' {
		cleaned = name[1:]
	}
	cleaned = path.Clean(cleaned) // e.g. "sub/." -> "sub"
	return cleaned
}

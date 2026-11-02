//go:build !windows

package sysfs

import (
	"io/fs"
	"os"
	"strings"

	"github.com/samyfodil/wazy/sys"
)

func openFileIn(r *os.Root, name string, oflag sys.Oflag, perm fs.FileMode) (*os.File, sys.Errno) {
	if errno := noFollowIn(r, name, oflag); errno != 0 {
		return nil, errno
	}
	f, err := r.OpenFile(name, toOsOpenFlag(oflag), perm&fs.ModePerm)
	return f, rootErrno(err)
}

func statIn(r *os.Root, name string, follow bool) (sys.Stat_t, sys.Errno) {
	var info fs.FileInfo
	var err error
	if follow {
		info, err = r.Stat(name)
	} else {
		info, err = r.Lstat(name)
	}
	if err != nil {
		return sys.Stat_t{}, rootErrno(err)
	}
	return sys.NewStat_t(info), 0
}

// unlinkIn removes the file name. os.Root.Remove removes a directory as
// readily, which unlink must refuse.
func unlinkIn(r *os.Root, name string) sys.Errno {
	if info, err := r.Lstat(name); err != nil {
		return rootErrno(err)
	} else if info.IsDir() {
		return sys.EISDIR
	}
	return rootErrno(r.Remove(name))
}

// rmdirIn removes the directory name, which os.Root.Remove would do for a
// file as readily.
func rmdirIn(r *os.Root, name string) sys.Errno {
	if info, err := r.Lstat(name); err != nil {
		return rootErrno(err)
	} else if !info.IsDir() {
		return sys.ENOTDIR
	}
	// POSIX lets rmdir report a non-empty directory as either ENOTEMPTY or
	// EEXIST, and illumos picks EEXIST where Linux and the BSDs pick ENOTEMPTY.
	// EEXIST has no other meaning for rmdir, so fold it.
	if errno := rootErrno(r.Remove(name)); errno == sys.EEXIST {
		return sys.ENOTEMPTY
	} else {
		return errno
	}
}

func renameIn(r *os.Root, from, to string) sys.Errno {
	if from == to {
		return 0
	}
	// os.Root.Rename refuses to replace a directory as of Go 1.27, reporting
	// EEXIST, where rename(2) replaces an empty one and reports a file over a
	// directory as EISDIR. Replacing it takes removing it first, as on Windows.
	if toInfo, err := r.Lstat(to); err == nil && toInfo.IsDir() {
		if fromInfo, err := r.Lstat(from); err != nil {
			return rootErrno(err)
		} else if !fromInfo.IsDir() {
			return sys.EISDIR
		} else if os.SameFile(fromInfo, toInfo) {
			// On a case-insensitive filesystem, names that differ only in
			// case reach the same directory, and the rename changes the case.
			return rootErrno(r.Rename(from, to))
		}
		if strings.HasPrefix(to, strings.TrimSuffix(from, "/")+"/") {
			return sys.EINVAL // a directory cannot move into itself
		}
		if errno := rmdirIn(r, to); errno != 0 {
			return errno
		}
	}
	// The same fold as rmdir's: EEXIST is the only other way POSIX lets
	// rename report "new names a non-empty directory".
	if errno := rootErrno(r.Rename(from, to)); errno == sys.EEXIST {
		return sys.ENOTEMPTY
	} else {
		return errno
	}
}

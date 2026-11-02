package sysfs

import (
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/samyfodil/wazy/sys"
)

func openFileIn(r *os.Root, name string, oflag sys.Oflag, perm fs.FileMode) (*os.File, sys.Errno) {
	if errno := noFollowIn(r, name, oflag); errno != 0 {
		return nil, errno
	}
	flag := toOsOpenFlag(oflag)
	f, err := r.OpenFile(name, flag, perm&fs.ModePerm)
	if err == nil && flag&(os.O_WRONLY|os.O_RDWR) == 0 && flag&(os.O_CREATE|os.O_TRUNC) != 0 {
		// os.Root gives a file it creates or truncates write access, which a
		// read-only open must not have, so reopen it read-only.
		f.Close()
		f, err = r.OpenFile(name, flag&^(os.O_CREATE|os.O_TRUNC|os.O_EXCL), 0)
	}
	errno := rootErrno(err)
	switch errno {
	case 0:
		return f, 0
	case sys.EINVAL:
		// WASI expects ENOTDIR for a file path with a trailing slash.
		if strings.HasSuffix(name, "/") {
			errno = sys.ENOTDIR
		}
	case sys.ENOTDIR:
		if !strings.HasSuffix(name, "/") {
			errno = pathNotFound(r, name)
		}
	case sys.ENOENT:
		if info, err := r.Lstat(name); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			// Either symlink or hard link not found. We change the returned
			// errno depending on if it is symlink or not to have consistent
			// behavior across OSes.
			if oflag&sys.O_DIRECTORY != 0 {
				// Dangling symlink dir must raise ENOTDIR.
				errno = sys.ENOTDIR
			} else {
				errno = sys.ELOOP
			}
		}
	}
	return nil, errno
}

// pathNotFound is the errno for ERROR_PATH_NOT_FOUND, which Windows reports
// both when a directory on the way to name is missing and when it is a file.
// POSIX tells the two apart, as ENOENT and ENOTDIR, and TinyGo's
// TestStatBadDir and Go's TestOpenError each check one of them.
func pathNotFound(r *os.Root, name string) sys.Errno {
	for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if info, err := r.Stat(dir); err == nil {
			if info.IsDir() {
				return sys.ENOENT
			}
			return sys.ENOTDIR
		}
	}
	return sys.ENOENT
}

// statIn stats name through a handle, as statPath does, which is where the
// inode comes from on Windows.
func statIn(r *os.Root, name string, follow bool) (sys.Stat_t, sys.Errno) {
	var h syscall.Handle
	if follow {
		f, err := r.OpenFile(name, os.O_RDONLY, 0)
		if err != nil {
			return sys.Stat_t{}, statErrno(r, name, err)
		}
		defer f.Close()
		h = syscall.Handle(f.Fd())
	} else {
		var err error
		if h, err = openLinkIn(r, name, fileReadAttributes); err != nil {
			return sys.Stat_t{}, statErrno(r, name, err)
		}
		defer syscall.CloseHandle(h)
	}
	return statHandle(h)
}

// statErrno is the errno for an error opening name to stat it.
func statErrno(r *os.Root, name string, err error) sys.Errno {
	if errno := rootErrno(err); errno != sys.ENOTDIR {
		return errno
	}
	return pathNotFound(r, name)
}

// unlinkIn removes the file or symlink name. os.Root.Remove removes a
// directory as readily, which unlink must refuse.
func unlinkIn(r *os.Root, name string) sys.Errno {
	if info, err := r.Lstat(name); err != nil {
		return rootErrno(err)
	} else if info.IsDir() {
		return sys.EISDIR
	}
	return rootErrno(r.Remove(name))
}

// rmdirIn removes the directory name, which os.Root.Remove would do for a
// file or a directory symlink as readily.
func rmdirIn(r *os.Root, name string) sys.Errno {
	if info, err := r.Lstat(name); err != nil {
		return rootErrno(err)
	} else if !info.IsDir() {
		return sys.ENOTDIR
	}
	return rootErrno(r.Remove(name))
}

// sameFileIn reports whether from and to name the same file.
func sameFileIn(r *os.Root, from, to string) bool {
	fromSt, errno := statIn(r, from, false)
	if errno != 0 {
		return false
	}
	toSt, errno := statIn(r, to, false)
	return errno == 0 && fromSt.Dev == toSt.Dev && fromSt.Ino == toSt.Ino
}

func renameIn(r *os.Root, from, to string) sys.Errno {
	if from == to {
		return 0
	}

	// Rename acts on the names, not what a symlink at either one points to.
	var fromIsDir, toIsDir bool
	if info, err := r.Lstat(from); err != nil {
		return rootErrno(err) // failed to stat from
	} else {
		fromIsDir = info.IsDir()
	}
	if info, err := r.Lstat(to); err != nil {
		if errno := rootErrno(err); errno != sys.ENOENT {
			return errno // failed to stat to
		}
		return rootErrno(r.Rename(from, to)) // file or dir to not-exist is ok
	} else {
		toIsDir = info.IsDir()
	}
	if sameFileIn(r, from, to) {
		// Names that differ only in case reach the same file on Windows, and
		// renaming one to the other changes the case.
		return rootErrno(r.Rename(from, to))
	}

	// Now, handle known cases
	switch {
	case !fromIsDir && toIsDir: // file to dir
		return sys.EISDIR
	case fromIsDir && !toIsDir: // dir to file
		return sys.ENOTDIR
	case fromIsDir && toIsDir: // dir to dir
		// We can't tell if a directory is empty or not, via stat information.
		// Reading the directory is expensive, as it can buffer large amounts
		// of data on fail. Instead, speculatively try to remove the directory.
		// This is only one syscall and won't buffer anything.
		if errno := rmdirIn(r, to); errno != 0 && errno != sys.ENOENT {
			return errno
		}
	}
	return rootErrno(r.Rename(from, to))
}

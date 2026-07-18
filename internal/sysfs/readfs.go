package sysfs

import (
	"io/fs"

	"github.com/samyfodil/wazy/sys"
)

type ReadFS struct {
	sys.FS
}

// OpenFile implements the same method as documented on sys.FS
func (r *ReadFS) OpenFile(path string, flag sys.Oflag, perm fs.FileMode) (sys.File, sys.Errno) {
	// Mask the mutually exclusive bits as they determine write mode.
	switch flag & (sys.O_RDONLY | sys.O_WRONLY | sys.O_RDWR) {
	case sys.O_WRONLY, sys.O_RDWR:
		// Return the correct error if a directory was opened for write.
		if flag&sys.O_DIRECTORY != 0 {
			return nil, sys.EISDIR
		}
		return nil, sys.ENOSYS
	default: // sys.O_RDONLY (integer zero) so we are ok!
	}

	f, errno := r.FS.OpenFile(path, flag, perm)
	if errno != 0 {
		return nil, errno
	}
	return &readFile{f}, 0
}

// Mkdir implements the same method as documented on sys.FS
func (r *ReadFS) Mkdir(path string, perm fs.FileMode) sys.Errno {
	return sys.EROFS
}

// Chmod implements the same method as documented on sys.FS
func (r *ReadFS) Chmod(path string, perm fs.FileMode) sys.Errno {
	return sys.EROFS
}

// Rename implements the same method as documented on sys.FS
func (r *ReadFS) Rename(from, to string) sys.Errno {
	return sys.EROFS
}

// Rmdir implements the same method as documented on sys.FS
func (r *ReadFS) Rmdir(path string) sys.Errno {
	return sys.EROFS
}

// Link implements the same method as documented on sys.FS
func (r *ReadFS) Link(_, _ string) sys.Errno {
	return sys.EROFS
}

// Symlink implements the same method as documented on sys.FS
func (r *ReadFS) Symlink(_, _ string) sys.Errno {
	return sys.EROFS
}

// Unlink implements the same method as documented on sys.FS
func (r *ReadFS) Unlink(path string) sys.Errno {
	return sys.EROFS
}

// Utimens implements the same method as documented on sys.FS
func (r *ReadFS) Utimens(path string, atim, mtim int64) sys.Errno {
	return sys.EROFS
}

// compile-time check to ensure readFile implements api.File.
var _ sys.File = (*readFile)(nil)

type readFile struct {
	sys.File
}

// Write implements the same method as documented on sys.File.
func (r *readFile) Write([]byte) (int, sys.Errno) {
	return 0, r.writeErr()
}

// Pwrite implements the same method as documented on sys.File.
func (r *readFile) Pwrite([]byte, int64) (n int, errno sys.Errno) {
	return 0, r.writeErr()
}

// Truncate implements the same method as documented on sys.File.
func (r *readFile) Truncate(int64) sys.Errno {
	return r.writeErr()
}

// Sync implements the same method as documented on sys.File.
func (r *readFile) Sync() sys.Errno {
	return sys.EBADF
}

// Datasync implements the same method as documented on sys.File.
func (r *readFile) Datasync() sys.Errno {
	return sys.EBADF
}

// Utimens implements the same method as documented on sys.File.
func (r *readFile) Utimens(int64, int64) sys.Errno {
	return sys.EBADF
}

func (r *readFile) writeErr() sys.Errno {
	if isDir, errno := r.IsDir(); errno != 0 {
		return errno
	} else if isDir {
		return sys.EISDIR
	}
	return sys.EBADF
}

package sys

import (
	"github.com/samyfodil/wazy/sys"
)

// compile-time check to ensure lazyDir implements sys.File.
var _ sys.File = (*lazyDir)(nil)

type lazyDir struct {
	sys.DirFile

	fs sys.FS
	f  sys.File
}

// Dev implements the same method as documented on sys.File
func (d *lazyDir) Dev() (uint64, sys.Errno) {
	if f, ok := d.file(); !ok {
		return 0, sys.EBADF
	} else {
		return f.Dev()
	}
}

// Ino implements the same method as documented on sys.File
func (d *lazyDir) Ino() (sys.Inode, sys.Errno) {
	if f, ok := d.file(); !ok {
		return 0, sys.EBADF
	} else {
		return f.Ino()
	}
}

// IsDir implements the same method as documented on sys.File
func (d *lazyDir) IsDir() (bool, sys.Errno) {
	// Note: we don't return a constant because we don't know if this is really
	// backed by a dir, until the first call.
	if f, ok := d.file(); !ok {
		return false, sys.EBADF
	} else {
		return f.IsDir()
	}
}

// IsAppend implements the same method as documented on sys.File
func (d *lazyDir) IsAppend() bool {
	return false
}

// SetAppend implements the same method as documented on sys.File
func (d *lazyDir) SetAppend(bool) sys.Errno {
	return sys.EISDIR
}

// Seek implements the same method as documented on sys.File
func (d *lazyDir) Seek(offset int64, whence int) (newOffset int64, errno sys.Errno) {
	if f, ok := d.file(); !ok {
		return 0, sys.EBADF
	} else {
		return f.Seek(offset, whence)
	}
}

// Stat implements the same method as documented on sys.File
func (d *lazyDir) Stat() (sys.Stat_t, sys.Errno) {
	if f, ok := d.file(); !ok {
		return sys.Stat_t{}, sys.EBADF
	} else {
		return f.Stat()
	}
}

// Readdir implements the same method as documented on sys.File
func (d *lazyDir) Readdir(n int) (dirents []sys.Dirent, errno sys.Errno) {
	if f, ok := d.file(); !ok {
		return nil, sys.EBADF
	} else {
		return f.Readdir(n)
	}
}

// Sync implements the same method as documented on sys.File
func (d *lazyDir) Sync() sys.Errno {
	if f, ok := d.file(); !ok {
		return sys.EBADF
	} else {
		return f.Sync()
	}
}

// Datasync implements the same method as documented on sys.File
func (d *lazyDir) Datasync() sys.Errno {
	if f, ok := d.file(); !ok {
		return sys.EBADF
	} else {
		return f.Datasync()
	}
}

// Utimens implements the same method as documented on sys.File
func (d *lazyDir) Utimens(atim, mtim int64) sys.Errno {
	if f, ok := d.file(); !ok {
		return sys.EBADF
	} else {
		return f.Utimens(atim, mtim)
	}
}

// file returns the underlying file or false if it doesn't exist.
func (d *lazyDir) file() (sys.File, bool) {
	if f := d.f; d.f != nil {
		return f, true
	}
	var errno sys.Errno
	d.f, errno = d.fs.OpenFile(".", sys.O_RDONLY, 0)
	switch errno {
	case 0:
		return d.f, true
	case sys.ENOENT:
		return nil, false
	default:
		panic(errno) // unexpected
	}
}

// Close implements fs.File
func (d *lazyDir) Close() sys.Errno {
	f := d.f
	if f == nil {
		return 0 // never opened
	}
	return f.Close()
}

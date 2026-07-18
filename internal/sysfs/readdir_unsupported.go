//go:build !linux

package sysfs

import "github.com/samyfodil/wazy/sys"

// direntGetdentsSupported is false on every platform except Linux. See the
// docs on this constant in readdir_linux.go for why Linux has a dedicated
// getdents64-based fast path, and why it isn't worth doing for other
// platforms here.
const direntGetdentsSupported = false

// readdirGetdents is never invoked outside Linux, since osFile.Readdir
// only calls it when direntGetdentsSupported is true. It exists only so
// osFile.Readdir compiles on every platform.
func (f *osFile) readdirGetdents(int) ([]sys.Dirent, sys.Errno) {
	return nil, sys.ENOSYS
}

// releaseDirentBuf is a no-op outside Linux: f.direntBuf is always nil on
// other platforms, since only readdir_linux.go ever populates it. It
// exists only so osFile.close compiles on every platform.
func (f *osFile) releaseDirentBuf() {}

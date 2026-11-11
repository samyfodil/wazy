package sysfs

import "github.com/samyfodil/wazy/sys"

// LutimensFS is a sys.FS that sets the times of a symlink itself, where
// Utimens sets those of what it points to.
type LutimensFS interface {
	Lutimens(path string, atim, mtim int64) sys.Errno
}

// Lutimens implements LutimensFS.
func (d *dirFS) Lutimens(path string, atim, mtim int64) sys.Errno {
	if atim == sys.UTIME_OMIT && mtim == sys.UTIME_OMIT {
		return 0
	}
	r, name, errno := d.open(path)
	if errno != 0 {
		return errno
	}
	defer d.done(r)
	return lutimensIn(r, name, atim, mtim)
}

// Lutimens implements LutimensFS.
func (r *ReadFS) Lutimens(string, int64, int64) sys.Errno {
	return sys.EROFS
}

//go:build unix

package sysfs

import (
	"io/fs"
	"syscall"

	"github.com/samyfodil/wazy/sys"
)

func inoFromFileInfo(_ string, info fs.FileInfo) (sys.Inode, sys.Errno) {
	switch v := info.Sys().(type) {
	case *sys.Stat_t:
		return v.Ino, 0
	case *syscall.Stat_t:
		return v.Ino, 0
	default:
		return 0, 0
	}
}

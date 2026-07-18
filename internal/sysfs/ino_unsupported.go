//go:build !(unix || windows)

package sysfs

import (
	"io/fs"

	"github.com/samyfodil/wazy/sys"
)

func inoFromFileInfo(_ string, info fs.FileInfo) (sys.Inode, sys.Errno) {
	if v, ok := info.Sys().(*sys.Stat_t); ok {
		return v.Ino, 0
	}
	return 0, 0
}

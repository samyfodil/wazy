package sysfs

import (
	"io/fs"

	"github.com/samyfodil/wazy/sys"
)

func defaultStatFile(f fs.File) (sys.Stat_t, sys.Errno) {
	if info, err := f.Stat(); err != nil {
		return sys.Stat_t{}, sys.UnwrapOSError(err)
	} else {
		return sys.NewStat_t(info), 0
	}
}

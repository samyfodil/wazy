//go:build !(unix || windows)

package sysfs

import "github.com/samyfodil/wazy/sys"

func setNonblock(fd uintptr, enable bool) sys.Errno {
	return sys.ENOSYS
}

func isNonblock(f *osFile) bool {
	return false
}

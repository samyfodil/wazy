//go:build darwin || freebsd || netbsd

package sysfs

import (
	"syscall"

	"github.com/samyfodil/wazy/sys"
)

// statTimes returns the timestamps of d in epoch nanoseconds. See the POSIX
// variant of this file, which names the same fields differently.
func statTimes(d *syscall.Stat_t) (atim, mtim, ctim sys.EpochNanos) {
	return sys.EpochNanos(d.Atimespec.Sec)*1e9 + sys.EpochNanos(d.Atimespec.Nsec),
		sys.EpochNanos(d.Mtimespec.Sec)*1e9 + sys.EpochNanos(d.Mtimespec.Nsec),
		sys.EpochNanos(d.Ctimespec.Sec)*1e9 + sys.EpochNanos(d.Ctimespec.Nsec)
}

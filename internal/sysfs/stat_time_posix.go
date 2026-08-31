//go:build linux || openbsd || dragonfly || solaris

package sysfs

import (
	"syscall"

	"github.com/samyfodil/wazy/sys"
)

// statTimes returns the timestamps of d in epoch nanoseconds. The BSD variant
// of this file names the same fields differently, as in /sys.stat_bsd.go.
func statTimes(d *syscall.Stat_t) (atim, mtim, ctim sys.EpochNanos) {
	return sys.EpochNanos(d.Atim.Sec)*1e9 + sys.EpochNanos(d.Atim.Nsec),
		sys.EpochNanos(d.Mtim.Sec)*1e9 + sys.EpochNanos(d.Mtim.Nsec),
		sys.EpochNanos(d.Ctim.Sec)*1e9 + sys.EpochNanos(d.Ctim.Nsec)
}

//go:build linux || darwin

package sysfs

import (
	"syscall"

	"github.com/samyfodil/wazy/sys"
)

// timesToTimespecs fills times from atim and mtim, reporting false when both
// are omitted and there is nothing to change. times is the caller's array
// rather than a returned pointer, which the compiler had to heap-allocate on
// every call.
func timesToTimespecs(atim int64, mtim int64, times *[2]syscall.Timespec) bool {
	// When both inputs are omitted, there is nothing to change.
	if atim == sys.UTIME_OMIT && mtim == sys.UTIME_OMIT {
		return false
	}

	if atim == sys.UTIME_OMIT {
		times[0] = syscall.Timespec{Nsec: _UTIME_OMIT}
		times[1] = syscall.NsecToTimespec(mtim)
	} else if mtim == sys.UTIME_OMIT {
		times[0] = syscall.NsecToTimespec(atim)
		times[1] = syscall.Timespec{Nsec: _UTIME_OMIT}
	} else {
		times[0] = syscall.NsecToTimespec(atim)
		times[1] = syscall.NsecToTimespec(mtim)
	}
	return true
}

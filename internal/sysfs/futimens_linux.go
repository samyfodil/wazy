package sysfs

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/samyfodil/wazy/sys"
)

const _UTIME_OMIT = unix.UTIME_OMIT

// On linux, implement futimens via utimensat with the NUL path.
func futimens(fd uintptr, atim, mtim int64) sys.Errno {
	var times [2]syscall.Timespec
	if !timesToTimespecs(atim, mtim, &times) {
		return 0
	}
	return sys.UnwrapOSError(utimensat(int(fd), 0 /* NUL */, &times, 0))
}

// utimensat is like syscall.utimensat special-cased to accept a NUL string for the path value.
func utimensat(dirfd int, strPtr uintptr, times *[2]syscall.Timespec, flags int) (err error) {
	_, _, e1 := syscall.Syscall6(syscall.SYS_UTIMENSAT, uintptr(dirfd), strPtr, uintptr(unsafe.Pointer(times)), uintptr(flags), 0, 0)
	if e1 != 0 {
		err = e1
	}
	return
}

// utimensatNoFollow sets the times of name in the directory dirfd, a symlink's
// own rather than what it points to.
func utimensatNoFollow(dirfd int, name string, times *[2]syscall.Timespec) sys.Errno {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return sys.EINVAL
	}
	const atSymlinkNofollow = 0x100
	return sys.UnwrapOSError(utimensat(dirfd, uintptr(unsafe.Pointer(p)), times, atSymlinkNofollow))
}

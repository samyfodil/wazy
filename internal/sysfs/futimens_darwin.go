package sysfs

import (
	"syscall"
	"unsafe"

	"github.com/samyfodil/wazy/sys"
)

const _UTIME_OMIT = -2

func futimens(fd uintptr, atim, mtim int64) sys.Errno {
	var times [2]syscall.Timespec
	if !timesToTimespecs(atim, mtim, &times) {
		return 0
	}
	_p0 := unsafe.Pointer(&times[0])

	// Warning: futimens only exists since High Sierra (10.13).
	_, _, e1 := syscall_syscall6(libc_futimens_trampoline_addr, fd, uintptr(_p0), 0, 0, 0, 0)
	return sys.UnwrapOSError(e1)
}

// libc_futimens_trampoline_addr is the address of the
// `libc_futimens_trampoline` symbol, defined in `futimens_darwin.s`.
//
// We use this to invoke the syscall through syscall_syscall6 imported below.
var libc_futimens_trampoline_addr uintptr

// Imports the futimens symbol from libc as `libc_futimens`.
//
// Note: CGO mechanisms are used in darwin regardless of the CGO_ENABLED value
// or the "cgo" build flag. See /RATIONALE.md for why.
//go:cgo_import_dynamic libc_futimens futimens "/usr/lib/libSystem.B.dylib"

// utimensatNoFollow sets the times of name in the directory dirfd, a symlink's
// own rather than what it points to.
func utimensatNoFollow(dirfd int, name string, times *[2]syscall.Timespec) sys.Errno {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return sys.EINVAL
	}
	const atSymlinkNofollow = 0x20
	_, _, e1 := syscall_syscall6(libc_utimensat_trampoline_addr, uintptr(dirfd), uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(times)), atSymlinkNofollow, 0, 0)
	return sys.UnwrapOSError(e1)
}

var libc_utimensat_trampoline_addr uintptr

//go:cgo_import_dynamic libc_utimensat utimensat "/usr/lib/libSystem.B.dylib"

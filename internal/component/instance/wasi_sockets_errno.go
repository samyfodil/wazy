//go:build !plan9

package instance

import "syscall"

// Socket dial/bind errno values used to map net errors to wasi:sockets codes.
// Plan 9 lacks these syscall constants (and has no BSD-socket errno model), so
// it gets inert sentinels in wasi_sockets_errno_plan9.go instead.
var (
	errConnRefused  error = syscall.ECONNREFUSED
	errAddrInUse    error = syscall.EADDRINUSE
	errAddrNotAvail error = syscall.EADDRNOTAVAIL
)

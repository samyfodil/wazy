package platform

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// stackSpanAlign is the Go runtime's page size. A heap object at least
// stackMinLarge bytes long gets a span of its own that starts on such a
// boundary, so rounding the size up to it makes the object own every page it
// touches.
const (
	stackSpanAlign = 8192
	stackMinLarge  = 32768 + stackSpanAlign
)

// MakeStack returns a zeroed buffer of at least n bytes for JIT code to use as
// its stack.
//
// OpenBSD checks the stack pointer at every system call and page fault, and
// kills the thread with SIGSEGV unless it points into memory mapped with
// MAP_STACK. JIT code runs with the stack pointer inside this buffer, so a
// page fault there (first touch of a linear-memory or code page) on plain heap
// memory crashes the process. The Go runtime solves the same problem for
// goroutine stacks by remapping their spans with MAP_STACK (osStackAlloc in
// runtime/os_openbsd.go); this does the same for a heap object that owns its
// pages, so the garbage collector still frees it. Leaving MAP_STACK on the
// pages after they are freed is harmless: they stay ordinary read-write memory.
func MakeStack(n int) []byte {
	size := max(n, stackMinLarge)
	size = (size + stackSpanAlign - 1) &^ (stackSpanAlign - 1)
	buf := make([]byte, size)
	if _, err := unix.MmapPtr(-1, 0, unsafe.Pointer(&buf[0]), uintptr(size),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON|unix.MAP_FIXED|unix.MAP_STACK); err != nil {
		panic(err)
	}
	return buf
}

//go:build unix && !darwin

package platform

import (
	"os"

	"golang.org/x/sys/unix"
)

func mapCodeSegmentFromFile(f *os.File, offset int64, size int) (code []byte, err error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	// MAP_PRIVATE without PROT_WRITE: the pages are never written, so they
	// stay shared with the page cache, and with every other process mapping
	// the same entry.
	if cerr := rc.Control(func(fd uintptr) {
		code, err = unix.Mmap(int(fd), offset, size, unix.PROT_READ|unix.PROT_EXEC, unix.MAP_PRIVATE)
	}); cerr != nil {
		return nil, cerr
	}
	return code, err
}

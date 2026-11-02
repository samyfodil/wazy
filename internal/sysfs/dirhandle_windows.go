package sysfs

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var procGetFinalPathNameByHandleW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFinalPathNameByHandleW")

// openRootOf opens the directory f has open as an os.Root, and returns where
// it is now. A directory opened with FILE_SHARE_DELETE can be renamed while
// open, so it is found by the path its handle has now, not the one it was
// opened by.
func openRootOf(f *os.File, path string) (*os.Root, string, error) {
	if current, err := finalPath(syscall.Handle(f.Fd())); err == nil {
		if r, err := os.OpenRoot(current); err == nil {
			return r, current, nil
		}
	}
	r, err := os.OpenRoot(path)
	return r, path, err
}

func finalPath(h syscall.Handle) (string, error) {
	buf := make([]uint16, syscall.MAX_PATH)
	for {
		n, _, err := procGetFinalPathNameByHandleW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
		if n == 0 {
			return "", err
		}
		if int(n) < len(buf) {
			p := syscall.UTF16ToString(buf[:n])
			if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
				return `\\` + rest, nil
			}
			return strings.TrimPrefix(p, `\\?\`), nil
		}
		buf = make([]uint16, n)
	}
}

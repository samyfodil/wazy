package sysfs

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var procGetFinalPathNameByHandleW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFinalPathNameByHandleW")

// dirFSOf is the directory f has open as a DirFS, found by the path its
// handle has now: one opened with FILE_SHARE_DELETE can be renamed while open.
// It is unbound, opening its root for each operation, as a root held open
// keeps Windows from removing or renaming the directory.
func dirFSOf(f *os.File, path string) (*dirFS, error) {
	if current, err := finalPath(syscall.Handle(f.Fd())); err == nil {
		path = current
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return &dirFS{dir: path, cleanedDir: ensureTrailingPathSeparator(path)}, nil
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

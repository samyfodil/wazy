package sysfs

import (
	"os"
	"strconv"
)

// dirFSOf is the directory f has open as a DirFS bound to it, which holds
// the directory open. Opening /proc/self/fd/N reaches the directory itself,
// wherever it has been renamed to, and its path is the fallback.
func dirFSOf(f *os.File, path string) (*dirFS, error) {
	r, err := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
	if err != nil {
		if r, err = os.OpenRoot(path); err != nil {
			return nil, err
		}
	}
	return boundDirFS(r, path), nil
}

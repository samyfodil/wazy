package sysfs

import (
	"os"
	"strconv"
)

// openRootOf opens the directory f has open as an os.Root, and returns the
// path it was opened by, which names it until it is renamed. /proc/self/fd
// reaches the directory itself, wherever it has been renamed to, and the
// path is the fallback where /proc is not mounted.
func openRootOf(f *os.File, path string) (*os.Root, string, error) {
	if r, err := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(int(f.Fd()))); err == nil {
		return r, path, nil
	}
	r, err := os.OpenRoot(path)
	return r, path, err
}

//go:build !linux && !darwin && !windows

package sysfs

import "os"

// openRootOf opens the directory f has open as an os.Root, by its path, and
// returns that path. One renamed since it was opened is not found there.
func openRootOf(_ *os.File, path string) (*os.Root, string, error) {
	r, err := os.OpenRoot(path)
	return r, path, err
}

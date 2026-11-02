//go:build !linux && !darwin && !windows

package sysfs

import "os"

// dirFSOf is the directory f has open as a DirFS bound to it, opened by its
// path. One renamed since it was opened is not found there.
func dirFSOf(_ *os.File, path string) (*dirFS, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return boundDirFS(r, path), nil
}

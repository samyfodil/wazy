//go:build !(unix || windows) || darwin

// On macOS, mapping a cache file executable works, but the first process to
// do so for a given file waited over a second for it, as long as compiling a
// mid-sized module, and the mapping saved no resident memory (measured on an
// M4, macOS 26). So the code is copied, as before.

package platform

import (
	"errors"
	"os"
)

func mapCodeSegmentFromFile(*os.File, int64, int) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

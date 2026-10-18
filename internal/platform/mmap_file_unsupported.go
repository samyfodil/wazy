//go:build !unix || darwin

// Where the code is copied rather than mapped, and why:
//
//   - macOS: mapping a cache file executable works, but the first process to
//     do so for a given file waited over a second for it, as long as
//     compiling a mid-sized module, and the mapping saved no resident memory
//     (measured on an M4, macOS 26).
//   - Windows: a file view (PAGE_EXECUTE_READ) works too, but Windows refuses
//     to delete or replace a file while any view of it is open, and the view
//     lives until the garbage collector finalizes the module. Removing a
//     cache directory, which applications and tests routinely do, would fail
//     long after every module was closed.

package platform

import (
	"errors"
	"os"
)

func mapCodeSegmentFromFile(*os.File, int64, int) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

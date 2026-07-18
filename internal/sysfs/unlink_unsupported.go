//go:build !(unix || windows)

package sysfs

import (
	"os"

	"github.com/samyfodil/wazy/sys"
)

func unlink(name string) sys.Errno {
	err := os.Remove(name)
	return sys.UnwrapOSError(err)
}

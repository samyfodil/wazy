//go:build !linux || mips || mipsle || mips64 || mips64le

package sysfs

import (
	"io/fs"
	"os"

	"github.com/samyfodil/wazy/sys"
)

// Outside Linux there is no openat2, so every path resolves through os.Root.

func openRootDir(*os.Root) *os.File { return nil }

func openFileBeneath(*os.File, string, sys.Oflag, fs.FileMode) (*os.File, sys.Errno, bool) {
	return nil, 0, false
}

func statBeneath(*os.File, string, bool) (sys.Stat_t, sys.Errno, bool) {
	return sys.Stat_t{}, 0, false
}

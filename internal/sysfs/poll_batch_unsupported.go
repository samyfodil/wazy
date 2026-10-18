//go:build !(linux || darwin || windows)

package sysfs

import "github.com/samyfodil/wazy/sys"

// PollReadiness has no batched implementation where _poll itself is
// unsupported. Returning ok=false makes callers fall back to polling each
// file on its own.
func PollReadiness(_ []sys.Pollable, _ int32) (ready []bool, errno sys.Errno, ok bool) {
	return nil, 0, false
}

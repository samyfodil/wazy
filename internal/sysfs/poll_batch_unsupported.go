//go:build !(linux || darwin)

package sysfs

import "github.com/samyfodil/wazy/experimental/sys"

// PollReadiness has no batched implementation off linux/darwin: the Windows
// _poll returns only a ready count, not per-fd revents, so it can't drive the
// any-ready mapping PollReadiness needs. Returning ok=false makes callers fall
// back to the (correct) sequential per-fd Poll loop.
func PollReadiness(_ []sys.Pollable, _ int32) (ready []bool, errno sys.Errno, ok bool) {
	return nil, 0, false
}

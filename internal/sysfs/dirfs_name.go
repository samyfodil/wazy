//go:build !windows

package sysfs

import "github.com/samyfodil/wazy/sys"

// hostName refuses a name the host would read differently from the guest.
// Every name means the same here.
func hostName(string) sys.Errno { return 0 }

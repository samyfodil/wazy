package sysfs

import (
	"strings"

	"github.com/samyfodil/wazy/sys"
)

// hostName refuses a name the host would read differently from the guest. A
// backslash is an ordinary character in a WASI name but a separator to
// Windows, which cannot store it in a file name, so no such file exists.
func hostName(name string) sys.Errno {
	if strings.ContainsRune(name, '\\') {
		return sys.ENOENT
	}
	return 0
}

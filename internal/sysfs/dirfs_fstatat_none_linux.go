//go:build linux && loong64

package sysfs

// loong64 has no fstatat, only statx, so a name in the root itself is
// stat'ed through os.Root.
const sysFstatat = 0

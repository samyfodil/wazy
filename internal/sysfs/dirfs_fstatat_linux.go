//go:build linux && (amd64 || ppc64 || ppc64le || s390x)

package sysfs

import "syscall"

const sysFstatat = syscall.SYS_NEWFSTATAT

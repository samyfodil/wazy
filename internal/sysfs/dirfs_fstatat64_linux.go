//go:build linux && (386 || arm)

package sysfs

import "syscall"

const sysFstatat = syscall.SYS_FSTATAT64

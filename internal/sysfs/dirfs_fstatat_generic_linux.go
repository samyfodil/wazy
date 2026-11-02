//go:build linux && (arm64 || riscv64)

package sysfs

import "syscall"

const sysFstatat = syscall.SYS_FSTATAT

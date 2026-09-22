//go:build linux && (arm64 || riscv64)

package storage

import "syscall"

const sysFstatat = syscall.SYS_FSTATAT

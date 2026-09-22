//go:build linux

package storage

import "syscall"

const sysFstatat = syscall.SYS_NEWFSTATAT

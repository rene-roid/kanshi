//go:build linux && (amd64 || arm64 || riscv64)

package storage

import (
	"syscall"
	"unsafe"
)

// statAt lstats name relative to the open directory dirfd. name must be
// followed by a NUL in its backing array, as getdents64 names are, so it can
// go to the kernel as-is: no path join, no string, no allocation, and the
// kernel resolves one component instead of the whole path from the root.
func statAt(dirfd int, _, name []byte, st *syscall.Stat_t) error {
	_, _, errno := syscall.Syscall6(sysFstatat, uintptr(dirfd), uintptr(unsafe.Pointer(&name[0])),
		uintptr(unsafe.Pointer(st)), atSymlinkNofollow|atNoAutomount, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

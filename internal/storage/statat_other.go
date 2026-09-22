//go:build linux && !amd64 && !arm64 && !riscv64

package storage

import "syscall"

// statAt falls back to a full-path lstat on architectures where the Stat_t
// layout does not line up with a plain fstatat.
func statAt(_ int, dir, name []byte, st *syscall.Stat_t) error {
	return syscall.Lstat(join(string(dir), string(name)), st)
}

//go:build !linux

package access

import "syscall"

func control(_, _ string, _ syscall.RawConn) error { return nil }

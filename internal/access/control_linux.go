package access

import "syscall"

// tcpUserTimeout is TCP_USER_TIMEOUT, which package syscall does not name.
const tcpUserTimeout = 0x12

// control bounds how long sent data may go unacknowledged before the kernel
// drops the connection. A phone that leaves Wi-Fi without closing its stream
// otherwise keeps it open, and the poller awake, for the ~15 minutes TCP
// retransmission takes to give up. Accepted connections inherit it.
func control(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, 30_000)
	})
	if err != nil {
		return err
	}
	return serr
}

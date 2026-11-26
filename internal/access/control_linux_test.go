package access

import (
	"net"
	"net/netip"
	"syscall"
	"testing"
)

// Connections accepted from a kanshi listener give up on a vanished peer
// after 30 s instead of the kernel's ~15 minutes.
func TestAcceptedConnsHaveUserTimeout(t *testing.T) {
	ln, err := listen(netip.MustParseAddr("127.0.0.1"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		got, gerr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout)
	}); err != nil || gerr != nil {
		t.Fatal(err, gerr)
	}
	if got != 30_000 {
		t.Fatalf("TCP_USER_TIMEOUT = %d ms, want 30000", got)
	}
}

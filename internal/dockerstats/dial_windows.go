package dockerstats

import (
	"context"
	"net"
	"os"
	"syscall"
	"time"
)

const (
	errorPipeBusy        = 231
	fileFlagOverlapped   = 0x40000000
	securitySQOSPresent  = 0x00100000
	securityIdentifyOnly = 0x00010000
)

// dialPipe opens a named pipe as a net.Conn.
//
// The handle is opened for overlapped I/O, which os.NewFile (Go 1.25+) hands
// to the runtime poller: reads and writes park the goroutine instead of a
// thread, deadlines work, and closing the file cancels a pending read. That is
// everything an http.Transport needs, without a third-party pipe library.
//
// The security flags stop the daemon from impersonating this process with
// more than an identification-level token.
func dialPipe(ctx context.Context, path string) (net.Conn, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	for {
		h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
			syscall.OPEN_EXISTING, fileFlagOverlapped|securitySQOSPresent|securityIdentifyOnly, 0)
		if err == nil {
			return &pipeConn{File: os.NewFile(uintptr(h), path), addr: pipeAddr(path)}, nil
		}
		if err != syscall.Errno(errorPipeBusy) {
			return nil, &net.OpError{Op: "dial", Net: "npipe", Addr: pipeAddr(path), Err: err}
		}
		// Every instance of the pipe is serving another client; one frees up
		// within milliseconds.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type pipeAddr string

func (a pipeAddr) Network() string { return "npipe" }
func (a pipeAddr) String() string  { return string(a) }

type pipeConn struct {
	*os.File
	addr pipeAddr
}

func (c *pipeConn) LocalAddr() net.Addr  { return c.addr }
func (c *pipeConn) RemoteAddr() net.Addr { return c.addr }

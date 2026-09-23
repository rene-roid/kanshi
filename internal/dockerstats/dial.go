package dockerstats

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// dialer connects to the daemon named by a DOCKER_HOST-style address:
// unix:///var/run/docker.sock, npipe:////./pipe/docker_engine or
// tcp://host:2375. TLS endpoints are not supported; the daemon Kanshi watches
// is the local one.
func dialer(host string) (func(ctx context.Context) (net.Conn, error), error) {
	scheme, addr, ok := strings.Cut(host, "://")
	if !ok {
		return nil, fmt.Errorf("unsupported DOCKER_HOST %q", host)
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	switch scheme {
	case "unix":
		return func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "unix", addr) }, nil
	case "tcp":
		return func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }, nil
	case "npipe":
		// npipe:////./pipe/docker_engine → \\.\pipe\docker_engine
		pipe := strings.ReplaceAll(addr, "/", `\`)
		return func(ctx context.Context) (net.Conn, error) { return dialPipe(ctx, pipe) }, nil
	}
	return nil, fmt.Errorf("unsupported DOCKER_HOST scheme %q", scheme)
}

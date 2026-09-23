package dockerstats

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rene-roid/kanshi/internal/config"
)

func TestDialerSchemes(t *testing.T) {
	for _, host := range []string{"unix:///var/run/docker.sock", "tcp://127.0.0.1:2375", "npipe:////./pipe/docker_engine"} {
		if _, err := dialer(host); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
	for _, host := range []string{"/var/run/docker.sock", "ssh://box", "https://x"} {
		if _, err := dialer(host); err == nil {
			t.Errorf("%s should be rejected", host)
		}
	}
}

func TestAbsentDaemonBacksOff(t *testing.T) {
	missing := "unix://" + filepath.Join(t.TempDir(), "docker.sock")
	c := New(config.Config{DockerHost: missing, DockerConcurrency: 2})
	r := c.Sample(context.Background())
	if !r.Unavailable || r.Error == "" {
		t.Fatalf("first sample = %+v, want unavailable", r)
	}
	if time.Until(c.absentUntil) < retryAbsent-time.Second {
		t.Errorf("no back-off recorded: %v", c.absentUntil)
	}
	// While backing off the socket is not dialled again; the cached answer
	// comes straight back.
	c.http = nil
	if again := c.Sample(context.Background()); !again.Unavailable {
		t.Errorf("second sample = %+v", again)
	}
}

func TestPortsCollapseDualStack(t *testing.T) {
	meta := containerMeta{Ports: []portMeta{
		{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
		{IP: "::", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
		{IP: "127.0.0.1", PrivatePort: 443, PublicPort: 8443, Type: "tcp"},
		{PrivatePort: 9000, Type: "tcp"},
		{IP: "0.0.0.0", PrivatePort: 53, PublicPort: 53, Type: "udp"},
	}}
	got := ports(meta)
	want := []Port{
		{Public: 53, Private: 53, Type: "udp"},
		{Public: 8080, Private: 80, Type: "tcp"},
		{Public: 8443, Private: 443, Type: "tcp", IP: "127.0.0.1"},
		{Public: 0, Private: 9000, Type: "tcp"},
	}
	if len(got) != len(want) {
		t.Fatalf("ports = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("port %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

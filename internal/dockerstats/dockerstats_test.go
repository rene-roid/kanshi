package dockerstats

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

func fakeDaemon(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(config.Config{DockerHost: "tcp://" + srv.Listener.Addr().String(), DockerConcurrency: 2})
}

// A running container whose stats call fails stays in the list, marked, so
// the counts and the rows agree and a sick service does not vanish.
func TestFailedStatsKeepTheRow(t *testing.T) {
	c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			fmt.Fprint(w, `[{"Id":"aaaaaaaaaaaaaaaa","Names":["/web"],"State":"running","Status":"Up 1 hour"},
				{"Id":"bbbbbbbbbbbbbbbb","Names":["/db"],"State":"running","Status":"Up 1 hour"},
				{"Id":"cccccccccccccccc","Names":["/old"],"State":"exited","Status":"Exited (0)"}]`)
		case strings.Contains(r.URL.Path, "/bbbbbbbbbbbbbbbb/"):
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	r := c.Sample(context.Background())
	if r.Error != "" || r.Running != 2 || r.Total != 3 || len(r.Containers) != 3 {
		t.Fatalf("sample = %+v", r)
	}
	noStats := map[string]bool{}
	for _, ct := range r.Containers {
		noStats[ct.Name] = ct.NoStats
	}
	if want := map[string]bool{"web": false, "db": true, "old": false}; fmt.Sprint(noStats) != fmt.Sprint(want) {
		t.Errorf("no_stats = %v, want %v", noStats, want)
	}
}

// A daemon that accepts the connection and never answers is reported as
// slow, not as missing, and returns when the caller's deadline does.
func TestHungDaemonTimesOut(t *testing.T) {
	c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := c.Sample(ctx)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("sample took %v", took)
	}
	if r.Unavailable || r.Error != "Docker did not answer in time." {
		t.Fatalf("sample = %+v", r)
	}
}

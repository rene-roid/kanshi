// Command kanshi serves an at-a-glance dashboard for a single homeserver.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yuuki824/kanshi/internal/config"
	"github.com/yuuki824/kanshi/internal/server"
)

// The frontend is baked into the binary so the runtime image can be scratch —
// nothing to copy, nothing to keep in sync with the code that serves it.
//
//go:embed web
var embedded embed.FS

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe a running instance and exit; used by HEALTHCHECK")
	flag.Parse()

	cfg := config.Load()
	if *healthcheck {
		os.Exit(probe(cfg))
	}

	web, err := webFS(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kanshi:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := server.New(cfg, web).Run(ctx); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "kanshi:", err)
		os.Exit(1)
	}
}

func webFS(cfg config.Config) (fs.FS, error) {
	if cfg.WebDir != "" {
		return os.DirFS(cfg.WebDir), nil
	}
	return fs.Sub(embedded, "web")
}

// probe is the container health check. It runs in the same binary because the
// runtime image is scratch: there is no shell and no curl to call instead.
func probe(cfg config.Config) int {
	host := cfg.Host
	// With network_mode: host the bind address may be the Tailscale IP, but a
	// wildcard bind is only reachable from inside via loopback.
	if host == "0.0.0.0" || host == "::" || host == "" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/healthz", host, cfg.Port))
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

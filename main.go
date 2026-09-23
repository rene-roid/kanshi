// Command kanshi serves an at-a-glance dashboard for a single machine.
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
	"runtime"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/rene-roid/kanshi/internal/access"
	"github.com/rene-roid/kanshi/internal/config"
	"github.com/rene-roid/kanshi/internal/server"
)

// version is stamped by the release build with -ldflags "-X main.version=…".
var version = "dev"

// The frontend is baked into the binary so the runtime image can be scratch —
// nothing to copy, nothing to keep in sync with the code that serves it.
//
//go:embed web
var embedded embed.FS

func main() {
	var flags config.Flags
	flag.StringVar(&flags.Access, "access", "", "who can reach the dashboard: local, lan, tailscale, all, or a mix like lan,tailscale (default local)")
	flag.IntVar(&flags.Port, "port", 0, "port to listen on (default 8100)")
	flag.StringVar(&flags.File, "config", "", "settings file to read (default: "+config.FileName+" next to the program, then in the user config directory)")
	open := flag.Bool("open", ownConsole(), "open the dashboard in the browser once it is listening")
	healthcheck := flag.Bool("healthcheck", false, "probe a running instance and exit; used by the container HEALTHCHECK")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("kanshi", version)
		return
	}
	cfg := config.Load(flags)
	if *healthcheck {
		os.Exit(probe(cfg))
	}
	resourceDefaults()

	mode, err := access.Parse(cfg.Access)
	if err != nil {
		// A typo here should narrow access, never widen it.
		logf("warning: %v; falling back to local only", err)
		mode = access.Mode{}
	}

	web, err := webFS(cfg)
	if err != nil {
		fail(err)
	}
	srv, err := server.New(cfg, web, version, logf)
	if err != nil {
		fail(err)
	}
	if *open {
		srv.OnReady = func() { openBrowser("http://localhost:" + strconv.Itoa(cfg.Port)) }
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx, mode); err != nil {
		fail(err)
	}
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "kanshi: "+format+"\n", args...)
}

func fail(err error) {
	logf("%v", err)
	if ownConsole() {
		// Double-clicked on Windows: the console closes with the process, so
		// hold it open long enough to read why.
		fmt.Fprint(os.Stderr, "Press Enter to close this window.")
		_, _ = fmt.Scanln()
	}
	os.Exit(1)
}

// resourceDefaults gives a bare binary the same conservative budget the
// Compose file sets: two scheduler threads are plenty for a 5-second poll and
// one walk, and a soft memory limit makes an unusually large walk collect
// harder rather than grow. Either can be overridden with GOMAXPROCS and
// GOMEMLIMIT.
func resourceDefaults() {
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(min(2, runtime.NumCPU()))
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(128 << 20)
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
// Loopback is listened on in every access mode, so that is what it asks.
func probe(cfg config.Config) int {
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Port))
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

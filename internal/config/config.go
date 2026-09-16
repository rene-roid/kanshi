// Package config holds runtime configuration, all via environment variables.
//
// Defaults are deliberately conservative: this box has 4 cores and ~28 other
// containers, so Kanshi should be invisible in `docker stats`.
package config

import (
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Config is read once at startup and never mutated, so it needs no locking.
type Config struct {
	// How often the live poller samples vitals + container stats.
	PollInterval time.Duration
	// Stop polling entirely once no browser has been connected for this long.
	// Nobody is looking, so there is no reason to keep waking the Docker daemon.
	IdleTimeout time.Duration

	// Max concurrent /stats requests against the Docker socket per tick.
	DockerConcurrency int
	DockerSocket      string

	// Storage walk. Roots are "label=path" or just "path".
	StorageRoots    []string
	StorageInterval time.Duration
	// Absolute container-side paths to skip entirely. Their bytes vanish from
	// the totals, so only exclude things you truly don't want counted.
	StorageExclude   []string
	StorageMinRescan time.Duration

	// Tree pruning, to keep the JSON the phone downloads small.
	TreeDepth int
	// Children smaller than this fraction of their parent are folded into an
	// aggregate node rather than shipped individually.
	TreeMinFraction float64
	TreeMaxChildren int

	Host string
	Port int

	// Serve web assets from this directory instead of the ones baked into the
	// binary. Only useful when iterating on the frontend.
	WebDir string
}

// Root is one labelled entry from StorageRoots.
type Root struct {
	Label string
	Path  string
}

// Roots expands the "label=path" entries. A bare path is labelled with its
// own basename.
func (c Config) Roots() []Root {
	out := make([]Root, 0, len(c.StorageRoots))
	for _, entry := range c.StorageRoots {
		label, p, found := strings.Cut(entry, "=")
		if !found || p == "" {
			p = label
			if base := path.Base(strings.TrimRight(label, "/")); base != "" && base != "." {
				label = base
			}
		}
		out = append(out, Root{Label: label, Path: p})
	}
	return out
}

// Load reads the environment. Unparseable values fall back to the default
// rather than refusing to start — a typo in one knob should not take the
// dashboard down.
func Load() Config {
	return Config{
		PollInterval:      envSeconds("KANSHI_POLL_INTERVAL", 5*time.Second),
		IdleTimeout:       envSeconds("KANSHI_IDLE_TIMEOUT", 30*time.Second),
		DockerConcurrency: envInt("KANSHI_DOCKER_CONCURRENCY", 8),
		DockerSocket:      envString("KANSHI_DOCKER_SOCKET", "/var/run/docker.sock"),
		StorageRoots:      envList("KANSHI_STORAGE_ROOTS", "/=/hostfs,/mnt/data=/mnt/data"),
		StorageInterval:   envSeconds("KANSHI_STORAGE_INTERVAL", 1800*time.Second),
		StorageExclude:    envList("KANSHI_STORAGE_EXCLUDE", ""),
		StorageMinRescan:  envSeconds("KANSHI_STORAGE_MIN_RESCAN", 30*time.Second),
		TreeDepth:         envInt("KANSHI_TREE_DEPTH", 4),
		TreeMinFraction:   envFloat("KANSHI_TREE_MIN_FRACTION", 0.005),
		TreeMaxChildren:   envInt("KANSHI_TREE_MAX_CHILDREN", 24),
		Host:              envString("KANSHI_HOST", "0.0.0.0"),
		Port:              envInt("KANSHI_PORT", 8100),
		WebDir:            envString("KANSHI_WEB_DIR", ""),
	}
}

func envString(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return def
	}
	return v
}

func envFloat(name string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
	if err != nil {
		return def
	}
	return v
}

// envSeconds accepts a bare number of seconds, matching the Compose file's
// plain integers.
func envSeconds(name string, def time.Duration) time.Duration {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
	if err != nil || v <= 0 {
		return def
	}
	return time.Duration(v * float64(time.Second))
}

func envList(name, def string) []string {
	raw := os.Getenv(name)
	if raw == "" {
		raw = def
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Package config holds runtime configuration.
//
// Every setting is an environment variable. The same KEY=VALUE pairs can also
// live in an optional kanshi.env file, which is what makes a double-clicked
// Windows executable configurable at all. A handful of settings also have a
// command-line flag. Precedence is flag, then environment, then file, then the
// built-in default.
//
// Defaults are deliberately conservative: the box this was written for has 4
// cores and ~28 other containers, so Kanshi should be invisible in `docker stats`.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Source says where a setting's value came from. The dashboard only lets you
// change the access mode when it is not pinned by a flag or the environment,
// because a change there would be silently overridden on the next start.
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
	SourceFlag    Source = "flag"
)

// Config is read once at startup. Only the access mode changes afterwards, and
// that is owned by the server, not this struct.
type Config struct {
	// How often the live poller samples vitals + container stats.
	PollInterval time.Duration
	// Stop polling entirely once no browser has been connected for this long.
	// Nobody is looking, so there is no reason to keep waking the Docker daemon.
	IdleTimeout time.Duration

	// Max concurrent /stats requests against the Docker daemon per tick.
	DockerConcurrency int
	// A DOCKER_HOST-style address: unix://, npipe:// or tcp://.
	DockerHost string

	// Storage map. Entries are "auto", "label=path" or just "path".
	StorageRoots []string
	// A cached folder size older than this is walked again the next time
	// its parent is opened.
	StorageInterval time.Duration
	// Paths to skip entirely. Their bytes vanish from the totals, so only
	// exclude things you truly don't want counted.
	StorageExclude []string
	// Rescan skips folders sized more recently than this.
	StorageMinRescan time.Duration
	// Share of one core, in percent, the walk may average. It sleeps between
	// directories to stay under it. 0 or 100+ means unthrottled.
	StorageCPU float64

	// How many levels below a walked folder are cached from the same walk.
	// Anything deeper is walked when it is opened.
	TreeDepth int
	// The file folder sizes are cached in. Empty keeps them in memory only,
	// to be walked again after a restart.
	StorageCache string

	// Where the host's root filesystem is mounted when Kanshi runs in a
	// container. Empty on a bare-metal install.
	HostRoot string

	// Who can reach the dashboard: "local", "lan", "tailscale", "all", or a
	// comma-separated mix, plus explicit addresses. Parsed by package access.
	Access       string
	AccessSource Source
	Port         int

	// The kanshi.env that was loaded, or where one would be written if the
	// access mode is changed from the dashboard. Empty when there is nowhere
	// sensible to write, such as a read-only container.
	File       string
	FileLoaded bool

	// Serve web assets from this directory instead of the ones baked into the
	// binary. Only useful when iterating on the frontend.
	WebDir string
}

// Flags are the command-line overrides. A zero value means "not given".
type Flags struct {
	Access string
	Port   int
	File   string
}

// Load reads flags, environment and kanshi.env. Unparseable values fall back
// to the default rather than refusing to start — a typo in one knob should not
// take the dashboard down.
func Load(flags Flags) Config {
	path, loaded := findFile(flags.File)
	var file map[string]string
	if loaded {
		file, _ = ReadFile(path)
	}
	l := lookup{file: file}

	access, accessSrc := l.access(flags.Access)
	port := l.int("KANSHI_PORT", 8100)
	if flags.Port > 0 {
		port = flags.Port
	}

	return Config{
		PollInterval:      l.seconds("KANSHI_POLL_INTERVAL", 5*time.Second),
		IdleTimeout:       l.seconds("KANSHI_IDLE_TIMEOUT", 30*time.Second),
		DockerConcurrency: l.int("KANSHI_DOCKER_CONCURRENCY", 8),
		DockerHost:        l.dockerHost(),
		StorageRoots:      l.list("KANSHI_STORAGE_ROOTS", "auto"),
		StorageInterval:   l.seconds("KANSHI_STORAGE_INTERVAL", 6*time.Hour),
		StorageExclude:    l.list("KANSHI_STORAGE_EXCLUDE", ""),
		StorageMinRescan:  l.seconds("KANSHI_STORAGE_MIN_RESCAN", 30*time.Second),
		StorageCPU:        l.float("KANSHI_STORAGE_CPU", 25),
		TreeDepth:         l.int("KANSHI_TREE_DEPTH", 4),
		StorageCache:      l.string("KANSHI_STORAGE_CACHE", defaultStorageCache()),
		HostRoot:          strings.TrimRight(l.string("KANSHI_HOST_ROOT", ""), `/\`),
		Access:            access,
		AccessSource:      accessSrc,
		Port:              port,
		File:              path,
		FileLoaded:        loaded,
		WebDir:            l.string("KANSHI_WEB_DIR", ""),
	}
}

// defaultStorageCache is in the user's cache folder: ~/.cache/kanshi on Linux,
// %LocalAppData%\kanshi on Windows. A scratch container has no home, so there
// it is set explicitly.
func defaultStorageCache() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "kanshi", "storage.cache")
}

// lookup resolves one setting against the environment, then the file.
type lookup struct {
	file map[string]string
}

func (l lookup) get(name string) (string, Source) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, SourceEnv
	}
	if v := strings.TrimSpace(l.file[name]); v != "" {
		return v, SourceFile
	}
	return "", SourceDefault
}

// access resolves the access mode. KANSHI_HOST is the setting this replaced;
// it is still honoured, one level below KANSHI_ACCESS from the same source, so
// an existing .env keeps meaning what it meant.
func (l lookup) access(flag string) (string, Source) {
	if flag != "" {
		return flag, SourceFlag
	}
	if v := strings.TrimSpace(os.Getenv("KANSHI_ACCESS")); v != "" {
		return v, SourceEnv
	}
	if v := strings.TrimSpace(os.Getenv("KANSHI_HOST")); v != "" {
		return hostToAccess(v), SourceEnv
	}
	if v := strings.TrimSpace(l.file["KANSHI_ACCESS"]); v != "" {
		return v, SourceFile
	}
	if v := strings.TrimSpace(l.file["KANSHI_HOST"]); v != "" {
		return hostToAccess(v), SourceFile
	}
	return "local", SourceDefault
}

func hostToAccess(host string) string {
	switch host {
	case "0.0.0.0", "::", "[::]", "*":
		return "all"
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return "local"
	}
	return host
}

// dockerHost honours DOCKER_HOST like the docker CLI does, then the older
// KANSHI_DOCKER_SOCKET (a bare socket path), then the platform default.
func (l lookup) dockerHost() string {
	if v, _ := l.get("DOCKER_HOST"); v != "" {
		return v
	}
	if v, _ := l.get("KANSHI_DOCKER_SOCKET"); v != "" {
		if strings.Contains(v, "://") {
			return v
		}
		return "unix://" + v
	}
	return defaultDockerHost
}

func (l lookup) string(name, def string) string {
	if v, _ := l.get(name); v != "" {
		return v
	}
	return def
}

func (l lookup) int(name string, def int) int {
	v, _ := l.get(name)
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func (l lookup) float(name string, def float64) float64 {
	v, _ := l.get(name)
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return n
}

// seconds accepts a bare number of seconds, matching the Compose file's plain
// integers.
func (l lookup) seconds(name string, def time.Duration) time.Duration {
	v, _ := l.get(name)
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 {
		return def
	}
	return time.Duration(n * float64(time.Second))
}

func (l lookup) list(name, def string) []string {
	raw, _ := l.get(name)
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

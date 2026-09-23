// Package roots decides which directories the storage map covers and reads
// how full their volumes are.
//
// With no configuration it picks sensible roots for the platform: on Linux the
// root filesystem plus every drive mounted under /mnt, on Windows the system
// drive, the user's profile folder and every other fixed drive. Explicit
// entries replace that, and the word "auto" mixes the defaults back in.
package roots

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Root is one entry in the storage map. Label is the path as the user knows
// it; Path is where Kanshi actually reads it, which differs only inside a
// container, where the host's / is mounted at KANSHI_HOST_ROOT.
type Root struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

// Sep is the separator labels are written with. The browser needs it to parse
// a pasted path and to draw breadcrumbs.
const Sep = string(filepath.Separator)

// How long a resolved list is reused. Vitals asks every tick; re-reading the
// mount table that often would be pointless work for something that changes
// when a drive is plugged in.
const cacheFor = 30 * time.Second

// Resolver turns the configured entries into roots, and caches the answer.
type Resolver struct {
	spec     []string
	hostRoot string

	mu     sync.Mutex
	cached []Root
	at     time.Time
}

func NewResolver(spec []string, hostRoot string) *Resolver {
	if hostRoot != "" && !isDir(hostRoot) {
		// The image sets KANSHI_HOST_ROOT=/hostfs; run without that mount,
		// the container's own filesystem is all there is to show.
		hostRoot = ""
	}
	return &Resolver{spec: spec, hostRoot: hostRoot}
}

// Roots returns the cached list, refreshing it once it is stale.
func (r *Resolver) Roots() []Root {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cached == nil || time.Since(r.at) > cacheFor {
		r.cached, r.at = r.resolve(), time.Now()
	}
	return r.cached
}

// Refresh re-resolves now. The storage walk calls this so a drive mounted a
// moment ago is included.
func (r *Resolver) Refresh() []Root {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cached, r.at = r.resolve(), time.Now()
	return r.cached
}

// HostRoot is the prefix host paths are read under, or "" on bare metal.
func (r *Resolver) HostRoot() string { return r.hostRoot }

func (r *Resolver) resolve() []Root {
	out := []Root{}
	seen := map[string]bool{}
	add := func(root Root) {
		key := Key(root.Path)
		if !seen[key] {
			seen[key] = true
			out = append(out, root)
		}
	}
	for _, entry := range r.spec {
		if strings.EqualFold(entry, "auto") {
			for _, root := range auto(r.hostRoot) {
				add(root)
			}
			continue
		}
		add(r.explicit(entry))
	}
	return out
}

// explicit parses "label=path" or a bare path. A bare path is labelled with
// itself, as seen from the host. Inside a container, a host path that does
// not exist there is looked up under the host root instead — so an old
// "/mnt/data" entry keeps working after its dedicated volume is gone.
func (r *Resolver) explicit(entry string) Root {
	label, p, found := strings.Cut(entry, "=")
	if !found || p == "" {
		p, label = label, ""
	}
	if r.hostRoot != "" && !within(r.hostRoot, p) && !isDir(p) && isDir(r.hostRoot+p) {
		p = r.hostRoot + p
	}
	if label == "" {
		label = r.HostPath(p)
	}
	return Root{Label: label, Path: p}
}

// HostPath strips the host-root prefix, turning "/hostfs/mnt/data" into
// "/mnt/data".
func (r *Resolver) HostPath(p string) string {
	if r.hostRoot == "" || !within(r.hostRoot, p) {
		return p
	}
	rest := p[len(r.hostRoot):]
	if rest == "" {
		return "/"
	}
	return rest
}

// ContainerPath is the reverse of HostPath: where a host path is readable.
// Excludes are written as host paths, so they go through this.
func (r *Resolver) ContainerPath(p string) string {
	if r.hostRoot == "" || within(r.hostRoot, p) {
		return p
	}
	return r.hostRoot + p
}

// within reports whether p is dir itself or somewhere below it.
func within(dir, p string) bool {
	d, q := Key(dir), Key(p)
	if d == q {
		return true
	}
	if !strings.HasSuffix(d, Sep) {
		d += Sep
	}
	return strings.HasPrefix(q, d)
}

// Within reports whether child is parent itself or inside it, comparing the
// way the platform's filesystem does.
func Within(parent, child string) bool { return within(parent, child) }

// Key normalises a path for comparison: cleaned, and on Windows lower-cased,
// since NTFS treats C:\Users and c:\users as the same directory.
func Key(p string) string {
	p = filepath.Clean(p)
	if caseInsensitive {
		p = strings.ToLower(p)
	}
	return p
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// Usage is how full the volume holding a root is.
type Usage struct {
	Total, Used, Free uint64
	// Volume identifies the filesystem, so two roots on the same one share a
	// single meter rather than showing the same numbers twice.
	Volume string
}

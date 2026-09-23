// Package storage builds a Filelight-style directory size map.
//
// The walk is one breadth-first pass per volume, run on a slow timer and
// cached — never recomputed per refresh, and never while nobody is looking.
// Sizes are what the files occupy on disk (st_blocks on Linux, the allocation
// size on Windows), so they match `du` rather than apparent size, and
// hardlinked files are counted once.
//
// The page never downloads the tree. It gets a one-line summary per root, then
// asks for one directory's listing at a time as it is drilled into, which the
// server answers from the cached walk without touching the disk.
package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rene-roid/kanshi/internal/config"
	"github.com/rene-roid/kanshi/internal/roots"
)

const (
	// A volume whose used space has moved less than this since the last walk
	// is not walked again: the map would come out the same. The larger of the
	// two wins, so a 16 TB array is not rescanned over a few gigabytes.
	changeFloor = 256 << 20
	changeShare = 0.005
	// Past this age a watched map is walked regardless, since moving files
	// around within a volume changes the map without changing used space.
	maxAge = 6 * time.Hour
)

/* ── wire payload ───────────────────────────────────────────────────────── */

// Status is the part of the snapshot that moves during a walk. It rides on
// every live frame, so the page only refetches anything when ScannedAt
// changes.
type Status struct {
	ScannedAt *float64  `json:"scanned_at"`
	Duration  *float64  `json:"duration"`
	Scanning  bool      `json:"scanning"`
	Progress  *Progress `json:"progress,omitempty"`
	Error     *string   `json:"error"`
}

// Progress is a live estimate of how far the in-flight walk has gotten,
// measured against the byte totals from the previous completed scan. There is
// nothing to compare against on the very first walk ever, so Progress is
// omitted rather than shipping a meaningless 0%.
type Progress struct {
	Root       string  `json:"root"`
	Percent    float64 `json:"percent"`
	BytesDone  int64   `json:"bytes_done"`
	BytesTotal int64   `json:"bytes_total"`
}

// Root summarises one walked root.
type Root struct {
	Name        string  `json:"name"`
	Path        string  `json:"root"`
	Size        int64   `json:"size"`
	Unreadable  int     `json:"unreadable"`
	WalkSeconds float64 `json:"walk_seconds"`
}

// Snapshot is what /api/storage returns. Sep is the separator root names are
// written with, so the page can split a pasted path and draw breadcrumbs.
type Snapshot struct {
	Status
	Roots []Root `json:"roots"`
	Sep   string `json:"sep"`
}

// Entry is one named item in a listing.
type Entry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Listing is one directory from the cached walk: every subdirectory, the
// largest files, and totals for everything that was counted but not named, so
// the parts always add up to Size.
type Listing struct {
	// Path is what was actually resolved, relative to the root, with "/"
	// between names on every platform. When the requested path no longer
	// exists it is the deepest ancestor that does, and Partial is set.
	Path    string  `json:"path"`
	Partial bool    `json:"partial,omitempty"`
	Size    int64   `json:"size"`
	Dirs    []Entry `json:"dirs"`
	Files   []Entry `json:"files"`
	// All files directly inside, including the ones named in Files.
	FileCount int   `json:"file_count"`
	FileBytes int64 `json:"file_bytes"`
	// Bytes in subdirectories below the scan depth, which have no entry of
	// their own.
	Deep int64 `json:"deep"`
}

/* ── scanner ────────────────────────────────────────────────────────────── */

// view is one root's place in a walked tree. A root that sits inside another
// root on the same volume shares that root's tree, starting further down.
type view struct {
	t     *tree
	start int32
}

// Scanner owns the cached walk. Reads are served under a read lock; only one
// walk runs at a time.
type Scanner struct {
	cfg   config.Config
	roots *roots.Resolver

	mu          sync.RWMutex
	status      Status
	published   []Root
	views       []view // parallel to published; immutable once published
	currentRoot string // label of the root the in-flight walk is on
	scannedAt   time.Time
	// What the volumes looked like when the published walk started, so the
	// scheduler can tell whether another walk would show anything new.
	baseline    map[string]uint64
	baselineKey string

	// progressDone and progressTotal change once per directory, far more often
	// than anything else here, so they are plain atomics rather than going
	// through mu, which every live frame reads.
	progressDone  atomic.Int64
	progressTotal atomic.Int64

	// scanMu serialises walks. A second request while one is in flight gets
	// the current snapshot rather than queueing a duplicate walk.
	scanMu   sync.Mutex
	lastScan time.Time
}

func New(cfg config.Config, r *roots.Resolver) *Scanner {
	return &Scanner{cfg: cfg, roots: r, published: []Root{}}
}

// Status returns the scan state plus a live progress estimate while a walk is
// running. It is cheap enough to call on every live tick.
func (s *Scanner) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.status
	if out.Scanning {
		if total := s.progressTotal.Load(); total > 0 {
			done := s.progressDone.Load()
			pct := round1(min(100, float64(done)/float64(total)*100))
			out.Progress = &Progress{Root: s.currentRoot, Percent: pct, BytesDone: done, BytesTotal: total}
		}
	}
	return out
}

func (s *Scanner) Snapshot() Snapshot {
	status := s.Status()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{Status: status, Roots: s.published, Sep: roots.Sep}
}

// List resolves rel ("home/yuuki", or "" for the root itself) against root
// number rootIdx of the last completed walk. It reports false only when there
// is no such root; a path that has since disappeared resolves to its deepest
// surviving ancestor instead.
func (s *Scanner) List(rootIdx int, rel string) (Listing, bool) {
	s.mu.RLock()
	views := s.views
	s.mu.RUnlock()
	if rootIdx < 0 || rootIdx >= len(views) {
		return Listing{}, false
	}
	t := views[rootIdx].t

	var out Listing
	n := t.nodes[views[rootIdx].start]
	var resolved []string
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		var hit *dirNode
		for _, c := range n.children {
			if sameName(t.nodes[c].name, seg) {
				hit = t.nodes[c]
				break
			}
		}
		if hit == nil {
			out.Partial = true
			break
		}
		n = hit
		resolved = append(resolved, seg)
	}

	out.Path = strings.Join(resolved, "/")
	out.Size = n.size
	out.Dirs = make([]Entry, len(n.children))
	for i, c := range n.children {
		out.Dirs[i] = Entry{Name: t.nodes[c].name, Size: t.nodes[c].size}
	}
	out.Files = make([]Entry, len(n.top))
	for i, f := range n.top {
		out.Files[i] = Entry{Name: f.name, Size: f.size}
	}
	out.FileCount = int(n.files)
	out.FileBytes = n.self
	out.Deep = n.deep
	return out, true
}

// previousTotal sums the byte sizes from the last completed scan, the
// baseline a new walk's progress is measured against. Roots nested inside
// another are already part of its total, so they are not added again.
func (s *Scanner) previousTotal() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total int64
	for i, r := range s.published {
		if s.views[i].start == 0 {
			total += r.Size
		}
	}
	return total
}

// beginScan applies the rate limit and flips Scanning on before returning, so
// a caller that immediately reads Snapshot() sees the walk has started even
// though the walk itself hasn't produced anything yet. It reports whether a
// scan was actually started; when it was, the caller must arrange for
// runScan to be called exactly once to release scanMu.
func (s *Scanner) beginScan(force bool) bool {
	if !s.scanMu.TryLock() {
		return false
	}
	if !force && !s.lastScan.IsZero() && time.Since(s.lastScan) < s.cfg.StorageMinRescan {
		s.scanMu.Unlock()
		return false
	}
	s.progressTotal.Store(s.previousTotal())
	s.progressDone.Store(0)
	s.mu.Lock()
	s.status.Scanning = true
	s.mu.Unlock()
	return true
}

func (s *Scanner) runScan(ctx context.Context) Snapshot {
	defer s.scanMu.Unlock()
	started := time.Now()

	list := s.roots.Refresh()
	baseline := usage(list)
	published, views, err := s.walkAll(ctx, list)

	s.mu.Lock()
	s.status.Scanning = false
	if err != nil {
		msg := err.Error()
		s.status.Error = &msg
	} else {
		elapsed := round2(time.Since(started).Seconds())
		at := float64(started.UnixNano()) / 1e9
		s.published, s.views, s.status.Error = published, views, nil
		s.status.ScannedAt, s.status.Duration = &at, &elapsed
		s.scannedAt, s.baseline, s.baselineKey = started, baseline, rootsKey(list)
	}
	s.mu.Unlock()

	s.lastScan = time.Now()
	// The walk's scratch space (the inode set, the BFS frontier) dwarfs what
	// is kept, and the next walk is half an hour away at the earliest. Hand it
	// back to the kernel now instead of letting it sit in the heap until then.
	debug.FreeOSMemory()
	return s.Snapshot()
}

// Scan rebuilds the tree, blocking until the walk finishes. Rate-limited
// unless force is set.
func (s *Scanner) Scan(ctx context.Context, force bool) Snapshot {
	if !s.beginScan(force) {
		return s.Snapshot()
	}
	return s.runScan(ctx)
}

// ScanAsync starts a rescan in the background and returns immediately with
// Scanning already true, so a REST handler can hand the browser something to
// watch instead of blocking the request for the length of the walk.
func (s *Scanner) ScanAsync(ctx context.Context, force bool) Snapshot {
	if !s.beginScan(force) {
		return s.Snapshot()
	}
	go s.runScan(ctx)
	return s.Snapshot()
}

func (s *Scanner) setCurrentRoot(label string) {
	s.mu.Lock()
	s.currentRoot = label
	s.mu.Unlock()
}

/* ── scheduling ─────────────────────────────────────────────────────────── */

// Loop keeps the map fresh — but only while somebody is looking at it.
//
// A full walk reads gigabytes of directory metadata, and with the cache
// squeezed by a small memory limit it reads them from disk every time. Doing
// that every half hour for nobody is the single most expensive thing Kanshi
// could do, so after the opening walk the map is only refreshed while a
// browser is connected (watching reports that), when a new one connects
// (wake), and then only if the volumes have actually changed.
func (s *Scanner) Loop(ctx context.Context, watching func() bool, wake <-chan struct{}) {
	s.Scan(ctx, true)

	interval := max(s.cfg.StorageInterval, time.Minute)
	lastCheck := time.Now()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-wake:
		}
		next := interval
		if watching() {
			if since := time.Since(lastCheck); since >= interval || s.rootsChanged() {
				lastCheck = time.Now()
				if s.stale() {
					s.Scan(ctx, true)
				}
			} else {
				next = interval - since
			}
		}
		timer.Reset(next)
	}
}

// rootsChanged reports a drive mounted or removed since the last walk. That
// is worth a walk straight away, not at the next interval.
func (s *Scanner) rootsChanged() bool {
	key := rootsKey(s.roots.Refresh())
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.baseline != nil && key != s.baselineKey
}

// stale reports whether a walk now would show something the published one
// does not.
func (s *Scanner) stale() bool {
	s.mu.RLock()
	baseline, key, at := s.baseline, s.baselineKey, s.scannedAt
	s.mu.RUnlock()
	if baseline == nil || time.Since(at) >= maxAge {
		return true
	}
	list := s.roots.Refresh()
	if rootsKey(list) != key {
		return true
	}
	for _, r := range list {
		u, err := roots.DiskUsage(r.Path)
		if err != nil {
			continue
		}
		prev, ok := baseline[r.Path]
		if !ok {
			return true
		}
		threshold := max(uint64(changeFloor), uint64(float64(u.Total)*changeShare))
		if absDiff(u.Used, prev) > threshold {
			return true
		}
	}
	return false
}

func usage(list []roots.Root) map[string]uint64 {
	out := make(map[string]uint64, len(list))
	for _, r := range list {
		if u, err := roots.DiskUsage(r.Path); err == nil {
			out[r.Path] = u.Used
		}
	}
	return out
}

func rootsKey(list []roots.Root) string {
	parts := make([]string, len(list))
	for i, r := range list {
		parts[i] = r.Label + "=" + r.Path
	}
	return strings.Join(parts, "\x00")
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

/* ── walking every root ─────────────────────────────────────────────────── */

func (s *Scanner) walkAll(ctx context.Context, list []roots.Root) ([]Root, []view, error) {
	// Linux applies setpriority(PRIO_PROCESS) and ioprio_set to the calling
	// thread, and Windows' background mode is per thread too, so the goroutine
	// is pinned first. It never unlocks: the runtime then retires the
	// deprioritised thread when the walk returns, instead of handing it back
	// to the scheduler for the poller to land on.
	done := make(chan struct{})
	var published []Root
	var views []view
	var err error
	go func() {
		defer close(done)
		runtime.LockOSThread()
		deprioritise()
		published, views, err = s.walkRoots(ctx, list)
	}()

	select {
	case <-done:
		return published, views, err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

type walked struct {
	view
	unreadable int
	seconds    float64
}

// walkRoots walks each volume once. Roots are taken outermost first, and any
// root inside the one being walked is picked up on the way past: C:\ and
// C:\Users\you cost one pass, not two. A nested root the outer walk could not
// reach — on another filesystem, excluded, or too deep — gets its own walk
// when its turn comes.
func (s *Scanner) walkRoots(ctx context.Context, list []roots.Root) ([]Root, []view, error) {
	paths := make([]string, len(list))
	for i, r := range list {
		paths[i] = r.Path
		if real, err := filepath.EvalSymlinks(r.Path); err == nil {
			paths[i] = real // the walk never follows links, so start past them
		}
	}
	order := make([]int, len(list))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(paths[order[a]]) < len(paths[order[b]]) })

	w := &walker{
		maxDepth: s.cfg.TreeDepth,
		excluded: s.excludes(),
		progress: &s.progressDone,
		pace:     newThrottle(s.cfg.StorageCPU),
		rd:       newDirReader(),
	}
	results := make([]*walked, len(list))
	for _, i := range order {
		if results[i] != nil {
			continue
		}
		if _, err := rootDevice(paths[i]); err != nil {
			continue // not mounted, or not a directory: leave it out
		}
		var nested []*nestedRoot
		for _, j := range order {
			if j != i && results[j] == nil && roots.Key(paths[j]) != roots.Key(paths[i]) && roots.Within(paths[i], paths[j]) {
				nested = append(nested, &nestedRoot{idx: j, path: paths[j], node: -1})
			}
		}

		s.setCurrentRoot(list[i].Label)
		started := time.Now()
		t, err := w.walk(ctx, paths[i], nested)
		if err != nil {
			return nil, nil, fmt.Errorf("%T: %w", err, err)
		}
		secs := round2(time.Since(started).Seconds())
		results[i] = &walked{view{t, 0}, t.unreadable, secs}
		for _, n := range nested {
			if n.node >= 0 {
				results[n.idx] = &walked{view{t, n.node}, n.unreadable, secs}
			}
		}
	}

	published := []Root{}
	var views []view
	for i, r := range results {
		if r == nil {
			continue
		}
		published = append(published, Root{
			Name:        list[i].Label,
			Path:        list[i].Path,
			Size:        r.t.nodes[r.start].size,
			Unreadable:  r.unreadable,
			WalkSeconds: r.seconds,
		})
		views = append(views, r.view)
	}
	return published, views, nil
}

// excludes normalises KANSHI_STORAGE_EXCLUDE. Entries may be written as host
// paths or, inside a container, as the paths the container sees; both forms
// are matched.
func (s *Scanner) excludes() map[string]bool {
	out := make(map[string]bool, 2*len(s.cfg.StorageExclude))
	for _, p := range s.cfg.StorageExclude {
		out[roots.Key(p)] = true
		out[roots.Key(s.roots.ContainerPath(p))] = true
	}
	return out
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

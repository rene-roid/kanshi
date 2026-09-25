// Package storage builds a Filelight-style directory size map, one folder at
// a time.
//
// Opening a folder reads that one directory live, and takes each subfolder's
// size from a small cache file. A subfolder the cache has never seen, or has
// not seen for StorageInterval, is walked in the background: only that
// subtree, and every folder the walk passes within TreeDepth levels is cached
// too, so drilling further in is usually instant. Nothing is walked at
// startup or for a page nobody has open, and the cache outlives restarts.
//
// Sizes are what files occupy on disk (st_blocks on Linux, the allocation
// size on Windows), so they match `du` rather than apparent size, and
// hard-linked files are counted once per walk.
package storage

import (
	"context"
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

/* ── wire payload ───────────────────────────────────────────────────────── */

// Status is the part of the snapshot that moves while folders are sized. It
// rides on every live frame, so the page only refetches anything when
// UpdatedAt changes.
type Status struct {
	Scanning bool `json:"scanning"`
	// The folder being walked, as the host knows it, and how many wait
	// behind it.
	Current  string    `json:"current,omitempty"`
	Queued   int       `json:"queued"`
	Progress *Progress `json:"progress,omitempty"`
	// When the last walk finished.
	UpdatedAt *float64 `json:"updated_at"`
	Error     *string  `json:"error"`
}

// Progress is a live estimate of how far the current walk has gotten,
// measured against the folder's size the last time it was walked. A folder
// walked for the first time has nothing to compare against, so Progress is
// omitted rather than shipping a meaningless 0%.
type Progress struct {
	Percent    float64 `json:"percent"`
	BytesDone  int64   `json:"bytes_done"`
	BytesTotal int64   `json:"bytes_total"`
}

// Root summarises one configured root.
type Root struct {
	Name string `json:"name"`
	Path string `json:"root"`
	// Null until the root has been opened with every folder in it sized.
	Size       *int64 `json:"size"`
	Unreadable int    `json:"unreadable"`
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
	// Waiting to be walked. Size is 0 until it has been.
	Pending bool `json:"pending,omitempty"`
}

// Listing is one directory: every subfolder, the largest files, and totals
// for the files that were counted but not named, so the parts always add up
// to Size once nothing is pending.
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
	// Subfolders that have no size yet.
	Pending int `json:"pending"`
	// Directories in here that could not be entered, so were not counted.
	Unreadable int `json:"unreadable"`
	// When the oldest subfolder size shown was measured; null when none is.
	SizedAt *float64 `json:"sized_at"`
}

/* ── scanner ────────────────────────────────────────────────────────────── */

// job is one folder waiting to be walked.
type job struct {
	path  string // as read, links resolved
	key   string // roots.Key(path), the cache key
	label string // as the host knows it
}

// Scanner answers listings from the disk and the cache, and walks folders
// the cache is missing one at a time, in the background.
type Scanner struct {
	cfg   config.Config
	roots *roots.Resolver
	cache *cache
	logf  func(string, ...any)

	mu        sync.Mutex
	queue     []job // most urgent first
	current   *job
	updatedAt *float64
	err       *string

	// progressDone changes once per directory, far more often than anything
	// else here, so it and its total are plain atomics rather than going
	// through mu, which every live frame takes.
	progressDone  atomic.Int64
	progressTotal atomic.Int64

	// wake tells Loop there is work. Buffered by one so a signal is never
	// lost and no sender ever blocks.
	wake chan struct{}
}

// New loads the cache at cfg.StorageCache. A cache that cannot be written is
// not fatal: sizes are then kept in memory, and walked again after a restart.
func New(cfg config.Config, r *roots.Resolver, logf func(string, ...any)) *Scanner {
	s := &Scanner{cfg: cfg, roots: r, logf: logf, wake: make(chan struct{}, 1)}
	c, err := openCache(cfg.StorageCache)
	if err != nil {
		logf("storage cache %s: %v — folder sizes will not survive a restart", cfg.StorageCache, err)
		c, _ = openCache("") // in memory, which cannot fail
	}
	s.cache = c
	return s
}

// Close saves what listings added to the cache since the last walk.
func (s *Scanner) Close() error { return s.cache.close() }

// Status returns the queue's state plus a live progress estimate for the
// current walk. It is cheap enough to call on every live tick.
func (s *Scanner) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Status{Queued: len(s.queue), UpdatedAt: s.updatedAt, Error: s.err}
	if s.current != nil {
		out.Scanning, out.Current = true, s.current.label
		if total := s.progressTotal.Load(); total > 0 {
			done := s.progressDone.Load()
			pct := round1(min(100, float64(done)/float64(total)*100))
			out.Progress = &Progress{Percent: pct, BytesDone: done, BytesTotal: total}
		}
	}
	out.Scanning = out.Scanning || out.Queued > 0
	return out
}

// rootDir is a configured root that is there right now.
type rootDir struct {
	label string
	path  string // as configured
	real  string // with links resolved, since the walk never follows them
}

func (s *Scanner) rootDirs() []rootDir {
	var out []rootDir
	for _, r := range s.roots.Roots() {
		real := r.Path
		if p, err := filepath.EvalSymlinks(r.Path); err == nil {
			real = p
		}
		if _, err := rootDevice(real); err != nil {
			continue // not mounted, or not a directory: leave it out
		}
		out = append(out, rootDir{label: r.Label, path: r.Path, real: real})
	}
	return out
}

func (s *Scanner) Snapshot() Snapshot {
	out := Snapshot{Status: s.Status(), Roots: []Root{}, Sep: roots.Sep}
	for _, r := range s.rootDirs() {
		root := Root{Name: r.label, Path: r.path}
		if c, ok := s.cache.get(roots.Key(r.real)); ok {
			root.Size, root.Unreadable = &c.size, c.unreadable
		}
		out.Roots = append(out.Roots, root)
	}
	return out
}

/* ── listing ────────────────────────────────────────────────────────────── */

// opened is a directory that has just been read.
type opened struct {
	root     rootDir
	dir      string
	resolved []string
	partial  bool
	locked   bool // the root itself could not be read
	dev      uint64
	entries  []dirEntry // valid until rd reads again
	subdirs  []string   // the entries a walk would enter
	excluded map[string]bool
}

// open resolves rel ("home/yuuki", or "" for the root itself) under root
// number rootIdx by reading each directory on the way down, so a path can
// only name what a walk would reach: no "..", no links, nothing excluded and
// no other filesystem. A path that has since disappeared resolves to its
// deepest surviving ancestor instead.
func (s *Scanner) open(rd *dirReader, rootIdx int, rel string) (*opened, bool) {
	list := s.rootDirs()
	if rootIdx < 0 || rootIdx >= len(list) {
		return nil, false
	}
	o := &opened{root: list[rootIdx], dir: list[rootIdx].real, excluded: s.excludes()}
	dev, err := rootDevice(o.dir)
	if err != nil {
		return nil, false
	}
	o.dev = dev
	if o.entries, _, err = rd.read(cstring(o.dir), dedupeIn(o.dir)); err != nil {
		o.locked = true
		return o, true
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		name, ok := o.find(seg)
		if !ok {
			o.partial = true
			break
		}
		next := joinPath(o.dir, name)
		entries, _, err := rd.read(cstring(next), dedupeIn(next))
		if err != nil {
			o.partial = true
			break
		}
		o.dir, o.entries = next, entries
		o.resolved = append(o.resolved, name)
	}
	for _, e := range o.entries {
		if o.walkable(e) {
			o.subdirs = append(o.subdirs, string(e.name))
		}
	}
	return o, true
}

// walkable reports whether a walk of o.dir would enter e.
func (o *opened) walkable(e dirEntry) bool {
	if !e.dir || e.dev != o.dev {
		return false
	}
	return len(o.excluded) == 0 || !o.excluded[roots.Key(joinPath(o.dir, string(e.name)))]
}

func (o *opened) find(seg string) (string, bool) {
	for _, e := range o.entries {
		if sameName(string(e.name), seg) && o.walkable(e) {
			return string(e.name), true
		}
	}
	return "", false
}

// List reads one directory and sizes its subfolders from the cache, queueing
// a walk for each one the cache is missing or has let go stale. It reports
// false only when there is no such root.
func (s *Scanner) List(rootIdx int, rel string) (Listing, bool) {
	rd := newDirReader()
	o, ok := s.open(rd, rootIdx, rel)
	if !ok {
		return Listing{}, false
	}
	out := Listing{Path: strings.Join(o.resolved, "/"), Partial: o.partial, Dirs: []Entry{}, Files: []Entry{}}
	if o.locked {
		out.Unreadable = 1
		return out, true
	}

	var top fileHeap
	var links map[uint64]struct{}
	for _, e := range o.entries {
		if e.dir {
			continue
		}
		if e.key != 0 {
			if links == nil {
				links = map[uint64]struct{}{}
			}
			if _, dup := links[e.key]; dup {
				continue
			}
			links[e.key] = struct{}{}
		}
		out.FileCount++
		out.FileBytes += e.size
		top.offer(e.size, e.name)
	}
	sort.Slice(top, func(a, b int) bool { return top[a].size > top[b].size })
	for _, f := range top {
		out.Files = append(out.Files, Entry{Name: f.name, Size: f.size})
	}

	dirKey := roots.Key(o.dir)
	cached := s.cache.children(dirKey)
	present := make(map[string]bool, len(o.subdirs))
	var missing, stale []job
	var oldest time.Time
	now := time.Now()
	for _, name := range o.subdirs {
		nk := nameKey(name)
		present[nk] = true
		c, ok := cached[nk]
		if !ok {
			out.Dirs = append(out.Dirs, Entry{Name: name, Pending: true})
			out.Pending++
			missing = append(missing, s.job(o.dir, name))
			continue
		}
		out.Dirs = append(out.Dirs, Entry{Name: name, Size: c.size})
		out.Size += c.size
		out.Unreadable += c.unreadable
		if oldest.IsZero() || c.at.Before(oldest) {
			oldest = c.at
		}
		if s.cfg.StorageInterval > 0 && now.Sub(c.at) > s.cfg.StorageInterval {
			stale = append(stale, s.job(o.dir, name))
		}
	}
	out.Size += out.FileBytes
	sort.SliceStable(out.Dirs, func(a, b int) bool { return out.Dirs[a].Size > out.Dirs[b].Size })
	if !oldest.IsZero() {
		at := float64(oldest.Unix())
		out.SizedAt = &at
	}

	s.cache.prune(dirKey, cached, present, s.rootKeys())
	// Fully sized, this folder's total is now as fresh as its oldest part, and
	// its parent's listing picks that up from the cache.
	if out.Pending == 0 {
		if oldest.IsZero() {
			oldest = now
		}
		s.cache.put(dirKey, sized{size: out.Size, unreadable: out.Unreadable, at: oldest})
	}
	s.enqueue(missing, true)
	s.enqueue(stale, false)
	return out, true
}

// Rescan re-walks every subfolder of one directory that was not sized in the
// last StorageMinRescan, ahead of anything already queued.
func (s *Scanner) Rescan(rootIdx int, rel string) (Status, bool) {
	o, ok := s.open(newDirReader(), rootIdx, rel)
	if !ok {
		return Status{}, false
	}
	cached := s.cache.children(roots.Key(o.dir))
	var jobs []job
	for _, name := range o.subdirs {
		if c, ok := cached[nameKey(name)]; ok && time.Since(c.at) < s.cfg.StorageMinRescan {
			continue
		}
		jobs = append(jobs, s.job(o.dir, name))
	}
	s.enqueue(jobs, true)
	return s.Status(), true
}

func (s *Scanner) job(dir, name string) job {
	p := joinPath(dir, name)
	return job{path: p, key: roots.Key(p), label: s.roots.HostPath(p)}
}

/* ── the walk queue ─────────────────────────────────────────────────────── */

// enqueue adds folders to walk. A folder inside one that is already queued or
// being walked is left to that walk, and queued folders inside a new one are
// dropped in its favour. Urgent jobs, the ones a listing is waiting on, go to
// the front, and so does a queued folder that already covers one.
func (s *Scanner) enqueue(jobs []job, urgent bool) {
	if len(jobs) == 0 {
		return
	}
	s.mu.Lock()
	var head []job
	for _, j := range jobs {
		if s.current != nil && roots.Within(s.current.key, j.key) {
			continue
		}
		covered := -1
		for i, q := range s.queue {
			if roots.Within(q.key, j.key) {
				covered = i
				break
			}
		}
		if covered >= 0 {
			if urgent {
				head = append(head, s.queue[covered])
				s.queue = append(s.queue[:covered], s.queue[covered+1:]...)
			}
			continue
		}
		kept := s.queue[:0]
		for _, q := range s.queue {
			if !roots.Within(j.key, q.key) {
				kept = append(kept, q)
			}
		}
		s.queue = kept
		if urgent {
			head = append(head, j)
		} else {
			s.queue = append(s.queue, j)
		}
	}
	s.queue = append(head, s.queue...)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default: // already pending
	}
}

// next takes the most urgent job off the queue, or reports that there is
// none left.
func (s *Scanner) next() (job, bool) {
	s.mu.Lock()
	if len(s.queue) == 0 {
		s.current = nil
		s.mu.Unlock()
		return job{}, false
	}
	j := s.queue[0]
	s.queue = s.queue[1:]
	s.mu.Unlock()

	var total int64
	if c, ok := s.cache.get(j.key); ok {
		total = c.size
	}
	s.progressTotal.Store(total)
	s.progressDone.Store(0)
	s.mu.Lock()
	s.current = &j
	s.mu.Unlock()
	return j, true
}

// Loop walks queued folders as they arrive. Listings are what fill the
// queue, so with nobody looking it sleeps.
func (s *Scanner) Loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		s.drain(ctx)
	}
}

// drain walks until the queue is empty.
//
// Linux applies setpriority(PRIO_PROCESS) and ioprio_set to the calling
// thread, and Windows' background mode is per thread too, so the goroutine is
// pinned first. It never unlocks: the runtime then retires the deprioritised
// thread when the queue runs dry, instead of handing it back to the scheduler
// for the poller to land on.
func (s *Scanner) drain(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		deprioritise()
		w := &walker{
			maxDepth: s.cfg.TreeDepth,
			excluded: s.excludes(),
			progress: &s.progressDone,
			pace:     newThrottle(s.cfg.StorageCPU),
			rd:       newDirReader(),
		}
		for ctx.Err() == nil {
			j, ok := s.next()
			if !ok {
				return
			}
			s.walkOne(ctx, w, j)
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	// A walk's scratch space (the inode set, the BFS frontier) dwarfs what is
	// kept. Hand it back to the kernel now instead of letting it sit in the
	// heap until the next one.
	debug.FreeOSMemory()
}

func (s *Scanner) walkOne(ctx context.Context, w *walker, j job) {
	started := time.Now()
	t, err := w.walk(ctx, j.path)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		// Gone, or not a directory any more. Recording it as an empty,
		// unreadable folder keeps a listing that still shows it from asking
		// for it again and again.
		t = &tree{nodes: []*dirNode{{name: baseName(j.path), unreadable: 1}}}
	}
	err = s.cache.store(j.key, t, started, s.rootKeys())

	at := float64(time.Now().UnixNano()) / 1e9
	s.mu.Lock()
	s.updatedAt = &at
	if err != nil {
		msg := "storage cache: " + err.Error()
		s.err = &msg
	} else {
		s.err = nil
	}
	s.mu.Unlock()
}

/* ── helpers ────────────────────────────────────────────────────────────── */

// rootKeys is every root's cache key, which a walk or prune elsewhere must
// not delete.
func (s *Scanner) rootKeys() []string {
	var out []string
	for _, r := range s.rootDirs() {
		out = append(out, roots.Key(r.real))
	}
	return out
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

// dedupeIn reports whether files in dir should be counted once per file ID,
// by the same rule as a walk: Windows only does it inside its system
// directory, and Linux always does, by link count.
func dedupeIn(dir string) bool {
	sys := systemRoot()
	return sys != "" && roots.Within(sys, dir)
}

// joinPath adds one name to a directory path read from disk.
func joinPath(dir, name string) string {
	if dir[len(dir)-1] == pathSep {
		return dir + name
	}
	return dir + string(pathSep) + name
}

// cstring is path with a NUL after it in the backing array, as the directory
// readers require.
func cstring(path string) []byte {
	b := append([]byte(path), 0)
	return b[:len(path)]
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

// Package storage builds a Filelight-style directory size map.
//
// The walk is one breadth-first getdents+fstatat pass per root, run on a slow
// timer and cached — never recomputed per refresh. Sizes come from st_blocks
// so they match `du` (actual blocks on disk) rather than apparent size, and
// hardlinked files are counted once.
//
// The page never downloads the tree. It gets a one-line summary per root, then
// asks for one directory's listing at a time as it is drilled into, which the
// server answers from the cached walk without touching the disk.
package storage

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/yuuki824/kanshi/internal/config"
)

// Largest individual files remembered per directory. Everything past this is
// still counted, just not named.
const topFiles = 32

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

// Snapshot is what /api/storage returns.
type Snapshot struct {
	Status
	Roots []Root `json:"roots"`
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
	// Path is what was actually resolved, relative to the root. When the
	// requested path no longer exists it is the deepest ancestor that does,
	// and Partial is set.
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

// Scanner owns the cached walk. Reads are served under a read lock; only one
// walk runs at a time.
type Scanner struct {
	cfg config.Config

	mu          sync.RWMutex
	status      Status
	roots       []Root
	trees       []*tree // parallel to roots; immutable once published
	currentRoot string  // label of the root the in-flight walk is on

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

func New(cfg config.Config) *Scanner {
	return &Scanner{cfg: cfg, roots: []Root{}}
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
	return Snapshot{Status: status, Roots: s.roots}
}

// List resolves rel ("home/yuuki", or "" for the root itself) against root
// number rootIdx of the last completed walk. It reports false only when there
// is no such root; a path that has since disappeared resolves to its deepest
// surviving ancestor instead.
func (s *Scanner) List(rootIdx int, rel string) (Listing, bool) {
	s.mu.RLock()
	trees := s.trees
	s.mu.RUnlock()
	if rootIdx < 0 || rootIdx >= len(trees) {
		return Listing{}, false
	}
	t := trees[rootIdx]

	var out Listing
	n := t.nodes[0]
	var resolved []string
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		var hit *dirNode
		for _, c := range n.children {
			if t.nodes[c].name == seg {
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
// baseline a new walk's progress is measured against. Zero on the very first
// scan, when there is nothing yet to compare to.
func (s *Scanner) previousTotal() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total int64
	for _, r := range s.roots {
		total += r.Size
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

	roots, trees, err := s.walkAll(ctx)

	s.mu.Lock()
	s.status.Scanning = false
	if err != nil {
		msg := err.Error()
		s.status.Error = &msg
	} else {
		elapsed := round2(time.Since(started).Seconds())
		at := float64(started.UnixNano()) / 1e9
		s.roots, s.trees, s.status.Error = roots, trees, nil
		s.status.ScannedAt, s.status.Duration = &at, &elapsed
	}
	s.mu.Unlock()

	s.lastScan = time.Now()
	// The walk's scratch space (the inode set, the BFS frontier) dwarfs what
	// is kept, and the next walk is half an hour away. Hand it back to the
	// kernel now instead of letting it sit in the heap until then.
	debug.FreeOSMemory()
	return s.Snapshot()
}

// Scan rebuilds the tree, blocking until the walk finishes. Rate-limited
// unless force is set by the timer.
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

// Loop is the background refresher — slow by default (30 min).
func (s *Scanner) Loop(ctx context.Context) {
	interval := s.cfg.StorageInterval
	if interval < time.Minute {
		interval = time.Minute
	}
	for {
		s.Scan(ctx, true)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (s *Scanner) walkAll(ctx context.Context) ([]Root, []*tree, error) {
	// Linux applies setpriority(PRIO_PROCESS) and ioprio_set to the calling
	// thread, so the goroutine is pinned first. It never unlocks: the runtime
	// then retires the deprioritised thread when the walk returns, instead of
	// handing it back to the scheduler for the poller to land on.
	done := make(chan struct{})
	var roots []Root
	var trees []*tree
	var err error
	go func() {
		defer close(done)
		runtime.LockOSThread()
		deprioritise()
		roots, trees, err = s.walkRoots(ctx)
	}()

	select {
	case <-done:
		return roots, trees, err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (s *Scanner) walkRoots(ctx context.Context) ([]Root, []*tree, error) {
	roots := []Root{}
	var trees []*tree
	pace := newThrottle(s.cfg.StorageCPU)
	for _, root := range s.cfg.Roots() {
		if info, err := os.Stat(root.Path); err != nil || !info.IsDir() {
			continue
		}
		s.setCurrentRoot(root.Label)
		started := time.Now()
		t, err := s.walk(ctx, root.Path, pace)
		if err != nil {
			return nil, nil, fmt.Errorf("%T: %w", err, err)
		}
		roots = append(roots, Root{
			Name:        root.Label,
			Path:        root.Path,
			Size:        t.nodes[0].size,
			Unreadable:  t.unreadable,
			WalkSeconds: round2(time.Since(started).Seconds()),
		})
		trees = append(trees, t)
	}
	return roots, trees, nil
}

/* ── the walk ───────────────────────────────────────────────────────────── */

// dirNode is one directory within TreeDepth of the root. Deeper directories
// have their bytes rolled into the nearest retained ancestor rather than
// getting a node of their own.
type dirNode struct {
	name     string
	self     int64 // bytes of files directly inside this dir
	deep     int64 // bytes below the retained depth
	size     int64 // self + deep + children
	files    int32 // number of files directly inside this dir
	children []int32
	top      fileHeap // largest files, capped at topFiles
}

// tree is one root's walk. nodes is in breadth-first order, so every parent
// comes before all of its children and nodes[0] is the root.
type tree struct {
	nodes      []*dirNode
	unreadable int
}

// frontier is one BFS level: every directory path back to back in a single
// buffer, each NUL-terminated so it can go straight to openat. One growing
// buffer per level rather than a string per directory — on a tree with a wide
// node_modules or overlay2 level, the string headers and allocator rounding
// alone would cost more than the paths.
type frontier struct {
	paths []byte
	dirs  []queued
}

// queued is a directory waiting its turn. node is the retained node its files
// are counted into: its own when own is set, otherwise its nearest retained
// ancestor.
type queued struct {
	start, end uint32 // its path is paths[start:end], with a NUL at end
	node       int32
	own        bool
}

func (f *frontier) path(q queued) []byte { return f.paths[q.start:q.end] }

func (f *frontier) push(parent, name []byte, node int32, own bool) queued {
	start := len(f.paths)
	f.paths = append(f.paths, parent...)
	if len(parent) != 1 || parent[0] != '/' {
		f.paths = append(f.paths, '/')
	}
	f.paths = append(f.paths, name...)
	q := queued{start: uint32(start), end: uint32(len(f.paths)), node: node, own: own}
	f.paths = append(f.paths, 0)
	f.dirs = append(f.dirs, q)
	return q
}

// pop takes back the last push.
func (f *frontier) pop(q queued) {
	f.paths = f.paths[:q.start]
	f.dirs = f.dirs[:len(f.dirs)-1]
}

func (f *frontier) reset() {
	f.paths = f.paths[:0]
	f.dirs = f.dirs[:0]
}

// Linux dirent64: d_ino (8), d_off (8), d_reclen (2), d_type (1), d_name.
const (
	direntReclen = 16
	direntType   = 18
	direntName   = 19
)

const (
	atSymlinkNofollow = 0x100
	atNoAutomount     = 0x800
)

// A variable, not a constant: a negative constant cannot convert to uintptr.
var atFDCWD = -100

// walk is a level-by-level BFS. Only directories within TreeDepth of the root
// get a node of their own; anything deeper is still fully traversed and
// counted, but its bytes roll up into the nearest retained ancestor. (On this
// host that is ~2.3k retained nodes instead of ~96k.)
//
// Going breadth-first means the frontier is never more than two levels of
// paths, and the retained levels are all finished before the deep, bulky part
// of the tree starts. Directories are opened by full path, so no descriptor is
// held open while its children wait in the queue.
//
// The inner loop does no allocation per entry: getdents64 records are parsed
// in place from one reused buffer, and each is stat'ed relative to its
// directory's descriptor with the NUL-terminated name straight out of that
// buffer. A string is only built for a directory kept in the tree, or for a
// file big enough to make the top-N list.
func (s *Scanner) walk(ctx context.Context, rootPath string, pace *throttle) (*tree, error) {
	var rootStat syscall.Stat_t
	if err := syscall.Lstat(rootPath, &rootStat); err != nil {
		return nil, err
	}
	rootDev := rootStat.Dev
	maxDepth := s.cfg.TreeDepth

	excluded := make(map[string]bool, len(s.cfg.StorageExclude))
	for _, p := range s.cfg.StorageExclude {
		excluded[p] = true
	}

	t := &tree{nodes: make([]*dirNode, 1, 4096)}
	t.nodes[0] = &dirNode{name: baseName(rootPath)}
	// Packed into one int rather than a (dev, ino) pair: overlay2 hardlinks
	// everything, so this set reaches six figures and a struct key costs
	// several times the bytes of a bare uint64.
	seenInodes := make(map[uint64]struct{})
	buf := make([]byte, 32*1024)
	var st syscall.Stat_t

	level, next := &frontier{}, &frontier{}
	level.paths = append([]byte(rootPath), 0)
	level.dirs = []queued{{end: uint32(len(rootPath)), node: 0, own: true}}

	for depth := 0; len(level.dirs) > 0; depth++ {
		childDepth := depth + 1
		for _, q := range level.dirs {
			dirPath := level.path(q)
			fd, err := openDir(dirPath)
			if err != nil {
				// A directory we cannot read would otherwise silently vanish from
				// the totals — count it so the UI can say the tree is incomplete
				// rather than quietly under-reporting.
				t.unreadable++
				continue
			}
			node := t.nodes[q.node]
			var bytesDone int64
			entries := 0

			for {
				n, err := syscall.Getdents(fd, buf)
				if err != nil || n <= 0 {
					break // end of directory, or a read error mid-directory
				}
				for off := 0; off < n; {
					reclen := int(*(*uint16)(unsafe.Pointer(&buf[off+direntReclen])))
					typ := buf[off+direntType]
					name := buf[off+direntName : off+reclen]
					off += reclen
					if end := indexNUL(name); end >= 0 {
						name = name[:end]
					}
					entries++
					// d_type comes back with the directory entry, so symlinks,
					// sockets and devices are rejected without a stat at all.
					if typ != syscall.DT_DIR && typ != syscall.DT_REG && typ != syscall.DT_UNKNOWN {
						continue
					}
					if len(name) == 0 || (name[0] == '.' && (len(name) == 1 || (len(name) == 2 && name[1] == '.'))) {
						continue
					}
					if statAt(fd, dirPath, name, &st) != nil {
						continue
					}
					switch st.Mode & syscall.S_IFMT {
					case syscall.S_IFDIR:
						// Don't cross into other filesystems — each root is walked
						// separately, so we'd otherwise double-count.
						if st.Dev != rootDev {
							continue
						}
						own := childDepth <= maxDepth
						childNode := q.node
						if own {
							childNode = int32(len(t.nodes))
						}
						child := next.push(dirPath, name, childNode, own)
						if len(excluded) > 0 && excluded[string(next.path(child))] {
							next.pop(child)
							continue
						}
						if own {
							t.nodes = append(t.nodes, &dirNode{name: string(name)})
							node.children = append(node.children, childNode)
						}
					case syscall.S_IFREG:
						if st.Nlink > 1 {
							key := uint64(st.Dev)<<48 | uint64(st.Ino)
							if _, dup := seenInodes[key]; dup {
								continue
							}
							seenInodes[key] = struct{}{}
						}
						size := int64(st.Blocks) * 512
						bytesDone += size
						if q.own {
							node.self += size
							node.files++
							node.top.offer(size, name)
						} else {
							node.deep += size
						}
					}
				}
			}
			syscall.Close(fd)
			// Every byte is counted here exactly once regardless of depth, the
			// same set the previous scan's totals cover — so this sum lines up
			// with progressTotal.
			s.progressDone.Add(bytesDone)

			if err := pace.wait(ctx, entries); err != nil {
				return nil, err
			}
		}
		level, next = next, level
		next.reset()
	}

	// Breadth-first order puts every child after its parent, so walking it
	// backwards totals each child before the parent that sums it.
	for i := len(t.nodes) - 1; i >= 0; i-- {
		node := t.nodes[i]
		node.size += node.self + node.deep
		for _, c := range node.children {
			node.size += t.nodes[c].size
		}
	}
	// Listings are served largest-first, so sort once here rather than on
	// every request.
	for _, node := range t.nodes {
		sort.Slice(node.children, func(a, b int) bool {
			return t.nodes[node.children[a]].size > t.nodes[node.children[b]].size
		})
		sort.Slice(node.top, func(a, b int) bool { return node.top[a].size > node.top[b].size })
	}
	return t, nil
}

/* ── being a good neighbour ─────────────────────────────────────────────── */

const (
	ioprioWhoProcess = 1
	ioprioClassBE    = 2
	ioprioClassShift = 13
)

// deprioritise puts the calling thread at the back of the queue for both the
// CPU (nice 19) and the disk (lowest best-effort I/O priority), so a rescan
// never makes the at-a-glance numbers stutter. The idle I/O class would be
// gentler still, but on a busy box it can starve the walk outright.
func deprioritise() {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19)
	syscall.RawSyscall(syscall.SYS_IOPRIO_SET, ioprioWhoProcess, 0, ioprioClassBE<<ioprioClassShift|7)
}

// throttle holds the walk to a fraction of one core. nice only matters when
// something else wants the CPU; on an otherwise quiet box a warm-cache walk
// would happily pin a core for a minute, which shows up on the very dashboard
// it is feeding. So after every stretch of work the walk sleeps long enough
// that the CPU it actually used — measured per thread, so time blocked on the
// disk costs nothing — averages out to the configured share.
type throttle struct {
	share float64 // of one core; >= 1 means unthrottled
	ops   int
	mark  time.Time
	cpu   time.Duration
	timer *time.Timer
}

// Enough entries between checks that the getrusage call is noise, few enough
// that each sleep is a few milliseconds rather than a visible stall.
const throttleEvery = 2048

func newThrottle(percent float64) *throttle {
	t := &throttle{share: percent / 100}
	if t.share <= 0 {
		t.share = 1
	}
	t.timer = time.NewTimer(time.Hour)
	t.timer.Stop()
	t.mark, t.cpu = time.Now(), threadCPU()
	return t
}

func (t *throttle) wait(ctx context.Context, ops int) error {
	if t.ops += ops; t.ops < throttleEvery {
		return nil
	}
	t.ops = 0
	if t.share < 1 {
		cpu := threadCPU()
		idle := time.Duration(float64(cpu-t.cpu)/t.share) - time.Since(t.mark)
		if idle > time.Millisecond {
			t.timer.Reset(min(idle, 2*time.Second))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.timer.C:
			}
		}
		t.mark, t.cpu = time.Now(), cpu
	}
	// Checked here rather than per entry: off the hot path, but still often
	// enough to abort a huge walk promptly.
	return ctx.Err()
}

// threadCPU is the user+system time of the calling thread, which the walk has
// to itself because it is locked to it.
func threadCPU() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_THREAD, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

/* ── bounded top-N of files ─────────────────────────────────────────────── */

type fileEntry struct {
	size int64
	name string
}

// fileHeap is a min-heap of the topFiles largest files seen, so a new file
// only has to beat the root to get in. It grows as needed rather than
// reserving topFiles slots, since most directories hold only a few files.
type fileHeap []fileEntry

// offer takes the name as bytes so the string is only allocated for a file
// that actually makes the cut.
func (h *fileHeap) offer(size int64, name []byte) {
	if size <= 0 {
		return
	}
	a := *h
	if len(a) < topFiles {
		a = append(a, fileEntry{size: size, name: string(name)})
		for i := len(a) - 1; i > 0; {
			p := (i - 1) / 2
			if a[p].size <= a[i].size {
				break
			}
			a[p], a[i] = a[i], a[p]
			i = p
		}
		*h = a
		return
	}
	if size <= a[0].size {
		return
	}
	a[0] = fileEntry{size: size, name: string(name)}
	for i := 0; ; {
		l, small := 2*i+1, i
		if l < len(a) && a[l].size < a[small].size {
			small = l
		}
		if r := l + 1; r < len(a) && a[r].size < a[small].size {
			small = r
		}
		if small == i {
			break
		}
		a[i], a[small] = a[small], a[i]
		i = small
	}
}

/* ── path helpers ───────────────────────────────────────────────────────── */

// join and baseName avoid path/filepath's Clean pass: every path here is built
// from an already-clean parent plus one getdents name, so there is nothing to
// normalise.
func join(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

func baseName(path string) string {
	for len(path) > 1 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == len(path)-1 {
				break
			}
			return path[i+1:]
		}
	}
	return path
}

// openDir opens a directory by a path that is followed by a NUL in its
// backing array, so the kernel reads it in place rather than from a copy.
func openDir(path []byte) (int, error) {
	fd, _, errno := syscall.Syscall6(syscall.SYS_OPENAT, uintptr(atFDCWD), uintptr(unsafe.Pointer(&path[0])),
		syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0, 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

func indexNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

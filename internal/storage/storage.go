// Package storage builds a Filelight-style directory size tree.
//
// The walk is a single getdents+lstat pass per root, run on a slow timer and
// cached — never recomputed per refresh. Sizes come from st_blocks so they
// match `du` (actual blocks on disk) rather than apparent size, and hardlinked
// files are counted once.
package storage

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yuuki824/kanshi/internal/config"
)

// Never ship a tile smaller than this, regardless of the fraction threshold.
const minAbsolute = 4 * 1024 * 1024

// Largest individual files kept per directory; the rest are aggregated.
const topFiles = 12

/* ── wire payload ───────────────────────────────────────────────────────── */

// Node is one tile in the treemap. Root, Unreadable and WalkSeconds are
// pointers so they appear only on the per-root nodes that actually have them,
// rather than on every one of the few thousand tiles below.
type Node struct {
	Name     string  `json:"name"`
	Path     string  `json:"path,omitempty"`
	Size     int64   `json:"size"`
	Kind     string  `json:"kind"`
	Children []*Node `json:"children,omitempty"`

	Root        *string  `json:"root,omitempty"`
	Unreadable  *int     `json:"unreadable,omitempty"`
	WalkSeconds *float64 `json:"walk_seconds,omitempty"`
}

// Snapshot is what /api/storage returns and what the treemap renders from.
type Snapshot struct {
	Roots     []*Node   `json:"roots"`
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

/* ── scanner ────────────────────────────────────────────────────────────── */

// Scanner owns the cached tree. Reads are served from the snapshot under a
// read lock; only one walk runs at a time.
type Scanner struct {
	cfg config.Config

	mu          sync.RWMutex
	state       Snapshot
	currentRoot string // label of the root the in-flight walk is on

	// progressDone and progressTotal are updated far more often than state (once
	// per file, not once per scan), so they are plain atomics rather than
	// going through mu — a walk of a few hundred thousand files would
	// otherwise contend the same lock the SSE poller and every REST handler
	// read from.
	progressDone  atomic.Int64
	progressTotal atomic.Int64

	// scanMu serialises walks. A second request while one is in flight gets
	// the current snapshot rather than queueing a duplicate walk.
	scanMu   sync.Mutex
	lastScan time.Time
}

func New(cfg config.Config) *Scanner {
	return &Scanner{cfg: cfg, state: Snapshot{Roots: []*Node{}}}
}

// Snapshot returns the cached tree, plus a live progress estimate while a
// walk is running. The returned nodes are never mutated after publication, so
// callers can marshal them without holding the lock.
func (s *Scanner) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.state
	if out.Scanning {
		if total := s.progressTotal.Load(); total > 0 {
			done := s.progressDone.Load()
			pct := round1(min(100, float64(done)/float64(total)*100))
			out.Progress = &Progress{Root: s.currentRoot, Percent: pct, BytesDone: done, BytesTotal: total}
		}
	}
	return out
}

// previousTotal sums the byte sizes from the last completed scan, the
// baseline a new walk's progress is measured against. Zero on the very first
// scan, when there is nothing yet to compare to.
func (s *Scanner) previousTotal() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total int64
	for _, r := range s.state.Roots {
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
	s.setScanning(true)
	return true
}

func (s *Scanner) runScan(ctx context.Context) Snapshot {
	defer s.scanMu.Unlock()
	started := time.Now()

	roots, err := s.walkAll(ctx)

	s.mu.Lock()
	s.state.Scanning = false
	if err != nil {
		msg := err.Error()
		s.state.Error = &msg
	} else {
		elapsed := round2(time.Since(started).Seconds())
		at := float64(started.UnixNano()) / 1e9
		s.state.Roots, s.state.Error = roots, nil
		s.state.ScannedAt, s.state.Duration = &at, &elapsed
	}
	out := s.state
	s.mu.Unlock()

	s.lastScan = time.Now()
	return out
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
// poll instead of blocking the request for the length of the walk (on this
// host, well over a minute).
func (s *Scanner) ScanAsync(ctx context.Context, force bool) Snapshot {
	if !s.beginScan(force) {
		return s.Snapshot()
	}
	go s.runScan(ctx)
	return s.Snapshot()
}

func (s *Scanner) setScanning(v bool) {
	s.mu.Lock()
	s.state.Scanning = v
	s.mu.Unlock()
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

func (s *Scanner) walkAll(ctx context.Context) ([]*Node, error) {
	// The walk is a long burst of syscalls competing with the live poller for
	// this container's CPU quota. Deprioritise it so a rescan never makes the
	// at-a-glance numbers stutter — the walk finishing a few seconds later is
	// invisible, a frozen dashboard is not.
	//
	// Linux applies setpriority(PRIO_PROCESS) to the calling thread, so the
	// goroutine is pinned first. It never unlocks: the runtime then retires
	// the niced thread when the walk returns, instead of handing it back to
	// the scheduler for the poller to land on.
	done := make(chan struct{})
	var roots []*Node
	var err error
	go func() {
		defer close(done)
		runtime.LockOSThread()
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
		roots, err = s.walkRoots(ctx)
	}()

	select {
	case <-done:
		return roots, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Scanner) walkRoots(ctx context.Context) ([]*Node, error) {
	var trees []*Node
	for _, root := range s.cfg.Roots() {
		if info, err := os.Stat(root.Path); err != nil || !info.IsDir() {
			continue
		}
		s.setCurrentRoot(root.Label)
		started := time.Now()
		w, err := s.walk(ctx, root.Path)
		if err != nil {
			return nil, fmt.Errorf("%T: %w", err, err)
		}
		tree := s.toTree(w, root.Path, s.cfg.TreeDepth)
		elapsed := round2(time.Since(started).Seconds())
		path, unreadable := root.Path, w.unreadable
		tree.Name = root.Label
		tree.Root, tree.Unreadable, tree.WalkSeconds = &path, &unreadable, &elapsed
		trees = append(trees, tree)
	}
	if trees == nil {
		trees = []*Node{}
	}
	return trees, nil
}

/* ── the walk ───────────────────────────────────────────────────────────── */

// dirNode is the intermediate form: one per directory retained at or above the
// tree depth. Deeper directories have their bytes rolled into the nearest
// retained ancestor rather than getting a node of their own.
type dirNode struct {
	name     string
	self     int64 // bytes of files directly inside this dir
	deep     int64 // bytes below the retained depth
	size     int64 // self + deep + children
	children []string
	files    fileHeap // largest files, capped at topFiles
}

type fileEntry struct {
	size int64
	name string
}

type walkResult struct {
	nodes      map[string]*dirNode
	unreadable int
}

type frame struct {
	path   string
	depth  int
	anchor string // nearest retained ancestor
}

// walk is an iterative DFS. Only directories within TreeDepth of the root get
// a node of their own; anything deeper is still fully traversed and counted,
// but its bytes roll up into the nearest retained ancestor. The tree is pruned
// to this depth before it is served anyway, so materialising a node per
// directory just burns memory. (On this host that is ~2.3k retained nodes
// instead of ~96k.)
func (s *Scanner) walk(ctx context.Context, rootPath string) (*walkResult, error) {
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

	res := &walkResult{nodes: make(map[string]*dirNode, 4096)}
	// Packed into one int rather than a (dev, ino) pair: overlay2 hardlinks
	// everything, so this set reaches six figures and a struct key costs
	// several times the bytes of a bare uint64.
	seenInodes := make(map[uint64]struct{})
	order := make([]string, 0, 4096)
	stack := []frame{{path: rootPath, depth: 0}}

	var st syscall.Stat_t
	ticks := 0

	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if excluded[f.path] {
			continue
		}
		// Checking the context per directory rather than per entry keeps this
		// off the hot path while still aborting a huge walk promptly.
		if ticks++; ticks%256 == 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
		}

		var node *dirNode
		anchor := f.anchor
		if f.depth <= maxDepth {
			node = &dirNode{name: baseName(f.path)}
			res.nodes[f.path] = node
			order = append(order, f.path)
			anchor = f.path
		} else {
			node = res.nodes[anchor]
		}

		dir, err := os.Open(f.path)
		if err != nil {
			// A directory we cannot read would otherwise silently vanish from
			// the totals — count it so the UI can say the tree is incomplete
			// rather than quietly under-reporting.
			res.unreadable++
			continue
		}

		for {
			// Read in batches so a directory with a million entries does not
			// materialise a million DirEntry values at once.
			entries, err := dir.ReadDir(512)
			for i := range entries {
				name := entries[i].Name()
				// d_type comes back with the directory entry, so symlinks are
				// rejected without a stat at all.
				if entries[i].Type()&os.ModeSymlink != 0 {
					continue
				}
				child := join(f.path, name)
				if syscall.Lstat(child, &st) != nil {
					continue
				}
				switch st.Mode & syscall.S_IFMT {
				case syscall.S_IFDIR:
					// Don't cross into other filesystems — each root is walked
					// separately, so we'd otherwise double-count.
					if st.Dev != rootDev {
						continue
					}
					stack = append(stack, frame{path: child, depth: f.depth + 1, anchor: anchor})
					if f.depth+1 <= maxDepth {
						node.children = append(node.children, child)
					}
				case syscall.S_IFREG:
					if st.Nlink > 1 {
						key := uint64(st.Dev)<<48 | uint64(st.Ino)
						if _, dup := seenInodes[key]; dup {
							continue
						}
						seenInodes[key] = struct{}{}
					}
					size := st.Blocks * 512
					if f.depth <= maxDepth {
						node.self += size
						node.files.offer(fileEntry{size: size, name: name})
					} else {
						node.deep += size
					}
					// Every byte is counted here exactly once regardless of
					// depth, the same set the previous scan's cached totals
					// cover — so this sum lines up with progressTotal.
					s.progressDone.Add(size)
				}
			}
			if err != nil || len(entries) == 0 {
				break // io.EOF, or a read error mid-directory
			}
		}
		dir.Close()
	}

	// Children always follow their parent in a DFS pre-order, so walking the
	// order backwards guarantees every child is totalled before its parent.
	for i := len(order) - 1; i >= 0; i-- {
		node := res.nodes[order[i]]
		total := node.self + node.deep
		for _, child := range node.children {
			if c, ok := res.nodes[child]; ok {
				total += c.size
			}
		}
		node.size = total
	}
	return res, nil
}

/* ── pruning ────────────────────────────────────────────────────────────── */

type candidate struct {
	path string // set for directories only
	name string
	size int64
	kind string
}

// toTree prunes the walk down to what is worth downloading to a phone: the
// biggest children at each level, with everything else folded into one
// aggregate tile so the areas still add up.
func (s *Scanner) toTree(w *walkResult, path string, depth int) *Node {
	node := w.nodes[path]
	if node == nil {
		return &Node{Name: baseName(path), Path: path, Kind: "dir"}
	}
	out := &Node{Name: node.name, Path: path, Size: node.size, Kind: "dir"}
	if depth <= 0 || node.size <= 0 {
		return out
	}

	entries := make([]candidate, 0, len(node.children)+topFiles+1)
	for _, child := range node.children {
		if c, ok := w.nodes[child]; ok {
			entries = append(entries, candidate{path: child, size: c.size, kind: "dir"})
		}
	}
	var fileBytesKept int64
	for _, f := range node.files.sorted() {
		entries = append(entries, candidate{name: f.name, size: f.size, kind: "file"})
		fileBytesKept += f.size
	}
	if remainder := node.self + node.deep - fileBytesKept; remainder > 0 {
		entries = append(entries, candidate{name: "other files", size: remainder, kind: "rest"})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].size > entries[j].size })

	threshold := int64(float64(node.size) * s.cfg.TreeMinFraction)
	if threshold < minAbsolute {
		threshold = minAbsolute
	}

	var children []*Node
	var folded int64
	var foldedCount int
	for _, e := range entries {
		if e.size < threshold || len(children) >= s.cfg.TreeMaxChildren {
			folded += e.size
			foldedCount++
			continue
		}
		if e.kind == "dir" {
			children = append(children, s.toTree(w, e.path, depth-1))
		} else {
			children = append(children, &Node{Name: e.name, Size: e.size, Kind: e.kind})
		}
	}
	if folded > 0 {
		// Keep the aggregate so child sizes still sum to the parent — otherwise
		// the treemap silently under-reports and the areas lie.
		children = append(children, &Node{
			Name: fmt.Sprintf("%d smaller items", foldedCount),
			Size: folded,
			Kind: "rest",
		})
	}
	out.Children = children
	return out
}

/* ── bounded top-N of files ─────────────────────────────────────────────── */

// fileHeap keeps the topFiles largest entries seen. At this size a linear scan
// for the minimum beats the bookkeeping a real heap would need, and it keeps
// the whole thing in one flat array with no allocation after the first few.
type fileHeap struct {
	items [topFiles]fileEntry
	n     int
	minAt int
}

func (h *fileHeap) offer(e fileEntry) {
	if h.n < topFiles {
		h.items[h.n] = e
		h.n++
		if h.n == topFiles {
			h.recomputeMin()
		} else if h.n == 1 || e.size < h.items[h.minAt].size {
			h.minAt = h.n - 1
		}
		return
	}
	if e.size <= h.items[h.minAt].size {
		return
	}
	h.items[h.minAt] = e
	h.recomputeMin()
}

func (h *fileHeap) recomputeMin() {
	h.minAt = 0
	for i := 1; i < h.n; i++ {
		if h.items[i].size < h.items[h.minAt].size {
			h.minAt = i
		}
	}
}

func (h *fileHeap) sorted() []fileEntry {
	out := make([]fileEntry, h.n)
	copy(out, h.items[:h.n])
	sort.Slice(out, func(i, j int) bool { return out[i].size > out[j].size })
	return out
}

/* ── path helpers ───────────────────────────────────────────────────────── */

// join and baseName avoid path/filepath's Clean pass: every path here is built
// from an already-clean parent plus one getdents name, so there is nothing to
// normalise and the walk calls these once per directory entry.
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

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

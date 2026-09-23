package storage

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rene-roid/kanshi/internal/roots"
)

// Largest individual files remembered per directory. Everything past this is
// still counted, just not named.
const topFiles = 32

// dirNode is one directory within TreeDepth of its root. Deeper directories
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

// tree is one volume's walk. nodes is in breadth-first order, so every parent
// comes before all of its children and nodes[0] is the root.
type tree struct {
	nodes      []*dirNode
	unreadable int
}

// dirEntry is one directory entry as the platform's reader reports it. name
// points into the reader's buffer and is only valid until its next read.
type dirEntry struct {
	name []byte
	dir  bool
	dev  uint64 // directories: the filesystem, so the walk never crosses into another
	size int64  // files: bytes allocated on disk
	key  uint64 // files that may have other hard links: counted once per key
}

// nestedRoot is a configured root that lies inside the one being walked.
type nestedRoot struct {
	idx        int
	path       string
	node       int32 // its node once the walk reaches it, else -1
	parent     int   // the nested root enclosing it, or -1 for the outer root
	unreadable int
}

const markSystem = -1

// walker carries what stays the same across every root of one scan.
type walker struct {
	maxDepth int
	excluded map[string]bool
	progress *atomic.Int64
	pace     *throttle
	rd       *dirReader
}

// frontier is one BFS level: every directory path back to back in a single
// buffer, each NUL-terminated so it can go straight to the kernel. One growing
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
	rel        uint16 // depth below the nearest root
	owner      int16  // index into the nested roots, or -1 for the outer root
	own        bool
	dedupe     bool // Windows: inside the system directory, where hard links live
}

func (f *frontier) path(q queued) []byte { return f.paths[q.start:q.end] }

func (f *frontier) push(parent, name []byte, q queued) queued {
	q.start = uint32(len(f.paths))
	f.paths = append(f.paths, parent...)
	if parent[len(parent)-1] != pathSep {
		f.paths = append(f.paths, pathSep)
	}
	f.paths = append(f.paths, name...)
	q.end = uint32(len(f.paths))
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

// walk is a level-by-level BFS. Only directories within maxDepth of their
// root get a node of their own; anything deeper is still fully traversed and
// counted, but its bytes roll up into the nearest retained ancestor. (On the
// host this was written for, that is ~2.3k retained nodes instead of ~96k.)
//
// A nested root restarts the depth count, so it gets full-depth detail of its
// own while its bytes still count toward the outer root.
//
// Going breadth-first means the frontier is never more than two levels of
// paths, and the retained levels are all finished before the deep, bulky part
// of the tree starts. Directories are opened by full path, so no descriptor is
// held open while its children wait in the queue.
func (w *walker) walk(ctx context.Context, rootPath string, nested []*nestedRoot) (*tree, error) {
	rootDev, err := rootDevice(rootPath)
	if err != nil {
		return nil, err
	}

	// Directories that need special handling, keyed by the BFS level at which
	// they can turn up, so the path is only normalised on those levels.
	marks := map[int]map[string]int{}
	mark := func(path string, m int) {
		rel := strings.Trim(roots.Key(path)[len(roots.Key(rootPath)):], string(pathSep))
		level := strings.Count(rel, string(pathSep)) + 1
		if marks[level] == nil {
			marks[level] = map[string]int{}
		}
		marks[level][roots.Key(path)] = m
	}
	for i, n := range nested {
		mark(n.path, i)
		n.parent = -1
		for j, o := range nested {
			if j != i && roots.Within(o.path, n.path) && (n.parent < 0 || len(o.path) > len(nested[n.parent].path)) {
				n.parent = j
			}
		}
	}
	rootDedupe := false
	if sys := systemRoot(); sys != "" && roots.Within(rootPath, sys) {
		if roots.Key(sys) == roots.Key(rootPath) {
			rootDedupe = true
		} else {
			mark(sys, markSystem)
		}
	}

	t := &tree{nodes: make([]*dirNode, 1, 4096)}
	t.nodes[0] = &dirNode{name: baseName(rootPath)}
	// Packed into one int rather than a (dev, ino) pair: overlay2 hardlinks
	// everything, so this set reaches six figures and a struct key costs
	// several times the bytes of a bare uint64.
	seenInodes := make(map[uint64]struct{})

	level, next := &frontier{}, &frontier{}
	level.paths = append([]byte(rootPath), 0)
	level.dirs = []queued{{end: uint32(len(rootPath)), node: 0, own: true, owner: -1, dedupe: rootDedupe}}

	for depth := 0; len(level.dirs) > 0; depth++ {
		levelMarks := marks[depth+1]
		checkPath := len(w.excluded) > 0 || levelMarks != nil
		for _, q := range level.dirs {
			dirPath := level.path(q)
			entries, seen, err := w.rd.read(dirPath, q.dedupe)
			if err != nil {
				// A directory we cannot read would otherwise silently vanish
				// from the totals — count it so the UI can say the tree is
				// incomplete rather than quietly under-reporting.
				t.unreadable++
				for o := int(q.owner); o >= 0; o = nested[o].parent {
					nested[o].unreadable++
				}
				continue
			}
			node := t.nodes[q.node]
			var bytesDone int64

			for i := range entries {
				e := &entries[i]
				if !e.dir {
					if e.key != 0 {
						if _, dup := seenInodes[e.key]; dup {
							continue
						}
						seenInodes[e.key] = struct{}{}
					}
					bytesDone += e.size
					if q.own {
						node.self += e.size
						node.files++
						node.top.offer(e.size, e.name)
					} else {
						node.deep += e.size
					}
					continue
				}

				// Don't cross into other filesystems — each root is walked
				// separately, so we'd otherwise double-count.
				if e.dev != rootDev {
					continue
				}
				rel := q.rel + 1
				child := next.push(dirPath, e.name, queued{
					node: q.node, rel: rel, owner: q.owner, dedupe: q.dedupe,
					own: q.own && int(rel) <= w.maxDepth,
				})
				nestedAt := -1
				if checkPath {
					key := normKey(next.path(child))
					if w.excluded[key] {
						next.pop(child)
						continue
					}
					if m, ok := levelMarks[key]; ok {
						if m == markSystem {
							child.dedupe = true
						} else if q.own {
							// Only when its parent has a node: otherwise it
							// has nowhere to hang, and gets its own walk.
							child.own, child.rel, child.owner = true, 0, int16(m)
							nestedAt = m
						}
					}
				}
				if child.own {
					child.node = int32(len(t.nodes))
					t.nodes = append(t.nodes, &dirNode{name: string(e.name)})
					node.children = append(node.children, child.node)
					if nestedAt >= 0 {
						nested[nestedAt].node = child.node
					}
				}
				next.dirs[len(next.dirs)-1] = child
			}
			// Every byte is counted here exactly once regardless of depth, the
			// same set the previous scan's totals cover — so this sum lines up
			// with progressTotal.
			w.progress.Add(bytesDone)

			if err := w.pace.wait(ctx, seen); err != nil {
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

// throttle holds the walk to a fraction of one core. Low priority only
// matters when something else wants the CPU; on an otherwise quiet box a
// warm-cache walk would happily pin a core for a minute, which shows up on the
// very dashboard it is feeding. So after every stretch of work the walk sleeps
// long enough that the CPU it actually used — measured per thread, so time
// blocked on the disk costs nothing — averages out to the configured share.
type throttle struct {
	share float64 // of one core; >= 1 means unthrottled
	ops   int
	mark  time.Time
	cpu   time.Duration
	timer *time.Timer
}

// Enough entries between checks that the CPU-time query is noise, few enough
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

func baseName(path string) string {
	for len(path) > 1 && os.IsPathSeparator(path[len(path)-1]) {
		path = path[:len(path)-1]
	}
	for i := len(path) - 1; i >= 0; i-- {
		if os.IsPathSeparator(path[i]) {
			if i == len(path)-1 {
				break
			}
			return path[i+1:]
		}
	}
	return path
}

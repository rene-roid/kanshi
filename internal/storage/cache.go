package storage

import (
	"bufio"
	"encoding/gob"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rene-roid/kanshi/internal/roots"
)

// Bumped whenever the file changes shape. This is a cache, so an old one is
// dropped and rebuilt rather than migrated.
const cacheVersion = 1

// sized is what the cache knows about one folder.
type sized struct {
	size       int64
	unreadable int
	at         time.Time
}

// cache holds every folder a walk has sized, grouped by parent so a listing's
// subfolders are one map lookup. Paths are roots.Key form: cleaned, and
// lower-cased on Windows.
//
// It is small enough to keep in memory whole (one entry per folder, a few
// thousand for all of / on the homeserver), and is written back to one file
// after each walk rather than entry by entry.
type cache struct {
	path string // "" keeps it in memory only

	mu    sync.Mutex
	dirs  map[string]map[string]sized // parent key → name key → size
	dirty bool
}

// cacheFile is the on-disk form. gob needs exported fields.
type cacheFile struct {
	Version int
	Dirs    map[string]map[string]cacheEntry
}

type cacheEntry struct {
	Size       int64
	Unreadable int
	SizedAt    int64 // unix seconds
}

// openCache loads the cache at path, or starts an in-memory one when path is
// empty. A file that is missing, from another version or not a cache at all
// starts it empty. It is then written straight away, so a path that cannot be
// written to is reported now rather than after the first walk.
func openCache(path string) (*cache, error) {
	c := &cache{path: path, dirs: map[string]map[string]sized{}}
	if path == "" {
		return c, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err == nil {
		var file cacheFile
		ok := gob.NewDecoder(bufio.NewReader(f)).Decode(&file) == nil && file.Version == cacheVersion
		f.Close()
		if ok {
			for parent, children := range file.Dirs {
				m := make(map[string]sized, len(children))
				for name, e := range children {
					m[name] = sized{size: e.Size, unreadable: e.Unreadable, at: time.Unix(e.SizedAt, 0)}
				}
				c.dirs[parent] = m
			}
			return c, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	c.dirty = true
	if err := c.save(); err != nil {
		return nil, err
	}
	return c, nil
}

// close writes out anything a listing changed since the last walk.
func (c *cache) close() error { return c.save() }

// save writes the cache if it has changed. It goes to a temporary file that
// then replaces the old one, so a crash mid-write leaves the previous cache
// rather than half of this one.
func (c *cache) save() error {
	c.mu.Lock()
	if c.path == "" || !c.dirty {
		c.mu.Unlock()
		return nil
	}
	file := cacheFile{Version: cacheVersion, Dirs: make(map[string]map[string]cacheEntry, len(c.dirs))}
	for parent, children := range c.dirs {
		m := make(map[string]cacheEntry, len(children))
		for name, s := range children {
			m[name] = cacheEntry{Size: s.size, Unreadable: s.unreadable, SizedAt: s.at.Unix()}
		}
		file.Dirs[parent] = m
	}
	c.dirty = false
	c.mu.Unlock()

	tmp, err := os.CreateTemp(filepath.Dir(c.path), filepath.Base(c.path)+".*")
	if err != nil {
		c.markDirty()
		return err
	}
	w := bufio.NewWriter(tmp)
	err = gob.NewEncoder(w).Encode(&file)
	if err == nil {
		err = w.Flush()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), c.path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		c.markDirty()
	}
	return err
}

func (c *cache) markDirty() {
	c.mu.Lock()
	c.dirty = true
	c.mu.Unlock()
}

// get returns what is cached for one folder.
func (c *cache) get(key string) (sized, bool) {
	parent, name := splitKey(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.dirs[parent][name]
	return s, ok
}

// children returns every cached subfolder of dir, by name key. The map is a
// copy, so the caller can hold on to it while walks carry on.
func (c *cache) children(dir string) map[string]sized {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]sized, len(c.dirs[dir]))
	for name, s := range c.dirs[dir] {
		out[name] = s
	}
	return out
}

// put records one folder's size. It is saved with the next walk or on close:
// a listing can always work it out again.
func (c *cache) put(key string, s sized) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set(key, s)
}

// set records one folder's size. The caller holds mu.
func (c *cache) set(key string, s sized) {
	parent, name := splitKey(key)
	m := c.dirs[parent]
	if m == nil {
		m = map[string]sized{}
		c.dirs[parent] = m
	}
	m[name] = s
	c.dirty = true
}

// store replaces everything cached at and below key with one walk's result,
// and saves the cache. Entries below the walk's depth are dropped rather than
// kept: they predate it, and a folder with no entry is simply walked again
// when it is opened. Other roots inside key are left alone, since the walk
// may not have entered them: /mnt/data is its own root, on its own
// filesystem, under /.
func (c *cache) store(key string, t *tree, at time.Time, rootKeys []string) error {
	c.mu.Lock()
	c.deleteSubtree(key, rootKeys)
	// Breadth-first order puts every parent before its children, so each
	// node's key is known by the time its children need it.
	keys := make([]string, len(t.nodes))
	keys[0] = key
	for i, n := range t.nodes {
		for _, ch := range n.children {
			keys[ch] = joinKey(keys[i], nameKey(t.nodes[ch].name))
		}
		c.set(keys[i], sized{size: n.size, unreadable: int(n.unreadable), at: at})
	}
	c.mu.Unlock()
	return c.save()
}

// prune forgets the subfolders of dir that are no longer there, so a folder
// recreated under an old name is sized afresh. Other roots are kept: a drive
// mounted in dir is not one of its subfolders, but is still there.
func (c *cache) prune(dir string, cached map[string]sized, present map[string]bool, rootKeys []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name := range cached {
		if key := joinKey(dir, name); !present[name] && !slices.Contains(rootKeys, key) {
			c.deleteSubtree(key, rootKeys)
		}
	}
}

// deleteSubtree removes key and everything below it, except for the
// subtrees of any other root in rootKeys. The caller holds mu.
func (c *cache) deleteSubtree(key string, rootKeys []string) {
	var keep []string
	for _, r := range rootKeys {
		if r != key && below(key, r) {
			keep = append(keep, r)
		}
	}
	parent, name := splitKey(key)
	if m, ok := c.dirs[parent]; ok {
		delete(m, name)
		if len(m) == 0 {
			delete(c.dirs, parent)
		}
	}
	for p, m := range c.dirs {
		if !below(key, p) || slices.ContainsFunc(keep, func(r string) bool { return below(r, p) }) {
			continue
		}
		// A kept root's own entry sits under a parent inside key, which
		// otherwise loses everything.
		for name := range m {
			if !slices.Contains(keep, joinKey(p, name)) {
				delete(m, name)
			}
		}
		if len(m) == 0 {
			delete(c.dirs, p)
		}
	}
	c.dirty = true
}

// below reports whether key is dir itself or somewhere inside it. Both are
// already in key form.
func below(dir, key string) bool {
	if key == dir {
		return true
	}
	if !strings.HasSuffix(dir, roots.Sep) {
		dir += roots.Sep
	}
	return strings.HasPrefix(key, dir)
}

// splitKey divides a path key into its parent's key and its own name. A
// volume root such as "/" or "c:\" has no parent and is its own name.
func splitKey(key string) (parent, name string) {
	parent = filepath.Dir(key)
	if parent == key {
		return "", key
	}
	return parent, filepath.Base(key)
}

func joinKey(dir, name string) string {
	if strings.HasSuffix(dir, roots.Sep) {
		return dir + name
	}
	return dir + roots.Sep + name
}

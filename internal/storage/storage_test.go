package storage

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rene-roid/kanshi/internal/config"
	"github.com/rene-roid/kanshi/internal/roots"
)

// Sizes are whole 4 KiB blocks and the contents are random, so the space each
// file takes on disk equals its length on any filesystem worth testing on.
const (
	sizeA      = 100 << 10
	sizeDeep   = 48 << 10
	sizeBig    = 200 << 10
	sizeNested = 28 << 10
	sizeSkip   = 500 << 10
)

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, size)
	rand.Read(b)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture builds:
//
//	root/big.bin, root/link.bin (a hard link to big.bin)
//	root/a/a.bin, root/a/b/c/d/e/deep.bin
//	root/nested/n.bin
//	root/skip/s.bin     — excluded
func fixture(t *testing.T) string {
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real // macOS-style /tmp links would otherwise not match
	}
	write(t, filepath.Join(root, "big.bin"), sizeBig)
	if err := os.Link(filepath.Join(root, "big.bin"), filepath.Join(root, "link.bin")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "a", "a.bin"), sizeA)
	write(t, filepath.Join(root, "a", "b", "c", "d", "e", "deep.bin"), sizeDeep)
	write(t, filepath.Join(root, "nested", "n.bin"), sizeNested)
	write(t, filepath.Join(root, "skip", "s.bin"), sizeSkip)
	return root
}

func testConfig(root, db string) config.Config {
	return config.Config{
		TreeDepth:        2,
		StorageCPU:       100,
		StorageExclude:   []string{filepath.Join(root, "skip")},
		StorageInterval:  time.Hour,
		StorageMinRescan: time.Hour,
		StorageCache:     db,
	}
}

func scanner(t *testing.T, root string, rootsSpec ...string) *Scanner {
	t.Helper()
	s := New(testConfig(root, ""), roots.NewResolver(rootsSpec, ""), t.Logf)
	t.Cleanup(func() { s.Close() })
	return s
}

// list reads a folder, walks whatever it queued, and reads it again.
func list(t *testing.T, s *Scanner, root int, rel string) Listing {
	t.Helper()
	if _, ok := s.List(root, rel); !ok {
		t.Fatalf("no root %d", root)
	}
	s.drain(context.Background())
	l, _ := s.List(root, rel)
	if l.Pending != 0 {
		t.Fatalf("%q still has %d pending after the queue drained", rel, l.Pending)
	}
	return l
}

func dirSizes(l Listing) map[string]int64 {
	out := map[string]int64{}
	for _, d := range l.Dirs {
		out[d.Name] = d.Size
	}
	return out
}

func TestListSizesSubfoldersInTheBackground(t *testing.T) {
	root := fixture(t)
	// Windows only de-duplicates hard links inside its system directory.
	old := systemRoot
	systemRoot = func() string { return root }
	defer func() { systemRoot = old }()
	s := scanner(t, root, root)

	first, _ := s.List(0, "")
	if first.Pending != 2 || len(first.Dirs) != 2 {
		t.Fatalf("first listing = %+v, want a and nested pending and skip left out", first)
	}
	if first.Size != first.FileBytes {
		t.Errorf("pending folders should add nothing yet: %+v", first)
	}
	if st := s.Status(); !st.Scanning || st.Queued != 2 {
		t.Errorf("status = %+v, want two walks queued", st)
	}
	if snap := s.Snapshot(); snap.Roots[0].Size != nil {
		t.Error("the root has no size until every folder in it does")
	}

	top := list(t, s, 0, "")
	got := dirSizes(top)
	if got["a"] != sizeA+sizeDeep || got["nested"] != sizeNested || len(got) != 2 {
		t.Errorf("dirs = %v", got)
	}
	if top.Dirs[0].Name != "a" {
		t.Errorf("dirs should be largest first: %+v", top.Dirs)
	}
	if top.FileBytes != sizeBig || top.FileCount != 1 {
		t.Errorf("files: %d bytes in %d files, want the hard link counted once", top.FileBytes, top.FileCount)
	}
	want := int64(sizeBig + sizeA + sizeDeep + sizeNested)
	if top.Size != want {
		t.Errorf("size = %d, want %d", top.Size, want)
	}
	if top.SizedAt == nil {
		t.Error("sized_at should say when the sizes were measured")
	}
	if snap := s.Snapshot(); snap.Roots[0].Size == nil || *snap.Roots[0].Size != want {
		t.Errorf("root summary = %+v", snap.Roots[0])
	}
	if st := s.Status(); st.Scanning || st.UpdatedAt == nil {
		t.Errorf("status after the walks = %+v", st)
	}
}

func TestDrillingPastTheCachedDepthWalksOnDemand(t *testing.T) {
	root := fixture(t)
	s := scanner(t, root, root)
	list(t, s, 0, "")

	// Walking a cached a, a/b and a/b/c (TreeDepth 2), so those open at once.
	ab, _ := s.List(0, "a/b")
	if ab.Pending != 0 || dirSizes(ab)["c"] != sizeDeep {
		t.Errorf("a/b = %+v", ab)
	}
	abc, _ := s.List(0, "a/b/c")
	if abc.Pending != 1 {
		t.Errorf("a/b/c/d is below the cached depth and should be pending: %+v", abc)
	}
	abc = list(t, s, 0, "a/b/c")
	if dirSizes(abc)["d"] != sizeDeep {
		t.Errorf("a/b/c = %+v", abc)
	}
}

func TestPathsOnlyReachWhatAWalkWould(t *testing.T) {
	root := fixture(t)
	s := scanner(t, root, root)
	for rel, want := range map[string]string{
		"a/nope/x": "a",
		"../..":    "",
		"a/../..":  "a",
		"skip":     "",
	} {
		l, ok := s.List(0, rel)
		if !ok || !l.Partial || l.Path != want {
			t.Errorf("%q resolved to %+v, want partial at %q", rel, l, want)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "loop")); err != nil {
			t.Fatal(err)
		}
		if l, _ := s.List(0, "loop"); !l.Partial {
			t.Errorf("a symlink was followed: %+v", l)
		}
		if _, ok := dirSizes(list(t, s, 0, ""))["loop"]; ok {
			t.Error("a symlink was listed as a folder")
		}
	}
	if _, ok := s.List(5, ""); ok {
		t.Error("an unknown root should report false")
	}
}

func TestSizesSurviveARestart(t *testing.T) {
	root := fixture(t)
	db := filepath.Join(t.TempDir(), "cache", "storage.cache")
	r := roots.NewResolver([]string{root}, "")

	s := New(testConfig(root, db), r, t.Logf)
	want := list(t, s, 0, "").Size
	s.Close()

	s = New(testConfig(root, db), r, t.Logf)
	defer s.Close()
	l, _ := s.List(0, "")
	if l.Pending != 0 || l.Size != want {
		t.Errorf("after reopening: %+v, want everything sized at %d", l, want)
	}
	if st := s.Status(); st.Scanning {
		t.Errorf("nothing should be walked again: %+v", st)
	}
}

func TestStaleAndRescannedFoldersAreWalkedAgain(t *testing.T) {
	root := fixture(t)
	s := scanner(t, root, root)
	list(t, s, 0, "")

	write(t, filepath.Join(root, "a", "more.bin"), sizeA)
	if l, _ := s.List(0, ""); dirSizes(l)["a"] != sizeA+sizeDeep || s.Status().Scanning {
		t.Error("a fresh size should be served from the cache without a walk")
	}

	// Rescan skips folders sized within StorageMinRescan...
	if st, _ := s.Rescan(0, ""); st.Queued != 0 {
		t.Errorf("rescan queued %d folders that were just sized", st.Queued)
	}
	// ...and a size older than StorageInterval is walked when it is shown.
	s.cfg.StorageInterval = time.Nanosecond
	if l, _ := s.List(0, ""); l.Pending != 0 {
		t.Errorf("a stale size should still be shown while it is refreshed: %+v", l)
	}
	if st := s.Status(); st.Queued != 2 {
		t.Errorf("status = %+v, want both folders queued", st)
	}
	s.drain(context.Background())
	s.cfg.StorageInterval = time.Hour
	if l, _ := s.List(0, ""); dirSizes(l)["a"] != 2*sizeA+sizeDeep {
		t.Errorf("a = %d after the refresh", dirSizes(l)["a"])
	}

	s.cfg.StorageMinRescan = 0
	if st, _ := s.Rescan(0, ""); st.Queued != 2 {
		t.Errorf("rescan queued %d, want 2", st.Queued)
	}
}

func TestRemovedFoldersAreForgotten(t *testing.T) {
	root := fixture(t)
	s := scanner(t, root, root)
	list(t, s, 0, "")
	aKey := roots.Key(filepath.Join(root, "a"))
	if _, ok := s.cache.get(joinKey(aKey, "b")); !ok {
		t.Fatal("the walk of a should have cached a/b")
	}

	if err := os.RemoveAll(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if _, ok := dirSizes(list(t, s, 0, ""))["a"]; ok {
		t.Error("a is gone but still listed")
	}
	if _, ok := s.cache.get(joinKey(aKey, "b")); ok {
		t.Error("a is gone but its subfolders are still cached")
	}

	// Recreated under the same name, it is sized afresh.
	write(t, filepath.Join(root, "a", "new.bin"), sizeNested)
	if l, _ := s.List(0, ""); l.Pending != 1 {
		t.Errorf("a recreated folder should be pending: %+v", l)
	}
}

func TestWalksLeaveOtherRootsCached(t *testing.T) {
	root := fixture(t)
	inner := filepath.Join(root, "a", "b")
	s := scanner(t, root, root, inner)
	// Excluded from walks, a/b is what a drive mounted there would be: its
	// own root, and not a subfolder of a.
	s.cfg.StorageExclude = append(s.cfg.StorageExclude, inner)
	list(t, s, 1, "")
	list(t, s, 1, "c")
	deep := roots.Key(filepath.Join(inner, "c", "d", "e"))
	if _, ok := s.cache.get(deep); !ok {
		t.Fatal("walking a/b/c/d should have cached a/b/c/d/e")
	}

	// Walking a, and then listing it without b in it, must leave everything
	// the other root knows alone.
	list(t, s, 0, "")
	list(t, s, 0, "a")
	if _, ok := s.cache.get(deep); !ok {
		t.Error("walking a dropped what the a/b root had cached")
	}
	if snap := s.Snapshot(); snap.Roots[1].Size == nil {
		t.Error("walking a dropped the a/b root's own size")
	}
}

func TestUnreadableDirectoriesAreCounted(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a non-root Unix user")
	}
	root := fixture(t)
	locked := filepath.Join(root, "a", "b")
	os.Chmod(locked, 0)
	defer os.Chmod(locked, 0o755)

	s := scanner(t, root, root)
	if top := list(t, s, 0, ""); top.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1", top.Unreadable)
	}
	if a, _ := s.List(0, "a"); a.Unreadable != 1 {
		t.Errorf("unreadable in a = %d, want 1", a.Unreadable)
	}
	if l, _ := s.List(0, "a/b"); !l.Partial || l.Path != "a" {
		t.Errorf("an unreadable folder resolves to its parent: %+v", l)
	}
}

func TestMissingRootIsLeftOut(t *testing.T) {
	root := fixture(t)
	snap := scanner(t, root, filepath.Join(root, "nope"), root).Snapshot()
	if len(snap.Roots) != 1 || snap.Roots[0].Path != root {
		t.Errorf("roots = %+v", snap.Roots)
	}
}

func TestQueueLeavesNestedFoldersToTheOuterWalk(t *testing.T) {
	s := scanner(t, t.TempDir())
	j := func(p string) job { return job{path: p, key: roots.Key(p)} }
	keys := func() []string {
		var out []string
		for _, q := range s.queue {
			out = append(out, q.path)
		}
		return out
	}
	x, xa, y := filepath.Join("/x"), filepath.Join("/x", "a"), filepath.Join("/y")

	s.enqueue([]job{j(xa), j(y)}, false)
	s.enqueue([]job{j(x)}, false)
	if got := keys(); len(got) != 2 || got[0] != y || got[1] != x {
		t.Errorf("queue = %v, want x to replace x/a", got)
	}
	s.enqueue([]job{j(xa)}, true)
	if got := keys(); len(got) != 2 || got[0] != x {
		t.Errorf("queue = %v, want x moved to the front for x/a", got)
	}
}

func TestDeleteSubtreeLeavesSiblingsAndOtherRoots(t *testing.T) {
	c, _ := openCache("")
	sep := roots.Sep
	base := filepath.Join(string(filepath.Separator), "data")
	b := base + sep + "b"
	paths := []string{
		// Removed along with b.
		b, b + sep + "c", b + sep + "c" + sep + "d",
		// Siblings whose names start the same way.
		base, base + sep + "b.x", base + sep + "b0", base + sep + "b0" + sep + "e", base + sep + "bc",
		// Another root inside b, and what is cached under it.
		b + sep + "mnt", b + sep + "mnt" + sep + "f", b + sep + "mnt" + sep + "f" + sep + "g",
	}
	for _, p := range paths {
		c.put(roots.Key(p), sized{size: 1, at: time.Now()})
	}
	c.mu.Lock()
	c.deleteSubtree(roots.Key(b), []string{roots.Key(b + sep + "mnt")})
	c.mu.Unlock()
	for i, p := range paths {
		if _, ok := c.get(roots.Key(p)); ok == (i < 3) {
			t.Errorf("%s: cached = %v", p, ok)
		}
	}
}

func TestAnUnusableCacheFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "storage.cache")
	if err := os.WriteFile(path, []byte("not a cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := openCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.dirs) != 0 {
		t.Errorf("dirs = %v", c.dirs)
	}
	c.put(roots.Key(filepath.Join(string(filepath.Separator), "x")), sized{size: 7, at: time.Now()})
	if err := c.close(); err != nil {
		t.Fatal(err)
	}
	if c, _ = openCache(path); len(c.dirs) != 1 {
		t.Errorf("the rewritten cache did not load back: %v", c.dirs)
	}
	if matches, _ := filepath.Glob(path + ".*"); len(matches) != 0 {
		t.Errorf("temporary files left behind: %v", matches)
	}
}

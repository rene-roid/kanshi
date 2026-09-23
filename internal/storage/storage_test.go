package storage

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
//	root/nested/n.bin   — configured as a root of its own
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

func scanner(root string, rootsSpec ...string) *Scanner {
	cfg := config.Config{
		TreeDepth:      2,
		StorageCPU:     100,
		StorageExclude: []string{filepath.Join(root, "skip")},
	}
	return New(cfg, roots.NewResolver(rootsSpec, ""))
}

func TestWalk(t *testing.T) {
	root := fixture(t)
	// Windows only de-duplicates hard links inside its system directory.
	old := systemRoot
	systemRoot = func() string { return root }
	defer func() { systemRoot = old }()

	s := scanner(root, root, filepath.Join(root, "nested"))
	snap := s.Scan(context.Background(), true)
	if snap.Error != nil {
		t.Fatal(*snap.Error)
	}
	if len(snap.Roots) != 2 {
		t.Fatalf("roots = %+v", snap.Roots)
	}

	want := int64(sizeBig + sizeA + sizeDeep + sizeNested) // link.bin once, skip/ never
	if got := snap.Roots[0].Size; got != want {
		t.Errorf("root size = %d, want %d", got, want)
	}
	if got := snap.Roots[1].Size; got != sizeNested {
		t.Errorf("nested root size = %d, want %d", got, sizeNested)
	}
	if s.views[1].t != s.views[0].t {
		t.Error("the nested root should come out of the outer root's walk, not a second one")
	}

	top, _ := s.List(0, "")
	names := map[string]bool{}
	for _, d := range top.Dirs {
		names[d.Name] = true
	}
	if !names["a"] || !names["nested"] || names["skip"] {
		t.Errorf("top-level dirs = %+v", top.Dirs)
	}
	if top.FileBytes != sizeBig || top.FileCount != 1 {
		t.Errorf("top-level files: %d bytes in %d files, want the hard link counted once", top.FileBytes, top.FileCount)
	}

	// Depth 2: a and a/b get listings; c, d and e roll up into a/b.
	ab, _ := s.List(0, "a/b")
	if ab.Deep != sizeDeep || len(ab.Dirs) != 0 {
		t.Errorf("a/b = %+v, want %d deep bytes and no dirs", ab, sizeDeep)
	}
	gone, ok := s.List(0, "a/b/c/d")
	if !ok || !gone.Partial || gone.Path != "a/b" {
		t.Errorf("below the depth resolves to the deepest listing: %+v", gone)
	}
	n, _ := s.List(1, "")
	if n.FileBytes != sizeNested {
		t.Errorf("nested root listing = %+v", n)
	}

	if s.stale() {
		t.Error("nothing changed since the walk, so it should not be stale")
	}
}

func TestSymlinksAreNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered by the junction test")
	}
	root := fixture(t)
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	snap := scanner(root, root).Scan(context.Background(), true)
	if want := int64(sizeBig + sizeA + sizeDeep + sizeNested); snap.Roots[0].Size != want {
		t.Errorf("size = %d, want %d", snap.Roots[0].Size, want)
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

	snap := scanner(root, root, filepath.Join(root, "nested")).Scan(context.Background(), true)
	if snap.Roots[0].Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1", snap.Roots[0].Unreadable)
	}
	if snap.Roots[1].Unreadable != 0 {
		t.Errorf("the nested root does not contain the locked dir: %d", snap.Roots[1].Unreadable)
	}
}

func TestMissingRootIsLeftOut(t *testing.T) {
	root := fixture(t)
	snap := scanner(root, filepath.Join(root, "nope"), root).Scan(context.Background(), true)
	if len(snap.Roots) != 1 || snap.Roots[0].Path != root {
		t.Errorf("roots = %+v", snap.Roots)
	}
}

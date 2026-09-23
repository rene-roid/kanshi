package roots

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWithin(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "a", "b")
	if !Within(dir, sub) || !Within(dir, dir) {
		t.Error("a directory contains itself and its descendants")
	}
	if Within(sub, dir) || Within(dir+"x", filepath.Join(dir+"x2", "y")) {
		t.Error("prefix of a name is not containment")
	}
}

func TestDiskUsage(t *testing.T) {
	u, err := DiskUsage(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if u.Total == 0 || u.Used > u.Total || u.Volume == "" {
		t.Errorf("usage = %+v", u)
	}
}

func TestAutoRootsExist(t *testing.T) {
	for _, r := range NewResolver([]string{"auto"}, "").Roots() {
		if info, err := os.Stat(r.Path); err != nil || !info.IsDir() {
			t.Errorf("auto root %+v is not a directory", r)
		}
	}
}

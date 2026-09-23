package storage

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestJunctionsAreNotFollowed(t *testing.T) {
	root := fixture(t)
	out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, "loop"), filepath.Join(root, "a")).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink: %v: %s", err, out)
	}
	old := systemRoot
	systemRoot = func() string { return root }
	defer func() { systemRoot = old }()

	snap := scanner(root, root).Scan(context.Background(), true)
	if want := int64(sizeBig + sizeA + sizeDeep + sizeNested); snap.Roots[0].Size != want {
		t.Errorf("size = %d, want %d: the junction was followed", snap.Roots[0].Size, want)
	}
}

func TestHardLinksOutsideTheSystemDirectoryCountTwice(t *testing.T) {
	root := fixture(t)
	snap := scanner(root, root).Scan(context.Background(), true)
	if want := int64(2*sizeBig + sizeA + sizeDeep + sizeNested); snap.Roots[0].Size != want {
		t.Errorf("size = %d, want %d", snap.Roots[0].Size, want)
	}
}

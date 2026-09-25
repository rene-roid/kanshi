package storage

import (
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
	s := scanner(t, root, root)
	top := list(t, s, 0, "")
	if _, ok := dirSizes(top)["loop"]; ok {
		t.Error("the junction was listed as a folder")
	}
	if l, _ := s.List(0, "loop"); !l.Partial {
		t.Errorf("the junction was followed: %+v", l)
	}
}

func TestHardLinksOutsideTheSystemDirectoryCountTwice(t *testing.T) {
	root := fixture(t)
	top := list(t, scanner(t, root, root), 0, "")
	if top.FileBytes != 2*sizeBig || top.FileCount != 2 {
		t.Errorf("files: %d bytes in %d files", top.FileBytes, top.FileCount)
	}
}

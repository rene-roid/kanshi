package roots

import (
	"os"
	"strings"
	"testing"
)

func TestAutoRootsWindows(t *testing.T) {
	got := NewResolver([]string{"auto"}, "").Roots()
	system := strings.TrimRight(os.Getenv("SystemDrive"), `\`) + `\`
	if len(got) == 0 || !strings.EqualFold(got[0].Path, system) {
		t.Fatalf("auto roots should start with the system drive %s: %+v", system, got)
	}
	home, _ := os.UserHomeDir()
	found := false
	for _, r := range got {
		found = found || strings.EqualFold(r.Path, home)
	}
	if !found {
		t.Errorf("the profile folder %s is missing from %+v", home, got)
	}
	a, _ := DiskUsage(got[0].Path)
	b, _ := DiskUsage(home)
	if a.Volume != b.Volume {
		t.Errorf("C:\\ and the profile folder should share a volume: %q vs %q", a.Volume, b.Volume)
	}
}

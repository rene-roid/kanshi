package vitals

import (
	"testing"

	"github.com/rene-roid/kanshi/internal/roots"
)

func TestSampleLooksLikeAMachine(t *testing.T) {
	r := New(roots.NewResolver([]string{"auto"}, ""))
	r.Prime()
	s := r.Sample()
	if s.CPU.Count == 0 || len(s.CPU.Cores) != s.CPU.Count {
		t.Errorf("cpu = %+v", s.CPU)
	}
	for _, c := range s.CPU.Cores {
		if c < 0 || c > 100 {
			t.Errorf("core at %v%%", c)
		}
	}
	if s.Memory.Total == 0 || s.Memory.Used > s.Memory.Total {
		t.Errorf("memory = %+v", s.Memory)
	}
	if s.Uptime <= 0 {
		t.Errorf("uptime = %v", s.Uptime)
	}
	if len(s.Filesystems) == 0 {
		t.Error("no volume meters")
	}
	if _, _, ok := r.netCounters(); !ok {
		t.Error("network counters unavailable")
	}
}

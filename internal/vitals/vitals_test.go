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
}

// The rates are deltas and may well be zero on a quiet machine, but the
// counters behind them are totals since boot: any running machine has read
// from its disk and talked on its network.
func TestCountersAreLive(t *testing.T) {
	r := New(roots.NewResolver([]string{"auto"}, ""))
	if rx, tx, ok := r.netCounters(); !ok || rx+tx == 0 {
		t.Errorf("network counters: rx=%d tx=%d ok=%v", rx, tx, ok)
	}
	if read, write, ok := r.diskCounters(); !ok || read+write == 0 {
		t.Errorf("disk counters: read=%d write=%d ok=%v", read, write, ok)
	}
}

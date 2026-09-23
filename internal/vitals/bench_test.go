package vitals

import (
	"testing"
	"time"

	"github.com/rene-roid/kanshi/internal/roots"
)

func BenchmarkSample(b *testing.B) {
	r := New(roots.NewResolver([]string{"auto"}, ""))
	r.Prime()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Step the baselines back so every iteration takes the steady-state
		// path rather than the short-window resample.
		r.sys.cpuAt = r.sys.cpuAt.Add(-time.Second)
		r.prevNet.at = r.prevNet.at.Add(-time.Second)
		r.prevDisk.at = r.prevDisk.at.Add(-time.Second)
		r.Sample()
	}
}

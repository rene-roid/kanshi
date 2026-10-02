package history

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func frame(n int) []byte {
	b, _ := json.Marshal(map[string]int{"n": n})
	return b
}

func open(t *testing.T, dir string, retention time.Duration) *Store {
	t.Helper()
	s, err := Open(dir, retention, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func frameN(t *testing.T, raw json.RawMessage) int {
	t.Helper()
	var v struct{ N int }
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("frame %q: %v", raw, err)
	}
	return v.N
}

// Records written, closed and read back by a fresh store come back whole,
// frames included, and each direction of At lands on the right one.
func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour).Truncate(time.Hour)
	s := open(t, dir, 24*time.Hour)
	for i := range 10 {
		// Straddles an hour boundary, so there are two segments.
		at := base.Add(time.Duration(50+i*2) * time.Minute)
		if err := s.Add(at, float64(i), float64(100-i), frame(i)); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	s = open(t, dir, 24*time.Hour)
	defer s.Close()
	if got := len(s.Series(0, 1<<62, 0)); got != 10 {
		t.Fatalf("points after reopen = %d, want 10", got)
	}
	t5 := base.Add(60 * time.Minute).Unix()
	for _, c := range []struct {
		t    int64
		dir  int
		want int
	}{
		{t5, 0, 5}, {t5 + 1, 0, 5}, {t5 + 100, 0, 6}, {t5, -1, 4}, {t5, 1, 6}, {0, 0, 0}, {1 << 62, 0, 9},
	} {
		p, raw, ok := s.At(c.t, c.dir)
		if !ok {
			t.Fatalf("At(%d, %d) found nothing", c.t, c.dir)
		}
		if n := frameN(t, raw); n != c.want || int(p.CPU) != c.want {
			t.Errorf("At(%d, %d) = frame %d cpu %v, want %d", c.t, c.dir, n, p.CPU, c.want)
		}
	}
	if _, _, ok := s.At(base.Add(50*time.Minute).Unix(), -1); ok {
		t.Error("nothing is before the first record")
	}
}

// The segment still being written is readable, since every record is flushed.
func TestReadsTheOpenSegment(t *testing.T) {
	s := open(t, t.TempDir(), time.Hour)
	defer s.Close()
	now := time.Now()
	_ = s.Add(now.Add(-2*time.Second), 1, 1, frame(1))
	_ = s.Add(now.Add(-time.Second), 2, 2, frame(2))
	_, raw, ok := s.At(now.Unix(), 0)
	if !ok || frameN(t, raw) != 2 {
		t.Fatalf("latest record = %s, %v", raw, ok)
	}
}

// A crash leaves a gzip stream without its trailer. Everything flushed before
// it is still read, and the next start writes a new file rather than
// appending behind the torn one.
func TestTornSegment(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, time.Hour)
	now := time.Now()
	for i := range 3 {
		_ = s.Add(now.Add(time.Duration(i-10)*time.Second), 0, 0, frame(i))
	}
	// Simulate the crash: drop the file without closing the gzip stream.
	s.f.Close()
	s.f, s.gz = nil, nil

	s = open(t, dir, time.Hour)
	defer s.Close()
	if got := len(s.Series(0, 1<<62, 0)); got != 3 {
		t.Fatalf("points after crash = %d, want 3", got)
	}
	_ = s.Add(now, 0, 0, frame(3))
	if _, raw, ok := s.At(now.Unix(), 0); !ok || frameN(t, raw) != 3 {
		t.Fatalf("record after crash = %s, %v", raw, ok)
	}
}

// Old segments are deleted once everything in them has aged out.
func TestRetention(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, 2*time.Hour)
	now := time.Now()
	for h := 5; h >= 0; h-- {
		_ = s.Add(now.Add(-time.Duration(h)*time.Hour), 0, 0, frame(h))
	}
	s.Close()
	first, _ := s.Span()
	if age := now.Unix() - first; age > int64(2*time.Hour/time.Second) {
		t.Errorf("oldest point is %ds old, retention is 2h", age)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"+ext))
	if len(files) > 3 {
		t.Errorf("%d segments left on disk, want at most 3", len(files))
	}
}

// Past n points the series is bucketed, and a bucket keeps its peak.
func TestSeriesKeepsPeaks(t *testing.T) {
	s := open(t, t.TempDir(), 24*time.Hour)
	defer s.Close()
	base := time.Now().Add(-time.Hour).Unix()
	for i := range 100 {
		cpu := 1.0
		if i == 37 {
			cpu = 99
		}
		_ = s.Add(time.Unix(base+int64(i), 0), cpu, 50, frame(i))
	}
	pts := s.Series(base, base+99, 10)
	if len(pts) > 10 {
		t.Fatalf("got %d points, want at most 10", len(pts))
	}
	peak := float32(0)
	for _, p := range pts {
		peak = max(peak, p.CPU)
	}
	if peak != 99 {
		t.Errorf("peak = %v, want 99", peak)
	}
	b, _ := json.Marshal(pts[:1])
	if string(b) != `[[`+itoa(pts[0].T)+`,1.0,50.0]]` {
		t.Errorf("wire form = %s", b)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

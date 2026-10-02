// Package history keeps what the dashboard showed, so the page can be scrolled
// back through it.
//
// Records go to one gzip file per clock hour (a new one also on every start),
// named after the Unix time of their first record. A record is one line: its
// time, CPU% and RAM% as plain text, then the frame as JSON. The three leading
// numbers are all the timeline needs, so they are kept in memory for the whole
// retention window and the frames stay on disk until one is asked for.
//
// The gzip stream is flushed after every record, so the file being written is
// readable up to its last line. A file cut short by a crash still reads up to
// the last flush; its tail is lost, nothing else is. Files are never appended
// to after the process that wrote them exits, so a torn one never hides the
// records written after it.
package history

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ext = ".jsonl.gz"

// A frame line can hold a few dozen containers; this is far past any of them.
const maxLine = 4 << 20

// Point is one record's summary: what the timeline draws.
type Point struct {
	T   int64
	CPU float32
	Mem float32
}

// MarshalJSON writes a point as [t, cpu, mem]. A week of them is tens of
// thousands of entries, so field names would be most of the payload.
func (p Point) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 24)
	b = append(b, '[')
	b = strconv.AppendInt(b, p.T, 10)
	b = append(b, ',')
	b = strconv.AppendFloat(b, float64(p.CPU), 'f', 1, 32)
	b = append(b, ',')
	b = strconv.AppendFloat(b, float64(p.Mem), 'f', 1, 32)
	return append(b, ']'), nil
}

type segment struct {
	start int64 // Unix time of its first record
	path  string
}

// Store is safe for concurrent use: one writer, any number of readers.
type Store struct {
	dir       string
	retention time.Duration
	logf      func(string, ...any)

	mu       sync.Mutex
	points   []Point   // every record kept, oldest first
	segments []segment // oldest first; the last one may be open
	f        *os.File
	gz       *gzip.Writer
	hour     int64 // the clock hour the open segment covers
}

// Open reads the summaries of every segment still inside the retention
// window and deletes the ones that are not.
func Open(dir string, retention time.Duration, logf func(string, ...any)) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, retention: retention, logf: logf}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ext) {
			continue
		}
		start, err := strconv.ParseInt(strings.TrimSuffix(name, ext), 10, 64)
		if err != nil {
			continue
		}
		s.segments = append(s.segments, segment{start: start, path: filepath.Join(dir, name)})
	}
	sort.Slice(s.segments, func(i, j int) bool { return s.segments[i].start < s.segments[j].start })
	s.prune(time.Now())

	for _, seg := range s.segments {
		_ = scan(seg.path, func(p Point, _ []byte) bool {
			// Clocks can step backwards; the timeline needs time order.
			if n := len(s.points); n == 0 || p.T > s.points[n-1].T {
				s.points = append(s.points, p)
			}
			return true
		})
	}
	return s, nil
}

// Add records one frame. Records must arrive in time order; one that does not
// (the clock stepped back) is dropped rather than reordering the file.
func (s *Store) Add(at time.Time, cpu, mem float64, frame []byte) error {
	t := at.Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.points); n > 0 && t <= s.points[n-1].T {
		return nil
	}
	if hour := t - t%3600; s.gz == nil || hour != s.hour {
		if err := s.rotate(t, hour); err != nil {
			return err
		}
	}

	var line bytes.Buffer
	fmt.Fprintf(&line, "%d\t%.1f\t%.1f\t", t, cpu, mem)
	line.Write(frame)
	line.WriteByte('\n')
	if _, err := s.gz.Write(line.Bytes()); err != nil {
		return err
	}
	// Flushed per record so readers see it now and a crash loses at most it.
	if err := s.gz.Flush(); err != nil {
		return err
	}
	s.points = append(s.points, Point{T: t, CPU: float32(cpu), Mem: float32(mem)})
	return nil
}

// rotate closes the open segment, starts a new one and drops what has aged
// out. Called with mu held.
func (s *Store) rotate(t, hour int64) error {
	s.closeSegment()
	path := filepath.Join(s.dir, strconv.FormatInt(t, 10)+ext)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	s.f, s.gz, s.hour = f, gz, hour
	s.segments = append(s.segments, segment{start: t, path: path})
	s.prune(time.Unix(t, 0))
	return nil
}

func (s *Store) closeSegment() {
	if s.gz == nil {
		return
	}
	if err := s.gz.Close(); err != nil {
		s.logf("history: %v", err)
	}
	if err := s.f.Close(); err != nil {
		s.logf("history: %v", err)
	}
	s.f, s.gz = nil, nil
}

// prune forgets every record older than the retention window. A segment is
// only deleted once the one after it also starts before the cutoff, so a
// segment straddling it is kept whole. Called with mu held, or before the
// store is shared.
func (s *Store) prune(now time.Time) {
	cutoff := now.Add(-s.retention).Unix()
	i := sort.Search(len(s.points), func(i int) bool { return s.points[i].T >= cutoff })
	s.points = append(s.points[:0:0], s.points[i:]...)

	keep := 0
	for keep+1 < len(s.segments) && s.segments[keep+1].start <= cutoff {
		if err := os.Remove(s.segments[keep].path); err != nil && !os.IsNotExist(err) {
			s.logf("history: %v", err)
		}
		keep++
	}
	s.segments = s.segments[keep:]
}

// Close finishes the open segment so it ends cleanly.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeSegment()
}

// Span is the first and last recorded time, or zeros when nothing is kept.
func (s *Store) Span() (first, last int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.points) == 0 {
		return 0, 0
	}
	return s.points[0].T, s.points[len(s.points)-1].T
}

// Series returns the points in [from, to]. Past n of them, the range is cut
// into n equal buckets and each keeps its highest CPU and RAM: a spike is the
// thing a timeline has to show, and averaging would sand it off. Empty buckets
// are left out, so a gap in the records stays a gap.
func (s *Store) Series(from, to int64, n int) []Point {
	s.mu.Lock()
	lo := sort.Search(len(s.points), func(i int) bool { return s.points[i].T >= from })
	hi := sort.Search(len(s.points), func(i int) bool { return s.points[i].T > to })
	pts := append([]Point(nil), s.points[lo:hi]...)
	s.mu.Unlock()

	if n <= 0 || len(pts) <= n || to <= from {
		return pts
	}
	width := float64(to-from+1) / float64(n)
	out := make([]Point, 0, n)
	bucket := -1
	for _, p := range pts {
		b := int(float64(p.T-from) / width)
		if b != bucket || len(out) == 0 {
			bucket = b
			out = append(out, p)
			continue
		}
		// The bucket takes the time of its CPU peak, so picking it on the
		// timeline opens the record that actually has the spike.
		last := &out[len(out)-1]
		if p.CPU > last.CPU {
			last.T, last.CPU = p.T, p.CPU
		}
		last.Mem = max(last.Mem, p.Mem)
	}
	return out
}

// At finds a record and reads its frame back. dir 0 takes the one nearest t,
// -1 the last one before t and +1 the first one after it.
func (s *Store) At(t int64, dir int) (Point, json.RawMessage, bool) {
	s.mu.Lock()
	i := sort.Search(len(s.points), func(i int) bool { return s.points[i].T >= t })
	switch dir {
	case -1:
		i--
	case 1:
		if i < len(s.points) && s.points[i].T == t {
			i++
		}
	default:
		if i == len(s.points) || (i > 0 && t-s.points[i-1].T < s.points[i].T-t) {
			i--
		}
	}
	if i < 0 || i >= len(s.points) {
		s.mu.Unlock()
		return Point{}, nil, false
	}
	p := s.points[i]
	k := sort.Search(len(s.segments), func(k int) bool { return s.segments[k].start > p.T }) - 1
	if k < 0 {
		s.mu.Unlock()
		return Point{}, nil, false
	}
	path := s.segments[k].path
	s.mu.Unlock()

	var frame json.RawMessage
	_ = scan(path, func(q Point, raw []byte) bool {
		if q.T == p.T {
			if json.Valid(raw) {
				frame = append(json.RawMessage(nil), raw...)
			}
			return false
		}
		return q.T < p.T
	})
	return p, frame, frame != nil
}

// scan reads a segment line by line until fn returns false. A torn last line
// or a truncated gzip stream ends the scan; everything before it is kept.
func scan(path string, fn func(p Point, frame []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return err
	}
	defer gz.Close()

	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		p, frame, ok := parse(sc.Bytes())
		if !ok {
			continue
		}
		if !fn(p, frame) {
			return nil
		}
	}
	return sc.Err()
}

// parse splits a record into its summary and frame. The frame is not parsed:
// startup reads every line of every segment, and only needs the summary. A
// line torn by a crash almost always stops short of the frame's closing brace,
// which is all that is checked.
func parse(line []byte) (Point, []byte, bool) {
	fields := bytes.SplitN(line, []byte{'\t'}, 4)
	if len(fields) != 4 || len(fields[3]) == 0 || fields[3][len(fields[3])-1] != '}' {
		return Point{}, nil, false
	}
	t, err1 := strconv.ParseInt(string(fields[0]), 10, 64)
	cpu, err2 := strconv.ParseFloat(string(fields[1]), 32)
	mem, err3 := strconv.ParseFloat(string(fields[2]), 32)
	if err1 != nil || err2 != nil || err3 != nil {
		return Point{}, nil, false
	}
	return Point{T: t, CPU: float32(cpu), Mem: float32(mem)}, fields[3], true
}

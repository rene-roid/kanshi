// Package vitals reads host-wide CPU / memory / disk / network figures.
//
// On Linux they come straight out of /proc and /sys. Kanshi runs with
// `network_mode: host` and without lxcfs, so those report real host values
// with no special configuration. On Windows they come from the same kernel32,
// ntdll and iphlpapi calls Task Manager uses.
package vitals

import (
	"math"
	"sync"
	"time"

	"github.com/rene-roid/kanshi/internal/roots"
)

// Below this gap the counter deltas are too small to divide by: a 20ms window
// turns a routine 2MB read into a fake 100MB/s spike. Hold the previous rate.
const minDT = 500 * time.Millisecond

// Reader owns the counter baselines every rate is measured against. One is
// created per process; the mutex only ever guards against a REST handler
// sampling at the same moment as the poller.
type Reader struct {
	roots *roots.Resolver

	mu       sync.Mutex
	sys      sysState // the platform's own baselines and cached handles
	prevNet  counterPair
	prevDisk counterPair
	netRate  [2]float64
	diskRate [2]float64
}

type counterPair struct {
	at   time.Time
	a, b uint64
	ok   bool
}

func New(r *roots.Resolver) *Reader { return &Reader{roots: r} }

/* ── payload ────────────────────────────────────────────────────────────── */

// Sample is the JSON the browser consumes; field names are part of the wire
// contract with web/app.js.
type Sample struct {
	TS          float64      `json:"ts"`
	CPU         CPU          `json:"cpu"`
	Memory      Memory       `json:"memory"`
	Swap        Swap         `json:"swap"`
	Network     RxTx         `json:"network"`
	DiskIO      ReadWrite    `json:"diskio"`
	Filesystems []Filesystem `json:"filesystems"`
	Uptime      float64      `json:"uptime"`
}

type CPU struct {
	Percent float64   `json:"percent"`
	Cores   []float64 `json:"cores"`
	// Load average and temperature are null where the platform has no such
	// thing (Windows has no load average) or no readable sensor.
	Load  *[3]float64 `json:"load"`
	Count int         `json:"count"`
	Temp  *float64    `json:"temp"`
}

type Memory struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Available uint64  `json:"available"`
	Cached    uint64  `json:"cached"`
	Percent   float64 `json:"percent"`
}

type Swap struct {
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Percent float64 `json:"percent"`
}

type RxTx struct {
	RX float64 `json:"rx"`
	TX float64 `json:"tx"`
}

type ReadWrite struct {
	Read  float64 `json:"read"`
	Write float64 `json:"write"`
}

type Filesystem struct {
	Label   string  `json:"label"`
	Path    string  `json:"path"`
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Free    uint64  `json:"free"`
	Percent float64 `json:"percent"`
}

/* ── entry points ───────────────────────────────────────────────────────── */

// Prime seeds the counter baselines so the first published frame shows real
// numbers instead of a screen of zeros.
func (r *Reader) Prime() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.primeCPU()
	rx, tx, ok := r.netCounters()
	rate(&r.prevNet, &r.netRate, rx, tx, ok)
	read, write, ok := r.diskCounters()
	rate(&r.prevDisk, &r.diskRate, read, write, ok)
}

// Sample takes one full reading of the host.
func (r *Reader) Sample() Sample {
	r.mu.Lock()
	defer r.mu.Unlock()

	cores := r.cpuCores()
	mem, sw := r.memory()

	rx, tx, netOK := r.netCounters()
	net := rate(&r.prevNet, &r.netRate, rx, tx, netOK)
	read, write, diskOK := r.diskCounters()
	disk := rate(&r.prevDisk, &r.diskRate, read, write, diskOK)

	var mean float64
	if len(cores) > 0 {
		var sum float64
		for _, c := range cores {
			sum += c
		}
		mean = round1(sum / float64(len(cores)))
	}
	if cores == nil {
		cores = []float64{} // the browser iterates this; never ship null
	}

	return Sample{
		TS: float64(time.Now().UnixNano()) / 1e9,
		CPU: CPU{
			Percent: mean,
			Cores:   cores,
			Load:    r.loadAvg(),
			Count:   len(cores),
			Temp:    r.temperature(),
		},
		Memory:      mem,
		Swap:        sw,
		Network:     RxTx{RX: net[0], TX: net[1]},
		DiskIO:      ReadWrite{Read: disk[0], Write: disk[1]},
		Filesystems: r.Filesystems(),
		Uptime:      r.uptime(),
	}
}

// Filesystems reports usage for each storage root's volume. Roots that share a
// volume — C:\ and the profile folder on it — get one meter, labelled with the
// first of them.
func (r *Reader) Filesystems() []Filesystem {
	list := r.roots.Roots()
	out := make([]Filesystem, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, root := range list {
		u, err := roots.DiskUsage(root.Path)
		if err != nil || u.Total == 0 || seen[u.Volume] {
			continue
		}
		seen[u.Volume] = true
		fs := Filesystem{Label: root.Label, Path: root.Path, Total: u.Total, Used: u.Used, Free: u.Free}
		if u.Used+u.Free > 0 {
			fs.Percent = round1(float64(u.Used) / float64(u.Used+u.Free) * 100)
		}
		out = append(out, fs)
	}
	return out
}

/* ── shared arithmetic ──────────────────────────────────────────────────── */

// busyPercent turns two counter deltas into a utilisation, clamped to 0–100.
func busyPercent(busyDelta, allDelta float64) float64 {
	if allDelta <= 0 || busyDelta <= 0 {
		return 0
	}
	return round1(math.Min(busyDelta/allDelta*100, 100))
}

func memoryFigures(total, avail, cached uint64) Memory {
	if avail > total {
		avail = total
	}
	used := total - avail
	m := Memory{Total: total, Used: used, Available: avail, Cached: cached}
	if total > 0 {
		m.Percent = round1(float64(used) / float64(total) * 100)
	}
	return m
}

func swapFigures(total, free uint64) Swap {
	if free > total {
		free = total
	}
	s := Swap{Total: total, Used: total - free}
	if total > 0 {
		s.Percent = round1(float64(s.Used) / float64(total) * 100)
	}
	return s
}

// rate turns two counter readings into bytes/second, holding the previous
// answer when the window is too short to divide by.
func rate(prev *counterPair, held *[2]float64, a, b uint64, ok bool) [2]float64 {
	if !ok {
		return [2]float64{0, 0}
	}
	now := time.Now()
	if prev.ok && now.Sub(prev.at) < minDT {
		return *held
	}
	was := *prev
	*prev = counterPair{at: now, a: a, b: b, ok: true}
	if !was.ok {
		return [2]float64{0, 0}
	}
	dt := now.Sub(was.at).Seconds()
	if dt <= 0 {
		return *held
	}
	// Counters are 64-bit but reset when an interface or disk disappears;
	// clamp rather than reporting a negative or an absurd spike.
	*held = [2]float64{diff(a, was.a) / dt, diff(b, was.b) / dt}
	return *held
}

func diff(now, prev uint64) float64 {
	if now < prev {
		return 0
	}
	return float64(now - prev)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

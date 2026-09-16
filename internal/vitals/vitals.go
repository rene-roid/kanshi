// Package vitals reads host-wide CPU / memory / disk / network figures
// straight out of /proc and /sys.
//
// Kanshi runs with `network_mode: host` and without lxcfs, so /proc/stat,
// /proc/meminfo, /proc/diskstats and /proc/net/dev all report real host values
// with no special configuration — the same reason the psutil version worked.
package vitals

import (
	"bufio"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yuuki824/kanshi/internal/config"
)

// Below this gap the counter deltas are too small to divide by: a 20ms window
// turns a routine 2MB read into a fake 100MB/s spike. Hold the previous rate.
const minDT = 500 * time.Millisecond

// Linux reports block counts in 512-byte sectors regardless of device
// geometry, so this is a constant rather than something to look up.
const sectorSize = 512

// Reader owns the counter baselines every rate is measured against. One is
// created per process; the mutex only ever guards against a REST handler
// sampling at the same moment as the poller.
type Reader struct {
	cfg config.Config

	mu       sync.Mutex
	prevCPU  []cpuTimes
	cpuAt    time.Time
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

func New(cfg config.Config) *Reader { return &Reader{cfg: cfg} }

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
	Percent float64    `json:"percent"`
	Cores   []float64  `json:"cores"`
	Load    [3]float64 `json:"load"`
	Count   int        `json:"count"`
	Temp    *float64   `json:"temp"`
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

/* ── CPU ────────────────────────────────────────────────────────────────── */

// cpuTimes mirrors one /proc/stat line. guest and guestNice are already
// counted inside user and nice, so they are subtracted back out of the total.
type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal, guest, guestNice uint64
}

// total is everything the CPU could have been doing, minus iowait — during
// iowait the core is genuinely idle, and folding it into the denominator makes
// a busy disk look like a busy processor.
func (t cpuTimes) total() uint64 {
	return t.user + t.nice + t.system + t.idle + t.irq + t.softirq + t.steal
}

func (t cpuTimes) busy() uint64 { return t.total() - t.idle }

func readCPUTimes() ([]cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []cpuTimes
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu") {
			break // the cpu lines always come first; stop before intr/btime
		}
		if strings.HasPrefix(line, "cpu ") {
			continue // the aggregate line; we average the per-core ones instead
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		var n [10]uint64
		for i := 1; i < len(fields) && i <= 10; i++ {
			n[i-1], _ = strconv.ParseUint(fields[i], 10, 64)
		}
		out = append(out, cpuTimes{
			user: n[0], nice: n[1], system: n[2], idle: n[3], iowait: n[4],
			irq: n[5], softirq: n[6], steal: n[7], guest: n[8], guestNice: n[9],
		})
	}
	return out, sc.Err()
}

// cpuCores returns per-core utilisation since the previous call.
//
// Every reading is a delta against the last one, so if that call was moments
// ago every core reads 0%. When the gap is too short to be meaningful, measure
// a real (short, blocking) window instead — Sample runs off the request path,
// so this never stalls anything the browser is waiting on.
func (r *Reader) cpuCores() []float64 {
	now, err := readCPUTimes()
	if err != nil {
		return nil
	}
	if r.prevCPU == nil || time.Since(r.cpuAt) < minDT {
		r.prevCPU, r.cpuAt = now, time.Now()
		time.Sleep(250 * time.Millisecond)
		if now, err = readCPUTimes(); err != nil {
			return nil
		}
	}

	prev := r.prevCPU
	r.prevCPU, r.cpuAt = now, time.Now()
	if len(prev) != len(now) {
		return make([]float64, len(now)) // core count changed; skip one frame
	}

	out := make([]float64, len(now))
	for i := range now {
		allDelta := float64(now[i].total()) - float64(prev[i].total())
		busyDelta := float64(now[i].busy()) - float64(prev[i].busy())
		if allDelta <= 0 || busyDelta <= 0 {
			continue
		}
		out[i] = round1(math.Min(busyDelta/allDelta*100, 100))
	}
	return out
}

/* ── memory ─────────────────────────────────────────────────────────────── */

func readMeminfo() map[string]uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil
	}
	defer f.Close()

	out := make(map[string]uint64, 64)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		// Everything except HugePages counts is reported in kB.
		if len(fields) > 1 && fields[1] == "kB" {
			v *= 1024
		}
		out[key] = v
	}
	return out
}

func memory(mi map[string]uint64) Memory {
	total := mi["MemTotal"]
	// MemAvailable is the kernel's own estimate and has been there since 3.14;
	// the fallback is only for exotic kernels.
	avail, ok := mi["MemAvailable"]
	if !ok {
		avail = mi["MemFree"] + mi["Cached"] + mi["Buffers"]
	}
	if avail > total {
		avail = total
	}
	used := total - avail
	m := Memory{
		Total:     total,
		Used:      used,
		Available: avail,
		// `free` counts reclaimable slab as cache, and so does htop.
		Cached: mi["Cached"] + mi["SReclaimable"] + mi["Buffers"],
	}
	if total > 0 {
		m.Percent = round1(float64(used) / float64(total) * 100)
	}
	return m
}

func swap(mi map[string]uint64) Swap {
	total, free := mi["SwapTotal"], mi["SwapFree"]
	if free > total {
		free = total
	}
	s := Swap{Total: total, Used: total - free}
	if total > 0 {
		s.Percent = round1(float64(s.Used) / float64(total) * 100)
	}
	return s
}

/* ── network and disk rates ─────────────────────────────────────────────── */

func readNetCounters() (rx, tx uint64, ok bool) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		_, rest, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue // the two header lines
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(fields[0], 10, 64)
		t, _ := strconv.ParseUint(fields[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx, true
}

// diskDevices picks the block devices worth summing. Where a disk has
// partitions we count the partitions; where it has none (loop0, a bare nvme
// namespace) we count the disk itself. Counting both would double every byte.
func diskDevices() map[string]bool {
	f, err := os.Open("/proc/partitions")
	if err != nil {
		return nil
	}
	defer f.Close()

	var names []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 4 || fields[0] == "major" {
			continue
		}
		names = append(names, fields[3])
	}

	// /proc/partitions lists a disk before its partitions, so walking it
	// backwards means a partition is always seen before its parent disk.
	out := make(map[string]bool, len(names))
	var kept []string
	for i := len(names) - 1; i >= 0; i-- {
		name := names[i]
		last := name[len(name)-1]
		if last >= '0' && last <= '9' {
			out[name] = true
			kept = append(kept, name)
			continue
		}
		if len(kept) == 0 || !strings.HasPrefix(kept[len(kept)-1], name) {
			out[name] = true
			kept = append(kept, name)
		}
	}
	return out
}

func readDiskCounters(want map[string]bool) (read, write uint64, ok bool) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// The kernel has grown this line over the years (14, 18, 20 fields);
		// the first ten have never moved.
		if len(fields) < 10 || !want[fields[2]] {
			continue
		}
		rs, _ := strconv.ParseUint(fields[5], 10, 64)
		ws, _ := strconv.ParseUint(fields[9], 10, 64)
		read += rs * sectorSize
		write += ws * sectorSize
	}
	return read, write, true
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

/* ── temperature ────────────────────────────────────────────────────────── */

// preferredSensors are the chip names that actually mean "the processor",
// most-specific first. Anything else is a last resort.
var preferredSensors = []string{"coretemp", "k10temp", "cpu_thermal", "acpitz", "zenpower"}

func temperature() *float64 {
	readings := hwmonReadings()
	for _, want := range preferredSensors {
		for _, r := range readings {
			if r.name == want {
				v := round1(r.celsius)
				return &v
			}
		}
	}
	if len(readings) > 0 {
		v := round1(readings[0].celsius)
		return &v
	}
	return nil
}

type reading struct {
	name    string
	celsius float64
}

func hwmonReadings() []reading {
	var out []reading
	for _, dir := range globDirs("/sys/class/hwmon") {
		name := strings.TrimSpace(readFile(dir + "/name"))
		if name == "" {
			// Pre-3.15 kernels hang the name off the backing device.
			name = strings.TrimSpace(readFile(dir + "/device/name"))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var files []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "temp") && strings.HasSuffix(e.Name(), "_input") {
				files = append(files, e.Name())
			}
		}
		// temp1 is the package on every driver that exposes one, so read the
		// lowest-numbered input rather than whichever the directory lists first.
		sort.Strings(files)
		for _, file := range files {
			if v, ok := milliCelsius(dir + "/" + file); ok {
				out = append(out, reading{name: name, celsius: v})
				break
			}
		}
	}
	// Boards with no hwmon driver still expose a thermal zone, and on ARM SBCs
	// that is the only place a CPU temperature appears at all.
	for _, dir := range globDirs("/sys/class/thermal") {
		if !strings.Contains(dir, "thermal_zone") {
			continue
		}
		if v, ok := milliCelsius(dir + "/temp"); ok {
			out = append(out, reading{name: strings.TrimSpace(readFile(dir + "/type")), celsius: v})
		}
	}
	return out
}

func milliCelsius(path string) (float64, bool) {
	raw := strings.TrimSpace(readFile(path))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v == 0 {
		return 0, false
	}
	return v / 1000, true
}

func globDirs(parent string) []string {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, parent+"/"+e.Name())
	}
	sort.Strings(out)
	return out
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

/* ── filesystems and uptime ─────────────────────────────────────────────── */

// Filesystems reports usage for each configured storage root, read straight
// off statfs.
func (r *Reader) Filesystems() []Filesystem {
	roots := r.cfg.Roots()
	out := make([]Filesystem, 0, len(roots))
	for _, root := range roots {
		var st syscall.Statfs_t
		if err := syscall.Statfs(root.Path, &st); err != nil {
			continue
		}
		bsize := uint64(st.Bsize)
		total := st.Blocks * bsize
		if total == 0 {
			continue
		}
		// Bavail excludes root-reserved blocks, so used+free won't equal total.
		// Report "used" the way df does, against the non-reserved capacity.
		free := st.Bavail * bsize
		used := total - st.Bfree*bsize
		fs := Filesystem{Label: root.Label, Path: root.Path, Total: total, Used: used, Free: free}
		if used+free > 0 {
			fs.Percent = round1(float64(used) / float64(used+free) * 100)
		}
		out = append(out, fs)
	}
	return out
}

func loadAvg() [3]float64 {
	fields := strings.Fields(readFile("/proc/loadavg"))
	var out [3]float64
	for i := 0; i < 3 && i < len(fields); i++ {
		v, _ := strconv.ParseFloat(fields[i], 64)
		out[i] = round2(v)
	}
	return out
}

// bootTime comes from /proc/stat's btime rather than /proc/uptime because the
// latter is namespaced on some runtimes and would report the container's age.
func bootTime() float64 {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			v, _ := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			return v
		}
	}
	return 0
}

/* ── entry points ───────────────────────────────────────────────────────── */

// Prime seeds the counter baselines so the first published frame shows real
// numbers instead of a screen of zeros.
func (r *Reader) Prime() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if times, err := readCPUTimes(); err == nil {
		r.prevCPU, r.cpuAt = times, time.Now()
	}
	rx, tx, ok := readNetCounters()
	rate(&r.prevNet, &r.netRate, rx, tx, ok)
	read, write, ok := readDiskCounters(diskDevices())
	rate(&r.prevDisk, &r.diskRate, read, write, ok)
}

// Sample takes one full reading of the host.
func (r *Reader) Sample() Sample {
	r.mu.Lock()
	defer r.mu.Unlock()

	cores := r.cpuCores()
	mi := readMeminfo()

	rx, tx, netOK := readNetCounters()
	net := rate(&r.prevNet, &r.netRate, rx, tx, netOK)
	read, write, diskOK := readDiskCounters(diskDevices())
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

	var uptime float64
	if bt := bootTime(); bt > 0 {
		uptime = float64(time.Now().UnixNano())/1e9 - bt
	}

	return Sample{
		TS: float64(time.Now().UnixNano()) / 1e9,
		CPU: CPU{
			Percent: mean,
			Cores:   cores,
			Load:    loadAvg(),
			Count:   len(cores),
			Temp:    temperature(),
		},
		Memory:      memory(mi),
		Swap:        swap(mi),
		Network:     RxTx{RX: net[0], TX: net[1]},
		DiskIO:      ReadWrite{Read: disk[0], Write: disk[1]},
		Filesystems: r.Filesystems(),
		Uptime:      uptime,
	}
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

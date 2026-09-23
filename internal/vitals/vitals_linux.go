package vitals

import (
	"bytes"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Linux reports block counts in 512-byte sectors regardless of device
// geometry, so this is a constant rather than something to look up.
const sectorSize = 512

// How often the sets of physical disks and network interfaces are re-read.
// Hot-plugging a NIC is rare; listing /sys every tick is not free.
const devicesEvery = time.Minute

// sysState is everything Linux sampling keeps between ticks.
//
// The /proc files stay open and are re-read with pread at offset zero, which
// makes the kernel regenerate them: no open/close per tick, and one reused
// buffer instead of a fresh one per file.
type sysState struct {
	stat, meminfo, netdev, diskstats, loadavg, temp procFile

	buf    []byte
	fields [][]byte

	prevCPU, curCPU []cpuTimes
	cpuAt           time.Time
	bootTime        float64

	netIfaces map[string]bool // physical interfaces; nil means "all but lo"
	disks     map[string]bool // whole physical disks
	devicesAt time.Time

	tempPath string
	tempAt   time.Time // last sensor search, so a sensorless box is not searched every tick
}

type procFile struct {
	fd   int
	open bool
}

// read returns the whole file, regenerated. The slice aliases the shared
// buffer and is only valid until the next read.
func (s *sysState) read(f *procFile, path string) []byte {
	if s.buf == nil {
		s.buf = make([]byte, 16*1024)
	}
	for {
		if !f.open {
			fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
			if err != nil {
				return nil
			}
			f.fd, f.open = fd, true
		}
		n, err := syscall.Pread(f.fd, s.buf, 0)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			syscall.Close(f.fd)
			f.open = false
			return nil
		}
		if n < len(s.buf) {
			return s.buf[:n]
		}
		// /proc/stat's interrupt line grows with the core count; a full
		// buffer means the file may have been cut short, so read it again.
		s.buf = make([]byte, 2*len(s.buf))
	}
}

/* ── CPU ────────────────────────────────────────────────────────────────── */

// cpuTimes mirrors one /proc/stat line. guest and guestNice are already
// counted inside user and nice, so they are left out of the total.
type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

// total is everything the CPU could have been doing, minus iowait — during
// iowait the core is genuinely idle, and folding it into the denominator makes
// a busy disk look like a busy processor.
func (t cpuTimes) total() uint64 {
	return t.user + t.nice + t.system + t.idle + t.irq + t.softirq + t.steal
}

func (t cpuTimes) busy() uint64 { return t.total() - t.idle }

// readCPU parses the per-core lines of /proc/stat into dst. The boot time is
// on a later line of the same file and never changes, so it is picked up once.
func (r *Reader) readCPU(dst []cpuTimes) []cpuTimes {
	s := &r.sys
	b := s.read(&s.stat, "/proc/stat")
	dst = dst[:0]
	for len(b) > 0 {
		var line []byte
		line, b = nextLine(b)
		if !bytes.HasPrefix(line, []byte("cpu")) {
			if s.bootTime > 0 {
				break // the cpu lines always come first; stop before intr
			}
			if rest, ok := bytes.CutPrefix(line, []byte("btime ")); ok {
				s.bootTime = float64(atou(bytes.TrimSpace(rest)))
				break
			}
			continue
		}
		if len(line) > 3 && line[3] == ' ' {
			continue // the aggregate line; we average the per-core ones instead
		}
		f := splitFields(line, s.fields)
		s.fields = f
		if len(f) < 5 {
			continue
		}
		var n [8]uint64
		for i := 1; i < len(f) && i <= 8; i++ {
			n[i-1] = atou(f[i])
		}
		dst = append(dst, cpuTimes{
			user: n[0], nice: n[1], system: n[2], idle: n[3], iowait: n[4],
			irq: n[5], softirq: n[6], steal: n[7],
		})
	}
	return dst
}

func (r *Reader) primeCPU() {
	r.sys.prevCPU = r.readCPU(r.sys.prevCPU)
	r.sys.cpuAt = time.Now()
}

// cpuCores returns per-core utilisation since the previous call.
//
// Every reading is a delta against the last one, so if that call was moments
// ago every core reads 0%. When the gap is too short to be meaningful, measure
// a real (short, blocking) window instead — Sample runs off the request path,
// so this never stalls anything the browser is waiting on.
func (r *Reader) cpuCores() []float64 {
	s := &r.sys
	s.curCPU = r.readCPU(s.curCPU)
	if len(s.curCPU) == 0 {
		return nil
	}
	if len(s.prevCPU) == 0 || time.Since(s.cpuAt) < minDT {
		s.prevCPU, s.curCPU = s.curCPU, s.prevCPU
		s.cpuAt = time.Now()
		time.Sleep(250 * time.Millisecond)
		if s.curCPU = r.readCPU(s.curCPU); len(s.curCPU) == 0 {
			return nil
		}
	}

	prev, now := s.prevCPU, s.curCPU
	out := make([]float64, len(now))
	if len(prev) == len(now) { // otherwise the core count changed; skip one frame
		for i := range now {
			out[i] = busyPercent(float64(now[i].busy())-float64(prev[i].busy()),
				float64(now[i].total())-float64(prev[i].total()))
		}
	}
	s.prevCPU, s.curCPU = now, prev
	s.cpuAt = time.Now()
	return out
}

/* ── memory ─────────────────────────────────────────────────────────────── */

func (r *Reader) memory() (Memory, Swap) {
	s := &r.sys
	var total, free, avail, buffers, cached, reclaimable, swapTotal, swapFree uint64
	haveAvail := false
	b := s.read(&s.meminfo, "/proc/meminfo")
	for len(b) > 0 {
		var line []byte
		line, b = nextLine(b)
		key, rest, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		// Every value used here is in kB.
		v := atou(bytes.TrimLeft(rest, " ")) * 1024
		switch string(key) { // no allocation: the compiler special-cases this
		case "MemTotal":
			total = v
		case "MemFree":
			free = v
		case "MemAvailable":
			avail, haveAvail = v, true
		case "Buffers":
			buffers = v
		case "Cached":
			cached = v
		case "SReclaimable":
			reclaimable = v
		case "SwapTotal":
			swapTotal = v
		case "SwapFree":
			swapFree = v
		}
	}
	// MemAvailable is the kernel's own estimate and has been there since 3.14;
	// the fallback is only for exotic kernels.
	if !haveAvail {
		avail = free + cached + buffers
	}
	// `free` counts reclaimable slab as cache, and so does htop.
	return memoryFigures(total, avail, cached+reclaimable+buffers), swapFigures(swapTotal, swapFree)
}

/* ── network and disk rates ─────────────────────────────────────────────── */

// refreshDevices re-reads which interfaces and disks are physical.
//
// Summing every line of /proc/net/dev counts a container's traffic up to three
// times — on its veth, on the Docker bridge, and on the real NIC — plus all
// loopback chatter. Summing every line of /proc/diskstats counts an LVM or
// dm-crypt volume's I/O again on the disk underneath. Only hardware that sysfs
// ties to a device is what actually crosses the wire or hits the platter.
func (s *sysState) refreshDevices() {
	if !s.devicesAt.IsZero() && time.Since(s.devicesAt) < devicesEvery {
		return
	}
	s.devicesAt = time.Now()
	s.netIfaces = withDevice("/sys/class/net")
	s.disks = withDevice("/sys/block")
	if len(s.disks) == 0 {
		s.disks = partitionDevices() // no sysfs; fall back to /proc/partitions
	}
}

// withDevice lists the entries of a sysfs class directory that are backed by
// real hardware (they have a "device" link).
func withDevice(dir string) map[string]bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make(map[string]bool)
	for _, e := range entries {
		if _, err := os.Stat(dir + "/" + e.Name() + "/device"); err == nil {
			out[e.Name()] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r *Reader) netCounters() (rx, tx uint64, ok bool) {
	s := &r.sys
	s.refreshDevices()
	b := s.read(&s.netdev, "/proc/net/dev")
	if b == nil {
		return 0, 0, false
	}
	for len(b) > 0 {
		var line []byte
		line, b = nextLine(b)
		name, rest, found := bytes.Cut(line, []byte(":"))
		if !found {
			continue // the two header lines
		}
		name = bytes.TrimSpace(name)
		if s.netIfaces != nil {
			if !s.netIfaces[string(name)] {
				continue
			}
		} else if string(name) == "lo" {
			continue
		}
		f := splitFields(rest, s.fields)
		s.fields = f
		if len(f) < 9 {
			continue
		}
		rx += atou(f[0])
		tx += atou(f[8])
	}
	return rx, tx, true
}

func (r *Reader) diskCounters() (read, write uint64, ok bool) {
	s := &r.sys
	s.refreshDevices()
	b := s.read(&s.diskstats, "/proc/diskstats")
	if b == nil {
		return 0, 0, false
	}
	for len(b) > 0 {
		var line []byte
		line, b = nextLine(b)
		f := splitFields(line, s.fields)
		s.fields = f
		// The kernel has grown this line over the years (14, 18, 20 fields);
		// the first ten have never moved.
		if len(f) < 10 || !s.disks[string(f[2])] {
			continue
		}
		read += atou(f[5]) * sectorSize
		write += atou(f[9]) * sectorSize
	}
	return read, write, true
}

// partitionDevices picks block devices from /proc/partitions. Where a disk
// has partitions we count the partitions; where it has none we count the disk
// itself. Counting both would double every byte.
func partitionDevices() map[string]bool {
	raw, err := os.ReadFile("/proc/partitions")
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
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

/* ── temperature ────────────────────────────────────────────────────────── */

// preferredSensors are the chip names that actually mean "the processor",
// most-specific first. Anything else is a last resort.
var preferredSensors = []string{"coretemp", "k10temp", "cpu_thermal", "acpitz", "zenpower"}

// temperature reads the one sensor file picked by findSensor. Searching
// /sys/class/hwmon means dozens of directory reads, so it happens once rather
// than every tick, and again only if that file stops answering.
func (r *Reader) temperature() *float64 {
	s := &r.sys
	if s.tempPath == "" {
		if !s.tempAt.IsZero() && time.Since(s.tempAt) < 5*time.Minute {
			return nil
		}
		s.tempAt = time.Now()
		if s.tempPath = findSensor(); s.tempPath == "" {
			return nil
		}
	}
	b := s.read(&s.temp, s.tempPath)
	v := atou(bytes.TrimSpace(b))
	if len(b) == 0 || v == 0 {
		if s.temp.open {
			syscall.Close(s.temp.fd)
			s.temp.open = false
		}
		s.tempPath = ""
		return nil
	}
	c := round1(float64(v) / 1000)
	return &c
}

type sensor struct {
	name, path string
}

func findSensor() string {
	sensors := hwmonSensors()
	for _, want := range preferredSensors {
		for _, s := range sensors {
			if s.name == want {
				return s.path
			}
		}
	}
	if len(sensors) > 0 {
		return sensors[0].path
	}
	return ""
}

func hwmonSensors() []sensor {
	var out []sensor
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
			if readable(dir + "/" + file) {
				out = append(out, sensor{name: name, path: dir + "/" + file})
				break
			}
		}
	}
	// Boards with no hwmon driver still expose a thermal zone, and on ARM SBCs
	// that is the only place a CPU temperature appears at all.
	for _, dir := range globDirs("/sys/class/thermal") {
		if strings.Contains(dir, "thermal_zone") && readable(dir+"/temp") {
			out = append(out, sensor{name: strings.TrimSpace(readFile(dir + "/type")), path: dir + "/temp"})
		}
	}
	return out
}

func readable(path string) bool {
	raw := strings.TrimSpace(readFile(path))
	return raw != "" && raw != "0"
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

/* ── load and uptime ────────────────────────────────────────────────────── */

func (r *Reader) loadAvg() *[3]float64 {
	s := &r.sys
	f := splitFields(s.read(&s.loadavg, "/proc/loadavg"), s.fields)
	s.fields = f
	if len(f) < 3 {
		return nil
	}
	var out [3]float64
	for i := 0; i < 3; i++ {
		out[i] = round2(atof(f[i]))
	}
	return &out
}

// uptime comes from /proc/stat's btime rather than /proc/uptime because the
// latter is namespaced on some runtimes and would report the container's age.
func (r *Reader) uptime() float64 {
	if r.sys.bootTime == 0 {
		r.readCPU(nil)
	}
	if r.sys.bootTime == 0 {
		return 0
	}
	return float64(time.Now().UnixNano())/1e9 - r.sys.bootTime
}

/* ── allocation-free parsing ────────────────────────────────────────────── */

func nextLine(b []byte) (line, rest []byte) {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return b[:i], b[i+1:]
	}
	return b, nil
}

// splitFields is strings.Fields over bytes, reusing dst's backing array.
func splitFields(b []byte, dst [][]byte) [][]byte {
	dst = dst[:0]
	for i := 0; i < len(b); {
		for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
			i++
		}
		start := i
		for i < len(b) && b[i] != ' ' && b[i] != '\t' {
			i++
		}
		if i > start {
			dst = append(dst, b[start:i])
		}
	}
	return dst
}

// atou parses the leading digits of b.
func atou(b []byte) uint64 {
	var n uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + uint64(c-'0')
	}
	return n
}

// atof parses the plain "12.34" decimals /proc/loadavg is written in.
func atof(b []byte) float64 {
	whole, frac, _ := bytes.Cut(b, []byte("."))
	v := float64(atou(whole))
	scale := 0.1
	for _, c := range frac {
		if c < '0' || c > '9' {
			break
		}
		v += float64(c-'0') * scale
		scale /= 10
	}
	return v
}

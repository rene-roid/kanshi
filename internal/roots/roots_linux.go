package roots

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const caseInsensitive = false

// auto is / plus every real filesystem mounted under /mnt.
func auto(hostRoot string) []Root {
	base := hostRoot
	if base == "" {
		base = "/"
	}
	out := []Root{{Label: "/", Path: base}}
	for _, point := range mountsUnder(hostRoot+"/mnt", "/proc/self/mountinfo") {
		label := strings.TrimPrefix(point, hostRoot)
		out = append(out, Root{Label: label, Path: point})
	}
	return out
}

// Kernel and virtual filesystems that can be mounted under /mnt but hold no
// data worth mapping.
var pseudoFS = map[string]bool{
	"autofs": true, "binfmt_misc": true, "bpf": true, "cgroup": true, "cgroup2": true,
	"configfs": true, "debugfs": true, "devpts": true, "devtmpfs": true, "efivarfs": true,
	"fusectl": true, "hugetlbfs": true, "mqueue": true, "nsfs": true, "overlay": true,
	"proc": true, "pstore": true, "ramfs": true, "rpc_pipefs": true, "securityfs": true,
	"squashfs": true, "sysfs": true, "tmpfs": true, "tracefs": true, "nfsd": true,
	"fuse.gvfsd-fuse": true, "fuse.portal": true, "fuse.lxcfs": true, "fuse.snapfuse": true,
}

// mountsUnder lists mount points at or below dir from a mountinfo file,
// sorted.
// Inside a container the host's mounts appear under the host root because the
// bind mount of / is recursive, so the same code finds them in both places.
func mountsUnder(dir, mountinfo string) []string {
	f, err := os.Open(mountinfo)
	if err != nil {
		return nil
	}
	defer f.Close()

	type mount struct{ source, fstype string }
	byPoint := map[string]mount{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// 36 35 98:0 /mnt1 /mnt/parent rw,noatime master:1 - ext3 /dev/root rw
		pre, post, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(pre)
		tail := strings.Fields(post)
		if len(fields) < 5 || len(tail) < 1 {
			continue
		}
		point := unescape(fields[4])
		if point != dir && !strings.HasPrefix(point, dir+"/") {
			continue
		}
		// Later lines are mounted on top of earlier ones at the same point,
		// so the last one is what a path lookup actually reaches.
		byPoint[point] = mount{source: fields[2] + " " + unescape(fields[3]), fstype: tail[0]}
	}

	points := make([]string, 0, len(byPoint))
	for p := range byPoint {
		points = append(points, p)
	}
	sort.Strings(points)

	// A bind mount of the same directory at two points would be walked, and
	// counted, twice. Keep the first.
	sources := map[string]bool{}
	out := points[:0]
	for _, p := range points {
		m := byPoint[p]
		if pseudoFS[m.fstype] || sources[m.source] {
			continue
		}
		sources[m.source] = true
		out = append(out, p)
	}
	return out
}

// unescape reverses mountinfo's octal escapes ("\040" is a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// DiskUsage reads a volume's size off statfs.
func DiskUsage(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, err
	}
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	// Bavail excludes root-reserved blocks, so used+free won't equal total.
	// "Used" is reported the way df does it.
	u := Usage{Total: total, Used: total - st.Bfree*bsize, Free: st.Bavail * bsize}
	var s syscall.Stat_t
	if err := syscall.Stat(path, &s); err == nil {
		u.Volume = strconv.FormatUint(uint64(s.Dev), 10)
	} else {
		u.Volume = path
	}
	return u, nil
}

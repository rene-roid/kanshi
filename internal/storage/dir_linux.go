package storage

import (
	"errors"
	"syscall"
	"time"
	"unsafe"
)

const pathSep = '/'

// normKey matches roots.Key for the clean paths the walk builds.
func normKey(p []byte) string { return string(p) }

func sameName(a, b string) bool { return a == b }

// nameKey is how one name appears in a cache key.
func nameKey(name string) string { return name }

// systemRoot is where Windows keeps its hard links. Linux has no such place:
// every multiply-linked file is spotted by its link count instead.
var systemRoot = func() string { return "" }

// rootDevice checks the root is a directory and returns the filesystem it is
// on, which the walk will not leave.
func rootDevice(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return 0, errors.New("not a directory")
	}
	return uint64(st.Dev), nil
}

// Linux dirent64: d_ino (8), d_off (8), d_reclen (2), d_type (1), d_name.
const (
	direntReclen = 16
	direntType   = 18
	direntName   = 19
)

const (
	atSymlinkNofollow = 0x100
	atNoAutomount     = 0x800
)

// A variable, not a constant: a negative constant cannot convert to uintptr.
var atFDCWD = -100

// dirReader lists directories with getdents64 and fstatat.
//
// The loop does no allocation per entry: records are parsed in place from one
// reused buffer, and each is stat'ed relative to its directory's descriptor
// with the NUL-terminated name straight out of that buffer, so the kernel
// resolves one path component rather than the whole path from the root.
type dirReader struct {
	buf     []byte
	names   []byte
	entries []dirEntry
	st      syscall.Stat_t
}

func newDirReader() *dirReader { return &dirReader{buf: make([]byte, 32*1024)} }

// read lists one directory. path must be followed by a NUL in its backing
// array. The result is only valid until the next call.
func (r *dirReader) read(path []byte, _ bool) ([]dirEntry, int, error) {
	fd, err := openDir(path)
	if err != nil {
		return nil, 0, err
	}
	defer syscall.Close(fd)

	r.entries, r.names = r.entries[:0], r.names[:0]
	seen := 0
	for {
		n, err := syscall.Getdents(fd, r.buf)
		if err != nil || n <= 0 {
			break // end of directory, or a read error mid-directory
		}
		for off := 0; off < n; {
			reclen := int(*(*uint16)(unsafe.Pointer(&r.buf[off+direntReclen])))
			typ := r.buf[off+direntType]
			name := r.buf[off+direntName : off+reclen]
			off += reclen
			if end := indexNUL(name); end >= 0 {
				name = name[:end]
			}
			seen++
			// d_type comes back with the directory entry, so symlinks,
			// sockets and devices are rejected without a stat at all.
			if typ != syscall.DT_DIR && typ != syscall.DT_REG && typ != syscall.DT_UNKNOWN {
				continue
			}
			if len(name) == 0 || (name[0] == '.' && (len(name) == 1 || (len(name) == 2 && name[1] == '.'))) {
				continue
			}
			if statAt(fd, path, name, &r.st) != nil {
				continue
			}
			var e dirEntry
			switch r.st.Mode & syscall.S_IFMT {
			case syscall.S_IFDIR:
				e.dir, e.dev = true, uint64(r.st.Dev)
			case syscall.S_IFREG:
				e.size = int64(r.st.Blocks) * 512
				if r.st.Nlink > 1 {
					e.key = uint64(r.st.Dev)<<48 | uint64(r.st.Ino)
				}
			default:
				continue
			}
			// Copied out because the next getdents call reuses buf.
			start := len(r.names)
			r.names = append(r.names, name...)
			e.name = r.names[start:len(r.names):len(r.names)]
			r.entries = append(r.entries, e)
		}
	}
	return r.entries, seen, nil
}

// openDir opens a directory by a path that is followed by a NUL in its
// backing array, so the kernel reads it in place rather than from a copy.
func openDir(path []byte) (int, error) {
	fd, _, errno := syscall.Syscall6(syscall.SYS_OPENAT, uintptr(atFDCWD), uintptr(unsafe.Pointer(&path[0])),
		syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0, 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

func indexNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

// join builds a full path without path/filepath's Clean pass: every path here
// is an already-clean parent plus one getdents name.
func join(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

const (
	ioprioWhoProcess = 1
	ioprioClassBE    = 2
	ioprioClassShift = 13
)

// deprioritise puts the calling thread at the back of the queue for both the
// CPU (nice 19) and the disk (lowest best-effort I/O priority), so a rescan
// never makes the at-a-glance numbers stutter. The idle I/O class would be
// gentler still, but on a busy box it can starve the walk outright.
func deprioritise() {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19)
	syscall.RawSyscall(syscall.SYS_IOPRIO_SET, ioprioWhoProcess, 0, ioprioClassBE<<ioprioClassShift|7)
}

// threadCPU is the user+system time of the calling thread, which the walk has
// to itself because it is locked to it.
func threadCPU() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_THREAD, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

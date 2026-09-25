package storage

import (
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

const pathSep = '\\'

// normKey matches roots.Key: NTFS names compare case-insensitively.
func normKey(p []byte) string { return strings.ToLower(string(p)) }

// sameName lets a pasted c:\users\... find C:\Users\... on disk.
func sameName(a, b string) bool { return strings.EqualFold(a, b) }

// nameKey is how one name appears in a cache key, lower-cased like roots.Key.
func nameKey(name string) string { return strings.ToLower(name) }

// systemRoot is C:\Windows. Its component store hard-links most of System32,
// so files there are de-duplicated by file ID; anywhere else hard links are
// rare enough that tracking every file ID would cost more memory than it saves.
// A variable so tests can point it at a scratch directory.
var systemRoot = func() string { return os.Getenv("SystemRoot") }

func rootDevice(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, errors.New("not a directory")
	}
	// Windows walks never cross into another volume anyway: mounted folders
	// are reparse points, and those are skipped.
	return 0, nil
}

var (
	kernel32                         = syscall.NewLazyDLL("kernel32.dll")
	procGetFileInformationByHandleEx = kernel32.NewProc("GetFileInformationByHandleEx")
	procSetThreadPriority            = kernel32.NewProc("SetThreadPriority")
	procGetThreadTimes               = kernel32.NewProc("GetThreadTimes")

	advapi32                  = syscall.NewLazyDLL("advapi32.dll")
	procLookupPrivilegeValueW = advapi32.NewProc("LookupPrivilegeValueW")
	procAdjustTokenPrivileges = advapi32.NewProc("AdjustTokenPrivileges")
)

const (
	fileListDirectory       = 0x0001
	fileShareAll            = syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | 0x4 // | FILE_SHARE_DELETE
	fileFlagBackupSemantics = 0x02000000
	fileIDBothDirectoryInfo = 10
	errorNoMoreFiles        = 18

	attrDirectory    = 0x10
	attrReparsePoint = 0x400
	// Junctions, symlinks and mounted folders all carry the name-surrogate
	// bit: they point somewhere else, and following them would loop or count
	// a directory twice. OneDrive placeholders do not, so they are walked.
	reparseNameSurrogate = 0x20000000

	// FILE_ID_BOTH_DIR_INFO field offsets.
	offNext       = 0
	offAllocation = 48
	offAttributes = 56
	offNameLength = 60
	offEaSize     = 64 // holds the reparse tag when attrReparsePoint is set
	offFileID     = 96
	offName       = 104

	currentThread             = ^uintptr(1) // GetCurrentThread's pseudo-handle, -2
	threadModeBackgroundBegin = 0x00010000
	sePrivilegeEnabled        = 0x2
)

// dirReader lists directories with GetFileInformationByHandleEx, which hands
// back names, attributes and allocation sizes for a whole batch of entries in
// one call — the same no-stat-per-file property the Linux reader gets from
// fstatat, and the size on disk rather than the logical length.
type dirReader struct {
	buf     []byte // 8-byte aligned, as the records inside it require
	path16  []uint16
	names   []byte
	entries []dirEntry
}

func newDirReader() *dirReader {
	words := make([]uint64, 64*1024/8)
	return &dirReader{buf: unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)}
}

func (r *dirReader) read(path []byte, dedupe bool) ([]dirEntry, int, error) {
	h, err := syscall.CreateFile(r.longPath(path), fileListDirectory, fileShareAll, nil,
		syscall.OPEN_EXISTING, fileFlagBackupSemantics, 0)
	if err != nil {
		return nil, 0, err
	}
	defer syscall.CloseHandle(h)

	r.entries, r.names = r.entries[:0], r.names[:0]
	seen := 0
	for first := true; ; first = false {
		ok, _, e := procGetFileInformationByHandleEx.Call(uintptr(h), fileIDBothDirectoryInfo,
			uintptr(unsafe.Pointer(&r.buf[0])), uintptr(len(r.buf)))
		if ok == 0 {
			if first && e != syscall.Errno(errorNoMoreFiles) {
				return nil, 0, e // opened, but listing it was refused
			}
			break
		}
		for off := 0; ; {
			rec := r.buf[off:]
			next := u32(rec, offNext)
			nameLen := int(u32(rec, offNameLength)) / 2
			name := unsafe.Slice((*uint16)(unsafe.Pointer(&rec[offName])), nameLen)
			r.add(name, rec, dedupe, &seen)
			if next == 0 {
				break
			}
			off += int(next)
		}
	}
	return r.entries, seen, nil
}

func (r *dirReader) add(name []uint16, rec []byte, dedupe bool, seen *int) {
	if len(name) == 1 && name[0] == '.' || len(name) == 2 && name[0] == '.' && name[1] == '.' {
		return
	}
	*seen++
	attrs := u32(rec, offAttributes)
	if attrs&attrReparsePoint != 0 && u32(rec, offEaSize)&reparseNameSurrogate != 0 {
		return
	}
	var e dirEntry
	if attrs&attrDirectory != 0 {
		e.dir = true
	} else {
		e.size = *(*int64)(unsafe.Pointer(&rec[offAllocation]))
		if dedupe {
			e.key = *(*uint64)(unsafe.Pointer(&rec[offFileID]))
		}
	}
	start := len(r.names)
	r.names = appendUTF8(r.names, name)
	e.name = r.names[start:len(r.names):len(r.names)]
	r.entries = append(r.entries, e)
}

// longPath converts to UTF-16 with the \\?\ prefix, which lifts the 260
// character limit that a deep node_modules tree would otherwise hit.
func (r *dirReader) longPath(path []byte) *uint16 {
	p := r.path16[:0]
	s := string(path)
	if strings.HasPrefix(s, `\\`) {
		p = append(p, []uint16(utf16.Encode([]rune(`\\?\UNC\`)))...)
		s = s[2:]
	} else {
		p = append(p, '\\', '\\', '?', '\\')
	}
	for _, c := range s {
		p = utf16.AppendRune(p, c)
	}
	p = append(p, 0)
	r.path16 = p
	return &p[0]
}

func appendUTF8(dst []byte, name []uint16) []byte {
	for i := 0; i < len(name); i++ {
		c := rune(name[i])
		if c < utf8.RuneSelf {
			dst = append(dst, byte(c))
			continue
		}
		if utf16.IsSurrogate(c) && i+1 < len(name) {
			if pair := utf16.DecodeRune(c, rune(name[i+1])); pair != utf8.RuneError {
				c = pair
				i++
			}
		}
		dst = utf8.AppendRune(dst, c)
	}
	return dst
}

func u32(b []byte, off int) uint32 { return *(*uint32)(unsafe.Pointer(&b[off])) }

var backupOnce sync.Once

// deprioritise puts the calling thread in background mode, which lowers its
// CPU, I/O and memory priority together — Windows' equivalent of nice 19 plus
// the lowest I/O class.
//
// It also enables SeBackupPrivilege where the token holds it (an elevated
// administrator), so folders whose ACLs shut out even administrators are
// still counted. That privilege only grants reading; nothing is ever opened
// for writing.
func deprioritise() {
	procSetThreadPriority.Call(currentThread, threadModeBackgroundBegin)
	backupOnce.Do(enableBackupPrivilege)
}

func enableBackupPrivilege() {
	var token syscall.Token
	proc, _ := syscall.GetCurrentProcess()
	if syscall.OpenProcessToken(proc, syscall.TOKEN_ADJUST_PRIVILEGES|syscall.TOKEN_QUERY, &token) != nil {
		return
	}
	defer token.Close()
	var privileges struct {
		count uint32
		luid  struct {
			low  uint32
			high int32
		}
		attributes uint32
	}
	name, _ := syscall.UTF16PtrFromString("SeBackupPrivilege")
	if ok, _, _ := procLookupPrivilegeValueW.Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&privileges.luid))); ok == 0 {
		return
	}
	privileges.count, privileges.attributes = 1, sePrivilegeEnabled
	procAdjustTokenPrivileges.Call(uintptr(token), 0, uintptr(unsafe.Pointer(&privileges)), 0, 0, 0)
}

// threadCPU is the kernel+user time of the calling thread.
func threadCPU() time.Duration {
	var created, exited, kernel, user syscall.Filetime
	if ok, _, _ := procGetThreadTimes.Call(currentThread, uintptr(unsafe.Pointer(&created)),
		uintptr(unsafe.Pointer(&exited)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); ok == 0 {
		return 0
	}
	ticks := func(f syscall.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return time.Duration(ticks(kernel)+ticks(user)) * 100
}

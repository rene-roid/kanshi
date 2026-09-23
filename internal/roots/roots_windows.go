package roots

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const caseInsensitive = true

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetLogicalDrives    = kernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW       = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetVolumePathNameW  = kernel32.NewProc("GetVolumePathNameW")
)

const driveFixed = 3

// auto is the system drive, the user's profile folder, then every other fixed
// drive. The profile folder sits on the system drive; the walk notices that
// and covers both in one pass.
func auto(string) []Root {
	system := strings.TrimRight(os.Getenv("SystemDrive"), `\`)
	if system == "" {
		system = "C:"
	}
	out := []Root{{Label: system + `\`, Path: system + `\`}}
	if home, err := os.UserHomeDir(); err == nil && isDir(home) {
		out = append(out, Root{Label: home, Path: home})
	}

	mask, _, _ := procGetLogicalDrives.Call()
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		drive := string(rune('A'+i)) + `:\`
		if strings.EqualFold(drive[:2], system) {
			continue
		}
		// Fixed covers internal disks and most external USB drives. Network
		// shares, optical drives and card readers are left out: walking
		// those is slow, and they come and go.
		p, _ := syscall.UTF16PtrFromString(drive)
		if t, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(p))); t == driveFixed && isDir(drive) {
			out = append(out, Root{Label: drive, Path: drive})
		}
	}
	return out
}

// DiskUsage reads a volume's size with GetDiskFreeSpaceExW.
func DiskUsage(path string) (Usage, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return Usage{}, err
	}
	var avail, total, free uint64
	r, _, e := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return Usage{}, e
	}
	u := Usage{Total: total, Used: total - free, Free: avail, Volume: strings.ToLower(path)}
	// The profile folder and C:\ are the same volume; GetVolumePathName says so.
	buf := make([]uint16, 300)
	if r, _, _ := procGetVolumePathNameW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r != 0 {
		u.Volume = strings.ToLower(syscall.UTF16ToString(buf))
	}
	return u, nil
}

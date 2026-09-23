package vitals

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

var (
	ntdll                        = syscall.NewLazyDLL("ntdll.dll")
	procNtQuerySystemInformation = ntdll.NewProc("NtQuerySystemInformation")

	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx  = kernel32.NewProc("GlobalMemoryStatusEx")
	procK32GetPerformanceInfo = kernel32.NewProc("K32GetPerformanceInfo")
	procGetTickCount64        = kernel32.NewProc("GetTickCount64")

	iphlpapi         = syscall.NewLazyDLL("iphlpapi.dll")
	procGetIfTable2  = iphlpapi.NewProc("GetIfTable2")
	procFreeMibTable = iphlpapi.NewProc("FreeMibTable")
)

const (
	systemProcessorPerformanceInformation = 8
	statusInfoLengthMismatch              = 0xC0000004
	ioctlDiskPerformance                  = 0x70020
	ifTypeSoftwareLoopback                = 24
	devicesEvery                          = time.Minute
)

type sysState struct {
	perf            []processorPerformance
	prevCPU, curCPU []cpuTimes
	cpuAt           time.Time

	disks   []syscall.Handle
	disksAt time.Time
}

/* ── CPU ────────────────────────────────────────────────────────────────── */

// processorPerformance is SYSTEM_PROCESSOR_PERFORMANCE_INFORMATION. Kernel
// time includes idle time.
type processorPerformance struct {
	IdleTime, KernelTime, UserTime, DpcTime, InterruptTime int64
	InterruptCount                                         uint32
	_                                                      uint32
}

type cpuTimes struct{ busy, total uint64 }

func (r *Reader) readCPU(dst []cpuTimes) []cpuTimes {
	s := &r.sys
	if s.perf == nil {
		s.perf = make([]processorPerformance, 64)
	}
	for {
		var n uint32
		size := uint32(len(s.perf)) * uint32(unsafe.Sizeof(s.perf[0]))
		st, _, _ := procNtQuerySystemInformation.Call(systemProcessorPerformanceInformation,
			uintptr(unsafe.Pointer(&s.perf[0])), uintptr(size), uintptr(unsafe.Pointer(&n)))
		if st == statusInfoLengthMismatch {
			s.perf = make([]processorPerformance, 2*len(s.perf))
			continue
		}
		if st != 0 {
			return dst[:0]
		}
		dst = dst[:0]
		for _, p := range s.perf[:n/uint32(unsafe.Sizeof(s.perf[0]))] {
			total := uint64(p.KernelTime + p.UserTime)
			dst = append(dst, cpuTimes{busy: total - uint64(p.IdleTime), total: total})
		}
		return dst
	}
}

func (r *Reader) primeCPU() {
	r.sys.prevCPU = r.readCPU(r.sys.prevCPU)
	r.sys.cpuAt = time.Now()
}

// cpuCores mirrors the Linux version: a delta against the previous call, with
// a short blocking window when that call was too recent to divide by.
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
	if len(prev) == len(now) {
		for i := range now {
			out[i] = busyPercent(float64(now[i].busy)-float64(prev[i].busy),
				float64(now[i].total)-float64(prev[i].total))
		}
	}
	s.prevCPU, s.curCPU = now, prev
	s.cpuAt = time.Now()
	return out
}

/* ── memory ─────────────────────────────────────────────────────────────── */

type memoryStatusEx struct {
	Length, MemoryLoad                     uint32
	TotalPhys, AvailPhys                   uint64
	TotalPageFile, AvailPageFile           uint64
	TotalVirtual, AvailVirtual, AvailExtra uint64
}

type performanceInformation struct {
	Size                                               uint32
	CommitTotal, CommitLimit, CommitPeak               uintptr
	PhysicalTotal, PhysicalAvailable, SystemCache      uintptr
	KernelTotal, KernelPaged, KernelNonpaged, PageSize uintptr
	HandleCount, ProcessCount, ThreadCount             uint32
}

// memory reports RAM. Windows has no swap partition in the Linux sense — its
// page file backs committed memory rather than overflowing RAM — so swap is
// reported as absent and the dashboard leaves that meter out.
func (r *Reader) memory() (Memory, Swap) {
	ms := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if ok, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); ok == 0 {
		return Memory{}, Swap{}
	}
	var cached uint64
	pi := performanceInformation{Size: uint32(unsafe.Sizeof(performanceInformation{}))}
	if ok, _, _ := procK32GetPerformanceInfo.Call(uintptr(unsafe.Pointer(&pi)), uintptr(pi.Size)); ok != 0 {
		cached = uint64(pi.SystemCache) * uint64(pi.PageSize)
	}
	return memoryFigures(ms.TotalPhys, ms.AvailPhys, cached), Swap{}
}

/* ── network ────────────────────────────────────────────────────────────── */

// mibIfRow2 is MIB_IF_ROW2. Go's own alignment rules reproduce the C layout;
// a test pins the size and the offsets that are read.
type mibIfRow2 struct {
	InterfaceLuid               uint64
	InterfaceIndex              uint32
	InterfaceGUID               [16]byte
	Alias                       [257]uint16
	Description                 [257]uint16
	PhysicalAddressLength       uint32
	PhysicalAddress             [32]byte
	PermanentPhysicalAddress    [32]byte
	Mtu                         uint32
	Type                        uint32
	TunnelType                  uint32
	MediaType                   uint32
	PhysicalMediumType          uint32
	AccessType                  uint32
	DirectionType               uint32
	InterfaceAndOperStatusFlags uint8
	OperStatus                  uint32
	AdminStatus                 uint32
	MediaConnectState           uint32
	NetworkGUID                 [16]byte
	ConnectionType              uint32
	TransmitLinkSpeed           uint64
	ReceiveLinkSpeed            uint64
	InOctets                    uint64
	InUcastPkts                 uint64
	InNUcastPkts                uint64
	InDiscards                  uint64
	InErrors                    uint64
	InUnknownProtos             uint64
	InUcastOctets               uint64
	InMulticastOctets           uint64
	InBroadcastOctets           uint64
	OutOctets                   uint64
	OutUcastPkts                uint64
	OutNUcastPkts               uint64
	OutDiscards                 uint64
	OutErrors                   uint64
	OutUcastOctets              uint64
	OutMulticastOctets          uint64
	OutBroadcastOctets          uint64
	OutQLen                     uint64
}

const (
	ifHardwareInterface = 1 << 0
	ifFilterInterface   = 1 << 1
)

// netCounters sums the physical adapters only. Hyper-V switches, WSL, VPN
// tunnels and the filter drivers stacked on a NIC all report the same bytes
// again, the same double counting the Linux side avoids via sysfs.
func (r *Reader) netCounters() (rx, tx uint64, ok bool) {
	// The table lives in memory Windows allocated, outside the Go heap, and
	// is handed back with FreeMibTable.
	var table unsafe.Pointer
	if st, _, _ := procGetIfTable2.Call(uintptr(unsafe.Pointer(&table))); st != 0 || table == nil {
		return 0, 0, false
	}
	defer procFreeMibTable.Call(uintptr(table))
	n := *(*uint32)(table)
	rows := unsafe.Slice((*mibIfRow2)(unsafe.Add(table, 8)), n)
	for i := range rows {
		row := &rows[i]
		flags := row.InterfaceAndOperStatusFlags
		if flags&ifHardwareInterface == 0 || flags&ifFilterInterface != 0 || row.Type == ifTypeSoftwareLoopback {
			continue
		}
		rx += row.InOctets
		tx += row.OutOctets
	}
	return rx, tx, true
}

/* ── disk ───────────────────────────────────────────────────────────────── */

type diskPerformance struct {
	BytesRead, BytesWritten                       int64
	ReadTime, WriteTime, IdleTime                 int64
	ReadCount, WriteCount, QueueDepth, SplitCount uint32
	QueryTime                                     int64
	StorageDeviceNumber                           uint32
	StorageManagerName                            [8]uint16
}

// diskCounters sums IOCTL_DISK_PERFORMANCE over every physical drive. Opening
// a drive with no access rights is enough for this call, so it needs no
// administrator token.
func (r *Reader) diskCounters() (read, write uint64, ok bool) {
	s := &r.sys
	if s.disksAt.IsZero() || time.Since(s.disksAt) > devicesEvery {
		s.reopenDisks()
	}
	for _, h := range s.disks {
		var perf diskPerformance
		var n uint32
		if err := syscall.DeviceIoControl(h, ioctlDiskPerformance, nil, 0,
			(*byte)(unsafe.Pointer(&perf)), uint32(unsafe.Sizeof(perf)), &n, nil); err != nil {
			continue
		}
		read += uint64(perf.BytesRead)
		write += uint64(perf.BytesWritten)
		ok = true
	}
	return read, write, ok
}

func (s *sysState) reopenDisks() {
	for _, h := range s.disks {
		syscall.CloseHandle(h)
	}
	s.disks = s.disks[:0]
	s.disksAt = time.Now()
	// Drive numbers can have gaps after a disk is removed, so every slot is
	// tried rather than stopping at the first miss.
	for i := 0; i < 32; i++ {
		p, _ := syscall.UTF16PtrFromString(fmt.Sprintf(`\\.\PhysicalDrive%d`, i))
		h, err := syscall.CreateFile(p, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil,
			syscall.OPEN_EXISTING, 0, 0)
		if err == nil {
			s.disks = append(s.disks, h)
		}
	}
}

/* ── load, temperature, uptime ──────────────────────────────────────────── */

// Windows keeps no load average, and its only CPU temperature source (the
// ACPI thermal zone over WMI) needs an administrator and is wrong on most
// desktop boards. Both are left null; the dashboard hides them.
func (r *Reader) loadAvg() *[3]float64  { return nil }
func (r *Reader) temperature() *float64 { return nil }

func (r *Reader) uptime() float64 {
	ms, _, _ := procGetTickCount64.Call()
	return float64(ms) / 1000
}

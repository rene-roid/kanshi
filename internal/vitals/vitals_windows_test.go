package vitals

import (
	"testing"
	"unsafe"
)

// The layouts below are read straight out of memory Windows filled in, so a
// wrong offset would silently report garbage rather than fail.
func TestStructLayouts(t *testing.T) {
	var row mibIfRow2
	if got := unsafe.Sizeof(row); got != 1352 {
		t.Errorf("sizeof(MIB_IF_ROW2) = %d, want 1352", got)
	}
	if got := unsafe.Offsetof(row.InterfaceAndOperStatusFlags); got != 1152 {
		t.Errorf("InterfaceAndOperStatusFlags at %d, want 1152", got)
	}
	if got := unsafe.Offsetof(row.InOctets); got != 1208 {
		t.Errorf("InOctets at %d, want 1208", got)
	}
	if got := unsafe.Offsetof(row.OutOctets); got != 1280 {
		t.Errorf("OutOctets at %d, want 1280", got)
	}
	if got := unsafe.Sizeof(diskPerformance{}); got != 88 {
		t.Errorf("sizeof(DISK_PERFORMANCE) = %d, want 88", got)
	}
	if got := unsafe.Sizeof(processorPerformance{}); got != 48 {
		t.Errorf("sizeof(SYSTEM_PROCESSOR_PERFORMANCE_INFORMATION) = %d, want 48", got)
	}
	if got := unsafe.Sizeof(memoryStatusEx{}); got != 64 {
		t.Errorf("sizeof(MEMORYSTATUSEX) = %d, want 64", got)
	}
}

func TestLoadAndTemperatureAreAbsent(t *testing.T) {
	r := &Reader{}
	if r.loadAvg() != nil || r.temperature() != nil {
		t.Error("Windows has neither; they must be null so the page hides them")
	}
}

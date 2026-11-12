//go:build windows

package bench

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetSystemTimes = kernel32.NewProc("GetSystemTimes")

// cpuTimes: GetSystemTimes, whose kernel time includes the idle time.
func cpuTimes() (busy, total uint64, ok bool) {
	var i, k, u windows.Filetime
	if r, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u))); r == 0 {
		return 0, 0, false
	}
	ft := func(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	return ft(k) + ft(u) - ft(i), ft(k) + ft(u), true
}

func pressure() (cpu, mem *float64) { return nil, nil }

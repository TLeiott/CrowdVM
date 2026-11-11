//go:build windows

package bench

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/crowdvm/crowdvm/internal/proto"
)

var (
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx    = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetLogicalProcessorInfo = kernel32.NewProc("GetLogicalProcessorInformation")
	winHvPlatform               = windows.NewLazySystemDLL("WinHvPlatform.dll")
	procWHvGetCapability        = winHvPlatform.NewProc("WHvGetCapability")
)

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

func cpuInfo(context.Context) (model string, cores int) {
	model = "unknown"
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		if s, _, err := k.GetStringValue("ProcessorNameString"); err == nil {
			model = strings.TrimSpace(s)
		}
		k.Close()
	}
	// SYSTEM_LOGICAL_PROCESSOR_INFORMATION is 32 bytes on 64-bit Windows:
	// ProcessorMask (8) | Relationship (4 + 4 pad) | union (16).
	const entry = 32
	var n uint32
	procGetLogicalProcessorInfo.Call(0, uintptr(unsafe.Pointer(&n)))
	if n == 0 || unsafe.Sizeof(uintptr(0)) != 8 {
		return model, 0
	}
	buf := make([]byte, n)
	if r, _, _ := procGetLogicalProcessorInfo.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return model, 0
	}
	for off := 0; off+entry <= int(n); off += entry {
		if *(*uint32)(unsafe.Pointer(&buf[off+8])) == 0 { // RelationProcessorCore
			cores++
		}
	}
	return model, cores
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func memInfo() (total, avail uint64) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0, 0
	}
	return m.TotalPhys, m.AvailPhys
}

func diskFree(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}

func openUncached(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_NO_BUFFERING|windows.FILE_FLAG_SEQUENTIAL_SCAN, 0)
	if err != nil {
		return os.Open(path)
	}
	return os.NewFile(uintptr(h), path), nil
}

func nvidiaSMIPath() string {
	if p, err := exec.LookPath("nvidia-smi"); err == nil {
		return p
	}
	p := filepath.Join(os.Getenv("SystemRoot"), "System32", "nvidia-smi.exe")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

func gpus(ctx context.Context) []proto.GPUInfo {
	var out []proto.GPUInfo
	if p := nvidiaSMIPath(); p != "" {
		out = nvidiaSMI(ctx, p)
	}
	have := map[string]int{}
	for _, g := range out {
		have[g.Vendor]++
	}
	s, err := run(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		"Get-CimInstance Win32_VideoController | Select-Object Name,AdapterRAM,DriverVersion | ConvertTo-Json -Compress")
	if err != nil || s == "" {
		return out
	}
	type vc struct {
		Name          string
		AdapterRAM    int64
		DriverVersion string
	}
	var list []vc
	if strings.HasPrefix(s, "{") {
		var one vc
		if json.Unmarshal([]byte(s), &one) == nil {
			list = []vc{one}
		}
	} else {
		_ = json.Unmarshal([]byte(s), &list)
	}
	for _, v := range list {
		n := strings.ToLower(v.Name)
		if strings.Contains(n, "basic display") || strings.Contains(n, "remote display") || strings.Contains(n, "virtual") {
			continue
		}
		vendor := vendorFromName(v.Name)
		if have[vendor] > 0 {
			continue
		}
		// AdapterRAM is a uint32 in WMI: capped at 4 GiB.
		out = append(out, proto.GPUInfo{Vendor: vendor, Model: v.Name, VRAMMiB: int(v.AdapterRAM >> 20), Driver: v.DriverVersion})
	}
	return out
}

func qemuBin() string {
	if p, err := exec.LookPath("qemu-system-x86_64"); err == nil {
		return p
	}
	for _, env := range []string{"ProgramFiles", "ProgramW6432"} {
		p := filepath.Join(os.Getenv(env), "qemu", "qemu-system-x86_64.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// virt: WHPX is usable when WinHvPlatform.dll exists (Windows Hypervisor
// Platform feature enabled) and reports the hypervisor as present.
func virt(ctx context.Context) proto.VirtInfo {
	v := proto.VirtInfo{Accel: "none"}
	if b := qemuBin(); b != "" {
		v.QEMU = qemuVersion(ctx, b)
	}
	if procWHvGetCapability.Find() == nil {
		var present, written uint32
		hr, _, _ := procWHvGetCapability.Call(0 /* WHvCapabilityCodeHypervisorPresent */, uintptr(unsafe.Pointer(&present)), 4, uintptr(unsafe.Pointer(&written)))
		if hr == 0 && present != 0 {
			v.Accel = "whpx"
			return v
		}
	}
	if v.QEMU != "" {
		v.Accel = "tcg"
	}
	return v
}

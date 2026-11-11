//go:build linux

package bench

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func cpuInfo(context.Context) (model string, cores int) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "unknown", 0
	}
	defer f.Close()
	type key struct{ phys, core string }
	seen := map[key]bool{}
	var cur key
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "model name", "Model", "cpu model":
			if model == "" {
				model = v
			}
		case "physical id":
			cur.phys = v
		case "core id":
			cur.core = v
			seen[cur] = true
		}
	}
	if model == "" {
		model = "unknown"
	}
	return model, len(seen)
}

// memInfo reads MemTotal / MemAvailable (bytes).
func memInfo() (total, avail uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fs[1], 10, 64)
		switch fs[0] {
		case "MemTotal:":
			total = v << 10
		case "MemAvailable:":
			avail = v << 10
		}
	}
	return
}

func diskFree(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

func openUncached(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
	return f, nil
}

func gpus(ctx context.Context) []proto.GPUInfo {
	out := nvidiaSMI(ctx, "nvidia-smi")
	out = append(out, rocmSMI(ctx)...)
	have := map[string]int{}
	for _, g := range out {
		have[g.Vendor]++
	}
	// PCI devices from sysfs for vendors the tools didn't cover.
	for _, g := range drmGPUs(ctx) {
		if have[g.Vendor] == 0 {
			out = append(out, g)
		}
	}
	return out
}

// drmGPUs lists /sys/class/drm/cardN devices (deduplicated by PCI slot) and
// names them via lspci when available.
func drmGPUs(ctx context.Context) []proto.GPUInfo {
	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]*")
	seen := map[string]bool{}
	var out []proto.GPUInfo
	for _, c := range cards {
		if strings.Contains(filepath.Base(c), "-") { // connectors (card1-DP-1)
			continue
		}
		dev, err := filepath.EvalSymlinks(filepath.Join(c, "device"))
		if err != nil {
			continue
		}
		slot := filepath.Base(dev)
		vendor := readTrim(filepath.Join(dev, "vendor"))
		if vendor == "" || seen[slot] {
			continue
		}
		seen[slot] = true
		g := proto.GPUInfo{Vendor: pciVendor(vendor)}
		if vram, err := strconv.ParseInt(readTrim(filepath.Join(dev, "mem_info_vram_total")), 10, 64); err == nil {
			g.VRAMMiB = int(vram >> 20)
		}
		if drv, err := os.Readlink(filepath.Join(dev, "driver")); err == nil {
			g.Driver = filepath.Base(drv)
		}
		g.Model = lspciName(ctx, slot)
		if g.Model == "" {
			g.Model = "PCI device " + readTrim(filepath.Join(dev, "device"))
		}
		out = append(out, g)
	}
	return out
}

func lspciName(ctx context.Context, slot string) string {
	s, err := run(ctx, "lspci", "-mm", "-s", slot)
	if err != nil {
		return ""
	}
	// 01:00.0 "VGA compatible controller" "NVIDIA Corporation" "GA102 [GeForce RTX 3080]" ...
	parts := strings.Split(s, "\"")
	if len(parts) >= 6 {
		return strings.TrimSpace(parts[5])
	}
	return ""
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func virt(ctx context.Context) proto.VirtInfo {
	v := proto.VirtInfo{Accel: "none", QEMU: qemuVersion(ctx, "qemu-system-x86_64")}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		f.Close()
		v.Accel = "kvm"
		for _, m := range []string{"kvm_intel", "kvm_amd"} {
			switch readTrim("/sys/module/" + m + "/parameters/nested") {
			case "Y", "y", "1":
				v.Nested = true
			}
		}
	} else if v.QEMU != "" {
		v.Accel = "tcg"
	}
	return v
}

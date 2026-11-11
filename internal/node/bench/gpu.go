package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// GPUs lists the machine's GPUs (best effort; tools that are missing are skipped).
func GPUs(ctx context.Context) []proto.GPUInfo {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	g := gpus(ctx)
	if g == nil {
		g = []proto.GPUInfo{}
	}
	return g
}

// Virt reports hardware acceleration, nested virtualization and the QEMU version.
func Virt(ctx context.Context) proto.VirtInfo {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return virt(ctx)
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func nvidiaSMI(ctx context.Context, bin string) []proto.GPUInfo {
	s, err := run(ctx, bin, "--query-gpu=name,memory.total,driver_version,compute_cap", "--format=csv,noheader,nounits")
	if err != nil && s == "" {
		// Older drivers don't know compute_cap.
		s, err = run(ctx, bin, "--query-gpu=name,memory.total,driver_version", "--format=csv,noheader,nounits")
		if err != nil {
			return nil
		}
	}
	var out []proto.GPUInfo
	for _, line := range strings.Split(s, "\n") {
		f := strings.Split(line, ",")
		if len(f) < 3 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		vram, _ := strconv.Atoi(f[1])
		g := proto.GPUInfo{Vendor: "nvidia", Model: f[0], VRAMMiB: vram, Driver: f[2]}
		if len(f) > 3 && f[3] != "[N/A]" {
			g.Compute = f[3]
		}
		out = append(out, g)
	}
	return out
}

// rocmSMI parses `rocm-smi --showproductname --showmeminfo vram --json`.
func rocmSMI(ctx context.Context) []proto.GPUInfo {
	s, err := run(ctx, "rocm-smi", "--showproductname", "--showmeminfo", "vram", "--showdriverversion", "--json")
	if err != nil && s == "" {
		return nil
	}
	var m map[string]map[string]string
	if json.Unmarshal([]byte(s), &m) != nil {
		return nil
	}
	var out []proto.GPUInfo
	for card, kv := range m {
		if !strings.HasPrefix(card, "card") {
			continue
		}
		g := proto.GPUInfo{Vendor: "amd", Model: kv["Card series"], Compute: kv["GFX Version"]}
		if g.Model == "" {
			g.Model = kv["Card model"]
		}
		if v, err := strconv.ParseInt(kv["VRAM Total Memory (B)"], 10, 64); err == nil {
			g.VRAMMiB = int(v >> 20)
		}
		g.Driver = m["system"]["Driver version"]
		out = append(out, g)
	}
	return out
}

func pciVendor(id string) string {
	switch strings.ToLower(strings.TrimPrefix(id, "0x")) {
	case "10de":
		return "nvidia"
	case "1002", "1022":
		return "amd"
	case "8086":
		return "intel"
	case "1af4", "1234", "15ad":
		return "virtual"
	}
	return "other"
}

func vendorFromName(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "nvidia"), strings.Contains(n, "geforce"), strings.Contains(n, "quadro"):
		return "nvidia"
	case strings.Contains(n, "amd"), strings.Contains(n, "radeon"):
		return "amd"
	case strings.Contains(n, "intel"):
		return "intel"
	}
	return "other"
}

func qemuVersion(ctx context.Context, bin string) string {
	s, err := run(ctx, bin, "--version")
	if err != nil {
		return ""
	}
	// "QEMU emulator version 9.2.0 (...)"
	line, _, _ := strings.Cut(s, "\n")
	if _, v, ok := strings.Cut(line, "version "); ok {
		v, _, _ = strings.Cut(v, " ")
		return v
	}
	return line
}

package bench

import (
	"math"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Category thresholds (lower bound of each tier; below "medium" is "low").
// CPU uses the multi-thread score in work units/s (see cpu.go; a Ryzen 7 5700X
// does ~3,400 single / ~43,000 multi, a 4-core laptop ~12,000 multi). GPU uses VRAM. Disk
// uses sequential write. Network uses min(down, up).
//
//	dimension  unit          medium   high     extreme
//	cpu        units/s       8,000    20,000   60,000
//	memory     total GiB     8        32       128
//	disk       write MB/s    300      1,000    3,000      (and "low" if < 50 GiB free)
//	network    Mbps          100      1,000    5,000      ("none" when not measured)
//	gpu        VRAM GiB      4        8        20         ("none" without a GPU, "low" below 4)
//
// Overall Score (0..1000): weighted mean of per-dimension sub-scores, each
// log-scaled between a floor and a ceiling (floor→0, ceiling→1000):
//
//	cpu 35% (1,000..120,000 units/s)   memory 20% (2..256 GiB)
//	disk 20% (50..5,000 MB/s)         network 15% (10..10,000 Mbps)
//	gpu 10% (0 without GPU, 1..48 GiB VRAM)
//
// multiplied by the virtualization factor: kvm/whpx 1.0, tcg 0.5, none 0.25
// (an unmeasured network counts as 500 Mbps).
//
// Tier: score < 250 bronze, < 500 silver, < 750 gold, else platinum.
// Without hardware acceleration (kvm/whpx) the tier is capped at bronze.
var Thresholds = map[string][3]float64{
	"cpu":     {8000, 20000, 60000},
	"memory":  {8, 32, 128},
	"disk":    {300, 1000, 3000},
	"network": {100, 1000, 5000},
	"gpu":     {4, 8, 20},
}

// Score fills b.Categories, b.Score and b.Tier from the measurements.
func Score(b *proto.Benchmark) {
	memGiB := float64(b.Memory.TotalMiB) / 1024
	netMbps := math.Min(b.Network.DownMbps, b.Network.UpMbps)
	var vramGiB float64
	for _, g := range b.GPUs {
		vramGiB = math.Max(vramGiB, float64(g.VRAMMiB)/1024)
	}

	b.Categories = map[string]string{
		"cpu":     category("cpu", b.CPU.MultiScore),
		"memory":  category("memory", memGiB),
		"disk":    category("disk", b.Disk.WriteMBps),
		"network": category("network", netMbps),
		"gpu":     category("gpu", vramGiB),
	}
	if b.Disk.FreeGiB < 50 {
		b.Categories["disk"] = "low"
	}
	if netMbps <= 0 {
		b.Categories["network"] = "none"
		netMbps = 500
	}
	if len(b.GPUs) == 0 {
		b.Categories["gpu"] = "none"
	}

	gpuSub := 0.0
	if len(b.GPUs) > 0 {
		gpuSub = sub(math.Max(vramGiB, 1), 1, 48)
	}
	s := 0.35*sub(b.CPU.MultiScore, 1000, 120000) +
		0.20*sub(memGiB, 2, 256) +
		0.20*sub(b.Disk.WriteMBps, 50, 5000) +
		0.15*sub(netMbps, 10, 10000) +
		0.10*gpuSub
	switch b.Virtualization.Accel {
	case "kvm", "whpx", "hvf":
	case "tcg":
		s *= 0.5
	default:
		s *= 0.25
	}
	b.Score = int(math.Round(s))
	switch {
	case b.Score < 250 || (b.Virtualization.Accel != "kvm" && b.Virtualization.Accel != "whpx" && b.Virtualization.Accel != "hvf"):
		b.Tier = "bronze"
	case b.Score < 500:
		b.Tier = "silver"
	case b.Score < 750:
		b.Tier = "gold"
	default:
		b.Tier = "platinum"
	}
}

func category(dim string, v float64) string {
	t := Thresholds[dim]
	switch {
	case v >= t[2]:
		return "extreme"
	case v >= t[1]:
		return "high"
	case v >= t[0]:
		return "medium"
	}
	return "low"
}

// sub log-scales v between lo (0) and hi (1000).
func sub(v, lo, hi float64) float64 {
	if v <= lo {
		return 0
	}
	if v >= hi {
		return 1000
	}
	return 1000 * math.Log(v/lo) / math.Log(hi/lo)
}

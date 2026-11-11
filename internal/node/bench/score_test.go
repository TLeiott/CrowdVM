package bench

import (
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestScore(t *testing.T) {
	low := proto.Benchmark{CPU: proto.CPUBench{MultiScore: 3000}, Memory: proto.MemoryBench{TotalMiB: 4096},
		Disk: proto.DiskBench{WriteMBps: 100, FreeGiB: 20}, Virtualization: proto.VirtInfo{Accel: "tcg"}}
	Score(&low)
	if low.Tier != "bronze" || low.Categories["gpu"] != "none" || low.Categories["network"] != "none" || low.Categories["cpu"] != "low" {
		t.Fatalf("low: %+v", low)
	}
	hi := proto.Benchmark{CPU: proto.CPUBench{MultiScore: 100000}, Memory: proto.MemoryBench{TotalMiB: 256 << 10},
		Disk: proto.DiskBench{WriteMBps: 6000, FreeGiB: 2000}, Network: proto.NetworkBench{DownMbps: 9000, UpMbps: 9000},
		GPUs: []proto.GPUInfo{{VRAMMiB: 49152}}, Virtualization: proto.VirtInfo{Accel: "kvm"}}
	Score(&hi)
	if hi.Tier != "platinum" || hi.Score < 900 {
		t.Fatalf("hi: %+v", hi)
	}
	for _, c := range hi.Categories {
		if c != "extreme" {
			t.Fatalf("hi categories %v", hi.Categories)
		}
	}
	// Same machine without acceleration is capped at bronze.
	hi.Virtualization.Accel = "none"
	Score(&hi)
	if hi.Tier != "bronze" {
		t.Fatalf("no accel tier %s", hi.Tier)
	}
}

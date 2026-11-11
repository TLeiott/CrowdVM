// Package bench measures a node quickly (< ~20 s, quick mode < 5 s) and
// categorizes it: CPU, memory, disk, network (to the server), GPUs and
// virtualization support. Workloads are pure Go; hardware details come from
// /proc, /sys and vendor tools on Linux and from Win32 APIs / PowerShell on
// Windows. See score.go for the category thresholds and the overall score.
package bench

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Options controls a benchmark run.
type Options struct {
	Quick bool
	// Dir is where the disk test writes its temp file (the node data dir).
	Dir string
	// ServerURL + Client enable the network test against
	// {ServerURL}/node/v1/bench/{download,upload}. Client may be a mux client
	// (any host) with ServerURL "http://server"; HostHint is then the real
	// server address used to classify the path (lan/tailscale/loopback).
	ServerURL string
	Client    *http.Client
	Header    http.Header // e.g. Authorization for plain-HTTP runs
	HostHint  string
	// Log receives progress lines (optional).
	Log func(format string, args ...any)
}

// Run executes every test and returns a filled proto.Benchmark. Individual
// test failures are logged and leave zero values; Run itself does not fail.
func Run(ctx context.Context, o Options) proto.Benchmark {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if o.Dir == "" {
		o.Dir = os.TempDir()
	}
	start := time.Now()
	b := proto.Benchmark{RanAt: start.UTC()}

	d := durations(o.Quick)

	b.CPU = CPU(ctx, d.cpu)
	logf("cpu: %s, %d cores / %d threads, single %.0f, multi %.0f", b.CPU.Model, b.CPU.Cores, b.CPU.Threads, b.CPU.SingleScore, b.CPU.MultiScore)

	b.Memory = Memory(ctx, d.mem)
	logf("memory: %d MiB total, %d MiB available, %.0f MB/s", b.Memory.TotalMiB, b.Memory.AvailableMiB, b.Memory.BandwidthMBps)

	var err error
	b.Disk, err = Disk(ctx, o.Dir, d.diskBytes)
	if err != nil {
		logf("disk: %v", err)
	}
	logf("disk: %.1f GiB free, write %.0f MB/s, read %.0f MB/s", b.Disk.FreeGiB, b.Disk.WriteMBps, b.Disk.ReadMBps)

	if o.Client != nil && o.ServerURL != "" {
		b.Network, err = Network(ctx, o.Client, o.ServerURL, o.Header, o.HostHint, d.net)
		if err != nil {
			logf("network: %v", err)
		}
		logf("network (%s): %.2f ms, down %.0f Mbps, up %.0f Mbps", b.Network.Via, b.Network.LatencyMs, b.Network.DownMbps, b.Network.UpMbps)
	} else {
		logf("network: skipped (not connected to a server)")
	}
	b.Network.Internet, b.Network.InternetLatencyMs = Internet(ctx, InternetTargets, 2*time.Second)
	logf("internet: %s, %.1f ms", b.Network.Internet, b.Network.InternetLatencyMs)

	b.GPUs = GPUs(ctx)
	for _, g := range b.GPUs {
		logf("gpu: %s %s, %d MiB VRAM, driver %s, compute %s", g.Vendor, g.Model, g.VRAMMiB, g.Driver, g.Compute)
	}
	b.Virtualization = Virt(ctx)
	logf("virtualization: accel %s, nested %v, qemu %q", b.Virtualization.Accel, b.Virtualization.Nested, b.Virtualization.QEMU)

	b.DurationMs = time.Since(start).Milliseconds()
	Score(&b)
	return b
}

type phaseDurations struct {
	cpu, mem, net time.Duration
	diskBytes     int64
}

func durations(quick bool) phaseDurations {
	if quick {
		return phaseDurations{cpu: 300 * time.Millisecond, mem: 300 * time.Millisecond, net: 500 * time.Millisecond, diskBytes: 64 << 20}
	}
	return phaseDurations{cpu: 1500 * time.Millisecond, mem: 1000 * time.Millisecond, net: 2 * time.Second, diskBytes: 512 << 20}
}

// threads returns the logical CPU count.
func threads() int { return runtime.NumCPU() }

// MemInfo returns total and available physical memory in bytes.
func MemInfo() (total, avail uint64) { return memInfo() }

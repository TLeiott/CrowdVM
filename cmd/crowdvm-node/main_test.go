package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/node"
	"github.com/crowdvm/crowdvm/internal/node/bench"
	"github.com/crowdvm/crowdvm/internal/proto"
)

func run(t *testing.T, args ...string) error {
	t.Helper()
	c := rootCmd()
	c.SetArgs(args)
	c.SetOut(new(bytes.Buffer))
	return c.Execute()
}

func TestConfigSetGet(t *testing.T) {
	dir := t.TempDir()
	for _, kv := range [][2]string{{"cpus", "3"}, {"memory", "6G"}, {"direct-host", "100.64.1.2"}, {"ui-addr", "off"}} {
		if err := run(t, "--data-dir", dir, "config", "set", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := node.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cp := cfg.Capacity(); cp.VCPU != 3 || cp.MemoryMiB != 6144 || cfg.DirectHost != "100.64.1.2" {
		t.Fatalf("config not saved: %+v %+v", cfg, cp)
	}
	if err := run(t, "--data-dir", dir, "config", "set", "bogus", "1"); err == nil {
		t.Fatal("expected error for unknown key")
	}
	if err := run(t, "--data-dir", dir, "config", "get"); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownDriver(t *testing.T) {
	cfg := &node.Config{Driver: "nope", DataDir: t.TempDir()}
	if _, err := newDriver(cfg, nil); err == nil || !strings.Contains(err.Error(), "qemu") {
		t.Fatalf("err = %v", err)
	}
}

func TestBenchLocalQuick(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the quick benchmark")
	}
	dir := t.TempDir()
	if err := run(t, "--data-dir", dir, "config", "set", "ui-addr", "off"); err != nil {
		t.Fatal(err)
	}
	if err := run(t, "--data-dir", dir, "bench", "--quick", "--json", "--local"); err != nil {
		t.Fatal(err)
	}
}

func TestPrintBench(t *testing.T) {
	b := proto.Benchmark{CPU: proto.CPUBench{Model: "X", Cores: 4, Threads: 8, SingleScore: 3000, MultiScore: 20000},
		Memory: proto.MemoryBench{TotalMiB: 16384}, Disk: proto.DiskBench{FreeGiB: 100, WriteMBps: 1500},
		GPUs: []proto.GPUInfo{{Vendor: "nvidia", Model: "RTX", VRAMMiB: 8192}}, Virtualization: proto.VirtInfo{Accel: "kvm"}}
	bench.Score(&b)
	var buf bytes.Buffer
	printBench(&buf, b)
	out := buf.String()
	for _, want := range []string{"RTX", "not measured", "tier", "kvm"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestR3StatusReadableMaps(t *testing.T) {
	var out bytes.Buffer
	s := node.Status{Benchmark: proto.Benchmark{Tier: "gold", Categories: map[string]string{"cpu": "high", "disk": "medium"}}}
	printStatus(&out, s, true, "")
	if strings.Contains(out.String(), "map[") || !strings.Contains(out.String(), "cpu=high, disk=medium") {
		t.Fatal(out.String())
	}
	if readableMap(nil) != "-" {
		t.Fatal("empty direct endpoints must be readable")
	}
}

// The Update line says why the daemon does not install a pending update.
func TestUpdateLineAutoUpdate(t *testing.T) {
	u := &proto.NodeUpdate{State: "outdated", Latest: "v1.3.0", Message: "Update by 14:00.", Command: "crowdvm-node update"}
	s := node.Status{Update: u, AutoUpdate: &node.AutoUpdateStatus{Mode: "required", Unavailable: "not running as a service"}}
	if l := updateLine(s); l != "OUTDATED: Update by 14:00.; auto-update unavailable: not running as a service; run crowdvm-node update" {
		t.Fatal(l)
	}
	s.AutoUpdate.Unavailable = ""
	if l := updateLine(s); strings.Contains(l, "auto-update") {
		t.Fatal(l)
	}
}

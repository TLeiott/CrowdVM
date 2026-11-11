package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// bootloop (opt-in stress): CROWDVM_E2E_BOOTLOOP=N boots N VMs as
// create → live fork → stop → resume on the other node (lazy chain) → stop
// → fork of the stopped sandbox (two workers), or with
// CROWDVM_E2E_BOOTLOOP_MODE=create only creates (four workers); at most 4
// VMs at a time (2 per node). Every boot must become ready. For a failed
// boot the VM's serial console, qemu log, vm.json and disk layers above the
// base image are kept under <CROWDVM_E2E_DIR>/bootloop-failed/<run>/<sandbox>.
//
//	CROWDVM_E2E=1 CROWDVM_E2E_BOOTLOOP=100 go test -count=1 -timeout 90m -run 'TestE2E/bootloop' ./test/e2e/
func testBootLoop(t *testing.T, c *cluster) {
	total, _ := strconv.Atoi(os.Getenv("CROWDVM_E2E_BOOTLOOP"))
	if total <= 0 {
		t.Skip("set CROWDVM_E2E_BOOTLOOP=N to boot N VMs")
	}
	keep := filepath.Join(runsRoot(), "bootloop-failed", filepath.Base(c.root))
	var mu sync.Mutex
	var boots []bootRec
	record := func(b bootRec) {
		mu.Lock()
		boots = append(boots, b)
		n := len(boots)
		mu.Unlock()
		if !b.ok {
			dst := filepath.Join(keep, b.id)
			c.keepFailedBoot(t, b.node, b.id, dst)
			t.Errorf("boot %d: %s %s on %s failed after %s (kept in %s): %s", n, b.kind, b.id, b.node.name, b.dur.Round(time.Second), dst, b.err)
		} else {
			t.Logf("boot %d: %s %s on %s ready in %s", n, b.kind, b.id, b.node.name, b.dur.Round(100*time.Millisecond))
		}
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(boots) }
	creates := os.Getenv("CROWDVM_E2E_BOOTLOOP_MODE") == "create"
	workers := 2
	if creates {
		workers = 4
	}
	var wg sync.WaitGroup
	for w := range workers {
		home, other := c.nodes[w%2], c.nodes[1-w%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; count() < total; i++ {
				name := fmt.Sprintf("bl%d-%d", w, i)
				if creates {
					if id, _ := c.bootOnce(t, "create", home, record, c.createArgs(home, name)...); id != "" {
						c.deleteSandbox(t, id)
					}
				} else {
					c.bootCycle(t, home, other, name, func() bool { return count() >= total }, record)
				}
			}
		}()
	}
	wg.Wait()

	failed := 0
	for _, b := range boots {
		if !b.ok {
			failed++
		}
	}
	// Launch → healthy, from the node logs.
	re := regexp.MustCompile(`guest healthy ([0-9.]+)s after launch`)
	var secs []float64
	for _, n := range c.nodes {
		b, _ := os.ReadFile(filepath.Join(c.logs, n.name+".log"))
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if f, err := strconv.ParseFloat(m[1], 64); err == nil {
				secs = append(secs, f)
			}
		}
	}
	sort.Float64s(secs)
	pct := func(p float64) float64 {
		if len(secs) == 0 {
			return 0
		}
		return secs[min(len(secs)-1, int(p*float64(len(secs))))]
	}
	t.Logf("bootloop: %d boots, %d failed; launch→healthy (%d samples) p50 %.1fs p95 %.1fs max %.1fs",
		len(boots), failed, len(secs), pct(0.5), pct(0.95), pct(1))
}

type bootRec struct {
	kind string
	id   string
	node *node
	ok   bool
	dur  time.Duration
	err  string
}

// bootOnce runs one booting CLI command (new, fork, resume) and records
// whether the sandbox became ready. It returns the sandbox id.
func (c *cluster) bootOnce(t *testing.T, kind string, on *node, record func(bootRec), args ...string) (string, bool) {
	t0 := time.Now()
	r := c.cliT(t, 8*time.Minute, args...)
	id := gets(r.event("created"), "id")
	if id == "" && kind == "resume" {
		id = args[1]
	}
	if id == "" {
		id = gets(r.last(), "id")
	}
	if id == "" {
		t.Errorf("%s: no sandbox id: %v", kind, r)
		return "", false
	}
	b := bootRec{kind: kind, id: id, node: on, ok: r.code == 0, dur: time.Since(t0)}
	if !b.ok {
		b.err = tail(r.out+r.err, 600)
	}
	record(b)
	return id, b.ok
}

// bootCycle runs one create → fork → stop → resume (other node) → stop →
// fork (home) cycle and deletes what it created.
func (c *cluster) bootCycle(t *testing.T, home, other *node, name string, done func() bool, record func(bootRec)) {
	boot := func(kind string, on *node, args ...string) (string, bool) {
		return c.bootOnce(t, kind, on, record, args...)
	}
	del := func(id string) {
		if id != "" {
			c.deleteSandbox(t, id)
		}
	}
	a, ok := boot("create", home, c.createArgs(home, name)...)
	defer func() { del(a) }()
	if !ok || done() {
		return
	}
	f, ok := boot("fork", home, "fork", a, "--wait", "--json", "--name", name+"-f")
	del(f)
	if !ok || done() {
		return
	}
	if r := c.cliT(t, 6*time.Minute, "stop", a, "--wait"); r.code != 0 {
		t.Errorf("stop %s: %v", a, r)
		return
	}
	if _, ok = boot("resume", other, "resume", a, "--json", "--node", other.name); !ok || done() {
		return
	}
	if r := c.cliT(t, 6*time.Minute, "stop", a, "--wait"); r.code != 0 {
		t.Errorf("stop %s: %v", a, r)
		return
	}
	g, _ := boot("fork-stopped", home, "fork", a, "--wait", "--json", "--node", home.name, "--name", name+"-g")
	del(g)
}

// createArgs: a small plain create, and every few names one like the e2e
// create group (default size, env, a setup script).
func (c *cluster) createArgs(home *node, name string) []string {
	args := []string{"new", "--json", "--type", "small", "--name", name, "--node", home.name, "--ttl", "3600"}
	if strings.HasSuffix(name, "1") || strings.HasSuffix(name, "3") || strings.HasSuffix(name, "7") {
		setup := filepath.Join(c.root, "bootloop-setup.sh")
		_ = os.WriteFile(setup, []byte("#!/bin/bash\necho setup > /home/user/setup-done\n"), 0o755)
		args = append(args, "--type", "default", "-e", "FOO=bar baz", "--setup-file", setup)
	}
	return args
}

// keepFailedBoot copies what explains a failed boot: the serial console,
// qemu.log, vm.json and the disk chain above the base image (qemu-img info
// in chain.json), plus the node log.
func (c *cluster) keepFailedBoot(t *testing.T, n *node, id, dst string) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Logf("keep %s: %v", id, err)
		return
	}
	_ = exec.Command("cp", filepath.Join(c.logs, n.name+".log"), dst).Run()
	dirs, _ := filepath.Glob(filepath.Join(n.dir, "*", "vms", id))
	d2, _ := filepath.Glob(filepath.Join(n.dir, "vms", id))
	for _, dir := range append(dirs, d2...) {
		_ = exec.Command("cp", "-r", dir, filepath.Join(dst, "vmdir")).Run()
		var rec struct {
			Disk string `json:"disk"`
		}
		b, _ := os.ReadFile(filepath.Join(dir, "vm.json"))
		if json.Unmarshal(b, &rec) != nil || rec.Disk == "" {
			continue
		}
		disk := filepath.Join(filepath.Dir(filepath.Dir(dir)), rec.Disk)
		out, err := exec.Command("qemu-img", "info", "-U", "--backing-chain", "--output=json", disk).Output()
		if err != nil {
			t.Logf("keep %s: qemu-img info %s: %v", id, disk, err)
			continue
		}
		_ = os.WriteFile(filepath.Join(dst, "chain.json"), out, 0o644)
		var chain []struct {
			Filename string `json:"filename"`
		}
		_ = json.Unmarshal(out, &chain)
		for _, l := range chain {
			if strings.Contains(l.Filename, "/images/") || strings.Contains(filepath.Base(l.Filename), "image-") {
				continue // the base image: same as CROWDVM_E2E_IMAGE
			}
			_ = exec.Command("cp", "--sparse=always", l.Filename, dst).Run()
		}
	}
}

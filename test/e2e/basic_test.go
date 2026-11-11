package e2e

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// ---------- nodes ----------

func testNodes(t *testing.T, c *cluster) {
	r := c.cli(t, "nodes").ok(t)
	ns := list(r.last(), "nodes")
	expect(t, len(ns) == 2, "want 2 nodes, got %d", len(ns))
	for _, n := range ns {
		name := gets(n, "name")
		expect(t, get(n, "online") == true, "%s offline", name)
		expect(t, get(n, "capacity.vcpu") == 8.0 && get(n, "capacity.memoryMiB") == 18432.0 && get(n, "capacity.maxSandboxes") == 2.0,
			"%s capacity %v", name, get(n, "capacity"))
		expect(t, contains([]any{"bronze", "silver", "gold", "platinum"}, get(n, "tier")), "%s tier %v", name, get(n, "tier"))
		if sc, _ := get(n, "score").(float64); sc <= 0 {
			t.Errorf("%s: score %v", name, get(n, "score"))
		}
		for _, dim := range []string{"cpu", "memory", "disk", "network", "gpu"} {
			expect(t, gets(n, "categories."+dim) != "", "%s: category %s missing: %v", name, dim, get(n, "categories"))
		}
		expect(t, gets(n, "accel") == "kvm", "%s accel %v", name, get(n, "accel"))
	}
	// Node 2 joined and benchmarked too; its network category needs the first run.
	if sum := get(r.last(), "summary.capacity.maxSandboxes"); sum != 4.0 {
		t.Errorf("REGRESSION B2: nodes summary.capacity.maxSandboxes = %v, want 4 (2 nodes × 2)", sum)
	}

	n1 := c.nodes[0]
	info := c.cli(t, "nodes", "info", n1.id).ok(t).last()
	expect(t, gets(info, "node.id") == n1.id || gets(info, "id") == n1.id, "nodes info: %v", info)

	// Labels (set by the harness for placement tests) are visible.
	expect(t, gets(info, "node.labels.zone") == "a" || gets(info, "labels.zone") == "a", "node 1 labels: %v", info)
	c.cli(t, "nodes", "update", n1.id, "--name", n1.name, "--label", "zone=a", "--label", "e2e=1").ok(t)
	waitFor(t, 10*time.Second, 500*time.Millisecond, "nodes update --label", func() (bool, string) {
		for _, n := range list(c.cli(t, "nodes").last(), "nodes") {
			if gets(n, "id") == n1.id {
				return gets(n, "labels.e2e") == "1" && gets(n, "labels.zone") == "a", fmt.Sprint(get(n, "labels"))
			}
		}
		return false, "node missing"
	})

	// Drain / undrain.
	c.cli(t, "nodes", "drain", c.nodes[1].id).ok(t)
	r = c.cliT(t, time.Minute, "new", "--json", "--type", "small", "--node", c.nodes[1].name, "--fail-fast")
	if ev := r.event("created"); ev != nil {
		c.cleanupSandbox(t, gets(ev, "id"))
	}
	expect(t, r.code != 0, "create pinned to a drained node succeeded: %v", r)
	c.cli(t, "nodes", "undrain", c.nodes[1].id).ok(t)

	// Node side: status command and local UI.
	st := c.nodeCmd(t, n1, 30*time.Second, "status")
	expectContains(t, strings.ToLower(st), "connected", "crowdvm-node status")
	resp, err := http.Get("http://" + n1.uiAddr + "/")
	if err != nil {
		t.Errorf("node UI: %v", err)
	} else {
		resp.Body.Close()
		expect(t, resp.StatusCode == 200, "node UI status %d", resp.StatusCode)
	}
}

// ---------- create ----------

func testCreate(t *testing.T, c *cluster) {
	dir := t.TempDir()
	local := filepath.Join(dir, "greeting.txt")
	must(t, os.WriteFile(local, []byte("hello from the host\n"), 0o644))
	setup := filepath.Join(dir, "setup.sh")
	must(t, os.WriteFile(setup, []byte("#!/bin/bash\nset -e\necho \"setup ran as $(whoami) in $PWD\" > /home/user/setup-done\n"), 0o755))

	// Auto-stop: a 45 s TTL, checked at the end of this group.
	ttlID := c.newSandbox(t, "--name", "e2e-ttl", "--ttl", "45", "--node", c.nodes[0].name)

	n2 := c.nodes[1]
	id := c.newSandbox(t, "--name", "e2e-create", "--ttl", "1800", "-e", "FOO=bar baz", "-e", "E2E_NUM=42",
		"--file", "/home/user/in/greeting.txt="+local, "--setup-file", setup, "--node", n2.name)

	sb := c.sandbox(t, id)
	expect(t, gets(sb, "type") == "small", "type %v", sb["type"])
	expect(t, get(sb, "vcpu") == 2.0 && get(sb, "memoryGB") == 4.0, "size %v/%v", sb["vcpu"], sb["memoryGB"])
	expect(t, get(sb, "ttlSeconds") == 1800.0, "ttlSeconds %v", sb["ttlSeconds"])
	expect(t, c.nodeOf(t, id) == n2.id, "pinned to %s, runs on %s", n2.id, c.nodeOf(t, id))
	expect(t, gets(sb, "name") == "e2e-create", "name %v", sb["name"])

	out := c.sh(t, id, `echo "$FOO|$E2E_NUM"; nproc; awk '/MemTotal/{print int($2/1024)}' /proc/meminfo; cat /home/user/in/greeting.txt`)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 4 {
		t.Fatalf("unexpected output %q", out)
	}
	expect(t, lines[0] == "bar baz|42", "env vars: %q", lines[0])
	expect(t, lines[1] == "2", "nproc %q", lines[1])
	if mib, _ := strconv.Atoi(lines[2]); mib < 3500 || mib > 4200 {
		t.Errorf("MemTotal %s MiB, want ~4096", lines[2])
	}
	expect(t, lines[3] == "hello from the host", "file: %q", lines[3])
	// Env vars are also in login shells (ssh / desktop terminals).
	expectContains(t, c.sh(t, id, `grep -h FOO /etc/profile.d/crowdvm-env.sh ~/.bashrc 2>/dev/null | head -1`), "FOO", "env in profile")

	// The setup script runs after ready, as user; its status is reported by the API.
	waitFor(t, 3*time.Minute, 2*time.Second, "setup script effect", func() (bool, string) {
		r := c.execR(t, id, "cat /home/user/setup-done 2>/dev/null")
		return strings.Contains(r.out, "setup ran as user in /home/user"), r.out
	})
	eventually(t, 30*time.Second, 2*time.Second, "REGRESSION B1: setupStatus done in the API", func() (bool, string) {
		sb := c.sandbox(t, id)
		return gets(sb, "setupStatus") == "done", fmt.Sprintf("setupStatus=%v err=%v; guest: %s", sb["setupStatus"], sb["setupError"],
			c.execR(t, id, "sudo cat /var/lib/crowdvm/setup.json 2>/dev/null | head -c 300").out)
	})

	// list / info / rename / extend
	ls := c.cli(t, "list").ok(t)
	expectContains(t, ls.out, id, "crowdvm list")
	c.cli(t, "rename", id, "e2e-renamed").ok(t)
	expect(t, gets(c.sandbox(t, id), "name") == "e2e-renamed", "rename")
	before := gets(c.sandbox(t, id), "archiveAfter")
	c.cli(t, "extend", id, "--ttl", "5400").ok(t)
	after := gets(c.sandbox(t, id), "archiveAfter")
	expect(t, after != before && after > before, "extend: archiveAfter %s → %s", before, after)
	c.cli(t, "extend", id, "--no-auto-stop").ok(t)
	expect(t, get(c.sandbox(t, id), "archiveAfter") == nil, "--no-auto-stop: archiveAfter %v", get(c.sandbox(t, id), "archiveAfter"))

	// Placement: label → node 1; size default = 4 vCPU / 8 GiB.
	id2 := c.newSandbox(t, "--type", "default", "--node-label", "zone=a", "--fail-fast", "--no-snapshots")
	expect(t, c.nodeOf(t, id2) == c.nodes[0].id, "zone=a placed on %s, want %s", c.nodeOf(t, id2), c.nodes[0].id)
	out = c.sh(t, id2, "nproc; awk '/MemTotal/{print int($2/1024)}' /proc/meminfo")
	f := strings.Fields(out)
	expect(t, len(f) == 2 && f[0] == "4", "default size nproc: %q", out)
	if len(f) == 2 {
		if mib, _ := strconv.Atoi(f[1]); mib < 7500 {
			t.Errorf("default size MemTotal %d MiB", mib)
		}
	}
	c.deleteSandbox(t, id2)
	c.deleteSandbox(t, id)

	// Placement that cannot be satisfied fails fast.
	for _, args := range [][]string{
		{"--type", "xlarge"},
		{"--min-tier", "platinum"},
		{"--node-label", "zone=nowhere"},
		{"--gpu", "--category", "cpu=extreme"},
	} {
		r := c.cliT(t, 2*time.Minute, append([]string{"new", "--json", "--fail-fast"}, args...)...)
		if ev := r.event("created"); ev != nil {
			c.cleanupSandbox(t, gets(ev, "id"))
		}
		if r.code == 0 {
			t.Errorf("new %v --fail-fast succeeded; want no_ready_machine", args)
		} else {
			expectContains(t, r.out+r.err, "no_ready_machine", fmt.Sprintf("new %v", args))
		}
	}

	// The TTL sandbox stopped by itself.
	sb = c.waitState(t, ttlID, 4*time.Minute, "archived")
	expect(t, qemuPID(ttlID) == 0, "auto-stopped sandbox still has a qemu process")

	// Sizes catalogue (API).
	r := c.api(t, "GET", "/server", nil)
	expect(t, r.status == 200, "GET /server: %v", r)
}

// ---------- exec ----------

func testExec(t *testing.T, c *cluster) {
	id := c.shared(t)
	// sync: exit code, stdout/stderr split
	r := c.cli(t, "exec", id, "echo out; echo err >&2; exit 3")
	expect(t, r.code == 3, "exit code %d", r.code)
	expect(t, strings.TrimSpace(r.out) == "out", "stdout %q", r.out)
	expectContains(t, r.err, "err", "stderr")
	// --json result
	j := c.cli(t, "exec", "--json", id, "--", "printf", "%s|", "a", "b c").ok(t).last()
	expect(t, gets(j, "stdout") == "a|b c|", "argv exec: %v", j)
	expect(t, get(j, "exitCode") == 0.0, "exitCode %v", j["exitCode"])
	// --cwd
	c.sh(t, id, "mkdir -p work/sub")
	expect(t, strings.TrimSpace(c.cli(t, "exec", id, "--cwd", "work/sub", "pwd").ok(t).out) == "/home/user/work/sub", "cwd")
	// timeout → 124
	r = c.cli(t, "exec", id, "--timeout", "2", "--", "sleep", "20")
	expect(t, r.code == 124, "timeout exit %d (%s)", r.code, r.err)
	// --stream prints lines as they come
	r = c.cli(t, "exec", id, "--stream", "for i in 1 2 3; do echo line$i; sleep 0.5; done; exit 5")
	expect(t, r.code == 5, "stream exit %d", r.code)
	expect(t, strings.Contains(r.out, "line1") && strings.Contains(r.out, "line3"), "stream output %q", r.out)
	// --timeout 0 runs in the background and streams
	r = c.cli(t, "exec", id, "--timeout", "0", "for i in 1 2; do echo bg$i; sleep 1; done")
	expect(t, r.code == 0 && strings.Contains(r.out, "bg2"), "timeout 0: %v", r)

	// detached + status + attach
	d := c.cli(t, "exec", id, "--detach", "for i in 1 2 3 4; do echo tick $i; sleep 1; done; echo bye >&2").ok(t).last()
	pid := strconv.Itoa(int(get(d, "processId").(float64)))
	st := c.cli(t, "exec", id, "--status", pid).ok(t).last()
	expect(t, get(st, "running") == true, "detached status: %v", st)
	r = c.cliT(t, time.Minute, "exec", id, "--attach", pid).ok(t)
	expectContains(t, r.out, "tick 4", "attach output")
	st = c.cli(t, "exec", id, "--status", pid).ok(t).last()
	expect(t, gets(st, "status") == "exited" && get(st, "exitCode") == 0.0, "after attach: %v", st)
	expectContains(t, gets(st, "stderr"), "bye", "status stderr tail")
	// kill
	d = c.cli(t, "exec", id, "--detach", "sleep 600").ok(t).last()
	pid = strconv.Itoa(int(get(d, "processId").(float64)))
	c.cli(t, "exec", id, "--kill", pid).ok(t)
	waitFor(t, 15*time.Second, 500*time.Millisecond, "killed process", func() (bool, string) {
		st := c.cli(t, "exec", id, "--status", pid).last()
		return get(st, "running") == false, fmt.Sprint(st)
	})

	// run: create, run, delete in one go
	before := strings.Count(c.cli(t, "list").ok(t).out, `"id":"bx_`)
	rr := c.cliT(t, 8*time.Minute, "run", "--type", "small", "--", "bash", "-c", "echo run-ok; exit 7")
	expect(t, rr.code == 7 && strings.Contains(rr.out, "run-ok"), "crowdvm run: %v", rr)
	eventually(t, 2*time.Minute, 2*time.Second, "run sandbox deleted", func() (bool, string) {
		l := c.cli(t, "list").out
		return strings.Count(l, `"id":"bx_`) == before, tail(l, 300)
	})

	// processes
	ps := c.cli(t, "ps", id).ok(t).last()
	expect(t, len(list(ps, "processes")) > 5, "ps: %v", tail(fmt.Sprint(ps), 300))

	// history: list + stored output (the API is the source of truth)
	marker := fmt.Sprintf("hist-%d", time.Now().UnixNano())
	c.sh(t, id, "echo "+marker)
	h := c.cli(t, "history", id, "--limit", "20").ok(t).last()
	var cmdID string
	for _, it := range list(h, "commands") {
		if strings.Contains(gets(it, "command"), marker) {
			cmdID = gets(it, "commandId")
		}
	}
	if cmdID == "" {
		t.Fatalf("history does not list the command: %s", tail(fmt.Sprint(h), 800))
	}
	ho := c.cli(t, "history", id, cmdID).ok(t)
	expectContains(t, ho.out, marker, "history output")
	hm := c.cli(t, "history", id, "--mode", "detached").ok(t).last()
	for _, it := range list(hm, "commands") {
		expect(t, gets(it, "mode") == "detached", "history --mode detached returned %v", gets(it, "mode"))
	}
	// API: command by id
	ar := c.api(t, "GET", "/sandboxes/"+id+"/command-history/"+cmdID, nil)
	expect(t, ar.status == 200, "GET command-history/{id}: %v", ar)
}

// ---------- files ----------

func testFiles(t *testing.T, c *cluster) {
	id := c.shared(t)
	p := "/sandboxes/" + id + "/files"
	ok := func(r apiResp, what string) map[string]any {
		t.Helper()
		if r.status/100 != 2 {
			t.Errorf("%s: %v", what, r)
		}
		return r.json()
	}
	ok(c.api(t, "PUT", p, map[string]any{"path": "/home/user/f/a.txt", "content": "alpha\nbeta\n"}), "write")
	ok(c.api(t, "PUT", p, map[string]any{"path": "/home/user/f/b.bin", "content": base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 255}), "encoding": "base64"}), "write base64")
	rd := ok(c.api(t, "GET", p+"?path=/home/user/f/a.txt", nil), "read")
	expect(t, gets(rd, "content") == "alpha\nbeta\n", "read content %v", rd["content"])
	rd = ok(c.api(t, "GET", p+"?path=/home/user/f/b.bin&encoding=base64", nil), "read base64")
	expect(t, gets(rd, "content") == "AAEC/w==", "base64 content %v", rd["content"])
	ls := ok(c.api(t, "GET", p+"/list?path=/home/user/f", nil), "list")
	expect(t, len(list(ls, "files")) == 2, "list: %v", ls["files"])
	in := ok(c.api(t, "GET", p+"/info?path=/home/user/f/a.txt", nil), "info")
	expect(t, get(in, "size") == 11.0 && get(in, "isDir") == false, "info %v", in)
	ok(c.api(t, "POST", p+"/folder", map[string]any{"path": "/home/user/f/dir"}), "folder")
	ok(c.api(t, "POST", p+"/move", map[string]any{"source": "/home/user/f/a.txt", "destination": "/home/user/f/dir/a.txt"}), "move")
	ok(c.api(t, "POST", p+"/permissions", map[string]any{"path": "/home/user/f/dir/a.txt", "mode": "600"}), "chmod")
	expect(t, strings.TrimSpace(c.sh(t, id, "stat -c %a /home/user/f/dir/a.txt")) == "600", "chmod applied")
	rep := ok(c.api(t, "POST", p+"/replace", map[string]any{"paths": []string{"/home/user/f/dir/a.txt"}, "pattern": "beta", "newValue": "gamma"}), "replace")
	_ = rep
	expect(t, strings.Contains(c.sh(t, id, "cat /home/user/f/dir/a.txt"), "gamma"), "replace applied")
	se := ok(c.api(t, "GET", p+"/search?path=/home/user/f&pattern=*.txt", nil), "search")
	expectContains(t, string(mustJSON(se)), "a.txt", "search")
	fi := ok(c.api(t, "GET", p+"/find?path=/home/user/f&pattern=gamma", nil), "find")
	expectContains(t, string(mustJSON(fi)), "a.txt", "find (content)")
	// raw get / put (no size limit)
	big := make([]byte, 6<<20)
	_, _ = rand.Read(big)
	ok(c.api(t, "PUT", p+"/raw?path=/home/user/f/big.bin", big), "raw put 6 MiB")
	raw := c.api(t, "GET", p+"/raw?path=/home/user/f/big.bin", nil)
	expect(t, raw.status == 200 && len(raw.body) == len(big) && string(raw.body[:64]) == string(big[:64]), "raw get: %d bytes status %d", len(raw.body), raw.status)
	tooBig := c.api(t, "GET", p+"?path=/home/user/f/big.bin", nil)
	expect(t, tooBig.status == 400, "read >5 MiB should be 400 file_too_large: %v", tooBig)
	// signed URLs
	u := ok(c.api(t, "POST", p+"/upload-url", map[string]any{"path": "/home/user/f/signed.txt"}), "upload-url")
	req, _ := http.NewRequest("PUT", gets(u, "url"), strings.NewReader("signed upload"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode/100 != 2 {
		t.Errorf("PUT signed URL: %v %v", err, resp)
	}
	dl := ok(c.api(t, "POST", p+"/download-url", map[string]any{"path": "/home/user/f/signed.txt"}), "download-url")
	if resp, err := http.Get(gets(dl, "url")); err != nil {
		t.Errorf("GET signed URL: %v", err)
	} else {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		expect(t, string(b) == "signed upload", "signed download: %d %q", resp.StatusCode, b)
	}
	bad := strings.Replace(gets(dl, "url"), "sig=", "sig=x", 1)
	if resp, err := http.Get(bad); err == nil {
		resp.Body.Close()
		expect(t, resp.StatusCode == 403, "tampered signed URL: %d", resp.StatusCode)
	}
	// path escape is refused
	esc := c.api(t, "GET", p+"?path=/etc/shadow", nil)
	expect(t, esc.status == 400 || esc.status == 403, "read /etc/shadow: %v", esc)
	// delete
	ok(c.api(t, "DELETE", p+"?path=/home/user/f/dir&recursive=true", nil), "delete dir")
	gone := c.api(t, "GET", p+"/info?path=/home/user/f/dir", nil)
	expect(t, gone.status == 404, "deleted dir info: %v", gone)

	// scp (system scp via tunnel), scp --api, push
	tmp := t.TempDir()
	src := filepath.Join(tmp, "up.txt")
	must(t, os.WriteFile(src, []byte("scp me\n"), 0o644))
	c.cliT(t, 2*time.Minute, "scp", src, id+":/home/user/scp-up.txt").ok(t)
	expect(t, c.sh(t, id, "cat /home/user/scp-up.txt") == "scp me\n", "scp upload")
	back := filepath.Join(tmp, "down.txt")
	c.cliT(t, 2*time.Minute, "scp", id+":/home/user/scp-up.txt", back).ok(t)
	b, _ := os.ReadFile(back)
	expect(t, string(b) == "scp me\n", "scp download %q", b)
	treeDir := filepath.Join(tmp, "tree")
	must(t, os.MkdirAll(filepath.Join(treeDir, "x/y"), 0o755))
	must(t, os.WriteFile(filepath.Join(treeDir, "x/y/z.txt"), []byte("deep"), 0o644))
	c.cliT(t, 2*time.Minute, "scp", "-r", treeDir, id+":/home/user/").ok(t)
	expect(t, c.sh(t, id, "cat /home/user/tree/x/y/z.txt") == "deep", "scp -r")
	c.cliT(t, 2*time.Minute, "scp", "--api", src, id+":/home/user/api-up.txt").ok(t)
	expect(t, c.sh(t, id, "cat /home/user/api-up.txt") == "scp me\n", "scp --api")

	// push: git-aware (tracked + untracked-not-ignored, no .git, no ignored)
	repo := filepath.Join(tmp, "proj")
	must(t, os.MkdirAll(filepath.Join(repo, "node_modules/x"), 0o755))
	must(t, os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules/\n*.log\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(repo, "new.txt"), []byte("untracked\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(repo, "debug.log"), []byte("ignored\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(repo, "node_modules/x/i.js"), []byte("ignored\n"), 0o644))
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "add", "main.go", ".gitignore")
	gitRun(t, repo, "-c", "user.email=e2e@x", "-c", "user.name=e2e", "commit", "-qm", "init")
	c.cliT(t, 2*time.Minute, "push", id, repo).ok(t)
	out := c.sh(t, id, "cd /home/user/proj && ls -A | sort | tr '\\n' ' '")
	expect(t, strings.TrimSpace(out) == ".gitignore main.go new.txt", "push contents: %q", out)
	// exec --push
	r := c.cliT(t, 2*time.Minute, "exec", id, "--push", repo, "cat main.go").ok(t)
	expectContains(t, r.out, "package main", "exec --push")
}

func gitRun(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// ---------- ssh, forward ----------

func testSSH(t *testing.T, c *cluster) {
	id := c.shared(t)
	r := c.cliT(t, 2*time.Minute, "ssh", id, "echo via-ssh && whoami").ok(t)
	expect(t, strings.Contains(r.out, "via-ssh") && strings.Contains(r.out, "user"), "ssh output %q", r.out)
	r = c.cliWith(t, cliOpt{stdin: "echo from-stdin $((6*7))\n", timeout: 2 * time.Minute}, "ssh", id, "--", "bash", "-s").ok(t)
	expectContains(t, r.out, "from-stdin 42", "ssh bash -s")
	cfg := c.cli(t, "ssh-config", id, "--print").ok(t)
	expectContains(t, cfg.out, "ProxyCommand", "ssh-config --print")

	// tunnel directly (ssh banner)
	tr := c.cliWith(t, cliOpt{timeout: 20 * time.Second, stdin: ""}, "tunnel", id, "22")
	expectContains(t, tr.out, "SSH-2.0", "tunnel to :22")

	// forward: a web server in the sandbox on :8765 → local port
	c.sh(t, id, "mkdir -p /home/user/www && echo forwarded-ok > /home/user/www/index.html")
	c.cli(t, "exec", id, "--detach", "cd /home/user/www && exec python3 -m http.server 8765 --bind 127.0.0.1").ok(t)
	lp := freePort(t)
	stop := c.background(t, "forward", "forward", id, "--remote", "8765", "--local", strconv.Itoa(lp))
	waitFor(t, 30*time.Second, 500*time.Millisecond, "forwarded port", func() (bool, string) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", lp))
		if err != nil {
			return false, err.Error()
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return strings.Contains(string(b), "forwarded-ok"), string(b)
	})
	stop()

	// reverse forward: a host listener reachable as 127.0.0.1:<p> inside the sandbox
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer l.Close()
	go func() {
		_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("reverse-ok")) }))
	}()
	rp := l.Addr().(*net.TCPAddr).Port
	stop = c.background(t, "forward-reverse", "forward", id, "--reverse", "--local", strconv.Itoa(rp))
	waitFor(t, 30*time.Second, time.Second, "reverse forward", func() (bool, string) {
		r := c.execR(t, id, fmt.Sprintf("curl -s -m 3 http://127.0.0.1:%d/", rp))
		return strings.Contains(r.out, "reverse-ok"), r.out + r.err
	})
	stop()
}

// background starts a long-running CLI command (forward) and returns its stop func.
func (c *cluster) background(t testing.TB, name string, args ...string) func() {
	t.Helper()
	logf, err := os.Create(filepath.Join(c.logs, "cli-"+name+".log"))
	must(t, err)
	cmd := exec.Command(c.cliBin(), args...)
	cmd.Env = c.baseEnv()
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	must(t, cmd.Start())
	var once atomic.Bool
	stop := func() {
		if once.Swap(true) {
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		logf.Close()
	}
	t.Cleanup(stop)
	return stop
}

// ---------- public URLs ----------

func testHost(t *testing.T, c *cluster) {
	id := c.shared(t)
	c.sh(t, id, "mkdir -p /home/user/site && echo hosted-ok > /home/user/site/index.html")
	c.cli(t, "exec", id, "--detach", "cd /home/user/site && exec python3 -m http.server 8080").ok(t)
	waitFor(t, 20*time.Second, 500*time.Millisecond, "guest http server", func() (bool, string) {
		r := c.execR(t, id, "curl -s -m 2 http://127.0.0.1:8080/")
		return strings.Contains(r.out, "hosted-ok"), r.out
	})

	h := c.cli(t, "host", id, "8080", "--title", "e2e site", "--json").ok(t).last()
	u := gets(h, "url")
	// The CLI prints the primary URL; the host-routed one comes from the API.
	hostURL := gets(c.api(t, "GET", "/sandboxes/"+id+"/host/8080", nil).json(), "hostUrl")
	expect(t, strings.Contains(u, "_token="), "private URL without token: %v", h)
	noFollow := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// path routing: ?_token= → 302 + cookie → 200
	pu, _ := url.Parse(u)
	resp, err := noFollow.Get(u)
	must(t, err)
	resp.Body.Close()
	expect(t, resp.StatusCode == 302, "token URL status %d, want 302", resp.StatusCode)
	var cookie *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == "_port_auth" {
			cookie = ck
		}
	}
	if cookie == nil {
		t.Fatalf("no _port_auth cookie: %v", resp.Header)
	}
	plain := pu.Scheme + "://" + pu.Host + pu.Path
	req, _ := http.NewRequest("GET", plain, nil)
	req.AddCookie(cookie)
	body, st := doBody(t, noFollow, req)
	expect(t, st == 200 && strings.Contains(body, "hosted-ok"), "path URL with cookie: %d %q", st, body)
	body, st = doBody(t, noFollow, mustReq("GET", plain))
	expect(t, st == 401, "path URL without token: %d %q", st, body)
	body, st = doBody(t, noFollow, mustReq("GET", plain+"?_token=wrong"))
	expect(t, st == 401, "path URL with wrong token: %d", st)

	// host routing: <id>-8080.localhost:<port>
	hu, _ := url.Parse(hostURL)
	if hu == nil || !strings.HasSuffix(hu.Hostname(), ".localhost") {
		t.Errorf("hostUrl %q not under .localhost", hostURL)
	} else {
		req := mustReq("GET", fmt.Sprintf("http://127.0.0.1:%d/?%s", c.port, hu.RawQuery))
		req.Host = hu.Host
		resp, err := noFollow.Do(req)
		must(t, err)
		resp.Body.Close()
		expect(t, resp.StatusCode == 302, "host routing with token: %d", resp.StatusCode)
		req = mustReq("GET", fmt.Sprintf("http://127.0.0.1:%d/", c.port))
		req.Host = hu.Host
		for _, ck := range resp.Cookies() {
			req.AddCookie(ck)
		}
		body, st := doBody(t, noFollow, req)
		expect(t, st == 200 && strings.Contains(body, "hosted-ok"), "host routing with cookie: %d %q", st, body)
	}

	// public: no token needed
	pub := c.cli(t, "host", id, "8080", "--public", "--json").ok(t).last()
	pubURL := gets(pub, "pathUrl")
	if pubURL == "" {
		pubURL = gets(pub, "url")
	}
	expect(t, !strings.Contains(pubURL, "_token"), "public URL has a token: %s", pubURL)
	body, st = doBody(t, noFollow, mustReq("GET", pubURL))
	expect(t, st == 200 && strings.Contains(body, "hosted-ok"), "public URL: %d %q", st, body)

	// custom subdomain
	c.cli(t, "subdomain", id, "e2esite").ok(t)
	req = mustReq("GET", fmt.Sprintf("http://127.0.0.1:%d/", c.port))
	req.Host = fmt.Sprintf("e2esite-8080.localhost:%d", c.port)
	body, st = doBody(t, noFollow, req)
	expect(t, st == 200 && strings.Contains(body, "hosted-ok"), "custom subdomain: %d %q", st, body)

	// firewall deny (server-side CIDR check)
	fw := c.api(t, "POST", "/sandboxes/"+id+"/host", map[string]any{"port": 8080, "public": true, "firewall": map[string]any{"deny": []string{"127.0.0.1/32"}}})
	expect(t, fw.status == 200, "host with firewall: %v", fw)
	body, st = doBody(t, noFollow, mustReq("GET", pubURL))
	expect(t, st == 403, "firewall deny: %d %q", st, body)
	fw = c.api(t, "POST", "/sandboxes/"+id+"/host", map[string]any{"port": 8080, "public": true, "firewall": map[string]any{"allow": []string{"127.0.0.0/8"}}})
	expect(t, fw.status == 200, "host with allow: %v", fw)
	body, st = doBody(t, noFollow, mustReq("GET", pubURL))
	expect(t, st == 200, "firewall allow: %d", st)

	// reserved port, list, url, hide
	rs := c.api(t, "POST", "/sandboxes/"+id+"/host", map[string]any{"port": 22})
	expect(t, rs.status == 400, "hosting port 22: %v", rs)
	ls := c.cli(t, "host", "list", id).ok(t)
	expectContains(t, ls.out, "8080", "host list")
	uu := c.cli(t, "host", "url", id, "8080").ok(t)
	expectContains(t, uu.out, "/p/"+id+"/8080", "host url")
	// Captured output is the bare URL (Boat: BACKEND_URL=$(host url 3001)),
	// also from inside the sandbox with the in-sandbox `host` command.
	expect(t, strings.HasPrefix(strings.TrimSpace(uu.out), "http") && !strings.ContainsAny(uu.out, "{["), "host url output: %q", uu.out)
	inURL := strings.TrimSpace(c.sh(t, id, "u=$(host url 8080); echo \"$u\""))
	expect(t, strings.HasPrefix(inURL, "http") && strings.Contains(inURL, "8080") && !strings.ContainsAny(inURL, "{["), "in-sandbox $(host url): %q", inURL)
	c.cli(t, "host", "hide", id, "8080").ok(t)
	_, st = doBody(t, noFollow, mustReq("GET", pubURL))
	expect(t, st == 404, "hidden port: %d", st)
	// nothing listening → 502
	c.cli(t, "host", id, "8099", "--public").ok(t)
	_, st = doBody(t, noFollow, mustReq("GET", fmt.Sprintf("%s/p/%s/8099/", c.url, id)))
	expect(t, st == 502, "hosted port with no listener: %d", st)
	// created with --host at create time is covered in saved-snapshots (new --from ... --host).
}

func mustReq(method, u string) *http.Request {
	r, err := http.NewRequest(method, u, nil)
	if err != nil {
		panic(err)
	}
	return r
}

func doBody(t testing.TB, cl *http.Client, req *http.Request) (string, int) {
	t.Helper()
	resp, err := cl.Do(req)
	if err != nil {
		t.Errorf("%s %s: %v", req.Method, req.URL, err)
		return "", 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), resp.StatusCode
}

// ---------- desktop + browser ----------

func testDesktop(t *testing.T, c *cluster) {
	id := c.shared(t)
	d := c.cliT(t, 3*time.Minute, "desktop", id, "--no-open").ok(t).last()
	du := gets(d, "desktopUrl")
	if du == "" {
		du = gets(d, "url")
	}
	expect(t, strings.Contains(du, "token="), "desktop URL without token: %v", d)
	// The viewer page loads.
	if u, err := url.Parse(du); err == nil {
		resp, err := http.Get(u.Scheme + "://" + u.Host + u.Path)
		if err == nil {
			resp.Body.Close()
			expect(t, resp.StatusCode == 200, "desktop viewer page: %d", resp.StatusCode)
		}
	}
	// VNC over the websocket.
	dj := c.api(t, "POST", "/sandboxes/"+id+"/desktop?vnc=1", map[string]any{}).json()
	banner, err := wsFirstMessage(gets(dj, "wsUrl"), gets(dj, "token"))
	if err != nil {
		t.Errorf("desktop websocket: %v", err)
	} else {
		expect(t, strings.HasPrefix(banner, "RFB 003."), "desktop ws banner %q", banner)
	}
	_, err = wsFirstMessage(gets(dj, "wsUrl"), "desktop.bogus.token.x")
	expect(t, err != nil, "desktop websocket accepted a bogus token")

	// screenshot + input
	shot := c.api(t, "GET", "/sandboxes/"+id+"/desktop/screenshot", nil)
	expect(t, shot.status == 200 && strings.HasPrefix(string(shot.body), "\x89PNG"), "screenshot: %d %d bytes %s", shot.status, len(shot.body), shot.header.Get("Content-Type"))
	in := c.api(t, "POST", "/sandboxes/"+id+"/desktop/input", map[string]any{"actions": []map[string]any{
		{"type": "move", "x": 200, "y": 200},
		{"type": "click", "button": "left"},
		{"type": "type", "text": "hello"},
		{"type": "key", "keys": "Escape"},
		{"type": "scroll", "direction": "down", "amount": 2},
	}})
	expect(t, in.status == 200 && get(in.json(), "performed") == 5.0, "desktop input: %v", in)
	bad := c.api(t, "POST", "/sandboxes/"+id+"/desktop/input", map[string]any{"actions": []map[string]any{{"type": "teleport"}}})
	expect(t, bad.status == 400, "invalid action: %v", bad)
	stj := c.api(t, "GET", "/sandboxes/"+id+"/desktop/status", nil).json()
	expect(t, get(stj, "running") == true, "desktop status %v", stj)

	// Boat's wallpaper recipe: a PNG at ~/.config/boat/wallpaper.png shows on the desktop.
	c.sh(t, id, `python3 -c "
import zlib,struct
w,h=64,64
raw=b''.join(b'\\x00'+b'\\xff\\x00\\x00'*w for _ in range(h))
ch=lambda t,d: struct.pack('>I',len(d))+t+d+struct.pack('>I',zlib.crc32(t+d)&0xffffffff)
open('/tmp/red.png','wb').write(b'\\x89PNG\\r\\n\\x1a\\n'+ch(b'IHDR',struct.pack('>IIBBBBB',w,h,8,2,0,0,0))+ch(b'IDAT',zlib.compress(raw))+ch(b'IEND',b''))
" && cp /tmp/red.png ~/.config/boat/wallpaper.png`)
	eventually(t, 90*time.Second, 3*time.Second, "wallpaper on the desktop", func() (bool, string) {
		sh := c.api(t, "GET", "/sandboxes/"+id+"/desktop/screenshot", nil)
		img, err := png.Decode(bytes.NewReader(sh.body))
		if err != nil {
			return false, err.Error()
		}
		b := img.Bounds()
		r, g, bl, _ := img.At(b.Max.X-40, b.Max.Y/2).RGBA()
		return r > 0xc000 && g < 0x4000 && bl < 0x4000, fmt.Sprintf("pixel r=%x g=%x b=%x", r>>8, g>>8, bl>>8)
	})
	c.sh(t, id, "rm -f ~/.config/boat/wallpaper.png")

	// browser view (Chrome only, isolated user)
	b := c.cliT(t, 3*time.Minute, "browser", id, "--no-open").ok(t).last()
	expect(t, strings.Contains(gets(b, "browserUrl")+gets(b, "url"), "view=browser"), "browser URL: %v", b)
	bj := c.api(t, "POST", "/sandboxes/"+id+"/browser", map[string]any{}).json()
	waitFor(t, 60*time.Second, 2*time.Second, "browser websocket", func() (bool, string) {
		banner, err := wsFirstMessage(gets(bj, "wsUrl"), gets(bj, "token"))
		if err != nil {
			return false, err.Error()
		}
		return strings.HasPrefix(banner, "RFB 003."), banner
	})
	ps := c.sh(t, id, "ps -eo user:20,args | grep -i [c]hrome | head -3")
	expectContains(t, ps, "crowdvm-browser", "browser runs as crowdvm-browser")
}

// wsFirstMessage opens a websocket with the CrowdVM token subprotocol and
// returns the first server message (the RFB banner for VNC).
func wsFirstMessage(wsURL, token string) (string, error) {
	u, err := url.Parse(wsURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("bad ws url %q", wsURL)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 10*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	req := mustReq("GET", "http://"+u.Host+u.RequestURI())
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(key))
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Protocol", "binary, crowdvm-token."+token)
	if err := req.Write(conn); err != nil {
		return "", err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 101 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return "", err
	}
	n := uint64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		_, _ = io.ReadFull(br, b[:])
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		_, _ = io.ReadFull(br, b[:])
		n = binary.BigEndian.Uint64(b[:])
	}
	if hdr[1]&0x80 != 0 {
		var m [4]byte
		_, _ = io.ReadFull(br, m[:])
	}
	if n > 1<<20 {
		return "", fmt.Errorf("frame too large: %d", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(br, p); err != nil {
		return "", err
	}
	return string(p), nil
}

// ---------- network isolation ----------

func hostIPs() []string {
	var ips []string
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				ips = append(ips, n.IP.String())
			}
		}
	}
	return ips
}

// lanIP is the address of the default route (no packet is sent).
// lanIP: this host's address on its default route, fixed for the whole run
// (node 2 allows exactly it; a VPN coming up mid-run must not change it).
var lanIP = sync.OnceValue(func() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
})

func testNetwork(t *testing.T, c *cluster) {
	id := c.shared(t) // on node 1, which has no net.allow overrides
	l, err := net.Listen("tcp", "0.0.0.0:0")
	must(t, err)
	defer l.Close()
	var hits atomic.Int32
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			_, _ = conn.Write([]byte("HTTP/1.0 200 OK\r\n\r\nhost-listener"))
			conn.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	targets := append([]string{"10.0.2.2"}, hostIPs()...)
	for _, ip := range targets {
		r := c.execR(t, id, fmt.Sprintf("curl -s -m 4 http://%s:%d/ && echo REACHED || echo blocked", ip, port))
		expect(t, strings.Contains(r.out, "blocked") && !strings.Contains(r.out, "REACHED"), "guest reached host listener at %s:%d: %q", ip, port, r.out)
	}
	expect(t, hits.Load() == 0, "host listener got %d connection(s) from the guest", hits.Load())
	// The guest's own loopback is not the host's.
	r := c.execR(t, id, fmt.Sprintf("curl -s -m 3 http://127.0.0.1:%d/ && echo REACHED || echo blocked", port))
	expectContains(t, r.out, "blocked", "guest 127.0.0.1")
	// Other private ranges and metadata are blocked too.
	r = c.execR(t, id, "curl -s -m 4 http://169.254.169.254/ && echo REACHED || echo blocked")
	expectContains(t, r.out, "blocked", "metadata address")
	// The server is reachable at 10.0.2.2:<server port>.
	r = c.execR(t, id, fmt.Sprintf("curl -s -m 5 -o /dev/null -w '%%{http_code}' http://10.0.2.2:%d/api/v1/server", c.port))
	code := strings.TrimSpace(r.out)
	expect(t, code != "" && code != "000", "guest → server at 10.0.2.2: %q %s", r.out, r.err)
	// Internet: DNS + HTTPS.
	r = c.execR(t, id, "curl -s -m 20 -o /dev/null -w '%{http_code}' https://example.com/")
	expect(t, strings.TrimSpace(r.out) == "200", "guest → internet: %q %s", r.out, r.err)
	r = c.execR(t, id, "getent hosts github.com | head -1")
	expect(t, strings.TrimSpace(r.out) != "", "guest DNS: %v", r)
}

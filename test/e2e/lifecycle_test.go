package e2e

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// ---------- webhooks (+ telemetry setup) ----------

func testWebhooks(t *testing.T, c *cluster) {
	r := c.cli(t, "webhook", "create", c.hooks.hookURL+"/hook", "--name", "e2e").ok(t)
	wh := r.last()
	secret := gets(wh, "secret")
	whID := gets(wh, "webhook.id")
	if whID == "" {
		whID = gets(wh, "id")
	}
	if !strings.HasPrefix(secret, "whsec_") || whID == "" {
		t.Fatalf("webhook create: %v", r)
	}
	c.hooks.setSecret(secret)
	c.cli(t, "webhook", "get", whID).ok(t)
	expectContains(t, c.cli(t, "webhook", "list").ok(t).out, whID, "webhook list")

	// test delivery, signed
	tr := c.api(t, "POST", "/webhooks/"+whID+"/test", nil)
	expect(t, tr.status == 200 && get(tr.json(), "test.ok") == true, "webhook test: %v", tr)
	d := c.waitHook(t, "webhook.test", "", 10*time.Second)
	expect(t, d.header.Get("X-CrowdVM-Event") == "webhook.test", "event header %q", d.header.Get("X-CrowdVM-Event"))

	// rotate: the new secret signs from now on
	rr := c.cli(t, "webhook", "rotate", whID).ok(t).last()
	ns := gets(rr, "secret")
	expect(t, strings.HasPrefix(ns, "whsec_") && ns != secret, "rotate: %v", rr)
	c.hooks.setSecret(ns)
	n0 := len(c.hooks.hooksFor("webhook.test", ""))
	tr = c.api(t, "POST", "/webhooks/"+whID+"/test", nil)
	expect(t, tr.status == 200, "webhook test after rotate: %v", tr)
	waitFor(t, 10*time.Second, 300*time.Millisecond, "second test delivery", func() (bool, string) {
		return len(c.hooks.hooksFor("webhook.test", "")) > n0, ""
	})
	last := c.hooks.hooksFor("webhook.test", "")
	expect(t, last[len(last)-1].sigValid, "signature with the rotated secret did not verify")

	dl := c.api(t, "GET", "/webhooks/"+whID+"/deliveries", nil)
	expect(t, dl.status == 200 && len(list(dl.json(), "deliveries")) >= 2, "deliveries: %v", dl)

	// An unreachable second webhook must not block the first (and must be removable).
	bad := c.cli(t, "webhook", "create", "http://127.0.0.1:1/nowhere", "--name", "e2e-dead", "--event", "ready").ok(t).last()
	badID := gets(bad, "webhook.id")
	c.cli(t, "webhook", "update", badID, "--name", "e2e-dead-2").ok(t)
	c.cli(t, "webhook", "remove", badID).ok(t)

	// OpenTelemetry export (checked in the "otel" group at the end).
	te := c.cli(t, "telemetry", "set", c.hooks.otlpURL, "-H", "x-e2e=1").ok(t).last()
	expect(t, get(te, "test.ok") == true, "telemetry test export: %v", te)
	expect(t, len(c.hooks.otlpReqs("/v1/metrics")) >= 1, "OTLP receiver got no test export")
}

// ---------- lifecycle ----------

func testLifecycle(t *testing.T, c *cluster) {
	n1, n2 := c.nodes[0], c.nodes[1]
	a := c.newSandbox(t, "--name", "lc-a", "--node", n2.name, "--ttl", "3600")
	c.waitHook(t, "sandbox.ready", a, 30*time.Second)
	c.sh(t, a, "mkdir -p /home/user/keep && echo persist-1 > /home/user/keep/data.txt && echo scratch > /tmp/scratch && sync")
	c.sh(t, a, "echo history-marker-lc")

	// Live fork on the same node: near-instant, sees the data.
	t0 := time.Now()
	fr := c.cliT(t, 5*time.Minute, "fork", a, "--wait", "--name", "lc-fork").ok(t)
	f := gets(fr.event("created"), "id")
	if f == "" {
		f = gets(fr.last(), "id")
	}
	c.cleanupSandbox(t, f)
	t.Logf("live fork took %s", time.Since(t0).Round(100*time.Millisecond))
	expect(t, c.nodeOf(t, f) == n2.id, "live fork on %s, source on %s", c.nodeOf(t, f), n2.id)
	expect(t, strings.TrimSpace(c.sh(t, f, "cat /home/user/keep/data.txt")) == "persist-1", "fork data")
	expect(t, strings.TrimSpace(c.sh(t, a, "hostname")) != strings.TrimSpace(c.sh(t, f, "hostname")) ||
		strings.TrimSpace(c.sh(t, a, "cat /etc/machine-id")) != strings.TrimSpace(c.sh(t, f, "cat /etc/machine-id")), "fork kept the machine identity")
	c.deleteSandbox(t, f)

	// Stop: snapshot, free the node.
	st := c.cliT(t, 6*time.Minute, "stop", a, "--wait").ok(t)
	expect(t, st.event("stopped") != nil, "stop --wait: %v", st)
	expect(t, c.state(t, a) == "archived", "state after stop: %s", c.state(t, a))
	waitFor(t, 30*time.Second, time.Second, "qemu exit", func() (bool, string) { return qemuPID(a) == 0, "" })
	c.waitHook(t, "sandbox.archived", a, 30*time.Second)
	expect(t, len(c.hooks.hooksFor("sandbox.hydrated", a)) == 0, "sandbox.hydrated sent on stop")

	// History and processes stay readable while stopped; exec does not run.
	h := c.cli(t, "history", a).ok(t)
	expectContains(t, h.out, "history-marker-lc", "history while stopped")
	var cmdID string
	for _, it := range list(h.last(), "commands") {
		if strings.Contains(gets(it, "command"), "history-marker-lc") {
			cmdID = gets(it, "commandId")
		}
	}
	if cmdID != "" {
		ho := c.cli(t, "history", a, cmdID).ok(t)
		expectContains(t, ho.out, "history-marker-lc", "stored output while stopped")
	} else {
		t.Errorf("stopped history lacks the marker command")
	}
	ps := c.api(t, "GET", "/sandboxes/"+a+"/processes", nil)
	expect(t, ps.status == 200 && get(ps.json(), "live") == false, "processes while stopped: %v", ps)
	ex := c.cli(t, "exec", a, "true")
	expect(t, ex.code != 0 && strings.Contains(ex.out+ex.err, "not_running"), "exec on a stopped sandbox: %v", ex)

	// Resume on the OTHER node (chain pulled from the server).
	rs := c.cliT(t, 10*time.Minute, "resume", a, "--node", n1.name).ok(t)
	c.waitHook(t, "sandbox.hydrated", a, 3*time.Minute) // a restore: after ready (lazy chain: when complete)
	expect(t, rs.event("ready") != nil, "resume: %v", rs)
	expect(t, c.nodeOf(t, a) == n1.id, "resumed on %s, want %s", c.nodeOf(t, a), n1.id)
	expect(t, strings.TrimSpace(c.sh(t, a, "cat /home/user/keep/data.txt")) == "persist-1", "data after resume")
	expect(t, strings.TrimSpace(c.execR(t, a, "cat /tmp/scratch 2>/dev/null || echo gone").out) == "gone", "/tmp survived stop")
	c.sh(t, a, "echo persist-2 > /home/user/keep/data2.txt && sync")

	// Stop again, fork the stopped sandbox onto node 2 (pulls the new layer).
	c.cliT(t, 6*time.Minute, "stop", a, "--wait").ok(t)
	fr = c.cliT(t, 10*time.Minute, "fork", a, "--wait", "--node", n2.name, "--name", "lc-fork-2").ok(t)
	g := gets(fr.event("created"), "id")
	c.cleanupSandbox(t, g)
	expect(t, c.nodeOf(t, g) == n2.id, "fork of stopped sandbox on %s, want %s", c.nodeOf(t, g), n2.id)
	expect(t, strings.TrimSpace(c.sh(t, g, "cat /home/user/keep/data2.txt")) == "persist-2", "data in fork of stopped sandbox")
	c.deleteSandbox(t, g)

	// Recover a hung guest: SIGSTOP qemu → degraded → recover → ready.
	c.cliT(t, 10*time.Minute, "resume", a, "--node", n2.name).ok(t)
	pid := qemuPID(a)
	if pid == 0 {
		t.Fatalf("no qemu process for %s", a)
	}
	must(t, syscall.Kill(pid, syscall.SIGSTOP))
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGCONT) })
	waitFor(t, 2*time.Minute, 2*time.Second, "degraded", func() (bool, string) {
		sb := c.sandbox(t, a)
		return gets(sb, "health") == "degraded", fmt.Sprintf("state=%v health=%v reason=%v", sb["state"], sb["health"], sb["healthReason"])
	})
	dg := c.waitHook(t, "sandbox.degraded", a, 30*time.Second)
	expect(t, gets(dg.event, "data.reason") == "guest_unreachable", "degraded reason %v", get(dg.event, "data.reason"))
	rc := c.cliT(t, 2*time.Minute, "recover", a).ok(t)
	_ = rc
	c.waitState(t, a, 5*time.Minute, "ready", "idle", "running")
	waitFor(t, 2*time.Minute, 2*time.Second, "healthy after recover", func() (bool, string) {
		sb := c.sandbox(t, a)
		return gets(sb, "health") == "ok", fmt.Sprintf("health=%v reason=%v", sb["health"], sb["healthReason"])
	})
	c.waitHook(t, "sandbox.recovered", a, time.Minute)
	expect(t, qemuPID(a) != 0 && qemuPID(a) != pid, "recover did not replace the hung qemu (old %d, now %d)", pid, qemuPID(a))
	expect(t, strings.TrimSpace(c.sh(t, a, "cat /home/user/keep/data2.txt")) == "persist-2", "data after recover")

	// Delete + deletion operation.
	del := c.cliT(t, 3*time.Minute, "delete", a, "--yes").ok(t)
	op := gets(del.event("deleting"), "operation.id")
	if op == "" {
		op = gets(del.last(), "operation.id")
	}
	if op == "" {
		t.Fatalf("no deletion operation id: %v", del)
	}
	waitFor(t, 2*time.Minute, time.Second, "deletion completed", func() (bool, string) {
		s := c.cli(t, "deletion", "status", op).last()
		st := gets(s, "operation.status")
		if st == "" {
			st = gets(s, "status")
		}
		return st == "completed", st
	})
	gr := c.api(t, "GET", "/sandboxes/"+a, nil)
	expect(t, gr.status == 404, "GET deleted sandbox: %v", gr)
	waitFor(t, 60*time.Second, time.Second, "qemu gone after delete", func() (bool, string) { return qemuPID(a) == 0, "" })
}

// deleteDuringUpload (B1): a sandbox deleted while its stop snapshot still
// uploads; its operation waits (waiting_for_uploads) and reports completed
// only once nothing of it is left on the server or the node.
func (c *cluster) deleteDuringUpload(t *testing.T, n *node) {
	b := c.newSandbox(t, "--name", "lc-upload", "--node", n.name)
	marker := "SECRET-UPLOAD-" + b
	c.sh(t, b, "head -c 1024M /dev/urandom > /home/user/big.bin && echo "+marker+" > /home/user/marker.txt && sync")
	r := c.api(t, "POST", "/sandboxes/"+b+"/stop", map[string]any{})
	expect(t, r.status/100 == 2, "stop: %v", r)
	waitFor(t, 6*time.Minute, 50*time.Millisecond, "archived", func() (bool, string) { st := c.state(t, b); return st == "archived", st })
	req, _ := http.NewRequest("DELETE", c.apiBase+"/sandboxes/"+b, nil)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("X-CrowdVM-Confirm-Delete", b)
	resp, err := httpc.Do(req)
	must(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	op := gets(apiResp{status: resp.StatusCode, body: body}.json(), "operation.id")
	expect(t, resp.StatusCode == 202 && op != "", "delete: %d %s", resp.StatusCode, body)
	leftovers := func() []string {
		var out []string
		for _, dir := range []string{filepath.Join(c.root, "server", "blobs", "snapshot"), filepath.Join(n.dir, "vm", "disks")} {
			if o, _ := exec.Command("grep", "-rlF", marker, dir).Output(); len(o) > 0 {
				out = append(out, strings.Fields(string(o))...)
			}
		}
		disks, _ := filepath.Glob(filepath.Join(n.dir, "vm", "disks", b+"-*"))
		metas, _ := exec.Command("grep", "-lF", b, "-r", filepath.Join(n.dir, "vm", "snapshots")).Output()
		return append(append(out, disks...), strings.Fields(string(metas))...)
	}
	stages := map[string]bool{}
	waitFor(t, 5*time.Minute, 100*time.Millisecond, "deletion during upload completed", func() (bool, string) {
		o := get(c.api(t, "GET", "/deletion-operations/"+op, nil).json(), "operation")
		m, _ := o.(map[string]any)
		st, _ := m["status"].(string)
		stage, _ := m["stage"].(string)
		stages[stage] = true
		if st != "completed" {
			return false, st + "/" + stage
		}
		left := leftovers() // checked the moment the op says completed
		expect(t, len(left) == 0, "op completed but data of %s is left: %v", b, left)
		return true, ""
	})
	t.Logf("delete during upload: stages seen %v", stages)
	// The leftovers check above is the contract. Whether the op had to wait
	// depends on timing: an upload that finished before the delete needs no
	// wait (the server tests cover the waiting path deterministically).
	if !stages["waiting_for_uploads"] {
		t.Logf("the upload finished before the delete reached it; no wait was needed")
	}
}

// ---------- snapshots (browse while stopped) ----------

func testSnapshots(t *testing.T, c *cluster) {
	s := c.newSandbox(t, "--name", "snap-src")
	c.sh(t, s, "mkdir -p /home/user/snapdir/sub && echo snap-a > /home/user/snapdir/a.txt && echo snap-b > /home/user/snapdir/sub/b.txt && sync")
	c.cliT(t, 6*time.Minute, "stop", s, "--wait").ok(t)

	ls := c.cli(t, "snapshots", s).ok(t).last()
	expect(t, len(list(ls, "snapshots")) >= 1, "snapshots list: %v", ls)
	lt := c.cli(t, "snapshot", "latest", s).ok(t).last()
	snap := gets(lt, "snapshot.id")
	if snap == "" {
		snap = gets(lt, "id")
	}
	if !strings.HasPrefix(snap, "snap_") {
		t.Fatalf("snapshot latest: %v", lt)
	}

	tr := c.cliT(t, 5*time.Minute, "snapshot", "tree", snap).ok(t)
	expectContains(t, tr.out, "snapdir/sub/b.txt", "snapshot tree")

	f := c.api(t, "GET", "/snapshots/"+snap+"/files?path=snapdir/a.txt", nil)
	expect(t, f.status == 200 && string(f.body) == "snap-a\n", "snapshot file: %d %q", f.status, tail(string(f.body), 200))
	dir := c.api(t, "GET", "/snapshots/"+snap+"/files?path=snapdir", nil)
	expect(t, dir.status == 200 && dir.header.Get("X-Snapshot-Entry-Kind") == "dir", "snapshot dir: %v", dir.status)
	names := tarNames(dir.body)
	expect(t, contains(names, "snapdir/sub/b.txt"), "dir tar entries %v", names)
	nf := c.api(t, "GET", "/snapshots/"+snap+"/files?path=nope.txt", nil)
	expect(t, nf.status == 404, "missing file: %d", nf.status)

	out := t.TempDir()
	pr := c.cliT(t, 5*time.Minute, "snapshot", "pull", snap, "-o", out, "--path", "/home/user/snapdir").ok(t)
	b, err := os.ReadFile(filepath.Join(out, "home_user/snapdir/sub/b.txt"))
	expect(t, err == nil && string(b) == "snap-b\n", "snapshot pull: %v %q (%s)", err, b, pr.out)
	// Boat: `snapshot pull <sandboxId>` pulls the newest snapshot of the sandbox (opus-1 #9).
	out2 := t.TempDir()
	pr = c.cliT(t, 5*time.Minute, "snapshot", "pull", s, "-o", out2, "--path", "/home/user/snapdir/a.txt").ok(t)
	b, err = os.ReadFile(filepath.Join(out2, "home_user/snapdir/a.txt"))
	expect(t, err == nil && string(b) == "snap-a\n" && gets(pr.last(), "snapshotId") == snap, "snapshot pull by sandbox id: %v %q (%s)", err, b, pr.out)

	dl := c.api(t, "GET", "/snapshots/"+snap+"/download", nil)
	if dl.status != 200 {
		t.Errorf("snapshot download: %v", dl)
	} else if ch := list(dl.json(), "chunks"); len(ch) == 0 {
		t.Errorf("snapshot download: no chunks: %v", dl)
	} else if u := gets(ch[0], "signedUrl"); u != "" {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Range", "bytes=0-15")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("chunk download: %v", err)
		} else {
			hd, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			expect(t, resp.StatusCode/100 == 2 && bytes.HasPrefix(hd, []byte("QFI")), "chunk: %d %q", resp.StatusCode, hd)
		}
	}

	// Snapshots stay browsable after another resume/stop; then delete them all.
	c.cli(t, "snapshot", "delete", "--all", s, "--yes").ok(t)
	ls = c.cli(t, "snapshots", s).ok(t).last()
	expect(t, len(list(ls, "snapshots")) == 0, "after snapshot delete --all: %v", ls)

	// Boat: once the snapshot data is gone, resume and fork fail with 409 snapshot_data_expired
	// (opus-2 #1: CrowdVM booted a blank disk and said ready).
	rr := c.cliT(t, 3*time.Minute, "resume", s)
	expect(t, rr.code != 0 && strings.Contains(rr.out+rr.err, "snapshot_data_expired"), "resume after snapshot delete --all: %v", rr)
	for _, act := range []string{"resume", "fork"} {
		r := c.api(t, "POST", "/sandboxes/"+s+"/"+act, map[string]any{})
		expect(t, r.status == 409 && gets(r.json(), "code") == "snapshot_data_expired", "%s after snapshot delete: %v", act, r)
	}
	expect(t, c.state(t, s) == "archived", "state after the refused resume: %s", c.state(t, s))

	c.restartDuringUpload(t)
	c.deleteDuringUpload(t, c.nodes[0])
	c.racePeriodicUpload(t)
}

// restartDuringUpload (opus-19 N1): the node restarts (SIGTERM, as an
// owner does to apply a config change) while it uploads a stopped
// sandbox's disk. The restarted node uploads the layers when the server
// asks, and a resume onto the other node comes up with the data.
func (c *cluster) restartDuringUpload(t *testing.T) {
	n1, n2 := c.nodes[0], c.nodes[1]
	// No periodic layer may take the write first: the stop's pre-flight save
	// then holds all 4 GiB (a periodic layer left a 12 MB stop layer that
	// uploaded before the SIGTERM landed). Node 1 reaches the server through
	// a link the test slows down (from here on, also for racePeriodicUpload).
	// Node 1's guests reach the server through a pinhole to the node's server
	// address, so node 1 goes back to it after the group (groups of merged
	// lanes may follow on this cluster); the group's guests on node 1 need
	// no server API meanwhile.
	link := c.nodeLink(t)
	c.killNode(t, n1)
	t.Cleanup(func() {
		select {
		case <-n1.proc.done: // a failed step left it stopped
		default:
			c.killNode(t, n1)
		}
		c.nodeCmd(t, n1, 30*time.Second, "config", "set", "server", c.url)
		c.startNode(t, n1)
	})
	c.nodeCmd(t, n1, 30*time.Second, "config", "set", "snapshot-interval", "off")
	c.nodeCmd(t, n1, 30*time.Second, "config", "set", "server", link.url())
	c.startNode(t, n1)
	src := c.newSandbox(t, "--name", "restart-src", "--node", n1.name)
	const sumCmd = "sha256sum /home/user/big.bin | cut -c1-64"
	r := c.execR(t, src, "head -c 4096M /dev/urandom > /home/user/big.bin && sync && "+sumCmd, "--timeout", "600")
	expect(t, r.code == 0, "write: %v", r)
	sum := strings.TrimSpace(r.out)
	// The 4 GiB upload must still run when the node restarts, however long
	// the guest takes to prepare the stop on a busy machine (38 s were seen,
	// longer than the upload at full speed).
	link.slow(t)
	r2 := c.api(t, "POST", "/sandboxes/"+src+"/stop", map[string]any{})
	expect(t, r2.status/100 == 2, "stop: %v", r2)
	waitFor(t, 6*time.Minute, 50*time.Millisecond, "archived", func() (bool, string) { st := c.state(t, src); return st == "archived", st })
	var unstored []string
	for _, sn := range list(c.api(t, "GET", "/sandboxes/"+src+"/snapshots", nil).json(), "snapshots") {
		if get(sn, "uploaded") == false {
			unstored = append(unstored, gets(sn, "id"))
		}
	}
	expect(t, len(unstored) > 0, "the upload ended before the restart; the restart was not exercised")
	c.killNode(t, n1)
	link.full()
	// Its layers are only on the stopped node: a resume elsewhere fails at
	// once with a retryable error instead of waiting for the boot timeout.
	rr := c.api(t, "POST", "/sandboxes/"+src+"/resume", map[string]any{"placement": map[string]any{"node": n2.name}})
	expect(t, rr.status == 409 && gets(rr.json(), "code") == "snapshot_unavailable", "resume elsewhere with the holder offline: %v", rr)
	logStart := 0
	if fi, err := os.Stat(n1.proc.log); err == nil {
		logStart = int(fi.Size())
	}
	c.nodeCmd(t, n1, 30*time.Second, "config", "set", "snapshot-interval", "60s")
	c.startNode(t, n1)
	t.Logf("restarted %s with unstored layers %v", n1.name, unstored)
	c.cliT(t, 15*time.Minute, "resume", src, "--node", n2.name).ok(t)
	expect(t, c.nodeOf(t, src) == n2.id, "%s on %s, want %s", src, c.nodeOf(t, src), n2.id)
	r = c.execR(t, src, sumCmd, "--timeout", "600")
	expect(t, strings.TrimSpace(r.out) == sum, "data after the restart: sha256 %q, want %s", r.out, sum)
	log, _ := os.ReadFile(n1.proc.log)
	expect(t, len(log) > logStart && strings.Contains(string(log[logStart:]), "server asked for a snapshot"), "the restarted node was never asked for its layers")
	c.deleteSandbox(t, src)
}

// killNode stops n with SIGTERM (an owner applying a config change) and
// waits until the server sees it offline.
func (c *cluster) killNode(t *testing.T, n *node) {
	t.Helper()
	must(t, n.proc.cmd.Process.Signal(syscall.SIGTERM))
	select {
	case <-n.proc.done:
	case <-time.After(time.Minute):
		t.Fatalf("%s did not exit on SIGTERM", n.name)
	}
	waitFor(t, time.Minute, 200*time.Millisecond, n.name+" offline", func() (bool, string) { return !c.nodeOnline(t, n), "" })
}

// startNode starts n again and waits until it is online.
func (c *cluster) startNode(t *testing.T, n *node) {
	t.Helper()
	n.proc = c.start(t, n.name, c.nodeBin(), "run", "--data-dir", n.dir)
	waitFor(t, time.Minute, 200*time.Millisecond, n.name+" online", func() (bool, string) { return c.nodeOnline(t, n), "" })
}

func (c *cluster) nodeOnline(t *testing.T, n *node) bool {
	for _, x := range list(c.api(t, "GET", "/nodes", nil).json(), "nodes") {
		if gets(x, "id") == n.id {
			return get(x, "online") == true
		}
	}
	return false
}

// racePeriodicUpload (opus-18 B2/B3): while a large periodic layer uploads,
// a stop then a resume onto the other node, and a fork onto the other node,
// build on it. Both wait for the upload and come up there with the data;
// the resume never answers resume_failed. (The stop goes first: two layers
// on the periodic one would make the stop merge it, which takes longer
// than the upload on loopback.)
func (c *cluster) racePeriodicUpload(t *testing.T) {
	n1, n2 := c.nodes[0], c.nodes[1]
	src := c.newSandbox(t, "--name", "race-src", "--node", n1.name)
	long := func(id, script string) string {
		t.Helper()
		r := c.execR(t, id, script, "--timeout", "600")
		if r.code != 0 {
			t.Fatalf("exec %q in %s: %v", script, id, r)
		}
		return strings.TrimSpace(r.out)
	}
	const sumCmd = "sha256sum /home/user/big.bin | cut -c1-64"
	sum := long(src, "head -c 6144M /dev/urandom > /home/user/big.bin && sync && "+sumCmd)
	// The next periodic layer (interval 60 s) holds the write. The server
	// lists it from the moment the node took it, not stored yet.
	var pending string
	waitFor(t, 3*time.Minute, 50*time.Millisecond, "a large periodic layer in flight", func() (bool, string) {
		for _, sn := range list(c.api(t, "GET", "/sandboxes/"+src+"/snapshots", nil).json(), "snapshots") {
			size, _ := get(sn, "sizeBytes").(float64)
			if gets(sn, "trigger") == "periodic" && get(sn, "uploaded") == false && size > 2<<30 {
				pending = gets(sn, "id")
				return true, ""
			}
		}
		return false, "none yet"
	})
	// It keeps uploading through the stop and the fork, however busy the
	// machine is (restartDuringUpload put node 1 on the link).
	link := c.nodeLink(t)
	link.slow(t)
	stored := func() bool {
		for _, sn := range list(c.api(t, "GET", "/sandboxes/"+src+"/snapshots", nil).json(), "snapshots") {
			if gets(sn, "id") == pending {
				return get(sn, "uploaded") == true
			}
		}
		return true
	}
	r := c.api(t, "POST", "/sandboxes/"+src+"/stop", map[string]any{})
	expect(t, r.status/100 == 2, "stop: %v", r)
	waitFor(t, 6*time.Minute, 50*time.Millisecond, "archived", func() (bool, string) { st := c.state(t, src); return st == "archived", st })
	stopRaced := !stored()
	r = c.api(t, "POST", "/sandboxes/"+src+"/fork", map[string]any{"name": "race-fork", "placement": map[string]any{"node": n2.name}})
	fork := gets(r.json(), "sandbox.id")
	expect(t, r.status == 202 && fork != "", "fork during the upload: %v", r)
	c.cleanupSandbox(t, fork)
	forkRaced := !stored()
	link.full()
	t.Logf("periodic layer %s still uploading after the stop: %v, at the fork: %v", pending, stopRaced, forkRaced)
	expect(t, stopRaced && forkRaced, "the upload of %s ended before the stop or the fork; the race was not exercised", pending)
	c.cliT(t, 10*time.Minute, "resume", src, "--node", n2.name).ok(t)
	c.waitState(t, fork, 10*time.Minute, "ready", "error")
	for _, id := range []string{src, fork} {
		expect(t, c.nodeOf(t, id) == n2.id, "%s on %s, want %s", id, c.nodeOf(t, id), n2.id)
		got := long(id, sumCmd)
		expect(t, got == sum, "data of %s after the race: sha256 %s, want %s", id, got, sum)
	}
}

func tarNames(b []byte) []string {
	var out []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			return out
		}
		out = append(out, strings.TrimPrefix(h.Name, "./"))
	}
}

// ---------- saved snapshots + share codes ----------

func testSavedSnapshots(t *testing.T, c *cluster) {
	bob := c.second(t)
	// The saver's environment has a secret file and a variable.
	env := "e2e-scrub"
	c.cli(t, "env", "new", env).ok(t)
	t.Cleanup(func() { c.cli(t, "env", "rm", env) })
	c.cliIn(t, "saver-secret-value\n", "env", "set-file", env, ".config/e2e/secret.txt").ok(t)
	c.cli(t, "env", "set-var", env, "SAVER_VAR=saver-only").ok(t)

	x := c.newSandbox(t, "--environment", env, "--name", "saver")
	expect(t, strings.TrimSpace(c.sh(t, x, "cat ~/.config/e2e/secret.txt")) == "saver-secret-value", "secret file in saver's sandbox")
	c.sh(t, x, `mkdir -p ~/keep ~/.ssh ~/.config/gh && echo kept > ~/keep/data.txt &&
		echo fake-private-key > ~/.ssh/id_ed25519 && echo "ssh-ed25519 AAAAsaverkey saver@x" >> ~/.ssh/authorized_keys &&
		echo https://saver:tok@github.com > ~/.git-credentials && echo "github.com: {oauth_token: saver}" > ~/.config/gh/hosts.yml && sync`)

	sv := c.cliT(t, 6*time.Minute, "snapshot", x, "e2e-saved").ok(t).last()
	expect(t, gets(sv, "status") != "failed", "save: %v", sv)
	expectContains(t, c.cli(t, "snapshot", "list").ok(t).out, "e2e-saved", "snapshot list")
	info := c.cli(t, "snapshot", "info", "e2e-saved").ok(t).last()
	expect(t, gets(info, "status") == "ready" || get(info, "available") == true || get(info, "snapshot.available") == true, "snapshot info: %v", info)
	c.deleteSandbox(t, x)

	// Own deploy (+ --host at create): data and own secrets are kept.
	y := c.newSandbox(t, "--from", "e2e-saved", "--name", "own-deploy", "--host", "8080")
	expect(t, strings.TrimSpace(c.sh(t, y, "cat ~/keep/data.txt")) == "kept", "own deploy data")
	hl := c.cli(t, "host", "list", y).ok(t)
	expectContains(t, hl.out, "8080", "--host at create")
	c.deleteSandbox(t, y)

	// Share code → another account deploys it: credentials are scrubbed.
	sh := c.cli(t, "snapshot", "share", "e2e-saved").ok(t).last()
	code := gets(sh, "shareCode")
	shareID := gets(sh, "share.id")
	if !strings.HasPrefix(code, "csh_") && !strings.HasPrefix(code, "bsh_") {
		t.Fatalf("share: %v", sh)
	}
	expectContains(t, c.cli(t, "snapshot", "shares", "e2e-saved").ok(t).out, shareID, "snapshot shares")

	r := c.cliWith(t, cliOpt{cfgDir: bob.cfg, timeout: 8 * time.Minute}, "new", "--json", "--type", "small", "--from", code, "--name", "foreign")
	z := gets(r.event("ready"), "id")
	if ev := r.event("created"); ev != nil {
		zid := gets(ev, "id")
		t.Cleanup(func() { c.cliWith(t, cliOpt{cfgDir: bob.cfg}, "delete", zid, "--yes") })
	}
	if z == "" {
		t.Fatalf("bob new --from share code: %v", r)
	}
	bsh := func(script string) string {
		r := c.cliWith(t, cliOpt{cfgDir: bob.cfg, timeout: 2 * time.Minute}, "exec", z, "--", "bash", "-lc", script)
		if r.code != 0 {
			t.Errorf("bob exec %q: %v", script, r)
		}
		return strings.TrimSpace(r.out)
	}
	expect(t, bsh("cat ~/keep/data.txt") == "kept", "foreign deploy lost ordinary data")
	for _, p := range []string{"~/.config/e2e/secret.txt", "~/.ssh/id_ed25519", "~/.git-credentials", "~/.config/gh/hosts.yml"} {
		expect(t, bsh("test -e "+p+" && echo PRESENT || echo gone") == "gone", "credential scrub: %s survived into another account's deploy", p)
	}
	expect(t, !strings.Contains(bsh("cat ~/.ssh/authorized_keys 2>/dev/null"), "saverkey"), "saver's authorized_keys survived")
	expect(t, bsh("echo ${SAVER_VAR:-unset}") == "unset", "saver's env var reached the foreign deploy")
	expect(t, strings.Contains(bsh("sudo cat /var/lib/crowdvm/scrub.json"), `"done":true`), "scrub.json not done")

	// Revoke the share: the code stops working.
	c.cli(t, "snapshot", "unshare", "e2e-saved", shareID).ok(t)
	r = c.cliWith(t, cliOpt{cfgDir: bob.cfg, timeout: 2 * time.Minute}, "new", "--json", "--type", "small", "--from", code, "--fail-fast")
	if ev := r.event("created"); ev != nil {
		zid := gets(ev, "id")
		t.Cleanup(func() { c.cliWith(t, cliOpt{cfgDir: bob.cfg}, "delete", zid, "--yes") })
	}
	expect(t, r.code != 0, "deploy from a revoked share code succeeded")
	c.cli(t, "snapshot", "rm", "e2e-saved").ok(t)
	expect(t, !strings.Contains(c.cli(t, "snapshot", "list").out, "e2e-saved"), "snapshot rm")

	testDeployAfterMerge(t, c)
}

// testDeployAfterMerge (review sonnet-13 B1): on one node, a deploy's
// compaction merges the saved snapshot's layer away while the saver still
// builds on it; the next deploy of that snapshot on the node must stop and
// resume with its data, and a snapshot saved from it must deploy too.
func testDeployAfterMerge(t *testing.T, c *cluster) {
	n1 := c.nodes[0]
	on := []string{"--node", n1.name}
	a := c.newSandbox(t, append(on, "--name", "merge-saver")...)
	c.sh(t, a, "echo base > ~/base.txt && sync")
	c.cliT(t, 6*time.Minute, "snapshot", a, "e2e-merge-base").ok(t)
	t.Cleanup(func() { c.cli(t, "snapshot", "rm", "e2e-merge-base") })
	c.cliT(t, 6*time.Minute, "stop", a, "--wait").ok(t) // its layers still build on the saved one

	stopResume := func(id string) {
		c.cliT(t, 6*time.Minute, "stop", id, "--wait").ok(t)
		c.cliT(t, 10*time.Minute, append([]string{"resume", id}, on...)...).ok(t)
	}
	// Three more layers on the saved one: the next snapshot merges them all
	// (fanout 4), the saved layer included.
	b := c.newSandbox(t, append(on, "--from", "e2e-merge-base", "--name", "merge-first")...)
	for i := range 3 {
		stopResume(b)
		c.sh(t, b, fmt.Sprintf("echo %d > ~/b%d.txt && sync", i, i))
	}
	c.cliT(t, 6*time.Minute, "stop", b, "--wait").ok(t)
	log, _ := os.ReadFile(filepath.Join(c.logs, n1.name+".log"))
	expect(t, strings.Contains(string(log), b+": merged"), "no compaction of %s in the node log", b)

	d := c.newSandbox(t, append(on, "--from", "e2e-merge-base", "--name", "merge-second")...)
	expect(t, c.nodeOf(t, d) == n1.id, "deploy on %s, want %s", c.nodeOf(t, d), n1.id)
	c.sh(t, d, "echo second > ~/d.txt && sync")
	stopResume(d)
	expect(t, strings.TrimSpace(c.sh(t, d, "cat ~/base.txt ~/d.txt")) == "base\nsecond", "deploy after merge lost data")
	c.cliT(t, 6*time.Minute, "snapshot", d, "e2e-merge-next").ok(t)
	t.Cleanup(func() { c.cli(t, "snapshot", "rm", "e2e-merge-next") })
	c.deleteSandbox(t, d)
	e := c.newSandbox(t, append(on, "--from", "e2e-merge-next", "--name", "merge-third")...)
	stopResume(e)
	expect(t, strings.TrimSpace(c.sh(t, e, "cat ~/base.txt ~/d.txt")) == "base\nsecond", "deploy of a snapshot saved from it lost data")
	for _, id := range []string{e, b, a} {
		c.deleteSandbox(t, id)
	}
}

// nodeLink is a TCP proxy between a node and the server whose node→server
// direction the test can slow down, so that an upload race does not depend
// on how busy the machine's disk is. One per cluster, closed on teardown.
type nodeLink struct {
	l    net.Listener
	rate atomic.Int64 // node→server bytes per second; 0: full speed
}

func (c *cluster) nodeLink(t testing.TB) *nodeLink {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.link == nil {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		must(t, err)
		c.link = &nodeLink{l: l}
		go c.link.serve(fmt.Sprintf("127.0.0.1:%d", c.port))
	}
	return c.link
}

func (k *nodeLink) url() string { return "http://" + k.l.Addr().String() }

// slow: 16 MB/s, a 4 GiB upload takes over 4 minutes.
func (k *nodeLink) slow(t testing.TB) { k.rate.Store(16 << 20); t.Cleanup(k.full) }
func (k *nodeLink) full()             { k.rate.Store(0) }

func (k *nodeLink) serve(target string) {
	for {
		in, err := k.l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer in.Close()
			out, err := net.Dial("tcp", target)
			if err != nil {
				return
			}
			defer out.Close()
			go func() { _, _ = io.Copy(in, out); in.Close(); out.Close() }()
			buf := make([]byte, 64<<10)
			for {
				n, err := in.Read(buf)
				if n > 0 {
					if _, werr := out.Write(buf[:n]); werr != nil {
						return
					}
					if r := k.rate.Load(); r > 0 {
						time.Sleep(time.Duration(int64(n) * int64(time.Second) / r))
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
}

package guest_test

// Real-VM checks of the guest agent (root mode) on a CrowdVM image:
//
//	CROWDVM_E2E=1 CROWDVM_E2E_IMAGE=~/.local/share/crowdvm-dev/images/ubuntu-24.04-crowdvm-v1.qcow2 \
//	  go test -v -run VM -timeout 30m ./internal/guest/
//
// One 2 GiB VM at a time (create, live snapshot + fork, stop + resume).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/vm/qemu"
	"github.com/crowdvm/crowdvm/internal/proto"
)

type vmEnv struct {
	t   *testing.T
	d   *qemu.Driver
	img string
}

type guestAPI struct {
	t    *testing.T
	addr string
	tok  string
}

func (g guestAPI) call(method, path string, body any) (int, map[string]any) {
	g.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+g.addr+path, rd)
	req.Header.Set("Authorization", "Bearer "+g.tok)
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		g.dumpJournal()
		g.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

// dumpJournal logs the agent's journal (after a crash it restarts).
func (g guestAPI) dumpJournal() {
	for i := 0; i < 20; i++ {
		time.Sleep(time.Second)
		b, _ := json.Marshal(map[string]any{"command": "sudo journalctl -u crowdvm-guest --no-pager -n 60", "timeoutSeconds": 30})
		req, _ := http.NewRequest("POST", "http://"+g.addr+"/v1/commands", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+g.tok)
		resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
		if err != nil {
			continue
		}
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		resp.Body.Close()
		g.t.Logf("guest journal:\n%v", m["stdout"])
		return
	}
}

func (g guestAPI) ok(method, path string, body any) map[string]any {
	g.t.Helper()
	st, m := g.call(method, path, body)
	if st != 200 {
		g.t.Fatalf("%s %s: %d %v", method, path, st, m)
	}
	return m
}

// sh runs a command as user via POST /v1/commands and returns stdout.
func (g guestAPI) sh(cmd string) string {
	g.t.Helper()
	m := g.ok("POST", "/v1/commands", map[string]any{"command": cmd, "timeoutSeconds": 120})
	if code, _ := m["exitCode"].(float64); code != 0 || m["exitCode"] == nil {
		g.t.Fatalf("%q: %v", cmd, m)
	}
	return strings.TrimSpace(m["stdout"].(string))
}

func buildStatic(t *testing.T, pkg, out string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("build %s: %v\n%s", pkg, err, b)
	}
	return err
}

func (e *vmEnv) start(id, parent string, boot proto.BootKind, g proto.GuestConfig) guestAPI {
	e.t.Helper()
	g.SandboxID, g.Token, g.Hostname, g.Boot, g.FirstBoot = id, "tok-"+id, id, boot, boot == proto.BootCreate
	spec := proto.VMSpec{SandboxID: id, ImageID: "img", ParentID: parent,
		Size: proto.Size{Name: "t", VCPU: 2, MemoryMiB: 2048, DiskGiB: 10}, Guest: g}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := e.d.Start(ctx, spec); err != nil {
		e.t.Fatalf("start %s: %v", id, err)
	}
	if err := e.d.Wait(ctx, id); err != nil {
		e.t.Fatalf("wait %s: %v", id, err)
	}
	addr, err := e.d.GuestAddr(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return guestAPI{t: e.t, addr: addr, tok: "tok-" + id}
}

func TestVMGuestCore(t *testing.T) {
	img := os.Getenv("CROWDVM_E2E_IMAGE")
	if os.Getenv("CROWDVM_E2E") != "1" || img == "" || runtime.GOOS != "linux" {
		t.Skip("set CROWDVM_E2E=1 and CROWDVM_E2E_IMAGE=<crowdvm qcow2>")
	}
	dir := e2eDir(t) // not /tmp: the image is copied there
	bin := filepath.Join(dir, "crowdvm-guest")
	if err := buildStatic(t, "./cmd/crowdvm-guest", bin); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(dir, "crowdvm")
	if buildStatic(t, "./cmd/crowdvm", cli) != nil {
		cli = ""
	}
	d, err := qemu.New(qemu.Options{DataDir: filepath.Join(dir, "node"), GuestBinary: bin, CLIBinary: cli,
		BootTimeout: 3 * time.Minute, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // never leave VMs behind, also when the test fails
		for _, st := range d.List() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			_ = d.Destroy(ctx, st.SandboxID)
			cancel()
		}
		d.Close()
	})
	imp := filepath.Join(dir, "img.qcow2") // ImportImage moves the file
	if out, err := exec.Command("cp", "--reflink=auto", img, imp).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v %s", err, out)
	}
	if err := d.Store().ImportImage("img", imp); err != nil {
		t.Fatal(err)
	}
	e := &vmEnv{t: t, d: d, img: img}

	// ---- create ---------------------------------------------------------
	t0 := time.Now()
	g := e.start("sbA", "", proto.BootCreate, proto.GuestConfig{
		Env:         map[string]string{"FOO": "bar"},
		SecretFiles: []proto.File{{Path: ".config/demo/secret", Content: []byte("s3cret")}},
	})
	t.Logf("TIMING create→healthy %s", time.Since(t0).Round(10*time.Millisecond))
	h := g.ok("GET", "/v1/health", nil)
	t.Logf("health %v", h)

	if got := g.sh(`echo "$DISPLAY|$FOO|$LANG"`); !strings.HasPrefix(got, ":0|bar|") {
		t.Errorf("command env %q", got)
	}
	cg := g.sh("cat /proc/self/cgroup")
	if !strings.Contains(cg, "user.slice/crowdvm-workloads.service/cmd_") {
		t.Errorf("command not in its workload cgroup: %s", cg)
	}
	if mm := g.sh("systemctl show user.slice -p MemoryMax --value"); mm == "infinity" || mm == "" {
		t.Errorf("user.slice MemoryMax %q", mm)
	} else {
		t.Logf("user.slice MemoryMax=%s", mm)
	}
	if adj := g.sh("cat /proc/self/oom_score_adj"); adj != "0" {
		t.Errorf("command oom_score_adj %s", adj)
	}
	if adj := g.sh("cat /proc/$(pgrep -xo crowdvm-guest)/oom_score_adj"); adj != "-1000" {
		t.Errorf("agent oom_score_adj %s", adj)
	}

	// OOM: exit 137 + oomKilled, the guest stays up (LIFE-043)
	m := g.ok("POST", "/v1/commands", map[string]any{"command": "tail /dev/zero", "timeoutSeconds": 120})
	if m["oomKilled"] != true || m["exitCode"] != float64(137) {
		t.Errorf("OOM result %v", m)
	}
	g.ok("GET", "/v1/health", nil)

	// df gauge + metric files for OTel
	if df := g.ok("GET", "/v1/files?path=/run/crowdvm/df", nil); len(strings.Fields(df["content"].(string))) != 2 {
		t.Errorf("df %v", df)
	}
	g.ok("GET", "/v1/files?path=/proc/meminfo", nil)

	// CLI from the config drive
	if cli != "" {
		if got := g.sh("readlink /usr/local/bin/host /usr/local/bin/boat /usr/local/bin/box; test -x /usr/local/bin/crowdvm && echo ok"); got != "crowdvm\ncrowdvm\ncrowdvm\nok" {
			t.Errorf("cli install %q", got)
		}
	}
	// firewall
	t.Logf("ufw: %s", g.sh("sudo ufw status 2>&1 | head -6 || true"))

	// tracer: a systemd service's output becomes a process record (CMD-021)
	g.sh("sudo systemd-run --unit=demo-ticker --quiet sh -c 'while :; do echo tick-$$; sleep 1; done'")
	// detached command survives an agent restart (CMD-010)
	det := g.ok("POST", "/v1/commands", map[string]any{"command": "while :; do echo d; sleep 1; done", "detached": true})
	pid := int(det["processId"].(float64))
	time.Sleep(3 * time.Second)
	{ // the agent dies while answering: ignore the transport error
		b, _ := json.Marshal(map[string]any{"command": "sudo systemctl restart crowdvm-guest", "timeoutSeconds": 30})
		req, _ := http.NewRequest("POST", "http://"+g.addr+"/v1/commands", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+g.tok)
		if resp, err := (&http.Client{Timeout: time.Minute}).Do(req); err == nil {
			resp.Body.Close()
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if resp, err := http.Get("http://" + g.addr + "/v1/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("guest did not come back after restart")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if s := g.ok("GET", fmt.Sprintf("/v1/commands/%d", pid), nil); s["status"] != "running" {
		t.Errorf("detached after agent restart %v", s)
	}
	time.Sleep(2 * time.Second)
	ex := g.ok("GET", "/v1/internal/history/export?flush=1", nil)
	var procRec, sawTick bool
	for _, r := range ex["records"].([]any) {
		if r := r.(map[string]any); r["mode"] == "process" && strings.Contains(r["command"].(string), "while") {
			procRec = true
		}
	}
	for _, c := range ex["chunks"].([]any) {
		b, _ := base64.StdEncoding.DecodeString(c.(map[string]any)["data"].(string))
		sawTick = sawTick || strings.Contains(string(b), "tick-")
	}
	if !procRec || !sawTick {
		t.Errorf("process record %v / tick output %v (records %d)", procRec, sawTick, len(ex["records"].([]any)))
	}
	g.ok("POST", "/v1/internal/history/ack", map[string]any{"batch": ex["batch"]})

	// apply-config: env reaches new shells, withheld secret deleted (ENV-007)
	ap := g.ok("POST", "/v1/internal/apply-config", map[string]any{"env": map[string]string{"FOO": "baz"}, "secretFiles": []any{}})
	t.Logf("apply-config %v", ap)
	if got := g.sh(`echo $FOO; bash -lc 'echo $FOO'; test -e ~/.config/demo/secret && echo still || echo gone`); got != "baz\nbaz\ngone" {
		t.Errorf("after apply-config %q", got)
	}

	// browser view as crowdvm-browser in a mount namespace (DESK-017)
	if g.sh("command -v Xvnc >/dev/null && command -v google-chrome-stable >/dev/null && echo y || echo n") == "y" {
		g.sh("id crowdvm-browser >/dev/null 2>&1 || sudo useradd --system --home-dir /var/lib/crowdvm-browser --create-home --shell /usr/sbin/nologin crowdvm-browser; sudo chmod 0750 /home/user")
		b := g.ok("POST", "/v1/internal/browser/start", map[string]any{"profile": "/data/chrome-profile"})
		if b["isolated"] != true || b["running"] != true {
			t.Errorf("browser %v", b)
		}
		time.Sleep(3 * time.Second)
		ns := g.sh(`pid=$(pgrep -u crowdvm-browser -f -- --crowdvm-browser | head -1); echo "pid=$pid"; sudo ls -A /proc/$pid/root/home/user | wc -l; sudo stat -c %U /data/chrome-profile`)
		t.Logf("browser ns: %s", ns)
		if !strings.HasSuffix(ns, "0\ncrowdvm-browser") {
			t.Errorf("browser sees /home/user or wrong profile owner: %q", ns)
		}
	}

	// ---- live snapshot (freeze/thaw) + fork ------------------------------
	g.sh(`echo fork-me > ~/marker; mkdir -p ~/.crowdvm; printf '#!/bin/bash\necho "$(date +%%s)" > ~/pre-stop-ran\n' > ~/.crowdvm/pre-stop; chmod +x ~/.crowdvm/pre-stop`)
	hostKeyA := g.sh("cat /etc/ssh/ssh_host_ed25519_key.pub | cut -d' ' -f2")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	snap, err := d.Snapshot(ctx, "sbA", "snapLive")
	cancel()
	if err != nil {
		t.Fatalf("live snapshot: %v", err)
	}
	g.ok("GET", "/v1/health", nil) // thawed
	g.sh("touch ~/after-thaw")
	g.ok("DELETE", fmt.Sprintf("/v1/commands/%d", pid), nil)

	// ---- stop + resume ----------------------------------------------------
	ctx, cancel = context.WithTimeout(context.Background(), 4*time.Minute)
	stopRef, err := d.Stop(ctx, "sbA", "snapStop", false)
	cancel()
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	g = e.start("sbA", stopRef.ID, proto.BootResume, proto.GuestConfig{Env: map[string]string{"FOO": "resumed"}})
	if got := g.sh("cat ~/pre-stop-ran >/dev/null && echo hook; cat ~/marker; echo $FOO"); got != "hook\nfork-me\nresumed" {
		t.Errorf("after resume %q", got)
	}
	if hk := g.sh("cat /etc/ssh/ssh_host_ed25519_key.pub | cut -d' ' -f2"); hk == hostKeyA {
		t.Error("host key not regenerated on resume (SSH-005)")
	}
	if fw := g.sh("sudo ufw status 2>/dev/null | head -1 || true"); fw != "" {
		t.Logf("ufw after resume: %s", fw)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 4*time.Minute)
	if _, err := d.Stop(ctx, "sbA", "snapStop2", true); err != nil {
		t.Logf("stop resumed: %v", err)
	}
	cancel()

	// ---- fork from the live snapshot: new machine-id, journal works --------
	g = e.start("sbB", snap.ID, proto.BootFork, proto.GuestConfig{})
	if got := g.sh("cat ~/marker; test -e ~/after-thaw && echo leaked || echo clean"); got != "fork-me\nclean" {
		t.Errorf("fork content %q", got)
	}
	time.Sleep(2 * time.Second)
	j := g.sh("journalctl --no-pager -n 3 -q 2>/dev/null | wc -l; ls /var/log/journal | wc -l; cat /etc/machine-id")
	t.Logf("fork journal: %s", j)
	if strings.HasPrefix(j, "0\n") {
		t.Errorf("journal empty after fork: %s", j)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 4*time.Minute)
	if _, err := d.Stop(ctx, "sbB", "snapB", true); err != nil {
		t.Logf("stop fork: %v", err)
	}
	cancel()
}

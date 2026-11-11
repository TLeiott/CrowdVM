package guest

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestPreStopHook(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	hook := filepath.Join(e.home, ".boat/pre-stop") // compat name
	os.MkdirAll(filepath.Dir(hook), 0o755)
	os.WriteFile(hook, []byte("#!/bin/bash\necho \"$PWD\" > \"$HOME/hook-ran\"\n"), 0o755)

	m := e.mustOK("POST", "/v1/internal/prepare-stop", map[string]any{"force": true})
	if m["preStop"] != nil {
		t.Fatalf("force ran the hook: %v", m)
	}
	if _, err := os.Stat(filepath.Join(e.home, "hook-ran")); err == nil {
		t.Fatal("force ran the hook")
	}
	m = e.mustOK("POST", "/v1/internal/prepare-stop", map[string]any{})
	ps, _ := m["preStop"].(map[string]any)
	if ps == nil || num(ps["exitCode"]) != 0 || ps["path"] != "/home/user/.boat/pre-stop" {
		t.Fatalf("preStop %v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(e.home, "hook-ran")); strings.TrimSpace(string(b)) != e.home {
		t.Fatalf("hook cwd %q", b)
	}
	// ~/.crowdvm/pre-stop wins; a failing (non-executable) hook does not stop the stop
	own := filepath.Join(e.home, ".crowdvm/pre-stop")
	os.MkdirAll(filepath.Dir(own), 0o755)
	os.WriteFile(own, []byte("echo bad >&2; exit 4\n"), 0o644)
	m = e.mustOK("POST", "/v1/internal/prepare-stop", nil)
	ps, _ = m["preStop"].(map[string]any)
	if ps == nil || num(ps["exitCode"]) != 4 || !strings.Contains(ps["outputTail"].(string), "bad") || m["frozen"] != false {
		t.Fatalf("failing hook %v", m)
	}
}

func TestApplyConfigAndManagedSecrets(t *testing.T) {
	root := t.TempDir()
	home, data := filepath.Join(root, "home"), filepath.Join(root, "data")
	cfg := proto.GuestConfig{Env: map[string]string{"OLD_VAR": "1"},
		SecretFiles: []proto.File{{Path: ".config/a/secret", Content: []byte("s1")}, {Path: ".config/b/secret", Content: []byte("s2")}}}
	e := newEnvAt(t, cfg, home, data)
	e.a.BootBackground(context.Background())
	if _, err := os.Stat(filepath.Join(home, ".config/a/secret")); err != nil {
		t.Fatal("secret not written")
	}
	m := e.mustOK("POST", "/v1/internal/apply-config", map[string]any{
		"env":         map[string]string{"NEW_VAR": "2"},
		"secretFiles": []proto.File{{Path: ".config/b/secret", Content: []byte("s2b")}},
	})
	if rm := m["filesRemoved"].([]any); len(rm) != 1 || rm[0] != "/home/user/.config/a/secret" {
		t.Fatalf("apply %v", m)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/a/secret")); !os.IsNotExist(err) {
		t.Fatal("withheld secret kept")
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".config/b/secret")); string(b) != "s2b" {
		t.Fatalf("secret b %q", b)
	}
	rc, _ := os.ReadFile(filepath.Join(home, ".bashrc"))
	if !strings.Contains(string(rc), "NEW_VAR") || strings.Contains(string(rc), "OLD_VAR") {
		t.Fatalf("bashrc %s", rc)
	}
	r := e.mustOK("POST", "/v1/commands", map[string]any{"command": `echo "${OLD_VAR:-none} $NEW_VAR"`})
	if strings.TrimSpace(r["stdout"].(string)) != "none 2" {
		t.Fatalf("command env %v", r)
	}
	e.mustOK("POST", "/v1/internal/apply-config", map[string]any{"envSet": map[string]string{"PATCHED": "p"}, "envUnset": []string{"NOPE"}})
	if env := e.a.config().Env; env["PATCHED"] != "p" || env["NEW_VAR"] != "2" {
		t.Fatalf("env patch %v", env)
	}
	e.mustOK("POST", "/v1/internal/apply-config", map[string]any{"envUnset": []string{"PATCHED"}})
	if _, still := e.a.config().Env["PATCHED"]; still {
		t.Fatal("envUnset")
	}
	// An agent restart in the same boot keeps the applied config (the old
	// config drive must not bring the withheld secret back).
	e.srv.Close()
	e2 := newEnvAt(t, cfg, home, data)
	if _, err := os.Stat(filepath.Join(home, ".config/a/secret")); !os.IsNotExist(err) {
		t.Fatal("agent restart rewrote a withheld secret")
	}
	if e2.a.config().Env["NEW_VAR"] != "2" {
		t.Fatalf("applied env lost: %v", e2.a.config().Env)
	}
	// The next boot (new boot id) uses its own config: b is no longer listed.
	e2.srv.Close()
	os.Remove(filepath.Join(data, "applied-config.json"))
	newEnvAt(t, proto.GuestConfig{}, home, data)
	if _, err := os.Stat(filepath.Join(home, ".config/b/secret")); !os.IsNotExist(err) {
		t.Fatal("stale secret not deleted at boot")
	}
}

func TestScrub(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for _, f := range []string{".ssh/id_ed25519", ".ssh/id_ed25519.pub", ".ssh/authorized_keys", ".config/gh/hosts.yml", "repo/x", "keep"} {
		os.MkdirAll(filepath.Dir(filepath.Join(home, f)), 0o755)
		os.WriteFile(filepath.Join(home, f), []byte("x"), 0o600)
	}
	e := newEnvAt(t, proto.GuestConfig{Scrub: &proto.GuestScrub{ID: "scr_1", Files: []string{"keep-not-there"},
		Dirs: []string{"repo"}, SSHKeys: true, AuthorizedKeys: true, GitHubLogout: true}}, home, filepath.Join(root, "data"))
	for _, f := range []string{".ssh/id_ed25519", ".ssh/id_ed25519.pub", ".config/gh/hosts.yml", "repo"} {
		if _, err := os.Stat(filepath.Join(home, f)); !os.IsNotExist(err) {
			t.Errorf("%s not scrubbed", f)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".ssh/authorized_keys")); len(b) != 0 {
		t.Error("authorized_keys not emptied")
	}
	if _, err := os.Stat(filepath.Join(home, "keep")); err != nil {
		t.Error("unlisted file deleted")
	}
	m := e.mustOK("GET", "/v1/internal/scrub", nil)
	if m["id"] != "scr_1" || m["done"] != true {
		t.Fatalf("scrub status %v", m)
	}
	if st, m := e.do("POST", "/v1/internal/apply-config", map[string]any{"scrub": map[string]any{"id": "scr_2", "dirs": []string{"/home/user"}}}); st != 200 || m["scrub"].(map[string]any)["done"] != false {
		t.Fatalf("home must never be scrubbed: %d %v", st, m)
	}
}

func TestHistoryExportAck(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	r := e.mustOK("POST", "/v1/commands", map[string]any{"command": "echo hello; echo oops >&2"})
	id := r["commandId"].(string)
	ex := e.mustOK("GET", "/v1/internal/history/export", nil)
	if ex["batch"] == "" || len(ex["records"].([]any)) != 1 {
		t.Fatalf("export %v", ex)
	}
	got := map[string]string{}
	for _, c := range ex["chunks"].([]any) {
		c := c.(map[string]any)
		b, _ := base64.StdEncoding.DecodeString(c["data"].(string))
		if c["commandId"] != id || num(c["offset"]) != 0 {
			t.Fatalf("chunk %v", c)
		}
		got[c["stream"].(string)] = string(b)
	}
	if got["stdout"] != "hello\n" || got["stderr"] != "oops\n" {
		t.Fatalf("chunks %v", got)
	}
	// unacked: comes again; acked: gone
	if ex2 := e.mustOK("GET", "/v1/internal/history/export", nil); len(ex2["records"].([]any)) != 1 {
		t.Fatalf("unacked export %v", ex2)
	}
	e.mustOK("POST", "/v1/internal/history/ack", map[string]any{"batch": ex["batch"]})
	if ex3 := e.mustOK("GET", "/v1/internal/history/export", nil); ex3["batch"] != "" || len(ex3["records"].([]any)) != 0 {
		t.Fatalf("after ack %v", ex3)
	}
	if st, _ := e.do("POST", "/v1/internal/history/ack", map[string]any{"batch": ex["batch"]}); st != 404 {
		t.Fatalf("double ack %d", st)
	}
	// detached output follows the 30 s cadence; flush sends it at once
	d := e.mustOK("POST", "/v1/commands", map[string]any{"command": "echo a; echo b; sleep 30", "detached": true})
	time.Sleep(300 * time.Millisecond)
	ex = e.mustOK("GET", "/v1/internal/history/export", nil)
	e.mustOK("POST", "/v1/internal/history/ack", map[string]any{"batch": ex["batch"]})
	ex = e.mustOK("GET", "/v1/internal/history/export", nil)
	if len(ex["chunks"].([]any)) != 0 {
		// the first export may carry it (never exported before); the second must wait
		t.Fatalf("detached output exported before its 30 s cadence: %v", ex)
	}
	ex = e.mustOK("GET", "/v1/internal/history/export?flush=1", nil)
	if len(ex["chunks"].([]any)) != 0 && ex["batch"] == "" {
		t.Fatalf("flush %v", ex)
	}
	e.mustOK("DELETE", "/v1/commands/"+strconv.Itoa(num(d["processId"])), nil)
}

func TestMonitoringOff(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{NoMonitoring: true})
	if strings.HasPrefix(e.a.hist.dir, filepath.Join(e.data, "history")) {
		t.Fatalf("unmonitored history on disk: %s", e.a.hist.dir)
	}
	r := e.mustOK("POST", "/v1/commands", map[string]any{"command": "echo live"})
	if r["stdout"] != "live\n" {
		t.Fatalf("sync output %v", r)
	}
	if st, m := e.do("GET", "/v1/command-history", nil); st != 409 || m["code"] != "monitoring_off" {
		t.Fatalf("history %d %v", st, m)
	}
	if ex := e.mustOK("GET", "/v1/internal/history/export?flush=1", nil); ex["batch"] != "" {
		t.Fatalf("export %v", ex)
	}
	if h := e.mustOK("GET", "/v1/health", nil); h["monitoring"] != false {
		t.Fatalf("health %v", h)
	}
	e.a.hist.update(e.a.hist.get(r["commandId"].(string)), func(m *meta) { m.FinishedAt = ptr(time.Now().Add(-time.Hour)) })
	if n := e.a.hist.prune(time.Now()); n != 1 {
		t.Fatalf("pruned %d", n)
	}
}

func TestHistoryRetention(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	r := e.mustOK("POST", "/v1/commands", map[string]any{"command": "true"})
	m := e.a.hist.get(r["commandId"].(string))
	if e.a.hist.prune(time.Now()) != 0 {
		t.Fatal("fresh record pruned")
	}
	e.a.hist.update(m, func(m *meta) { m.FinishedAt = ptr(time.Now().Add(-91 * 24 * time.Hour)) })
	if e.a.hist.prune(time.Now()) != 1 {
		t.Fatal("90 d record kept")
	}
}

func TestRepoSetupBlocking(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := filepath.Join(t.TempDir(), "proj")
	for _, c := range [][]string{{"init", "-q", "-b", "main", src}, {"-C", src, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", c, out)
		}
	}
	e := newEnv(t, proto.GuestConfig{FirstBoot: true, Repos: []proto.Repo{{URL: src, SetupScript: "sleep 1", SetupBlocking: true}}})
	if !e.a.repoSetupBlocking() {
		t.Fatal("not blocking before the background work")
	}
	if st := e.mustOK("GET", "/v1/internal/setup-status", nil); st["blocking"] != true {
		t.Fatalf("status %v", st)
	}
	done := make(chan struct{})
	go func() { e.a.BootBackground(context.Background()); close(done) }()
	<-done
	if e.a.repoSetupBlocking() {
		t.Fatal("still blocking after setup")
	}
	// an agent restart in the same boot never blocks again
	a2, err := New(Options{Config: e.a.cfg, Home: e.home, DataDir: e.data, Unprivileged: true, Logger: e.a.log})
	if err != nil {
		t.Fatal(err)
	}
	a2.Boot(context.Background())
	if a2.repoSetupBlocking() {
		t.Fatal("blocking after an agent restart")
	}
}

func TestMetricFilesAndSamples(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	for _, p := range []string{"/proc/meminfo", "/proc/stat"} {
		if m := e.mustOK("GET", "/v1/files?path="+q(p), nil); !strings.Contains(m["content"].(string), map[string]string{"/proc/meminfo": "MemTotal", "/proc/stat": "cpu"}[p]) {
			t.Fatalf("%s: %v", p, m)
		}
	}
	if st, _ := e.do("GET", "/v1/files?path="+q("/etc/passwd"), nil); st != 400 {
		t.Fatalf("/etc/passwd readable: %d", st)
	}
	e.a.samples.tick(time.Now())
	e.a.samples.tick(time.Now().Add(time.Second))
	if m := e.mustOK("GET", "/v1/files?path="+q("/run/crowdvm/df"), nil); len(strings.Fields(m["content"].(string))) != 2 {
		t.Fatalf("df %v", m)
	}
	ex := e.mustOK("GET", "/v1/internal/history/export", nil)
	ss := ex["samples"].([]any)
	if len(ss) != 2 {
		t.Fatalf("samples %v", ex)
	}
	s1 := ss[1].(map[string]any)
	if num(s1["memoryTotalBytes"]) == 0 || num(s1["diskTotalBytes"]) == 0 || len(s1["processes"].([]any)) == 0 || len(s1["processes"].([]any)) > 12 {
		t.Fatalf("sample %v", s1)
	}
	e.mustOK("POST", "/v1/internal/history/ack", map[string]any{"batch": ex["batch"]})
	if ex := e.mustOK("GET", "/v1/internal/history/export", nil); len(ex["samples"].([]any)) != 0 {
		t.Fatalf("samples after ack %v", ex)
	}
}

func TestFreezeThawUnprivileged(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	if m := e.mustOK("POST", "/v1/internal/freeze", map[string]any{"timeoutSeconds": 5}); len(m["mounts"].([]any)) != 0 {
		t.Fatalf("freeze %v", m)
	}
	e.mustOK("POST", "/v1/internal/thaw", nil)
	if st, _ := e.do("POST", "/v1/internal/freeze", map[string]any{"timeoutSeconds": 9999}); st != 400 {
		t.Fatalf("bad timeout %d", st)
	}
}

func TestTracerEntries(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	tr := e.a.tracer
	pid := os.Getpid()
	ent := func(kv ...string) map[string]any {
		m := map[string]any{"_PID": strconv.Itoa(pid), "_TRANSPORT": "stdout"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tr.entry(ent("_SYSTEMD_UNIT", "myapp.service", "_CMDLINE", "/usr/bin/myapp --serve", "MESSAGE", "listening"))
	tr.entry(ent("_SYSTEMD_UNIT", "myapp.service", "_CMDLINE", "/usr/bin/myapp --serve", "MESSAGE", "boom", "PRIORITY", "3"))
	tr.entry(ent("_SYSTEMD_UNIT", "crowdvm-guest.service", "MESSAGE", "platform"))
	tr.entry(ent("_SYSTEMD_UNIT", "ssh.service", "MESSAGE", "platform"))
	tr.entry(map[string]any{"_PID": strconv.Itoa(pid), "_SYSTEMD_UNIT": "myapp.service", "MESSAGE": []any{1, 2, 3}}) // binary
	tr.entry(map[string]any{"_PID": "1", "CONTAINER_ID_FULL": "abc123", "CONTAINER_NAME": "web", "IMAGE_NAME": "nginx", "MESSAGE": "GET /"})
	recs, _, _ := e.a.hist.list(map[string]bool{"process": true}, "", "", 100)
	if len(recs) != 2 {
		t.Fatalf("records %+v", recs)
	}
	byCmd := map[string]CommandRecord{}
	for _, r := range recs {
		byCmd[r.Command] = r
	}
	app := byCmd["/usr/bin/myapp --serve"]
	if app.Mode != "process" || app.ProcessID == nil || *app.ProcessID != pid || app.StdoutBytes != int64(len("listening\n")) || app.StderrBytes != 5 {
		t.Fatalf("app record %+v", app)
	}
	if _, ok := byCmd["docker container web (nginx)"]; !ok {
		t.Fatalf("container record missing: %+v", recs)
	}
	// rate limit: past the 1 MiB burst the output is dropped and flagged
	big := strings.Repeat("x", 600<<10)
	for i := 0; i < 3; i++ {
		tr.entry(ent("_SYSTEMD_UNIT", "myapp.service", "_CMDLINE", "/usr/bin/myapp --serve", "MESSAGE", big))
	}
	m := e.a.hist.get(app.CommandID)
	if r := e.a.hist.record(m); !r.OutputTruncated || r.StdoutBytes > traceBurst+(64<<10) {
		t.Fatalf("rate limit %+v", r)
	}
}

func TestBrowserProfileValidation(t *testing.T) {
	for p, ok := range map[string]bool{"": true, "/data/chrome": true, DefaultBrowserProfile: true, "/home/user/x": false,
		"/etc/x": false, "relative": false, "/data/it's": false, "/": false, "/tmp/p": false} {
		if _, err := validBrowserProfile(p); (err == nil) != ok {
			t.Errorf("%q: err=%v", p, err)
		}
	}
}

func TestDetachedLogPath(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	d := e.mustOK("POST", "/v1/commands", map[string]any{"command": "echo out; echo err >&2", "detached": true})
	pid := num(d["processId"])
	if d["logPath"] != "/home/user/.ascii/processes/"+strconv.Itoa(pid)+".log" {
		t.Fatalf("logPath %v", d)
	}
	time.Sleep(300 * time.Millisecond)
	b, err := os.ReadFile(filepath.Join(e.home, ".ascii/processes", strconv.Itoa(pid)+".err.log"))
	if err != nil || string(b) != "err\n" {
		t.Fatalf("err log %q %v", b, err)
	}
}

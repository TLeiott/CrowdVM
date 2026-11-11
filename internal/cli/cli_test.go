package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/crowdvm/crowdvm/internal/client/clienttest"
	"github.com/spf13/cobra"
)

type env struct {
	t      *testing.T
	f      *clienttest.Fake
	dir    string
	vars   map[string]string
	ran    []*exec.Cmd
	opened []string
	tools  map[string]bool // programs LookPath finds
}

func newEnv(t *testing.T) *env {
	f := clienttest.New(t)
	dir := t.TempDir()
	return &env{t: t, f: f, dir: dir, tools: map[string]bool{}, vars: map[string]string{
		"CROWDVM_API_BASE":   f.URL + "/api/v1",
		"CROWDVM_API_KEY":    clienttest.Key,
		"CROWDVM_CONFIG_DIR": dir,
		"CROWDVM_NO_BROWSER": "1",
	}}
}

type result struct {
	out, err string
	code     int
}

// lines decodes stdout as JSON Lines.
func (r result) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(r.out))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not JSON: %q (%v)\nstdout:\n%s", sc.Text(), err, r.out)
		}
		out = append(out, m)
	}
	return out
}

func (r result) last(t *testing.T) map[string]any {
	l := r.lines(t)
	if len(l) == 0 {
		t.Fatalf("no output; stderr: %s", r.err)
	}
	return l[len(l)-1]
}

func (e *env) runIn(stdin string, args ...string) result {
	return e.runCtx(context.Background(), stdin, args...)
}

func (e *env) runCtx(ctx context.Context, stdin string, args ...string) result {
	var out, errb bytes.Buffer
	a := &App{
		In: strings.NewReader(stdin), Out: &out, Err: &errb,
		Getenv:  func(k string) string { return e.vars[k] },
		Exe:     "/opt/crowdvm/crowdvm",
		Poll:    2 * time.Millisecond,
		OpenURL: func(u string) error { e.opened = append(e.opened, u); return nil },
		Run:     func(c *exec.Cmd) error { e.ran = append(e.ran, c); return nil },
		LookPath: func(name string) (string, error) {
			if e.tools[name] {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
	}
	code := a.Execute(ctx, args)
	return result{out.String(), errb.String(), code}
}

func (e *env) run(args ...string) result { return e.runIn("", args...) }

func (e *env) mustRun(args ...string) result {
	e.t.Helper()
	r := e.run(args...)
	if r.code != 0 {
		e.t.Fatalf("%v: exit %d\nstdout: %s\nstderr: %s", args, r.code, r.out, r.err)
	}
	return r
}

func reqBody(t *testing.T, r *clienttest.Request) map[string]any {
	t.Helper()
	if r == nil {
		t.Fatal("request not sent")
	}
	m := map[string]any{}
	_ = json.Unmarshal(r.Body, &m)
	return m
}

func TestNewJSONEvents(t *testing.T) {
	e := newEnv(t)
	r := e.mustRun("new", "--type", "large", "--no-auto-stop", "-e", "A=1", "--host", "3000:public", "--name", "w")
	lines := r.lines(t)
	if lines[0]["event"] != "created" || lines[len(lines)-1]["event"] != "ready" {
		t.Fatalf("events: %v", lines)
	}
	id, _ := lines[len(lines)-1]["id"].(string)
	if !strings.HasPrefix(id, "bx_") {
		t.Fatalf("ready id %q", id)
	}
	body := reqBody(t, e.f.Find("POST", "/api/v1/sandboxes"))
	if body["type"] != "large" || body["ttlSeconds"] != nil || body["env"].(map[string]any)["A"] != "1" {
		t.Fatalf("create body %v", body)
	}
	if _, ok := body["ttlSeconds"]; !ok {
		t.Fatal("--no-auto-stop must send ttlSeconds:null")
	}
	hp := body["hostedPorts"].([]any)[0].(map[string]any)
	if hp["port"] != float64(3000) || hp["public"] != true {
		t.Fatalf("hostedPorts %v", hp)
	}
	if e.f.Find("POST", "/api/v1/sandboxes").Header.Get("Idempotency-Key") == "" {
		t.Fatal("no idempotency key")
	}
	if e.f.Count("POST /api/v1/sandboxes/"+id+"/commands") == 0 {
		t.Fatal("ready must be confirmed by a no-op command")
	}
	// The new sandbox becomes "current".
	attachFrames(e, map[string]any{"type": "stdout", "data": "hello\n"}, map[string]any{"type": "exit", "exitCode": 0})
	r = e.mustRun("exec", "echo", "hello")
	if r.out != "hello\n" {
		t.Fatalf("exec on current (raw even when piped): %q", r.out)
	}
	r = e.mustRun("exec", "--json", "echo", "hello")
	if res := r.last(t); res["stdout"] != "hello\n" {
		t.Fatalf("exec --json: %v", res)
	}
	if e.f.Last("POST /api/v1/sandboxes/"+id+"/commands") == nil {
		t.Fatal("current alias not used")
	}
}

func TestNewTextMode(t *testing.T) {
	e := newEnv(t)
	r := e.mustRun("new", "--json=false")
	if !strings.HasPrefix(strings.TrimSpace(r.out), "bx_") || !strings.Contains(r.err, "ready") {
		t.Fatalf("stdout %q stderr %q", r.out, r.err)
	}
}

func TestListFilterAndInfo(t *testing.T) {
	e := newEnv(t)
	up := e.f.AddSandbox("running")
	stopped := e.f.AddSandbox("archived")
	r := e.mustRun("list")
	ids := []string{}
	for _, s := range r.last(t)["sandboxes"].([]any) {
		ids = append(ids, s.(map[string]any)["id"].(string))
	}
	if len(ids) != 1 || ids[0] != up {
		t.Fatalf("default filter: %v", ids)
	}
	r = e.mustRun("list", "--filter", "s", "--json=false")
	if !strings.Contains(r.out, stopped) || strings.Contains(r.out, up) || !strings.Contains(r.out, "stopped") {
		t.Fatalf("filter s: %s", r.out)
	}
	r = e.mustRun("info", up, "--json=false")
	if !strings.Contains(r.out, "state:     running") || !strings.Contains(r.out, "4 vCPU") {
		t.Fatalf("info: %s", r.out)
	}
	// Boat's JSON shape: .sandbox.state with the CLI's state names.
	if sb, _ := e.mustRun("info", stopped).last(t)["sandbox"].(map[string]any); sb == nil || sb["state"] != "stopped" || sb["id"] != stopped {
		t.Fatalf("info --json: %v", e.mustRun("info", stopped).out)
	}
}

func TestErrorOutput(t *testing.T) {
	e := newEnv(t)
	r := e.run("info", "bx_missing")
	last := r.last(t)
	if r.code != 1 || last["event"] != "error" || last["code"] != "sandbox_not_found" || last["status"] != float64(404) {
		t.Fatalf("code %d last %v", r.code, last)
	}
	r = e.run("info", "bx_missing", "--json=false")
	if r.code != 1 || !strings.Contains(r.err, "(sandbox_not_found, HTTP 404)") || !strings.Contains(r.err, "hint: check the id") {
		t.Fatalf("text error: %q", r.err)
	}
	r = e.run("info", "--bogus")
	if r.code != 2 || !strings.Contains(r.err, "unknown flag") {
		t.Fatalf("usage error: %d %q", r.code, r.err)
	}
	delete(e.vars, "CROWDVM_API_KEY")
	r = e.run("list", "--json=false")
	if r.code != 1 || !strings.Contains(r.err, "not_signed_in") || !strings.Contains(r.err, "crowdvm login") {
		t.Fatalf("not signed in: %q", r.err)
	}
}

// attachFrames makes the attach stream of a detached command (text-mode exec)
// answer with these frames; the returned func changes them.
func attachFrames(e *env, frames ...map[string]any) func(...map[string]any) {
	var mu sync.Mutex
	e.f.On("GET /api/v1/sandboxes/{id}/commands/{pid}/stream", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fs := frames
		mu.Unlock()
		enc := json.NewEncoder(w)
		for _, f := range fs {
			enc.Encode(f)
		}
	})
	return func(fs ...map[string]any) { mu.Lock(); frames = fs; mu.Unlock() }
}

func TestExecSyncExitCodeAndQuoting(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	set := attachFrames(e, map[string]any{"type": "stderr", "data": "bye\n"}, map[string]any{"type": "exit", "exitCode": 3})
	r := e.run("exec", id, "exit 3")
	if r.code != 3 || r.err != "bye\n" {
		t.Fatalf("exit %d stderr %q", r.code, r.err)
	}
	set(map[string]any{"type": "exit", "exitCode": 0})
	e.mustRun("exec", id, "--cwd", "repo", "--timeout", "120", "--", "echo", "a b", "--flag")
	body := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/commands"))
	// Text mode runs detached and the CLI enforces the timeout.
	if body["command"] != "echo 'a b' --flag" || body["cwd"] != "repo" || body["detached"] != true {
		t.Fatalf("body %v", body)
	}
	e.mustRun("exec", id, "bash", "-lc", "echo $HOME")
	body = reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/commands"))
	if body["command"] != `bash -lc 'echo $HOME'` {
		t.Fatalf("argv quoting %v", body["command"])
	}
}

func TestExecStream(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	attachFrames(e, map[string]any{"type": "stdout", "data": "streamed\n"}, map[string]any{"type": "exit", "exitCode": 0})
	r := e.run("exec", id, "--stream", "--json=false", "echo streamed")
	if r.code != 0 || r.out != "streamed\n" {
		t.Fatalf("%d %q %q", r.code, r.out, r.err)
	}
	r = e.mustRun("exec", id, "--stream", "--json", "echo x")
	types := []string{}
	for _, l := range r.lines(t) {
		types = append(types, l["type"].(string))
	}
	if strings.Join(types, ",") != "started,stdout,exit" {
		t.Fatalf("frames %v", types)
	}
}

func TestExecDetachStatusKillAttach(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	r := e.mustRun("exec", id, "--detach", "npm run dev")
	if r.last(t)["processId"] != float64(4242) {
		t.Fatalf("detach %v", r.out)
	}
	if b := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/commands")); b["detached"] != true {
		t.Fatalf("detached not sent %v", b)
	}
	r = e.mustRun("exec", id, "--status", "4242", "--json=false")
	if !strings.Contains(r.out, "process 4242: exited (exit 3)") || !strings.Contains(r.out, "tail") {
		t.Fatalf("status %q", r.out)
	}
	e.mustRun("exec", id, "--kill", "4242")
	if e.f.Last("DELETE /api/v1/sandboxes/"+id+"/commands/4242") == nil {
		t.Fatal("kill not sent")
	}
	r = e.run("exec", id, "--attach", "4242", "--json=false")
	if r.code != 7 || r.out != "part1\npart2\n" {
		t.Fatalf("attach: %d %q %q", r.code, r.out, r.err)
	}
	if last := e.f.Last("GET /api/v1/sandboxes/" + id + "/commands/4242/stream"); !strings.Contains(last.Query, "stdoutOffset=6") {
		t.Fatalf("reconnect offsets %q", last.Query)
	}
	// --timeout 0 runs detached and follows.
	r = e.run("exec", id, "--timeout", "0", "--json=false", "make test")
	if r.code != 7 || r.out != "part1\npart2\n" {
		t.Fatalf("long: %d %q %q", r.code, r.out, r.err)
	}
}

func TestExecClientTimeoutKills(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	e.f.On("GET /api/v1/sandboxes/{id}/commands/{pid}/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	e.f.On("GET /api/v1/sandboxes/{id}/commands/{pid}", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "command.status", map[string]any{"processId": 4242, "status": "running", "running": true})
	})
	// Ctrl-C (context cancel) while following a long command kills it.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r := e.runCtx(ctx, "", "exec", id, "--timeout", "0", "--json=false", "sleep 999")
	if r.code != 130 {
		t.Fatalf("interrupt: %d %q", r.code, r.err)
	}
	if e.f.Last("DELETE /api/v1/sandboxes/"+id+"/commands/4242") == nil {
		t.Fatal("interrupted long command must be killed")
	}
}

func TestLoginLogoutStatus(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "CROWDVM_API_KEY")
	delete(e.vars, "CROWDVM_API_BASE")
	r := e.run("status", "--check", "--api-url", e.f.URL)
	if r.code != 1 || r.last(t)["account"].(map[string]any)["signedIn"] != false {
		t.Fatalf("status before login: %d %s", r.code, r.out)
	}
	r = e.runIn(clienttest.Key+"\n", "login", "--server", e.f.URL, "--key-stdin")
	if r.code != 0 || r.last(t)["event"] != "signed_in" {
		t.Fatalf("login: %d %s %s", r.code, r.out, r.err)
	}
	cfg, err := loadConfig(e.dir)
	if err != nil || cfg.Profiles["default"].APIKey != clienttest.Key || cfg.Profiles["default"].Server != e.f.URL {
		t.Fatalf("config %+v %v", cfg.Profiles["default"], err)
	}
	if st, _ := os.Stat(filepath.Join(e.dir, "config.json")); st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("config mode %v", st.Mode())
	}
	r = e.mustRun("status", "--check")
	if r.last(t)["account"].(map[string]any)["signedIn"] != true {
		t.Fatalf("status %s", r.out)
	}
	e.mustRun("logout")
	if r := e.run("status", "--check"); r.code != 1 {
		t.Fatal("status --check after logout must fail")
	}
	r = e.runIn("hunter2\n", "login", "--username", "alice", "--password-stdin")
	if r.code != 0 || r.last(t)["method"] != "password" {
		t.Fatalf("password login %d %s %s", r.code, r.out, r.err)
	}
	if b := reqBody(t, e.f.Last("POST /auth/login")); b["client"] != "cli" || b["password"] != "hunter2" {
		t.Fatalf("login body %v", b)
	}
}

func TestDeviceLoginTwoStep(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "CROWDVM_API_KEY")
	e.f.DevicePolls = 1
	r := e.mustRun("login")
	ev := r.last(t)
	if ev["event"] != "login" || ev["code"] != "ABCD-EFGH" || !strings.HasSuffix(ev["url"].(string), "/device?code=ABCD-EFGH") {
		t.Fatalf("login event %v", ev)
	}
	// Running it again shows the same pending link, without a new start.
	e.mustRun("login")
	if n := e.f.Count("POST /auth/device/start"); n != 1 {
		t.Fatalf("device start called %d times", n)
	}
	r = e.mustRun("login", "--new")
	if r.last(t)["event"] != "login_replaced" {
		t.Fatalf("--new: %v", r.out)
	}
	r = e.mustRun("onboard")
	if l := r.lines(t); len(l) < 2 || l[len(l)-2]["event"] != "signed_in" || l[len(l)-1]["event"] != "onboarded" {
		t.Fatalf("onboard: %s", r.out)
	}
	cfg, _ := loadConfig(e.dir)
	if p := cfg.Profiles["default"]; p.APIKey != clienttest.Key || p.PendingLogin != nil {
		t.Fatalf("profile %+v", p)
	}
	if len(e.opened) != 0 {
		t.Fatal("JSON mode must not open a browser")
	}
}

func TestDeleteNeedsYes(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	r := e.run("delete", id)
	if r.code != 1 || r.last(t)["code"] != "confirmation_required" {
		t.Fatalf("%d %s", r.code, r.out)
	}
	r = e.mustRun("delete", id, "--yes")
	if l := r.lines(t); l[0]["event"] != "deleting" || l[len(l)-1]["event"] != "deleted" {
		t.Fatalf("delete events %v", l)
	}
	if e.f.Sandboxes[id] != nil {
		t.Fatal("not deleted")
	}
}

func TestLifecycleCommands(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("running")
	r := e.mustRun("stop", id)
	if ev := r.last(t); ev["event"] != "stopping" || ev["operation"].(map[string]any)["id"] != "stop_1" {
		t.Fatalf("stop %v", ev)
	}
	r = e.mustRun("resume", id, "--type", "small", "--ttl", "600")
	if r.last(t)["event"] != "ready" {
		t.Fatalf("resume %s", r.out)
	}
	if b := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/resume")); b["type"] != "small" || b["ttlSeconds"] != float64(600) {
		t.Fatalf("resume body %v", b)
	}
	r = e.mustRun("fork", id, "--wait")
	fid := r.last(t)["id"].(string)
	if fid == id || r.last(t)["event"] != "ready" {
		t.Fatalf("fork %s", r.out)
	}
	e.mustRun("extend", id, "--hours", "2")
	if b := reqBody(t, e.f.Last("PATCH")); b["ttlSeconds"] != float64(7200) {
		t.Fatalf("extend %v", b)
	}
	e.mustRun("rename", id, "api worker")
	e.mustRun("share", id)
	e.mustRun("recover", id)
}

func TestPushSendsArchive(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	src := filepath.Join(t.TempDir(), "proj")
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.MkdirAll(filepath.Join(src, ".git"), 0o755)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(src, "sub", "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(src, ".git", "HEAD"), []byte("ref"), 0o644)
	r := e.mustRun("push", id, src)
	ev := r.last(t)
	if ev["event"] != "pushed" || ev["to"] != "/home/user/proj" || ev["files"] != float64(2) {
		t.Fatalf("push %v", ev)
	}
	var archive []byte
	for k, v := range e.f.Files {
		if strings.HasPrefix(k, id+":/tmp/crowdvm-upload-") {
			archive = v
		}
	}
	names := tarNames(t, archive)
	if strings.Join(names, ",") != "a.txt,sub/run.sh" {
		t.Fatalf("archive entries %v", names)
	}
	cmd := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/commands"))["command"].(string)
	if !strings.Contains(cmd, "d=/home/user/proj") || !strings.Contains(cmd, "tar -xzf /tmp/crowdvm-upload-") {
		t.Fatalf("extract command %q", cmd)
	}
}

func tarNames(t *testing.T, b []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			names = append(names, h.Name)
			if h.Name == "sub/run.sh" && h.Mode&0o111 == 0 && os.PathSeparator == '/' {
				t.Error("executable bit lost")
			}
		}
	}
	sort.Strings(names)
	return names
}

func TestScpAPIFallback(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	local := filepath.Join(t.TempDir(), "in.txt")
	os.WriteFile(local, []byte("payload"), 0o644)
	e.mustRun("scp", local, id+":")
	if string(e.f.Files[id+":/home/user/in.txt"]) != "payload" {
		t.Fatalf("upload: %v", e.f.Files)
	}
	out := filepath.Join(t.TempDir(), "out.txt")
	e.mustRun("scp", id+":in.txt", out)
	if b, _ := os.ReadFile(out); string(b) != "payload" {
		t.Fatalf("download %q", b)
	}
	if r := e.run("scp", local, out); r.code != 2 {
		t.Fatalf("local→local must be a usage error, got %d", r.code)
	}
}

func TestSSHAndScpUseSystemTools(t *testing.T) {
	e := newEnv(t)
	e.tools["ssh"], e.tools["scp"] = true, true
	id := e.f.AddSandbox("ready")
	e.mustRun("ssh", "-t", id, "--", "tmux", "attach")
	c := e.ran[len(e.ran)-1]
	args := strings.Join(c.Args, " ")
	for _, want := range []string{"ProxyCommand=/opt/crowdvm/crowdvm tunnel %h 22", "-t", "-- user@" + id + " tmux attach", "StrictHostKeyChecking=yes"} {
		if !strings.Contains(args, want) {
			t.Fatalf("ssh args missing %q: %s", want, args)
		}
	}
	pub, err := os.ReadFile(filepath.Join(e.dir, "id_ed25519.pub"))
	if err != nil || !strings.HasPrefix(string(pub), "ssh-ed25519 ") {
		t.Fatalf("key: %q %v", pub, err)
	}
	if b := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/sshkey")); b["key"] != strings.TrimSpace(string(pub)) {
		t.Fatalf("sshkey body %v", b)
	}
	kh, _ := os.ReadFile(filepath.Join(e.dir, "known_hosts"))
	if !strings.HasPrefix(string(kh), id+" ssh-ed25519 ") {
		t.Fatalf("known_hosts %q", kh)
	}
	e.mustRun("scp", "-r", "./dir", id+":/srv/")
	args = strings.Join(e.ran[len(e.ran)-1].Args, " ")
	if !strings.HasSuffix(args, "-r ./dir user@"+id+":/srv/") {
		t.Fatalf("scp args %s", args)
	}
}

func TestTunnelStdio(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	e.f.On("GET /api/v1/sandboxes/{id}/tunnel", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		nc := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
		line, _ := bufio.NewReader(nc).ReadString('\n')
		nc.Write([]byte("port " + r.URL.Query().Get("port") + ": " + line))
		nc.Close()
	})
	r := e.runIn("ping\n", "tunnel", id, "8080")
	if r.code != 0 || r.out != "port 8080: ping\n" {
		t.Fatalf("%d %q %q", r.code, r.out, r.err)
	}
}

func TestForward(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan result)
	go func() { done <- e.runCtx(ctx, "", "forward", id, "--remote", "5432", "--local", itoa(port)) }()
	var conn net.Conn
	var err error
	for i := 0; i < 200; i++ {
		if conn, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port))); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("hello"))
	buf := make([]byte, 5)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("echo %q %v", buf, err)
	}
	conn.Close()
	cancel()
	r := <-done
	if r.code != 0 || r.lines(t)[0]["event"] != "forwarding" {
		t.Fatalf("forward: %d %s %s", r.code, r.out, r.err)
	}
	if q := e.f.Last("GET /api/v1/sandboxes/" + id + "/tunnel").Query; q != "port=5432" {
		t.Fatalf("tunnel port %q", q)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestHostAndDesktop(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	r := e.mustRun("host", id, "3000", "--public", "--json=false")
	if strings.TrimSpace(r.out) != "http://"+id+"-3000.localhost/" {
		t.Fatalf("host %q", r.out)
	}
	r = e.mustRun("desktop", id, "--json=false")
	if !strings.Contains(r.out, "/desktop/"+id) || len(e.opened) != 1 {
		t.Fatalf("desktop %q opened %v", r.out, e.opened)
	}
	r = e.mustRun("desktop", id)
	if r.last(t)["viewerUrl"] == nil || len(e.opened) != 1 {
		t.Fatalf("desktop json %s", r.out)
	}
}

func TestPromptFlow(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	r := e.mustRun("prompt", id, "--provider", "codex", "fix it")
	l := r.lines(t)
	types := []string{}
	for _, x := range l {
		types = append(types, firstNonEmpty(s(x["event"]), s(x["type"])))
	}
	if strings.Join(types, ",") != "queued,prompt,response,done" {
		t.Fatalf("events %v", types)
	}
	if b := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/prompt")); b["provider"] != "codex" || b["prompt"] != "fix it" {
		t.Fatalf("prompt body %v", b)
	}
	// The next prompt continues the remembered conversation.
	r = e.mustRun("prompt", id, "--json=false", "again")
	if b := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/prompt")); b["conversationId"] != "conv_1" {
		t.Fatalf("conversation not continued: %v", b)
	}
	if !strings.Contains(r.out, "done: again") || strings.Contains(r.out, "done: fix it") {
		t.Fatalf("only new events expected:\n%s", r.out)
	}
	e.mustRun("steer", id, "focus")
	if b := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/steer")); b["conversation"] != "conv_1" {
		t.Fatalf("steer body %v", b)
	}
	r = e.mustRun("conversations", id, "--json=false")
	if !strings.Contains(r.out, "conv_1") || !strings.Contains(r.out, "this shell") {
		t.Fatalf("convos %q", r.out)
	}
	e.mustRun("interrupt", id, "--convo", "current")
	if q := e.f.Last("POST /api/v1/sandboxes/" + id + "/interrupt").Query; q != "conversation=conv_1" {
		t.Fatalf("interrupt query %q", q)
	}
}

func TestSnapshotsCommands(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	r := e.mustRun("snapshot", id, "web-stack")
	if s := r.last(t); s["status"] != "ready" || s["name"] != "web-stack" {
		t.Fatalf("save %v", s)
	}
	r = e.mustRun("snapshot", "share", "web-stack", "--json=false")
	if strings.TrimSpace(r.out) != "bsh_secretcode" {
		t.Fatalf("share %q", r.out)
	}
	r = e.mustRun("snapshot", "list", "--json=false")
	if !strings.Contains(r.out, "web-stack") {
		t.Fatalf("list %q", r.out)
	}
}

func TestRunCreatesExecsDeletes(t *testing.T) {
	e := newEnv(t)
	r := e.run("run", "--json=false", "--", "make", "test")
	if r.code != 7 || r.out != "part1\npart2\n" || !strings.Contains(r.err, "exit 7 ·") || !strings.Contains(r.err, "deleted") {
		t.Fatalf("run: %d %q %q", r.code, r.out, r.err)
	}
	if len(e.f.Sandboxes) != 0 {
		t.Fatal("sandbox not deleted")
	}
	if b := reqBody(t, e.f.Find("POST", "/api/v1/sandboxes")); b["snapshots"] != false {
		t.Fatalf("run sandbox should not snapshot: %v", b)
	}
}

func TestMiscCommands(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	r := e.mustRun("nodes", "--json=false")
	if !strings.Contains(r.out, "desk") || !strings.Contains(r.out, "cpu=fast") || !strings.Contains(r.out, "12 vCPU/24 GB of 16 vCPU/32 GB") {
		t.Fatalf("nodes %q", r.out)
	}
	r = e.mustRun("credits", "--json=false")
	if !strings.Contains(r.out, "balance: 42.5 credits") {
		t.Fatalf("credits %q", r.out)
	}
	r = e.mustRun("history", id, "--json=false")
	if !strings.Contains(r.out, "cmd_1") || !strings.Contains(r.out, "npm test") {
		t.Fatalf("history %q", r.out)
	}
	r = e.mustRun("history", id, "cmd_1", "--json=false")
	if r.out != "log line\n" {
		t.Fatalf("history logs %q", r.out)
	}
	r = e.mustRun("ps", id, "--json=false")
	if !strings.Contains(r.out, "/sbin/init") {
		t.Fatalf("ps %q", r.out)
	}
	r = e.mustRun("agent-instructions")
	if !strings.Contains(r.out, "crowdvm push") || !strings.Contains(r.out, "crowdvm stop") {
		t.Fatalf("guide %q", r.out)
	}
	r = e.mustRun("completions", "bash")
	if !strings.Contains(r.out, "crowdvm") {
		t.Fatal("completion script")
	}
	r = e.mustRun("limits", "--json=false")
	if !strings.Contains(r.out, "active sandboxes: 1/10") {
		t.Fatalf("limits %q", r.out)
	}
}

// Every command must have help text and parse --help.
func TestHelpForAllCommands(t *testing.T) {
	e := newEnv(t)
	a := &App{}
	a.defaults()
	root := a.rootCmd()
	var paths [][]string
	var visit func(p []string, cmds []*cobra.Command)
	visit = func(p []string, cmds []*cobra.Command) {
		for _, c := range cmds {
			if c.Hidden {
				continue
			}
			if c.Short == "" {
				t.Errorf("%v %s has no short help", p, c.Name())
			}
			q := append(append([]string{}, p...), c.Name())
			paths = append(paths, q)
			visit(q, c.Commands())
		}
	}
	visit(nil, root.Commands())
	if len(paths) < 60 {
		t.Fatalf("only %d commands", len(paths))
	}
	for _, p := range paths {
		if r := e.run(append(p, "--help")...); r.code != 0 {
			t.Errorf("%v --help: %d %s", p, r.code, r.err)
		}
	}
}

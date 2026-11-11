package guest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

const testToken = "tok_test"

type env struct {
	t    *testing.T
	a    *Agent
	srv  *httptest.Server
	home string
	data string
}

func newEnv(t *testing.T, cfg proto.GuestConfig) *env {
	t.Helper()
	root := t.TempDir()
	return newEnvAt(t, cfg, filepath.Join(root, "home"), filepath.Join(root, "data"))
}

func newEnvAt(t *testing.T, cfg proto.GuestConfig, home, data string) *env {
	t.Helper()
	if cfg.Token == "" {
		cfg.Token = testToken
	}
	if cfg.SandboxID == "" {
		cfg.SandboxID = "bx_test"
	}
	a, err := New(Options{Config: cfg, Home: home, DataDir: data, Unprivileged: true,
		Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	a.Boot(context.Background())
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, a: a, srv: srv, home: a.home, data: a.data}
}

// do sends a JSON request and decodes a JSON response.
func (e *env) do(method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	resp := e.raw(method, path, body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		e.t.Fatalf("%s %s: bad JSON (%d): %s", method, path, resp.StatusCode, b)
	}
	return resp.StatusCode, m
}

func (e *env) raw(method, path string, body any) *http.Response {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func (e *env) mustOK(method, path string, body any) map[string]any {
	e.t.Helper()
	st, m := e.do(method, path, body)
	if st != 200 || m["ok"] != true {
		e.t.Fatalf("%s %s: %d %v", method, path, st, m)
	}
	return m
}

// ndjson reads all lines of a streaming response.
func (e *env) ndjson(method, path string, body any) []map[string]any {
	e.t.Helper()
	resp := e.raw(method, path, body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	var out []map[string]any
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			e.t.Fatalf("bad frame %q", sc.Text())
		}
		out = append(out, m)
	}
	return out
}

func q(s string) string { return url.QueryEscape(s) }

func num(v any) int { f, _ := v.(float64); return int(f) }

func joinFrames(frames []map[string]any, typ string) string {
	var b strings.Builder
	for _, f := range frames {
		if f["type"] == typ {
			b.WriteString(f["data"].(string))
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------

func TestAuthAndHealth(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	resp, _ := http.Get(e.srv.URL + "/v1/health")
	if resp.StatusCode != 200 {
		t.Fatalf("health: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Get(e.srv.URL + "/v1/files/list")
	if resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp.Body.Close()
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/files/list", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSyncCommand(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{Env: map[string]string{"FOO": "bar baz"}})
	os.MkdirAll(filepath.Join(e.home, "sub"), 0o755)
	m := e.mustOK("POST", "/v1/commands", map[string]any{
		"command": `echo "out:$FOO:$EXTRA"; pwd; echo err >&2; exit 3`, "cwd": "sub", "env": map[string]string{"EXTRA": "x"}})
	if m["type"] != "command.finished" || num(m["exitCode"]) != 3 || m["success"] != false {
		t.Fatalf("bad result %v", m)
	}
	if !strings.HasPrefix(m["stdout"].(string), "out:bar baz:x\n"+filepath.Join(e.home, "sub")) || m["stderr"] != "err\n" {
		t.Fatalf("output %q %q", m["stdout"], m["stderr"])
	}
	if m["cwd"] != "/home/user/sub" || !strings.HasPrefix(m["commandId"].(string), "cmd_") {
		t.Fatalf("cwd/commandId %v", m)
	}
	// timeout
	start := time.Now()
	m = e.mustOK("POST", "/v1/commands", map[string]any{"command": "sleep 20", "timeoutSeconds": 1})
	if m["timedOut"] != true || m["exitCode"] != nil || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout %v", m)
	}
	st, rec := e.do("GET", "/v1/command-history/"+m["commandId"].(string), nil)
	if st != 200 || rec["command"].(map[string]any)["status"] != "timed_out" {
		t.Fatalf("history status %v", rec)
	}
	// background child must not block a sync command
	start = time.Now()
	e.mustOK("POST", "/v1/commands", map[string]any{"command": "sleep 4 >/dev/null 2>&1 & echo started"})
	if time.Since(start) > 5*time.Second {
		t.Fatal("sync command waited for its background child")
	}
	// validation
	for _, body := range []map[string]any{
		{"command": "true", "timeoutSeconds": 0}, {"command": "true", "timeoutSeconds": 601},
		{"command": ""}, {"command": "true", "cwd": "nope"}, {"command": "true", "cwd": "/etc"},
	} {
		if st, m := e.do("POST", "/v1/commands", body); st != 400 {
			t.Fatalf("%v: %d %v", body, st, m)
		}
	}
}

func TestStreamCommand(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	frames := e.ndjson("POST", "/v1/commands", map[string]any{
		"command": "for i in 1 2 3; do echo line$i; sleep 0.1; done; echo é€ >&2; exit 2", "stream": true})
	if frames[0]["type"] != "started" || frames[0]["commandId"] == nil {
		t.Fatalf("first frame %v", frames[0])
	}
	last := frames[len(frames)-1]
	if last["type"] != "exit" || num(last["exitCode"]) != 2 || last["success"] != false || last["timedOut"] != false {
		t.Fatalf("last frame %v", last)
	}
	if got := joinFrames(frames, "stdout"); got != "line1\nline2\nline3\n" {
		t.Fatalf("stdout %q", got)
	}
	if got := joinFrames(frames, "stderr"); got != "é€\n" {
		t.Fatalf("stderr %q", got)
	}
	st, h := e.do("GET", "/v1/command-history?mode=stream", nil)
	if st != 200 || len(h["commands"].([]any)) != 1 {
		t.Fatalf("history %v", h)
	}
}

func TestDetachedStatusAttachKill(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	m := e.mustOK("POST", "/v1/commands", map[string]any{
		"command": "echo one; sleep 0.3; echo two; echo warn >&2; sleep 0.3; echo three; exit 7", "detached": true})
	if m["type"] != "command.started" || num(m["processId"]) == 0 || num(m["pid"]) != num(m["processId"]) {
		t.Fatalf("start %v", m)
	}
	pid := fmt.Sprint(num(m["processId"]))
	frames := e.ndjson("GET", "/v1/commands/"+pid+"/stream", nil)
	if frames[0]["type"] != "attached" {
		t.Fatalf("first %v", frames[0])
	}
	if got := joinFrames(frames, "stdout"); got != "one\ntwo\nthree\n" {
		t.Fatalf("attach stdout %q", got)
	}
	last := frames[len(frames)-1]
	if last["type"] != "exit" || num(last["exitCode"]) != 7 || last["status"] != "exited" || num(last["stdoutOffset"]) != 14 {
		t.Fatalf("exit frame %v", last)
	}
	// attach again from an offset: only the rest
	frames = e.ndjson("GET", "/v1/commands/"+pid+"/stream?stdoutOffset=8&stderrOffset=5", nil)
	if got := joinFrames(frames, "stdout"); got != "three\n" {
		t.Fatalf("offset attach %q", got)
	}
	// status, tail and offsets
	s := e.mustOK("GET", "/v1/commands/"+pid, nil)
	if s["status"] != "exited" || s["running"] != false || num(s["exitCode"]) != 7 || s["stdout"] != "one\ntwo\nthree\n" || s["stderr"] != "warn\n" || s["known"] != true {
		t.Fatalf("status %v", s)
	}
	s = e.mustOK("GET", "/v1/commands/"+pid+"?tailBytes=6", nil)
	if s["stdout"] != "three\n" || s["stdoutTruncated"] != true {
		t.Fatalf("tail %v", s)
	}
	s = e.mustOK("GET", "/v1/commands/"+pid+"?stdoutOffset=4", nil)
	if s["stdout"] != "two\nthree\n" || num(s["stdoutNextOffset"]) != 14 || num(s["stderrNextOffset"]) != 5 || s["more"] != false {
		t.Fatalf("offsets %v", s)
	}

	// kill a long-running command and its children
	m = e.mustOK("POST", "/v1/commands", map[string]any{"command": "sleep 300 & sleep 300; echo never", "detached": true})
	pid = fmt.Sprint(num(m["processId"]))
	time.Sleep(200 * time.Millisecond)
	s = e.mustOK("GET", "/v1/commands/"+pid, nil)
	if s["running"] != true || s["status"] != "running" {
		t.Fatalf("running %v", s)
	}
	k := e.mustOK("DELETE", "/v1/commands/"+pid, nil)
	cmd := k["command"].(map[string]any)
	if k["signaled"] != true || k["running"] != false || cmd["status"] != "killed" {
		t.Fatalf("kill %v", k)
	}
	k = e.mustOK("DELETE", "/v1/commands/"+pid, nil)
	if k["signaled"] != false {
		t.Fatalf("second kill %v", k)
	}
	if st, _ := e.do("GET", "/v1/commands/999999999", nil); st != 404 {
		t.Fatalf("unknown pid: %d", st)
	}
}

func TestAttachHeartbeatReconnect(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	e.a.attachMaxDuration = 700 * time.Millisecond
	e.a.heartbeatEvery = 200 * time.Millisecond
	m := e.mustOK("POST", "/v1/commands", map[string]any{"command": "echo hi; sleep 30", "detached": true})
	pid := fmt.Sprint(num(m["processId"]))
	frames := e.ndjson("GET", "/v1/commands/"+pid+"/stream", nil)
	last := frames[len(frames)-1]
	if last["type"] != "reconnect" || num(last["stdoutOffset"]) != 3 {
		t.Fatalf("reconnect %v", frames)
	}
	hb := 0
	for _, f := range frames {
		if f["type"] == "heartbeat" {
			hb++
		}
	}
	if hb == 0 {
		t.Fatalf("no heartbeat in %v", frames)
	}
	e.mustOK("DELETE", "/v1/commands/"+pid, nil)
}

func TestHistoryPaginationLogsAndRestart(t *testing.T) {
	root := t.TempDir()
	home, data := filepath.Join(root, "home"), filepath.Join(root, "data")
	e := newEnvAt(t, proto.GuestConfig{}, home, data)
	var ids []string
	for i := 0; i < 5; i++ {
		m := e.mustOK("POST", "/v1/commands", map[string]any{"command": fmt.Sprintf("printf 'n%d-123'", i)})
		ids = append(ids, m["commandId"].(string))
	}
	m := e.mustOK("POST", "/v1/commands", map[string]any{"command": "echo bg; sleep 60", "detached": true})
	bgPID := num(m["processId"])
	bgID := m["commandId"].(string)
	time.Sleep(200 * time.Millisecond)

	// pagination, newest first
	var got []string
	cursor := ""
	for page := 0; ; page++ {
		path := "/v1/command-history?limit=2&mode=sync"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		h := e.mustOK("GET", path, nil)
		for _, c := range h["commands"].([]any) {
			got = append(got, c.(map[string]any)["commandId"].(string))
		}
		if h["nextCursor"] == nil {
			break
		}
		cursor = h["nextCursor"].(string)
		if page > 5 {
			t.Fatal("pagination loops")
		}
	}
	if len(got) != 5 || got[0] != ids[4] || got[4] != ids[0] {
		t.Fatalf("pages %v vs %v", got, ids)
	}
	h := e.mustOK("GET", "/v1/command-history?q=PRINTF%20%27N2", nil)
	if c := h["commands"].([]any); len(c) != 1 {
		t.Fatalf("q filter %v", h)
	}
	h = e.mustOK("GET", "/v1/command-history?mode=detached", nil)
	if c := h["commands"].([]any); len(c) != 1 || c[0].(map[string]any)["status"] != "running" {
		t.Fatalf("detached filter %v", h)
	}
	// logs with offset/limit/base64
	l := e.mustOK("GET", "/v1/command-history/"+ids[1]+"/logs?offset=3&limit=2", nil)
	if l["data"] != "12" || num(l["nextOffset"]) != 5 || num(l["size"]) != 6 || l["done"] != false {
		t.Fatalf("logs %v", l)
	}
	l = e.mustOK("GET", "/v1/command-history/"+ids[1]+"/logs?offset=5&encoding=base64", nil)
	if b, _ := base64.StdEncoding.DecodeString(l["data"].(string)); string(b) != "3" || l["done"] != true {
		t.Fatalf("logs b64 %v", l)
	}

	// "restart" the agent on the same disk: history survives, the running
	// detached command is adopted.
	e.srv.Close()
	e2 := newEnvAt(t, proto.GuestConfig{}, home, data)
	r := e2.mustOK("GET", "/v1/command-history/"+ids[2], nil)["command"].(map[string]any)
	if r["status"] != "exited" || num(r["exitCode"]) != 0 || num(r["stdoutBytes"]) != 6 {
		t.Fatalf("after restart %v", r)
	}
	s := e2.mustOK("GET", fmt.Sprintf("/v1/commands/%d", bgPID), nil)
	if s["status"] != "running" || s["stdout"] != "bg\n" {
		t.Fatalf("adopted %v", s)
	}
	k := e2.mustOK("DELETE", fmt.Sprintf("/v1/commands/%d", bgPID), nil)
	if k["signaled"] != true || k["command"].(map[string]any)["status"] != "killed" {
		t.Fatalf("kill adopted %v", k)
	}
	// a record from another boot becomes lost
	mp := filepath.Join(data, "history", bgID, "meta.json")
	b, _ := os.ReadFile(mp)
	var mm meta
	json.Unmarshal(b, &mm)
	mm.Status, mm.BootID, mm.FinishedAt = "running", "other-boot", nil
	b, _ = json.Marshal(mm)
	os.WriteFile(mp, b, 0o644)
	e3 := newEnvAt(t, proto.GuestConfig{}, home, data)
	r = e3.mustOK("GET", "/v1/command-history/"+bgID, nil)["command"].(map[string]any)
	if r["status"] != "lost" {
		t.Fatalf("other boot %v", r)
	}
}

func TestProcesses(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	m := e.mustOK("POST", "/v1/commands", map[string]any{"command": "exec sleep 30", "detached": true})
	pid := num(m["processId"])
	defer e.do("DELETE", fmt.Sprintf("/v1/commands/%d", pid), nil)
	time.Sleep(200 * time.Millisecond)
	p := e.mustOK("GET", "/v1/processes", nil)
	if p["live"] != true || num(p["total"]) == 0 {
		t.Fatalf("processes %v", p)
	}
	for _, x := range p["processes"].([]any) {
		pr := x.(map[string]any)
		if num(pr["pid"]) == pid {
			if pr["commandId"] != m["commandId"] || !strings.Contains(pr["command"].(string), "sleep 30") {
				t.Fatalf("row %v", pr)
			}
			return
		}
	}
	t.Fatal("command not listed")
}

func TestEventsStream(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/internal/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	e.mustOK("POST", "/v1/commands", map[string]any{"command": "true"})
	sc := bufio.NewScanner(resp.Body)
	seen := map[string]bool{}
	for !(seen[EvCommandStarted] && seen[EvCommandExited]) && sc.Scan() {
		var ev proto.GuestEvent
		json.Unmarshal(sc.Bytes(), &ev)
		seen[ev.Type] = true
	}
	if !seen[EvCommandExited] {
		t.Fatalf("events %v", seen)
	}
}

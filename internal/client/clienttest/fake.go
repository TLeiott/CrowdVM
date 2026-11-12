// Package clienttest is an in-memory fake of the CrowdVM /api/v1 surface for
// tests of the client, the CLI and other consumers. It implements enough of
// the API to be realistic (sandboxes, commands incl. NDJSON streams, files,
// tunnels, prompts/events, saved snapshots, device login) and lets tests
// override any route with On.
package clienttest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// Key is the API key the fake accepts.
const Key = "cvm_testkey"

// Request is one recorded request.
type Request struct {
	Method, Path, Query string
	Header              http.Header
	Body                []byte
}

// Fake is a fake API server.
type Fake struct {
	*httptest.Server
	mu        sync.Mutex
	defaults  *http.ServeMux
	overrides *http.ServeMux
	nOverride int

	Requests  []Request
	Sandboxes map[string]map[string]any
	Files     map[string][]byte // "bx_id:/path" → content
	Events    []map[string]any
	Saved     map[string]map[string]any
	// Exec decides the result of a command: stdout, stderr, exit code.
	Exec func(sandbox, command string) (string, string, int)
	// DevicePolls is how many polls stay pending before approval.
	DevicePolls int
	polls       int
	nextID      int
}

// New starts a fake server; it is closed with t.Cleanup.
func New(t testing.TB) *Fake {
	f := &Fake{
		defaults:  http.NewServeMux(),
		overrides: http.NewServeMux(),
		Sandboxes: map[string]map[string]any{},
		Files:     map[string][]byte{},
		Saved:     map[string]map[string]any{},
		Exec:      defaultExec,
	}
	f.routes()
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// On overrides a route ("METHOD /api/v1/path" with Go mux patterns).
func (f *Fake) On(pattern string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overrides.HandleFunc(pattern, h)
}

// AddSandbox inserts a sandbox in the given state and returns its id.
func (f *Fake) AddSandbox(state string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addSandbox(state)
}

func (f *Fake) addSandbox(state string) string {
	f.nextID++
	id := fmt.Sprintf("bx_test%04d", f.nextID)
	f.Sandboxes[id] = map[string]any{"id": id, "name": "Sandbox " + id, "state": state, "type": "default",
		"vcpu": 4, "memoryGB": 8, "desktopAvailable": true, "snapshotAvailable": true, "createdAt": "2026-11-08T10:00:00Z"}
	return id
}

// Last returns the last recorded request whose "METHOD path" has the prefix.
func (f *Fake) Last(prefix string) *Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.Requests) - 1; i >= 0; i-- {
		r := f.Requests[i]
		if strings.HasPrefix(r.Method+" "+r.Path, prefix) {
			return &r
		}
	}
	return nil
}

// Find returns the last request with exactly this method and path.
func (f *Fake) Find(method, path string) *Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.Requests) - 1; i >= 0; i-- {
		if r := f.Requests[i]; r.Method == method && r.Path == path {
			return &r
		}
	}
	return nil
}

// Count counts recorded requests whose "METHOD path" has the prefix.
func (f *Fake) Count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.Requests {
		if strings.HasPrefix(r.Method+" "+r.Path, prefix) {
			n++
		}
	}
	return n
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil && r.Header.Get("Upgrade") == "" {
		body, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
	}
	f.mu.Lock()
	f.Requests = append(f.Requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
	h, pattern := f.overrides.Handler(r)
	f.mu.Unlock()
	if pattern != "" {
		h.ServeHTTP(w, r)
		return
	}
	public := strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/upload/") || strings.HasPrefix(r.URL.Path, "/download/")
	if !public && r.Header.Get("Authorization") != "Bearer "+Key {
		Error(w, 401, "unauthorized", "missing or invalid API key")
		return
	}
	f.defaults.ServeHTTP(w, r)
}

// OK writes {"ok":true,"type":typ, ...fields}.
func OK(w http.ResponseWriter, typ string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["ok"], fields["type"] = true, typ
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(fields)
}

// Error writes the error envelope.
func Error(w http.ResponseWriter, status int, code, msg string) {
	ErrorDetails(w, status, code, msg, nil)
}

func ErrorDetails(w http.ResponseWriter, status int, code, msg string, details map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "type": "sandbox.error", "status": status, "code": code,
		"message": msg, "requestId": "req_test", "error": map[string]any{"code": code, "message": msg, "status": status, "details": details}})
}

func decode(r *http.Request) map[string]any {
	m := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&m)
	return m
}

func defaultExec(_, cmd string) (string, string, int) {
	switch {
	case cmd == "true":
		return "", "", 0
	case cmd == "false":
		return "", "", 1
	case strings.HasPrefix(cmd, "echo "):
		return strings.Trim(strings.TrimPrefix(cmd, "echo "), "'") + "\n", "", 0
	case strings.HasPrefix(cmd, "exit "):
		n, _ := strconv.Atoi(strings.TrimPrefix(cmd, "exit "))
		return "", "bye\n", n
	}
	return "ran: " + cmd + "\n", "", 0
}

func (f *Fake) sandbox(w http.ResponseWriter, r *http.Request) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.Sandboxes[r.PathValue("id")]
	if sb == nil {
		Error(w, 404, "sandbox_not_found", "no sandbox "+r.PathValue("id"))
	}
	return sb
}

func (f *Fake) routes() {
	m := f.defaults
	v := "/api/v1"
	m.HandleFunc("GET "+v+"/me", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "user.info", map[string]any{"user": map[string]any{"id": "usr_1", "login": "alice", "email": "alice@example.com"}})
	})
	m.HandleFunc("GET "+v+"/limits", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "limits", map[string]any{"activeSandboxes": len(f.Sandboxes), "maxActiveSandboxes": 10, "canStart": true,
			"starts":       map[string]any{"minute": map[string]any{"remaining": 5}},
			"machineTypes": []any{map[string]any{"type": "default", "available": true, "fitsNow": 3}}})
	})
	m.HandleFunc("GET "+v+"/credits", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "credits.info", map[string]any{"wallet": map[string]any{"balanceCredits": 42.5, "enforced": false}, "earnedCredits": 3, "spentCredits": 1})
	})
	m.HandleFunc("GET "+v+"/nodes", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "node.list", map[string]any{"nodes": []any{map[string]any{"id": "node_1", "name": "desk", "online": true, "os": "linux", "arch": "amd64",
			"tier": "fast", "capacity": map[string]any{"vcpu": 16, "memoryMiB": 32768}, "free": map[string]any{"vcpu": 12, "memoryMiB": 24576},
			"sandboxes": 1, "categories": map[string]any{"cpu": "fast", "disk": "nvme"}}}})
	})
	m.HandleFunc("POST "+v+"/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		req := decode(r)
		f.mu.Lock()
		id := f.addSandbox("provisioning")
		sb := f.Sandboxes[id]
		if t, ok := req["type"].(string); ok && t != "" {
			sb["type"] = t
		}
		if n, ok := req["name"].(string); ok && n != "" {
			sb["name"] = n
		}
		sb["polls"] = 0
		f.mu.Unlock()
		w.WriteHeader(202)
		OK(w, "sandbox.created", map[string]any{"status": "provisioning", "sandbox": sb})
	})
	m.HandleFunc("GET "+v+"/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		var list []any
		for _, sb := range f.Sandboxes {
			list = append(list, sb)
		}
		f.mu.Unlock()
		OK(w, "sandbox.list", map[string]any{"sandboxes": list, "pageInfo": map[string]any{"hasMore": false, "limit": 100}})
	})
	m.HandleFunc("GET "+v+"/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		sb := f.sandbox(w, r)
		if sb == nil {
			return
		}
		f.mu.Lock()
		// Sandboxes become ready on the second read.
		if n, ok := sb["polls"].(int); ok {
			sb["polls"] = n + 1
			if n+1 >= 2 && (sb["state"] == "provisioning") {
				sb["state"] = "ready"
			}
		}
		out := map[string]any{"sandbox": sb}
		b, _ := json.Marshal(out)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
	m.HandleFunc("PATCH "+v+"/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		sb := f.sandbox(w, r)
		if sb == nil {
			return
		}
		req := decode(r)
		f.mu.Lock()
		for k, val := range req {
			sb[k] = val
		}
		f.mu.Unlock()
		OK(w, "sandbox.updated", map[string]any{"sandbox": sb})
	})
	m.HandleFunc("DELETE "+v+"/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if r.Header.Get("X-CrowdVM-Confirm-Delete") != id {
			Error(w, 409, "confirmation_required", "confirm header mismatch")
			return
		}
		f.mu.Lock()
		delete(f.Sandboxes, id)
		f.mu.Unlock()
		w.WriteHeader(202)
		OK(w, "sandbox.deleting", map[string]any{"operation": map[string]any{"id": "bdop_1", "kind": "sandbox", "targetId": id, "status": "completed"}})
	})
	for _, act := range []string{"stop", "resume", "fork", "share", "unshare", "recover", "interrupt"} {
		act := act
		m.HandleFunc("POST "+v+"/sandboxes/{id}/"+act, func(w http.ResponseWriter, r *http.Request) {
			sb := f.sandbox(w, r)
			if sb == nil {
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			out := map[string]any{"id": sb["id"], "status": act}
			switch act {
			case "stop":
				sb["state"] = "archived"
				out["sandbox"] = sb
				out["status"] = "archiving"
				sb["stop"] = map[string]any{"id": "stop_1", "status": "pending"}
			case "resume":
				sb["state"], sb["polls"] = "provisioning", 0
			case "fork":
				id := f.addSandbox("provisioning")
				f.Sandboxes[id]["polls"] = 0
				out["id"], out["sandbox"] = id, f.Sandboxes[id]
			}
			OK(w, "sandbox."+act, out)
		})
	}
	m.HandleFunc("POST "+v+"/sandboxes/{id}/commands", func(w http.ResponseWriter, r *http.Request) {
		if f.sandbox(w, r) == nil {
			return
		}
		req := decode(r)
		cmd, _ := req["command"].(string)
		out, errOut, code := f.Exec(r.PathValue("id"), cmd)
		switch {
		case req["detached"] == true:
			OK(w, "command.started", map[string]any{"processId": 4242, "pid": 4242, "command": cmd, "commandId": "cmd_1"})
		case req["stream"] == true:
			w.Header().Set("Content-Type", "application/x-ndjson")
			enc := json.NewEncoder(w)
			enc.Encode(map[string]any{"type": "started", "commandId": "cmd_1"})
			if out != "" {
				enc.Encode(map[string]any{"type": "stdout", "data": out})
			}
			if errOut != "" {
				enc.Encode(map[string]any{"type": "stderr", "data": errOut})
			}
			enc.Encode(map[string]any{"type": "exit", "exitCode": code, "success": code == 0})
		default:
			OK(w, "command.result", map[string]any{"commandId": "cmd_1", "exitCode": code, "stdout": out, "stderr": errOut, "success": code == 0})
		}
	})
	m.HandleFunc("GET "+v+"/sandboxes/{id}/commands/{pid}", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "command.status", map[string]any{"processId": 4242, "status": "exited", "known": true, "running": false, "exitCode": 3, "stdout": "tail\n"})
	})
	m.HandleFunc("DELETE "+v+"/sandboxes/{id}/commands/{pid}", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "command.stopped", map[string]any{"processId": 4242, "running": false, "signaled": true})
	})
	m.HandleFunc("GET "+v+"/sandboxes/{id}/commands/{pid}/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		if r.URL.Query().Get("stdoutOffset") == "" {
			// First connection: some output, then a reconnect.
			enc.Encode(map[string]any{"type": "attached", "processId": 4242})
			enc.Encode(map[string]any{"type": "stdout", "data": "part1\n", "nextOffset": 6})
			enc.Encode(map[string]any{"type": "reconnect", "stdoutOffset": 6, "stderrOffset": 0})
			return
		}
		enc.Encode(map[string]any{"type": "stdout", "data": "part2\n", "nextOffset": 12})
		enc.Encode(map[string]any{"type": "exit", "exitCode": 7, "status": "exited"})
	})
	m.HandleFunc("GET "+v+"/sandboxes/{id}/command-history", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "command.history", map[string]any{"commands": []any{map[string]any{"commandId": "cmd_1", "sandboxId": r.PathValue("id"),
			"command": "npm test", "mode": "sync", "status": "exited", "exitCode": 0, "startedAt": "2026-11-08T10:00:00Z"}}})
	})
	m.HandleFunc("GET "+v+"/sandboxes/{id}/command-history/{cid}/logs", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "command.logs", map[string]any{"commandId": r.PathValue("cid"), "stream": r.URL.Query().Get("stream"),
			"encoding": "utf8", "data": "log line\n", "offset": 0, "nextOffset": 9, "done": true})
	})
	m.HandleFunc("GET "+v+"/sandboxes/{id}/processes", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "process.list", map[string]any{"live": true, "total": 1, "processes": []any{map[string]any{"pid": 1, "cpuPercent": 1.5, "memoryMB": 20, "user": "root", "command": "/sbin/init"}}})
	})
	// files
	m.HandleFunc("POST "+v+"/sandboxes/{id}/files/upload-url", func(w http.ResponseWriter, r *http.Request) {
		req := decode(r)
		OK(w, "file.upload_url", map[string]any{"url": "/upload/" + r.PathValue("id") + "?path=" + fmt.Sprint(req["path"]), "method": "PUT", "path": req["path"]})
	})
	m.HandleFunc("PUT /upload/{id}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.Files[r.PathValue("id")+":"+r.URL.Query().Get("path")] = b
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	m.HandleFunc("POST "+v+"/sandboxes/{id}/files/download-url", func(w http.ResponseWriter, r *http.Request) {
		req := decode(r)
		OK(w, "file.download_url", map[string]any{"url": "/download/" + r.PathValue("id") + "?path=" + fmt.Sprint(req["path"]), "method": "GET"})
	})
	m.HandleFunc("GET /download/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		b, ok := f.Files[r.PathValue("id")+":"+r.URL.Query().Get("path")]
		f.mu.Unlock()
		if !ok {
			Error(w, 404, "file_not_found", "no such file")
			return
		}
		w.Write(b)
	})
	m.HandleFunc("GET "+v+"/sandboxes/{id}/files/info", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		f.mu.Lock()
		b, ok := f.Files[r.PathValue("id")+":"+p]
		isDir := false
		for k := range f.Files {
			if strings.HasPrefix(k, r.PathValue("id")+":"+strings.TrimRight(p, "/")+"/") {
				isDir = true
			}
		}
		f.mu.Unlock()
		if !ok && !isDir && p != "/home/user" {
			Error(w, 404, "file_not_found", "no such file")
			return
		}
		OK(w, "file.info", map[string]any{"name": p[strings.LastIndex(p, "/")+1:], "path": p, "isDir": isDir || p == "/home/user", "size": len(b), "mode": "0644"})
	})
	m.HandleFunc("PUT "+v+"/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		req := decode(r)
		b, _ := base64.StdEncoding.DecodeString(fmt.Sprint(req["content"]))
		f.mu.Lock()
		f.Files[r.PathValue("id")+":"+fmt.Sprint(req["path"])] = b
		f.mu.Unlock()
		OK(w, "file.written", map[string]any{"path": req["path"], "size": len(b)})
	})
	m.HandleFunc("GET "+v+"/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		b, ok := f.Files[r.PathValue("id")+":"+r.URL.Query().Get("path")]
		f.mu.Unlock()
		if !ok {
			Error(w, 404, "file_not_found", "no such file")
			return
		}
		OK(w, "file.read", map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(b), "size": len(b)})
	})
	m.HandleFunc("POST "+v+"/sandboxes/{id}/files/permissions", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "file.permissions_set", nil)
	})
	m.HandleFunc("DELETE "+v+"/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		delete(f.Files, r.PathValue("id")+":"+r.URL.Query().Get("path"))
		f.mu.Unlock()
		OK(w, "file.deleted", nil)
	})
	// access
	m.HandleFunc("POST "+v+"/sandboxes/{id}/sshkey", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "ssh_key.configured", map[string]any{"success": true, "sshUser": "user", "hostKey": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeHostKey"})
	})
	m.HandleFunc("POST "+v+"/sandboxes/{id}/host", func(w http.ResponseWriter, r *http.Request) {
		req := decode(r)
		port := fmt.Sprint(req["port"])
		u := "http://" + r.PathValue("id") + "-" + port + ".localhost/"
		if req["public"] != true {
			u += "?_token=secret"
		}
		OK(w, "host.port", map[string]any{"port": req["port"], "url": u, "isProtected": req["public"] != true, "access": "private"})
	})
	m.HandleFunc("POST "+v+"/sandboxes/{id}/desktop", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "desktop.url", map[string]any{"desktopUrl": "http://server/desktop/" + r.PathValue("id") + "?_token=x", "mode": "vnc"})
	})
	m.HandleFunc("GET "+v+"/sandboxes/{id}/tunnel", func(w http.ResponseWriter, r *http.Request) {
		if f.sandbox(w, r) == nil {
			return
		}
		if r.URL.Query().Get("port") == "" {
			Error(w, 400, "bad_request", "port required")
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		nc := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
		defer nc.Close()
		_, _ = io.Copy(nc, nc) // echo
	})
	// agents
	m.HandleFunc("POST "+v+"/sandboxes/{id}/prompt", func(w http.ResponseWriter, r *http.Request) {
		req := decode(r)
		conv, _ := req["conversationId"].(string)
		if conv == "" {
			conv = "conv_1"
		}
		f.mu.Lock()
		f.Events = append(f.Events,
			map[string]any{"id": fmt.Sprintf("ev_%d", len(f.Events)+1), "type": "prompt", "conversationId": conv, "data": map[string]any{"prompt": req["prompt"]}},
			map[string]any{"id": fmt.Sprintf("ev_%d", len(f.Events)+2), "type": "response", "conversationId": conv, "data": map[string]any{"content": "done: " + fmt.Sprint(req["prompt"])}})
		f.mu.Unlock()
		w.WriteHeader(202)
		OK(w, "prompt.queued", map[string]any{"id": "pr_1", "promptId": "pr_1", "conversationId": conv, "status": "queued", "provider": "claude"})
	})
	m.HandleFunc("GET "+v+"/sandboxes/{id}/prompts/{pid}", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "prompt.run", map[string]any{"promptRun": map[string]any{"id": "pr_1", "promptId": "pr_1", "status": "finished", "done": true}})
	})
	m.HandleFunc("GET "+v+"/sandboxes/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		evs := append([]map[string]any(nil), f.Events...)
		f.mu.Unlock()
		OK(w, "events.list", map[string]any{"events": evs, "pageInfo": map[string]any{"hasMore": false, "limit": 100}})
	})
	m.HandleFunc("GET "+v+"/sandboxes/{id}/conversations", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "conversations", map[string]any{"conversations": []any{map[string]any{"id": "conv_1", "prompts": 1, "running": false, "current": true, "lastPromptPreview": "hi"}}})
	})
	m.HandleFunc("POST "+v+"/sandboxes/{id}/steer", func(w http.ResponseWriter, r *http.Request) {
		req := decode(r)
		OK(w, "sandbox.steered", map[string]any{"conversationId": req["conversation"], "native": true, "mode": "native", "status": "steered"})
	})
	// snapshots
	m.HandleFunc("POST "+v+"/saved-snapshots", func(w http.ResponseWriter, r *http.Request) {
		req := decode(r)
		name := fmt.Sprint(req["name"])
		s := map[string]any{"name": name, "status": "saving", "sourceSandboxId": req["sandboxId"], "createdAt": "2026-11-08T10:00:00Z"}
		f.mu.Lock()
		f.Saved[name] = s
		f.mu.Unlock()
		w.WriteHeader(202)
		OK(w, "saved_snapshot.saving", map[string]any{"status": "saving", "snapshot": s})
	})
	m.HandleFunc("GET "+v+"/saved-snapshots/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		s := f.Saved[r.PathValue("name")]
		if s != nil {
			s["status"] = "ready"
		}
		f.mu.Unlock()
		if s == nil {
			Error(w, 404, "not_found", "no saved snapshot")
			return
		}
		OK(w, "saved_snapshot.info", map[string]any{"snapshot": s})
	})
	m.HandleFunc("GET "+v+"/saved-snapshots", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		var l []any
		for _, s := range f.Saved {
			l = append(l, s)
		}
		f.mu.Unlock()
		OK(w, "saved_snapshot.list", map[string]any{"snapshots": l})
	})
	m.HandleFunc("POST "+v+"/saved-snapshots/{name}/shares", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "share.created", map[string]any{"name": r.PathValue("name"), "shareCode": "bsh_secretcode", "share": map[string]any{"id": "shr_1", "codePrefix": "bsh_sec", "status": "active"}})
	})
	// auth
	m.HandleFunc("POST /auth/device/start", func(w http.ResponseWriter, r *http.Request) {
		OK(w, "device.started", map[string]any{"deviceCode": "dev_123", "userCode": "ABCD-EFGH", "verificationUrl": "/device",
			"verificationUrlComplete": "/device?code=ABCD-EFGH", "interval": 1, "expiresIn": 900})
	})
	m.HandleFunc("POST /auth/device/poll", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.polls++
		n := f.polls
		f.mu.Unlock()
		if n <= f.DevicePolls {
			OK(w, "device.poll", map[string]any{"status": "pending"})
			return
		}
		OK(w, "device.poll", map[string]any{"status": "approved", "token": Key, "user": map[string]any{"id": "usr_1", "login": "alice"}})
	})
	m.HandleFunc("POST /auth/login", func(w http.ResponseWriter, r *http.Request) {
		req := decode(r)
		if req["password"] != "hunter2" {
			Error(w, 401, "invalid_credentials", "Wrong username or password.")
			return
		}
		OK(w, "auth.login", map[string]any{"token": Key, "user": map[string]any{"id": "usr_1", "login": req["username"]}})
	})
}

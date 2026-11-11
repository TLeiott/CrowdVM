// Command devserver serves the CrowdVM dashboard (package web) with an
// in-memory mock of /auth and /api/v1 so every page is clickable without a
// real server. It also serves a fake RFB desktop for the noVNC viewer.
//
//	go run ./web/devserver [-addr 127.0.0.1:8480] [-autologin] [-dir web]
//
// Sign in as demo / demo (or sign up with any name).
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/crowdvm/crowdvm/web"
)

var (
	mu        sync.Mutex
	autologin bool
	now       = time.Now()
)

func ago(d time.Duration) time.Time { return now.Add(-d) }
func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

type M = map[string]any

func main() {
	addr := flag.String("addr", "127.0.0.1:8480", "listen address")
	flag.BoolVar(&autologin, "autologin", false, "treat every request as signed in (for screenshots)")
	dir := flag.String("dir", "", "serve the dashboard from this directory instead of the embedded copy (live edits)")
	flag.Parse()
	seed()

	mux := http.NewServeMux()
	routes(mux)
	var static fs.FS = web.FS
	if *dir != "" {
		static = os.DirFS(*dir)
	}
	files := http.FileServer(http.FS(static))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" {
			if f, err := static.Open(p); err == nil {
				st, _ := f.Stat()
				f.Close()
				if !st.IsDir() {
					w.Header().Set("Cache-Control", "no-cache")
					files.ServeHTTP(w, r)
					return
				}
			}
		}
		b, _ := fs.ReadFile(static, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	})
	log.Printf("CrowdVM dashboard devserver on http://%s  (demo / demo)", *addr)
	log.Fatal(http.ListenAndServe(*addr, logReq(mux)))
}

func logReq(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") {
			log.Printf("%s %s", r.Method, r.URL.RequestURI())
		}
		h.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- envelope

func ok(w http.ResponseWriter, status int, typ string, f M) {
	if f == nil {
		f = M{}
	}
	f["ok"] = true
	f["type"] = typ
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(f)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(M{"ok": false, "type": "sandbox.error", "status": status, "code": code, "message": msg,
		"error": M{"code": code, "message": msg, "status": status}, "requestId": fmt.Sprintf("req_%08x", rand.Uint32())})
}

func body(r *http.Request) M {
	m := M{}
	json.NewDecoder(r.Body).Decode(&m)
	return m
}

func str(m M, k string) string { s, _ := m[k].(string); return s }

// ---------------------------------------------------------------- auth

type user struct {
	ID, Login, Display, Password string
	Created                      time.Time
}

var (
	users    = map[string]*user{}
	sessions = map[string]string{} // token -> login
	devices  = map[string]bool{"WDJB-MJHT": false}
)

func current(r *http.Request) *user {
	if c, err := r.Cookie("crowdvm_session"); err == nil {
		if l, ok := sessions[c.Value]; ok {
			return users[l]
		}
	}
	if autologin {
		return users["demo"]
	}
	return nil
}

func userJSON(u *user) M {
	return M{"id": u.ID, "login": u.Login, "username": u.Login, "displayName": u.Display, "email": nil, "isAdmin": u.Login == "demo",
		"connectionMethod": "email", "accountStatus": "active", "createdAt": ts(u.Created)}
}

// authed wraps a handler: 401 without a session, 403 for cookie mutations without the CSRF header.
func authed(h func(http.ResponseWriter, *http.Request, *user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		u := current(r)
		mu.Unlock()
		if u == nil {
			fail(w, 401, "unauthorized", "Sign in first.")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-CrowdVM-CSRF") != "1" {
			fail(w, 403, "csrf_required", "Missing X-CrowdVM-CSRF header.")
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/commands") && !strings.HasSuffix(r.URL.Path, "/desktop/ws") {
			time.Sleep(time.Duration(40+rand.Intn(80)) * time.Millisecond) // feel like a network
		}
		mu.Lock()
		defer mu.Unlock()
		h(w, r, u)
	}
}

func setSession(w http.ResponseWriter, u *user) {
	tok := fmt.Sprintf("s%x", rand.Uint64())
	sessions[tok] = u.Login
	http.SetCookie(w, &http.Cookie{Name: "crowdvm_session", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// ---------------------------------------------------------------- data

type sandbox struct {
	ID, Name, State, Type, Env, Error, Node, Visibility, CreatedBy, Setup string
	EnvVersion                                                            int
	TTL                                                                   *int
	Created, Stopped, Snap, ArchiveAfter                                  time.Time
	Labels                                                                map[string]string
	Ports                                                                 []M
	Commands                                                              []M
	Convs                                                                 []*conv
	Snaps                                                                 []M
	Degraded                                                              string
}

type conv struct {
	ID, Harness, Preview string
	Created, Last        time.Time
	Prompts              int
	Running              bool
	Events               []M
}

var (
	sandboxes  []*sandbox
	nodes      []M
	saved      []M
	shares     = map[string][]M{}
	envs       []M
	keys       []M
	hooks      []M
	orgs       []M
	members    []M
	pending    []M
	myInvites  []M
	profiles   []M
	fsys       = map[string]*fnode{}
	aiProxy    = M{"enabled": true, "enabledAt": ts(ago(240 * time.Hour)), "proxyUrl": "http://10.0.2.2:8420/ai-proxy", "pricing": M{"dailyDollars": 0, "dollarsPer1000Requests": 0}}
	activeOrg  = "acct_demo"
	newPrivate = false
)

var sizes = map[string][3]float64{"small": {2, 4, 0.5}, "default": {4, 8, 1}, "large": {8, 16, 2}, "xlarge": {16, 32, 5.5}}

func ip(n int) *int { return &n }

func nodeByID(id string) M {
	for _, n := range nodes {
		if n["id"] == id {
			return n
		}
	}
	return nil
}

func (s *sandbox) json(full bool) M {
	sz := sizes[s.Type]
	m := M{"id": s.ID, "name": s.Name, "state": s.State, "type": s.Type, "vcpu": int(sz[0]), "memoryGB": int(sz[1]), "billingMultiplier": sz[2],
		"labels": s.Labels, "createdAt": ts(s.Created), "updatedAt": ts(s.Created), "environment": nil, "environmentVersion": nil,
		"desktopAvailable": live(s.State), "snapshotAvailable": !s.Snap.IsZero(), "snapshotCompletedAt": ts(s.Snap), "lastSnapshotStatus": nil,
		"ttlSeconds": nil, "archiveAfter": nil, "stoppedAt": nil, "error": nil, "health": "ok", "hydrated": true, "createdBy": s.CreatedBy, "access": "owner",
		"visibility": nil, "setupStatus": nil, "node": nil, "sshEndpoint": nil}
	if s.Env != "" {
		m["environment"], m["environmentVersion"] = s.Env, s.EnvVersion
	}
	if s.TTL != nil {
		m["ttlSeconds"] = *s.TTL
		if live(s.State) {
			m["archiveAfter"] = ts(s.ArchiveAfter)
		}
	}
	if !s.Snap.IsZero() {
		m["lastSnapshotStatus"] = "completed"
	}
	if s.State == "archived" {
		m["stoppedAt"] = ts(s.Stopped)
	}
	if s.Error != "" {
		m["error"] = s.Error
	}
	if s.Degraded != "" {
		m["health"], m["healthReason"], m["degradedSince"] = "degraded", s.Degraded, ts(ago(4*time.Minute))
	}
	if s.Visibility != "" {
		m["visibility"] = s.Visibility
	}
	if s.Setup != "" {
		m["setupStatus"] = s.Setup
	}
	if s.Node != "" && s.State != "archived" {
		m["nodeId"] = s.Node
	}
	return m
}

// flatNode mirrors internal/server nodeJSON: NodeInfo fields flattened, benchmark only in detail.
func flatNode(n M, detail bool) M {
	info := n["info"].(M)
	b := info["benchmark"].(M)
	m := M{"id": n["id"], "name": n["name"], "online": n["online"], "disabled": false, "ownerId": "acct_" + n["owner"].(M)["login"].(string),
		"os": info["os"], "arch": info["arch"], "driver": info["driver"], "version": info["version"], "labels": info["labels"],
		"capacity": info["capacity"], "usage": n["usage"], "tier": b["tier"], "score": b["score"], "categories": b["categories"],
		"gpus": len(b["gpus"].([]M)), "nested": b["virtualization"].(M)["nested"], "accel": b["virtualization"].(M)["accel"], "lastSeenAt": n["lastSeenAt"]}
	if detail {
		m["benchmark"] = b
	}
	return m
}

func live(state string) bool { return state == "ready" || state == "idle" || state == "running" }

func find(id string) *sandbox {
	for _, s := range sandboxes {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func bench(tier string, score int, cpu string, cores, threads int, memGiB int, disk [3]float64, net [3]float64, via string, gpus []M, cats map[string]string) M {
	return M{"ranAt": ts(ago(time.Duration(rand.Intn(72)+2) * time.Hour)), "durationMs": 41000, "score": score, "tier": tier, "categories": cats,
		"cpu":            M{"model": cpu, "cores": cores, "threads": threads, "singleScore": 1800.0 + float64(score)*3.1, "multiScore": float64(threads) * (1700 + float64(score)*2.4)},
		"memory":         M{"totalMiB": memGiB * 1024, "availableMiB": memGiB * 700, "bandwidthMBps": 9000 + float64(score)*25},
		"disk":           M{"path": "/var/lib/crowdvm", "freeGiB": disk[0], "readMBps": disk[1], "writeMBps": disk[2]},
		"network":        M{"latencyMs": net[0], "downMbps": net[1], "upMbps": net[2], "via": via},
		"gpus":           gpus,
		"virtualization": M{"accel": "kvm", "nested": score > 600, "qemu": "9.2.0"}}
}

func node(id, name, owner, os string, online bool, labels map[string]string, b M, capv [4]int, use [4]int, seen time.Duration) M {
	accel := "kvm"
	if os == "windows" {
		accel = "whpx"
		b["virtualization"].(M)["accel"] = accel
	}
	return M{"id": id, "name": name, "online": online, "owner": M{"login": owner, "displayName": owner}, "lastSeenAt": ts(ago(seen)),
		"info": M{"nodeId": id, "name": name, "version": "0.1.0", "os": os, "arch": "amd64", "driver": "qemu", "labels": labels, "benchmark": b,
			"capacity": M{"vcpu": capv[0], "memoryMiB": capv[1] * 1024, "diskGiB": capv[2], "maxSandboxes": capv[3]}},
		"usage": M{"vcpu": use[0], "memoryMiB": use[1] * 1024, "diskGiB": use[2], "vms": use[3]}}
}

func seed() {
	users["demo"] = &user{ID: "acct_demo", Login: "demo", Display: "Dana Demo", Password: "demo", Created: ago(41 * 24 * time.Hour)}
	users["sam"] = &user{ID: "acct_sam", Login: "sam", Display: "Sam Okafor", Password: "sam", Created: ago(30 * 24 * time.Hour)}

	nodes = []M{
		node("node_7h2kq9xa", "atlas", "dana", "linux", true, map[string]string{"room": "rack"},
			bench("platinum", 912, "AMD Ryzen 9 7950X 16-Core", 16, 32, 128, [3]float64{1480, 6900, 5200}, [3]float64{0.4, 9400, 9100}, "lan",
				[]M{{"vendor": "nvidia", "model": "NVIDIA GeForce RTX 4090", "vramMiB": 24564, "compute": "8.9"}},
				map[string]string{"cpu": "extreme", "memory": "extreme", "disk": "extreme", "network": "high", "gpu": "extreme"}),
			[4]int{24, 96, 900, 8}, [4]int{12, 40, 260, 3}, time.Second),
		node("node_m4b8w1pe", "workbench-lab", "sam", "linux", true, nil,
			bench("gold", 701, "Intel Core i7-12700K", 12, 20, 64, [3]float64{620, 3100, 2700}, [3]float64{0.9, 2300, 2200}, "lan", []M{},
				map[string]string{"cpu": "high", "memory": "high", "disk": "high", "network": "high", "gpu": "none"}),
			[4]int{12, 40, 400, 4}, [4]int{8, 24, 102, 2}, 2*time.Second),
		node("node_q2z6r3vn", "kai-gaming-pc", "kai", "windows", true, map[string]string{"gpu": "yes"},
			bench("gold", 664, "AMD Ryzen 7 5800X3D", 8, 16, 32, [3]float64{410, 3500, 2900}, [3]float64{6.8, 410, 95}, "tailscale",
				[]M{{"vendor": "nvidia", "model": "NVIDIA GeForce RTX 3070", "vramMiB": 8192, "compute": "8.6"}},
				map[string]string{"cpu": "high", "memory": "medium", "disk": "high", "network": "medium", "gpu": "high"}),
			[4]int{8, 16, 200, 2}, [4]int{0, 0, 0, 0}, 3*time.Second),
		node("node_x9t1c5md", "old-thinkpad", "lee", "linux", true, nil,
			bench("bronze", 238, "Intel Core i5-8350U", 4, 8, 16, [3]float64{88, 480, 390}, [3]float64{3.2, 290, 280}, "lan", []M{},
				map[string]string{"cpu": "low", "memory": "low", "disk": "medium", "network": "medium", "gpu": "none"}),
			[4]int{4, 8, 60, 1}, [4]int{2, 4, 51, 1}, time.Second),
		node("node_d3n7y8hs", "mini-01", "dana", "linux", true, nil,
			bench("silver", 455, "Intel N100", 4, 4, 16, [3]float64{210, 1600, 1100}, [3]float64{0.6, 2350, 2340}, "lan", []M{},
				map[string]string{"cpu": "medium", "memory": "low", "disk": "medium", "network": "high", "gpu": "none"}),
			[4]int{4, 12, 180, 2}, [4]int{4, 8, 51, 1}, 4*time.Second),
		node("node_f5v0j2kt", "office-nuc", "priya", "linux", false, nil,
			bench("silver", 512, "Intel Core i7-1165G7", 4, 8, 32, [3]float64{340, 2900, 1800}, [3]float64{1.1, 900, 880}, "tailscale", []M{},
				map[string]string{"cpu": "medium", "memory": "medium", "disk": "high", "network": "medium", "gpu": "none"}),
			[4]int{6, 20, 300, 3}, [4]int{0, 0, 0, 0}, 3*time.Hour+12*time.Minute),
	}

	mk := func(id, name, state, typ, env, nodeID string, created time.Duration, ttl *int, labels map[string]string) *sandbox {
		s := &sandbox{ID: id, Name: name, State: state, Type: typ, Env: env, EnvVersion: 3, Node: nodeID, TTL: ttl, Created: ago(created),
			Labels: labels, CreatedBy: "Dana Demo", Snap: ago(time.Duration(rand.Intn(50)+1) * time.Minute)}
		if labels == nil {
			s.Labels = map[string]string{}
		}
		if ttl != nil {
			s.ArchiveAfter = now.Add(time.Duration(*ttl/2) * time.Second)
		}
		if state == "archived" {
			s.Stopped = ago(created / 3)
		}
		s.Snaps = []M{
			{"id": "6f1c2b7a-3d4e-4f5a-9b0c-1d2e3f4a5b01", "sandboxId": id, "status": "completed", "kind": "base", "generation": 0, "createdAt": ts(s.Created.Add(10 * time.Minute)), "sizeBytes": 2_310_000_000, "fileCount": 214_552, "contentFileCount": 18_204},
			{"id": "6f1c2b7a-3d4e-4f5a-9b0c-1d2e3f4a5b02", "sandboxId": id, "status": "completed", "kind": "incremental", "generation": 1, "createdAt": ts(s.Snap), "sizeBytes": 148_000_000, "fileCount": 214_903, "contentFileCount": 18_555},
		}
		sandboxes = append(sandboxes, s)
		return s
	}
	a := mk("bx_k7m2p9qd", "checkout-refactor", "running", "large", "base", "node_7h2kq9xa", 3*time.Hour, ip(3600), map[string]string{"task": "checkout", "agent": "claude"})
	a.Visibility = "shared"
	a.Setup = "done"
	mk("bx_w3x8n1tv", "flaky-test-hunt", "ready", "default", "base", "node_m4b8w1pe", 50*time.Minute, ip(7200), map[string]string{"ci": "true"})
	b := mk("bx_r5c9h2jz", "gpu-finetune", "provisioning", "xlarge", "ml", "node_7h2kq9xa", 40*time.Second, nil, nil)
	b.Snap = time.Time{}
	mk("bx_a1d4f6gq", "docs-site-preview", "idle", "small", "base", "node_x9t1c5md", 26*time.Hour, ip(86400), nil)
	mk("bx_b8e3k5ms", "api-migration", "archived", "default", "base", "", 5*24*time.Hour, ip(3600), map[string]string{"task": "migrate"})
	c := mk("bx_t6y2u4wp", "electron-repro", "error", "default", "", "node_f5v0j2kt", 7*time.Hour, ip(3600), nil)
	c.Error = "node office-nuc went offline; the last snapshot is safe. Resume to start it on another node."
	d := mk("bx_h9j3l5nx", "bench-nightly", "running", "default", "base", "node_d3n7y8hs", 6*time.Hour, ip(14400), nil)
	d.Degraded = "Node mini-01 answers heartbeats slowly (2.4 s)."
	mk("bx_e2g7i9oz", "playground", "archived", "small", "", "", 12*24*time.Hour, nil, nil)

	a.Ports = []M{{"port": 3000, "url": "http://bx_k7m2p9qd-3000.192.168.1.20.nip.io/?_token=tok_9a8b7c", "access": "private", "isProtected": true, "title": "Next.js dev"}}
	a.Commands = []M{
		cmdRec(a.ID, "npm test -- --runInBand", "exited", 0, 34*time.Minute, 41*time.Second),
		cmdRec(a.ID, "git status --short", "exited", 0, 36*time.Minute, time.Second),
		cmdRec(a.ID, "npm run build", "exited", 1, 52*time.Minute, 73*time.Second),
		cmdRec(a.ID, "npm run dev", "running", 0, 2*time.Hour, 0),
	}
	cv := &conv{ID: "8f1c2b7a-3d4e-4f5a-9b0c-1d2e3f4a5b6c", Harness: "claude", Preview: "Refactor the checkout flow to use the new payments client", Created: ago(2 * time.Hour), Last: ago(20 * time.Minute), Prompts: 2}
	cv.Events = []M{
		ev("prompt", ago(2*time.Hour), M{"prompt": "Refactor the checkout flow to use the new payments client. Keep the public API of CheckoutService unchanged and make the tests pass.", "status": "finished"}),
		ev("response", ago(2*time.Hour-30*time.Second), M{"content": "", "model": "claude-opus-5-5", "tools": []M{{"name": "Read", "input": M{"path": "src/checkout/service.ts"}}, {"name": "Bash", "input": M{"command": "rg -n 'PaymentsClient' src | head"}}}}),
		ev("response", ago(2*time.Hour-4*time.Minute), M{"content": "I moved CheckoutService onto PaymentsClient v2:\n\n• `createIntent` now goes through `client.intents.create` with idempotency keys\n• refunds use the new `client.refunds` namespace\n• the old adapter in src/checkout/legacy.ts is deleted\n\nAll 214 tests pass (`npm test`).", "model": "claude-opus-5-5"}),
		ev("prompt", ago(20*time.Minute), M{"prompt": "Now add a retry with backoff around createIntent for 5xx responses.", "status": "finished"}),
		ev("response", ago(18*time.Minute), M{"content": "", "model": "claude-opus-5-5", "tools": []M{{"name": "Edit", "input": M{"path": "src/checkout/service.ts"}}, {"name": "Bash", "input": M{"command": "npm test -- checkout"}}}}),
		ev("response", ago(17*time.Minute), M{"content": "Added `withRetry` (3 attempts, 200 ms → 1.6 s, full jitter) around `createIntent`; 4xx errors are not retried. Two new tests cover both paths.", "model": "claude-opus-5-5"}),
	}
	a.Convs = []*conv{cv, {ID: "1a2b3c4d-0000-4f5a-9b0c-aaaaaaaaaaaa", Harness: "codex", Preview: "Why is the e2e suite slow?", Created: ago(5 * time.Hour), Last: ago(5 * time.Hour), Prompts: 1,
		Events: []M{ev("prompt", ago(5*time.Hour), M{"prompt": "Why is the e2e suite slow?", "status": "finished"}), ev("response", ago(5*time.Hour-time.Minute), M{"content": "Most of the time is Playwright launching a fresh browser per spec. Reusing one context per worker cuts it from 9m to 3m.", "model": "gpt-6.1-sol"})}}}

	saved = []M{
		{"name": "node22-pnpm", "status": "ready", "sourceSandboxId": "bx_w3x8n1tv", "snapshotId": "6f1c2b7a-3d4e-4f5a-9b0c-1d2e3f4a5b02", "type": "default", "sizeBytes": 6_820_000_000, "createdAt": ts(ago(9 * 24 * time.Hour))},
		{"name": "checkout-baseline", "status": "ready", "sourceSandboxId": "bx_k7m2p9qd", "snapshotId": "6f1c2b7a-3d4e-4f5a-9b0c-1d2e3f4a5b01", "type": "large", "sizeBytes": 11_400_000_000, "createdAt": ts(ago(26 * time.Hour))},
		{"name": "cuda-12", "status": "saving", "sourceSandboxId": "bx_r5c9h2jz", "type": "xlarge", "createdAt": ts(ago(2 * time.Minute))},
	}
	shares["node22-pnpm"] = []M{{"id": "shr_01", "codePrefix": "bsh_7kq2", "status": "active", "useCount": 4, "lastUsedAt": ts(ago(3 * time.Hour)), "revokedAt": nil, "createdAt": ts(ago(8 * 24 * time.Hour))}}

	envs = []M{
		{"id": "0b6f7d1e-0000-4000-8000-000000000001", "name": "base", "isDefault": true, "latestVersionNumber": 3, "safeForThirdParties": false, "passGithub": true, "passSecrets": true, "passSandboxCredentials": true, "passAgentsCredentials": true,
			"envContents": "NODE_ENV=development\nDATABASE_URL=postgres://dev:dev@db.lan:5432/app\nSENTRY_DSN=https://abc@sentry.lan/4\n", "secretFiles": []M{{"path": ".npmrc", "contents": "//npm.lan/:_authToken=npm_xxx\n"}},
			"selectedRepositories": []M{{"id": "repo_1", "url": "https://git.lan/acme/web.git", "baseBranch": "main", "setupScript": "pnpm install"}, {"id": "repo_2", "url": "https://github.com/acme/payments-client.git", "baseBranch": "v2"}},
			"setupScript":          "sudo apt-get install -y ripgrep jq\n",
			"versions":             []M{{"id": "v1", "versionNumber": 1, "sandboxCount": 0, "createdAt": ts(ago(30 * 24 * time.Hour))}, {"id": "v2", "versionNumber": 2, "sandboxCount": 1, "createdAt": ts(ago(9 * 24 * time.Hour))}, {"id": "v3", "versionNumber": 3, "sandboxCount": 4, "createdAt": ts(ago(2 * 24 * time.Hour))}}},
		{"id": "0b6f7d1e-0000-4000-8000-000000000002", "name": "ml", "isDefault": false, "latestVersionNumber": 1, "safeForThirdParties": false, "passGithub": true, "passSecrets": true, "passAgentsCredentials": true,
			"envContents": "HF_HOME=/home/user/.cache/hf\nWANDB_API_KEY=wandb_xxx\n", "secretFiles": []M{}, "selectedRepositories": []M{},
			"versions": []M{{"id": "v1m", "versionNumber": 1, "sandboxCount": 1, "createdAt": ts(ago(4 * 24 * time.Hour))}}},
		{"id": "0b6f7d1e-0000-4000-8000-000000000003", "name": "customer-demos", "isDefault": false, "latestVersionNumber": 1, "safeForThirdParties": true,
			"envContents": "", "secretFiles": []M{}, "selectedRepositories": []M{}, "versions": []M{{"id": "v1c", "versionNumber": 1, "sandboxCount": 0, "createdAt": ts(ago(14 * 24 * time.Hour))}}},
	}

	keys = []M{
		apiKey("key_1", "GitHub Actions", "ci", []string{"sandbox.create", "sandbox.read", "exec"}, ago(2*time.Hour), 18_402, now.Add(64*24*time.Hour), false),
		apiKey("key_2", "crowdvm CLI (laptop)", "admin", []string{"*"}, ago(9*time.Minute), 3_120, time.Time{}, false),
		apiKey("key_3", "Grafana reader", "read-only", []string{"sandbox.read"}, ago(30*time.Hour), 840, now.Add(5*24*time.Hour), true),
	}
	hooks = []M{{"id": "wh_0123456789abcdef01234567", "name": "CI relay", "url": "http://ci.lan:8080/hooks/crowdvm", "events": []string{"sandbox.ready", "sandbox.archived", "sandbox.error"}, "createdAt": ts(ago(20 * 24 * time.Hour)), "updatedAt": ts(ago(3 * 24 * time.Hour))}}

	orgs = []M{
		{"id": "acct_demo", "name": "demo", "type": "personal", "role": "owner", "memberCount": 1},
		{"id": "team_acme", "name": "acme", "type": "org", "role": "owner", "memberCount": 4},
		{"id": "team_lab", "name": "hack-lab", "type": "org", "role": "member", "memberCount": 9},
	}
	members = []M{
		{"id": "acct_demo", "login": "demo", "displayName": "Dana Demo", "role": "owner", "joinedAt": ts(ago(40 * 24 * time.Hour))},
		{"id": "acct_sam", "login": "sam", "displayName": "Sam Okafor", "role": "admin", "joinedAt": ts(ago(30 * 24 * time.Hour))},
		{"id": "acct_kai", "login": "kai", "displayName": "Kai Lindqvist", "role": "member", "joinedAt": ts(ago(12 * 24 * time.Hour))},
		{"id": "acct_lee", "login": "lee", "displayName": "Lee Park", "role": "member", "joinedAt": ts(ago(2 * 24 * time.Hour))},
	}
	pending = []M{{"id": "inv_1", "email": "priya@lab.lan", "role": "member", "status": "pending", "createdAt": ts(ago(26 * time.Hour))}}
	myInvites = []M{{"id": "inv_9", "org": M{"id": "team_rust", "name": "rust-meetup"}, "invitedBy": "jo"}}

	profiles = []M{{"id": "prof_default", "name": "default", "isDefault": true, "owner": M{"id": "acct_demo", "kind": "personal", "name": "demo", "aiProxyEnabled": true}, "canManage": true,
		"connected": []string{"anthropic-api-key", "openai-api-key"}, "customApis": []string{"LINEAR_API_KEY"},
		"accounts":  M{"anthropic-api-key": []M{{"id": "a1", "label": "…x9Q2", "active": true}}, "openai-api-key": []M{{"id": "a2", "label": "…7hTe", "active": true}}},
		"createdAt": ts(ago(40 * 24 * time.Hour)), "updatedAt": ts(ago(2 * 24 * time.Hour))},
		{"id": "prof_acme", "name": "acme-team", "isDefault": false, "owner": M{"id": "team_acme", "kind": "org", "name": "acme", "aiProxyEnabled": true}, "canManage": true,
			"connected": []string{"anthropic-api-key"}, "customApis": []string{}, "createdAt": ts(ago(10 * 24 * time.Hour)), "updatedAt": ts(ago(10 * 24 * time.Hour))}}

	// guest filesystem (shared by all sandboxes)
	dir := func(p string) { fsys[p] = &fnode{dir: true, mod: ago(3 * time.Hour)} }
	file := func(p, c string) { fsys[p] = &fnode{content: c, mod: ago(time.Duration(rand.Intn(300)) * time.Minute)} }
	for _, d := range []string{"/", "/home", "/home/user", "/home/user/web", "/home/user/web/src", "/home/user/web/src/checkout", "/home/user/payments-client", "/home/user/.config", "/tmp"} {
		dir(d)
	}
	file("/home/user/web/package.json", "{\n  \"name\": \"acme-web\",\n  \"private\": true,\n  \"scripts\": {\n    \"dev\": \"next dev\",\n    \"build\": \"next build\",\n    \"test\": \"vitest\"\n  }\n}\n")
	file("/home/user/web/README.md", "# acme web\n\nRun `pnpm dev` and open the public URL for port 3000.\n")
	file("/home/user/web/src/checkout/service.ts", "import { PaymentsClient } from '@acme/payments-client';\n\nexport class CheckoutService {\n  constructor(private client: PaymentsClient) {}\n\n  async createIntent(cartId: string) {\n    return withRetry(() => this.client.intents.create({ cartId }));\n  }\n}\n")
	file("/home/user/web/src/checkout/retry.ts", "export async function withRetry<T>(fn: () => Promise<T>, attempts = 3): Promise<T> {\n  let err: unknown;\n  for (let i = 0; i < attempts; i++) {\n    try { return await fn(); } catch (e) { err = e; await sleep(200 * 2 ** i * Math.random()); }\n  }\n  throw err;\n}\n")
	file("/home/user/.bashrc", "export PATH=$HOME/.local/bin:$PATH\n")
	file("/home/user/notes.md", "- [x] move to payments v2\n- [ ] retry on 5xx\n")
	fsys["/home/user/web/logo.png"] = &fnode{content: string([]byte{0x89, 'P', 'N', 'G', 0, 1, 2}), binary: true, mod: ago(time.Hour)}
}

func ev(typ string, t time.Time, data M) M {
	return M{"id": fmt.Sprintf("ev_%x", rand.Uint32()), "type": typ, "timestamp": t.Unix(), "taskId": nil, "data": data}
}

func cmdRec(sb, cmd, status string, code int, startedAgo, took time.Duration) M {
	m := M{"commandId": fmt.Sprintf("cmd_%x", rand.Uint32()), "sandboxId": sb, "command": cmd, "cwd": "/home/user/web", "mode": "stream", "status": status,
		"exitCode": code, "stdoutBytes": rand.Intn(40000), "stderrBytes": rand.Intn(900), "outputTruncated": false, "startedAt": ts(ago(startedAgo)), "finishedAt": nil, "processId": nil}
	if status == "running" {
		m["exitCode"], m["mode"], m["processId"] = nil, "detached", 4121
	} else {
		m["finishedAt"] = ts(ago(startedAgo - took))
	}
	return m
}

func apiKey(id, name, preset string, actions []string, used time.Time, reqs int, exp time.Time, soon bool) M {
	m := M{"id": id, "name": name, "credentialLane": "scoped-v1", "keyPrefix": "cvm", "keyLastFour": fmt.Sprintf("%04x", rand.Intn(65536)), "sandboxId": nil,
		"createdAt": ts(ago(25 * 24 * time.Hour)), "lastUsedAt": ts(used), "usage": M{"requests": reqs, "windowDays": 30}, "resources": M{"total": 12, "sandboxes": 12, "agents": 0},
		"expiresAt": ts(exp), "expired": false, "expiringSoon": soon, "scope": M{"preset": preset, "actions": actions, "sandboxes": "*", "environments": "*"}}
	return m
}

// ---------------------------------------------------------------- fake guest fs

type fnode struct {
	dir, binary bool
	content     string
	mod         time.Time
}

func entry(p string, n *fnode) M {
	mode := "0644"
	if n.dir {
		mode = "0755"
	}
	return M{"name": path.Base(p), "path": p, "isDir": n.dir, "isSymlink": false, "size": len(n.content), "mode": mode, "modifiedAt": ts(n.mod)}
}

func listDir(p string) []M {
	out := []M{}
	for k, n := range fsys {
		if k != p && path.Dir(k) == p {
			out = append(out, entry(k, n))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return out
}

func cleanPath(p string) string {
	if p == "" || p == "~" {
		return "/home/user"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/home/user/" + strings.TrimPrefix(p, "~/")
	}
	return path.Clean(p)
}

// ---------------------------------------------------------------- routes

func routes(mux *http.ServeMux) {
	// auth
	mux.HandleFunc("GET /auth/me", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if u := current(r); u != nil {
			ok(w, 200, "user.info", M{"user": userJSON(u)})
			return
		}
		fail(w, 401, "unauthorized", "Not signed in.")
	})
	mux.HandleFunc("PATCH /auth/me", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		u.Display = str(body(r), "displayName")
		ok(w, 200, "user.info", M{"user": userJSON(u)})
	}))
	mux.HandleFunc("POST /auth/login", func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		mu.Lock()
		defer mu.Unlock()
		u := users[str(b, "username")]
		if u == nil || u.Password != str(b, "password") {
			fail(w, 401, "invalid_credentials", "Wrong username or password.")
			return
		}
		setSession(w, u)
		ok(w, 200, "user.info", M{"user": userJSON(u)})
	})
	mux.HandleFunc("POST /auth/signup", func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		mu.Lock()
		defer mu.Unlock()
		l := str(b, "username")
		if users[l] != nil {
			fail(w, 409, "username_taken", "That username is taken.")
			return
		}
		if len(str(b, "password")) < 8 {
			fail(w, 400, "weak_password", "Use at least 8 characters.")
			return
		}
		u := &user{ID: "acct_" + l, Login: l, Display: l, Password: str(b, "password"), Created: time.Now()}
		users[l] = u
		setSession(w, u)
		ok(w, 201, "user.info", M{"user": userJSON(u)})
	})
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if c, err := r.Cookie("crowdvm_session"); err == nil {
			delete(sessions, c.Value)
		}
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "crowdvm_session", Path: "/", MaxAge: -1})
		ok(w, 200, "auth.logged_out", nil)
	})
	mux.HandleFunc("POST /auth/password", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		b := body(r)
		if str(b, "currentPassword") != u.Password {
			fail(w, 403, "wrong_password", "The current password is wrong.")
			return
		}
		u.Password = str(b, "newPassword")
		ok(w, 200, "auth.password_changed", nil)
	}))
	mux.HandleFunc("POST /auth/device/approve", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		code := strings.ToUpper(str(body(r), "userCode"))
		if _, found := devices[code]; !found {
			fail(w, 404, "device_code_not_found", "Unknown or expired code.")
			return
		}
		devices[code] = true
		ok(w, 200, "device.approved", nil)
	}))

	A := func(pattern string, h func(http.ResponseWriter, *http.Request, *user)) {
		sp := strings.SplitN(pattern, " ", 2)
		mux.HandleFunc(sp[0]+" /api/v1"+sp[1], authed(h))
	}

	// account / orgs
	A("GET /me", func(w http.ResponseWriter, r *http.Request, u *user) { ok(w, 200, "user.info", M{"user": userJSON(u)}) })
	A("GET /orgs", func(w http.ResponseWriter, r *http.Request, u *user) {
		for _, o := range orgs {
			o["active"] = o["id"] == activeOrg
			if o["id"] == "team_acme" {
				o["newSandboxesPrivate"] = newPrivate
				o["memberCount"] = len(members)
			}
		}
		ok(w, 200, "org.list", M{"orgs": orgs, "activeWalletChosen": true})
	})
	A("POST /orgs", func(w http.ResponseWriter, r *http.Request, u *user) {
		o := M{"id": "team_" + strings.ToLower(str(body(r), "name")), "type": "org", "role": "owner", "memberCount": 1}
		o["name"] = strings.TrimPrefix(o["id"].(string), "team_")
		orgs = append(orgs, o)
		ok(w, 201, "org.created", M{"org": o})
	})
	A("PATCH /orgs/active", func(w http.ResponseWriter, r *http.Request, u *user) {
		b := body(r)
		activeOrg = "acct_demo"
		if s := str(b, "org"); s != "" && s != "personal" {
			activeOrg = s
		}
		ok(w, 200, "org.active_updated", M{"active": M{"id": activeOrg}})
	})
	A("PATCH /orgs/{org}", func(w http.ResponseWriter, r *http.Request, u *user) {
		b := body(r)
		if v, has := b["newSandboxesPrivate"].(bool); has {
			newPrivate = v
		}
		for _, o := range orgs {
			if o["id"] == r.PathValue("org") && str(b, "name") != "" {
				o["name"] = str(b, "name")
			}
		}
		ok(w, 200, "org.updated", nil)
	})
	A("GET /orgs/{org}/members", func(w http.ResponseWriter, r *http.Request, u *user) {
		if r.PathValue("org") != "team_acme" {
			ok(w, 200, "org.members", M{"members": []M{{"id": "acct_jo", "login": "jo", "displayName": "Jo", "role": "owner", "joinedAt": ts(ago(90 * 24 * time.Hour))}, {"id": "acct_demo", "login": "demo", "displayName": "Dana Demo", "role": "member", "joinedAt": ts(ago(20 * 24 * time.Hour))}}, "invites": []M{}})
			return
		}
		ok(w, 200, "org.members", M{"members": members, "invites": pending})
	})
	A("GET /orgs/{org}/invites", func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "org.invites", M{"invites": pending})
	})
	A("POST /orgs/{org}/invites", func(w http.ResponseWriter, r *http.Request, u *user) {
		b := body(r)
		code := fmt.Sprintf("cvi_%016x", rand.Uint64())
		inv := M{"id": fmt.Sprintf("inv_%x", rand.Uint32()), "email": b["email"], "role": str(b, "role"), "status": "pending", "createdAt": ts(time.Now())}
		pending = append(pending, inv)
		ok(w, 201, "org.invite_created", M{"invite": inv, "code": code, "url": "http://" + r.Host + "/invite?code=" + code})
	})
	A("POST /org-invites/accept", func(w http.ResponseWriter, r *http.Request, u *user) {
		if !strings.HasPrefix(str(body(r), "code"), "cvi_") {
			fail(w, 404, "invalid_invite", "The invite is wrong, used, revoked or expired.")
			return
		}
		o := M{"id": "team_rust", "name": "rust-meetup", "type": "org", "role": "member", "memberCount": 14}
		orgs = append(without(orgs, "id", "team_rust"), o)
		ok(w, 200, "org.joined", M{"org": o})
	})
	A("DELETE /orgs/{org}/invites/{id}", func(w http.ResponseWriter, r *http.Request, u *user) {
		pending = without(pending, "id", r.PathValue("id"))
		ok(w, 200, "org.invite_revoked", nil)
	})
	A("PATCH /orgs/{org}/members/{id}", func(w http.ResponseWriter, r *http.Request, u *user) {
		role := str(body(r), "role")
		for _, m := range members {
			if m["id"] == r.PathValue("id") {
				m["role"] = role
			}
		}
		ok(w, 200, "org.member_updated", nil)
	})
	A("DELETE /orgs/{org}/members/{id}", func(w http.ResponseWriter, r *http.Request, u *user) {
		id := r.PathValue("id")
		if id == "me" {
			orgs = without(orgs, "id", r.PathValue("org"))
			if activeOrg == r.PathValue("org") {
				activeOrg = "acct_demo"
			}
		}
		members = without(members, "id", id)
		ok(w, 200, "org.member_removed", nil)
	})
	A("GET /org-invites", func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "org.invites", M{"invites": myInvites})
	})
	A("POST /org-invites/{id}/accept", func(w http.ResponseWriter, r *http.Request, u *user) {
		for _, i := range myInvites {
			if i["id"] == r.PathValue("id") {
				o := i["org"].(M)
				orgs = append(orgs, M{"id": o["id"], "name": o["name"], "type": "org", "role": "member", "memberCount": 14})
			}
		}
		myInvites = without(myInvites, "id", r.PathValue("id"))
		ok(w, 200, "org.joined", nil)
	})
	A("DELETE /org-invites/{id}", func(w http.ResponseWriter, r *http.Request, u *user) {
		myInvites = without(myInvites, "id", r.PathValue("id"))
		ok(w, 200, "org.invite_declined", nil)
	})
	A("GET /limits", func(w http.ResponseWriter, r *http.Request, u *user) {
		n := 0
		for _, s := range sandboxes {
			if s.State != "archived" && s.State != "error" {
				n++
			}
		}
		ok(w, 200, "limits", M{"canStart": true, "activeSandboxes": n, "maxActiveSandboxes": 10, "billingStatus": "credits"})
	})
	A("GET /credits", func(w http.ResponseWriter, r *http.Request, u *user) {
		daily := []M{}
		var earned, spent float64
		for i := 29; i >= 0; i-- {
			day := ago(time.Duration(i) * 24 * time.Hour)
			e := math.Round((6+8*math.Abs(math.Sin(float64(i)/3))+float64(rand.Intn(4)))*10) / 10
			s := math.Round((3+7*math.Abs(math.Cos(float64(i)/4))+float64(rand.Intn(5)))*10) / 10
			if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
				s = math.Round(s*3) / 10
			}
			earned += e
			spent += s
			daily = append(daily, M{"day": day.Format("2006-01-02"), "earned": e, "spent": s})
		}
		ledger := []M{
			{"createdAt": ts(ago(14 * time.Minute)), "kind": "earn", "delta": 2.4, "reason": "Hosting on atlas", "nodeId": "node_7h2kq9xa"},
			{"createdAt": ts(ago(40 * time.Minute)), "kind": "usage", "delta": -2.0, "reason": "checkout-refactor · large · 1 h", "sandboxId": "bx_k7m2p9qd"},
			{"createdAt": ts(ago(70 * time.Minute)), "kind": "usage", "delta": -0.5, "reason": "docs-site-preview · small · 1 h", "sandboxId": "bx_a1d4f6gq"},
			{"createdAt": ts(ago(2 * time.Hour)), "kind": "earn", "delta": 1.0, "reason": "Hosting on mini-01", "nodeId": "node_d3n7y8hs"},
			{"createdAt": ts(ago(3 * 24 * time.Hour)), "kind": "grant", "delta": 100, "reason": "Granted by admin: welcome credits"},
		}
		ok(w, 200, "credits.info", M{"wallet": M{"id": u.ID, "balanceCredits": 184.6, "enforced": false}, "earnedCredits": earned, "spentCredits": spent,
			"rates": M{"creditsPerDefaultHour": 1, "nodeEarnRate": 1}, "daily": daily, "ledger": ledger})
	})

	// nodes
	A("GET /nodes", func(w http.ResponseWriter, r *http.Request, u *user) {
		out := []M{}
		for _, n := range nodes {
			out = append(out, flatNode(n, false))
		}
		ok(w, 200, "node.list", M{"nodes": out})
	})
	A("GET /nodes/{nid}", func(w http.ResponseWriter, r *http.Request, u *user) {
		if n := nodeByID(r.PathValue("nid")); n != nil {
			ok(w, 200, "node.info", M{"node": flatNode(n, true)})
			return
		}
		fail(w, 404, "node_not_found", "No such node.")
	})
	A("POST /join-tokens", func(w http.ResponseWriter, r *http.Request, u *user) {
		tok := fmt.Sprintf("cvmj_%016x", rand.Uint64())
		ok(w, 201, "join_token.created", M{"token": tok, "command": "crowdvm-node join --server http://" + r.Host + " --token " + tok, "expiresAt": ts(time.Now().Add(24 * time.Hour))})
	})

	// sandboxes
	A("GET /sandboxes", func(w http.ResponseWriter, r *http.Request, u *user) {
		out := []M{}
		for i := len(sandboxes) - 1; i >= 0; i-- {
			out = append(out, sandboxes[i].json(false))
		}
		ok(w, 200, "sandbox.list", M{"sandboxes": out, "pageInfo": M{"hasMore": false, "limit": 200, "nextCursor": nil}})
	})
	A("POST /sandboxes", func(w http.ResponseWriter, r *http.Request, u *user) {
		b := body(r)
		typ := str(b, "type")
		if typ == "" {
			typ = "default"
		}
		name := str(b, "name")
		if name == "" {
			name = "sandbox " + time.Now().Format("2006-01-02 15:04")
		}
		env := str(b, "environment")
		if env == "" && b["noEnv"] != true {
			env = "base"
		}
		var ttl *int
		if v, has := b["ttlSeconds"].(float64); has {
			ttl = ip(int(v))
		}
		nodeID := "node_m4b8w1pe"
		if p, _ := b["placement"].(M); p != nil && str(p, "node") != "" {
			nodeID = str(p, "node")
		}
		s := &sandbox{ID: newID(), Name: name, State: "provisioning", Type: typ, Env: env, EnvVersion: 3, Node: nodeID, TTL: ttl, Created: time.Now(), Labels: map[string]string{}, CreatedBy: u.Display}
		if l, _ := b["labels"].(M); l != nil {
			for k, v := range l {
				s.Labels[k] = fmt.Sprint(v)
			}
		}
		if str(b, "setupScript") != "" {
			s.Setup = "pending"
		}
		if ttl != nil {
			s.ArchiveAfter = time.Now().Add(time.Duration(*ttl) * time.Second)
		}
		sandboxes = append(sandboxes, s)
		transition(s, "provisioning", "provisioned", 2*time.Second)
		transition(s, "provisioned", "ready", 5*time.Second)
		ok(w, 201, "sandbox.created", M{"status": "provisioning", "ttlSeconds": ttl, "sandbox": s.json(true)})
	})
	A("GET /sandboxes/{id}", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		ok(w, 200, "sandbox.info", M{"sandbox": s.json(true)})
	}))
	A("PATCH /sandboxes/{id}", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		b := body(r)
		if n := str(b, "name"); n != "" {
			s.Name = n
		}
		if v, has := b["ttlSeconds"]; has {
			if f, isNum := v.(float64); isNum {
				s.TTL = ip(int(f))
				s.ArchiveAfter = time.Now().Add(time.Duration(f) * time.Second)
			} else {
				s.TTL = nil
			}
		}
		ok(w, 200, "sandbox.updated", M{"sandbox": s.json(true)})
	}))
	A("DELETE /sandboxes/{id}", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		if r.Header.Get("X-CrowdVM-Confirm-Delete") != s.ID {
			fail(w, 409, "confirm_delete_mismatch", "X-CrowdVM-Confirm-Delete must equal the sandbox id.")
			return
		}
		for i, x := range sandboxes {
			if x == s {
				sandboxes = append(sandboxes[:i], sandboxes[i+1:]...)
				break
			}
		}
		ok(w, 202, "sandbox.deleting", M{"id": s.ID})
	}))
	A("POST /sandboxes/{id}/stop", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		s.State = "archiving"
		go func() {
			time.Sleep(4 * time.Second)
			mu.Lock()
			if s.State == "archiving" {
				s.State, s.Stopped, s.Snap = "archived", time.Now(), time.Now()
			}
			mu.Unlock()
		}()
		ok(w, 202, "sandbox.stopping", M{"id": s.ID, "status": "archiving", "sandbox": s.json(true)})
	}))
	resume := withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		b := body(r)
		if t := str(b, "type"); t != "" {
			s.Type = t
		}
		s.State, s.Error, s.Node = "provisioning", "", "node_7h2kq9xa"
		transition(s, "provisioning", "ready", 4*time.Second)
		ok(w, 202, "sandbox.resuming", M{"id": s.ID, "status": "provisioning", "sandbox": s.json(true)})
	})
	A("POST /sandboxes/{id}/resume", resume)
	A("POST /sandboxes/{id}/recover", resume)
	A("POST /sandboxes/{id}/fork", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		b := body(r)
		f := *s
		f.ID, f.State, f.Created, f.Convs, f.Commands, f.Ports = newID(), "cloning", time.Now(), nil, nil, nil
		if n := str(b, "name"); n != "" {
			f.Name = n
		}
		sandboxes = append(sandboxes, &f)
		transition(&f, "cloning", "ready", 3*time.Second)
		ok(w, 202, "sandbox.forking", M{"id": f.ID, "status": "cloning", "sandbox": f.json(true)})
	}))
	A("POST /sandboxes/{id}/share", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		s.Visibility = "shared"
		ok(w, 200, "sandbox.shared", nil)
	}))
	A("POST /sandboxes/{id}/unshare", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		s.Visibility = "private"
		ok(w, 200, "sandbox.unshared", nil)
	}))
	A("GET /sandboxes/{id}/usage", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		secs := time.Since(s.Created).Seconds()
		if s.State == "archived" {
			secs = s.Stopped.Sub(s.Created).Seconds()
		}
		mult := sizes[s.Type][2]
		ok(w, 200, "sandbox.usage", M{"sandboxId": s.ID, "sandboxType": s.Type, "billingMultiplier": mult, "seconds": int(secs * mult), "credits": secs * mult / 3600, "running": live(s.State), "since": ts(s.Created), "until": ts(time.Now())})
	}))
	A("GET /sandboxes/{id}/processes", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		ok(w, 200, "process.list", M{"live": true, "total": 143, "sampledAt": ts(time.Now()), "processes": []M{
			{"pid": 4121, "command": "node /home/user/web/node_modules/.bin/next dev", "cpuPercent": 18.2 + rand.Float64()*10, "memoryMB": 812},
			{"pid": 3990, "command": "claude --dangerously-skip-permissions", "cpuPercent": 4.1 + rand.Float64()*3, "memoryMB": 344},
			{"pid": 812, "command": "/usr/bin/dockerd -H fd://", "cpuPercent": 0.6, "memoryMB": 96},
			{"pid": 1504, "command": "Xvnc :1 -geometry 1920x1080", "cpuPercent": 1.9, "memoryMB": 140},
			{"pid": 1, "command": "/sbin/init", "cpuPercent": 0.0, "memoryMB": 12},
		}})
	}))
	A("GET /sandboxes/{id}/command-history", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		ok(w, 200, "command.list", M{"commands": orEmpty(s.Commands), "nextCursor": nil})
	}))
	A("GET /sandboxes/{id}/command-history/{cid}/logs", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		data := "\n> acme-web@1.0.0 test\n> vitest --run\n\n ✓ src/checkout/service.test.ts (12)\n ✓ src/checkout/retry.test.ts (2)\n\n Test Files  2 passed (2)\n      Tests  14 passed (14)\n"
		if r.URL.Query().Get("stream") == "stderr" {
			data = ""
		}
		for _, c := range s.Commands {
			if c["commandId"] == r.PathValue("cid") && c["out"] != nil {
				data = c["out"].(string)
				if r.URL.Query().Get("stream") == "stderr" {
					data, _ = c["err"].(string)
				}
			}
		}
		ok(w, 200, "command.logs", M{"commandId": r.PathValue("cid"), "stream": r.URL.Query().Get("stream"), "offset": 0, "nextOffset": len(data), "size": len(data), "encoding": "utf8", "data": data, "done": true})
	}))
	A("DELETE /sandboxes/{id}/commands/{pid}", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		for _, c := range s.Commands {
			if fmt.Sprint(c["processId"]) == r.PathValue("pid") {
				c["status"], c["exitCode"], c["finishedAt"] = "killed", nil, ts(time.Now())
			}
		}
		ok(w, 200, "command.stopped", M{"running": false, "signaled": true})
	}))
	mux.HandleFunc("POST /api/v1/sandboxes/{id}/commands", authedStream(runCommand))

	// files
	A("GET /sandboxes/{id}/files/list", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		p := cleanPath(r.URL.Query().Get("path"))
		n := fsys[p]
		if n == nil || !n.dir {
			fail(w, 404, "not_found", "No such folder: "+p)
			return
		}
		ok(w, 200, "file.list", M{"path": p, "files": listDir(p), "truncated": false})
	}))
	A("GET /sandboxes/{id}/files", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		p := cleanPath(r.URL.Query().Get("path"))
		n := fsys[p]
		if n == nil || n.dir {
			fail(w, 404, "not_found", "No such file: "+p)
			return
		}
		enc := "utf8"
		if n.binary {
			enc = "base64"
		}
		ok(w, 200, "file.read", M{"path": p, "encoding": enc, "size": len(n.content), "content": n.content})
	}))
	A("PUT /sandboxes/{id}/files", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		b := body(r)
		p := cleanPath(str(b, "path"))
		fsys[p] = &fnode{content: str(b, "content"), mod: time.Now()}
		ok(w, 200, "file.written", M{"path": p, "encoding": "utf8", "size": len(str(b, "content"))})
	}))
	A("DELETE /sandboxes/{id}/files", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		p := cleanPath(r.URL.Query().Get("path"))
		for k := range fsys {
			if k == p || strings.HasPrefix(k, p+"/") {
				delete(fsys, k)
			}
		}
		ok(w, 200, "file.deleted", M{"path": p})
	}))
	A("POST /sandboxes/{id}/files/folder", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		p := cleanPath(str(body(r), "path"))
		fsys[p] = &fnode{dir: true, mod: time.Now()}
		ok(w, 200, "folder.created", entry(p, fsys[p]))
	}))
	A("POST /sandboxes/{id}/files/move", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		b := body(r)
		src, dst := cleanPath(str(b, "source")), cleanPath(str(b, "destination"))
		if n := fsys[src]; n != nil {
			delete(fsys, src)
			fsys[dst] = n
		}
		ok(w, 200, "file.moved", M{"path": src, "destination": dst})
	}))
	A("POST /sandboxes/{id}/files/download-url", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		p := cleanPath(str(body(r), "path"))
		ok(w, 200, "file.download_url", M{"url": "/dl?path=" + p, "method": "GET", "path": p, "expiresAt": ts(time.Now().Add(5 * time.Minute))})
	}))
	A("POST /sandboxes/{id}/files/upload-url", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		p := cleanPath(str(body(r), "path"))
		ok(w, 200, "file.upload_url", M{"url": "/ul?path=" + p, "method": "PUT", "path": p, "expiresAt": ts(time.Now().Add(10 * time.Minute))})
	}))
	mux.HandleFunc("GET /dl", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := fsys[r.URL.Query().Get("path")]
		mu.Unlock()
		if n == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(path.Base(r.URL.Query().Get("path"))))
		io.WriteString(w, n.content)
	})
	mux.HandleFunc("PUT /ul", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		mu.Lock()
		fsys[r.URL.Query().Get("path")] = &fnode{content: string(b), mod: time.Now()}
		mu.Unlock()
		w.WriteHeader(200)
	})

	// agent
	A("GET /sandboxes/{id}/conversations", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		out := []M{}
		sort.Slice(s.Convs, func(i, j int) bool { return s.Convs[i].Last.After(s.Convs[j].Last) })
		for i, c := range s.Convs {
			out = append(out, M{"id": c.ID, "createdAt": ts(c.Created), "lastPromptAt": ts(c.Last), "prompts": c.Prompts, "running": c.Running, "lastHarness": c.Harness, "lastModel": nil, "lastPromptPreview": c.Preview, "current": i == 0})
		}
		ok(w, 200, "conversation.list", M{"id": s.ID, "conversations": out})
	}))
	A("GET /sandboxes/{id}/events", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		evs := []M{}
		for _, c := range s.Convs {
			if c.ID == r.URL.Query().Get("conversation") {
				evs = c.Events
			}
		}
		ok(w, 200, "events.list", M{"id": s.ID, "events": evs})
	}))
	A("POST /sandboxes/{id}/prompt", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		b := body(r)
		var c *conv
		for _, x := range s.Convs {
			if x.ID == str(b, "conversationId") {
				c = x
			}
		}
		if c == nil {
			c = &conv{ID: fmt.Sprintf("%08x-0000-4000-8000-%012x", rand.Uint32(), rand.Uint64()&0xffffffffffff), Harness: str(b, "provider"), Created: time.Now()}
			s.Convs = append(s.Convs, c)
		}
		text := str(b, "prompt")
		c.Preview, c.Last, c.Running = trunc(text, 80), time.Now(), true
		c.Prompts++
		p := ev("prompt", time.Now(), M{"prompt": text, "status": "running"})
		c.Events = append(c.Events, p)
		go fakeAgent(c, p, text)
		ok(w, 202, "prompt.queued", M{"id": s.ID, "promptId": p["id"], "conversationId": c.ID, "status": "queued", "provider": c.Harness, "promptRun": M{"id": p["id"], "status": "queued", "done": false}})
	}))
	A("POST /sandboxes/{id}/steer", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		b := body(r)
		for _, c := range s.Convs {
			if c.ID == str(b, "conversation") {
				c.Events = append(c.Events, ev("steer", time.Now(), M{"message": str(b, "message"), "native": true, "mode": "native"}))
			}
		}
		ok(w, 200, "steer.accepted", M{"native": true, "mode": "native"})
	}))
	A("POST /sandboxes/{id}/interrupt", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		for _, c := range s.Convs {
			if c.ID == r.URL.Query().Get("conversation") {
				c.Running = false
				for _, e := range c.Events {
					if d := e["data"].(M); d["status"] == "running" {
						d["status"] = "interrupted"
					}
				}
			}
		}
		ok(w, 200, "sandbox.interrupted", M{"id": s.ID, "status": "interrupted"})
	}))

	// desktop
	A("POST /sandboxes/{id}/desktop", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		if !live(s.State) {
			fail(w, 409, "machine_not_running", "The sandbox is not running.")
			return
		}
		tok := fmt.Sprintf("dt_%x", rand.Uint64())
		ok(w, 200, "desktop.url", M{"desktopUrl": "http://" + r.Host + "/desktop/" + s.ID + "?token=" + tok, "wsUrl": "/api/v1/sandboxes/" + s.ID + "/desktop/ws?token=" + tok, "mode": "vnc", "provisioning": false})
	}))
	mux.HandleFunc("GET /api/v1/sandboxes/{id}/desktop/ws", fakeVNC)

	// ports
	A("GET /sandboxes/{id}/host", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		ok(w, 200, "host.list", M{"ports": orEmpty(s.Ports)})
	}))
	A("POST /sandboxes/{id}/host", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		b := body(r)
		port := int(b["port"].(float64))
		access, url := "private", fmt.Sprintf("http://%s-%d.192.168.1.20.nip.io/?_token=tok_%x", s.ID, port, rand.Uint32())
		if b["public"] == true {
			access, url = "public", fmt.Sprintf("http://%s-%d.192.168.1.20.nip.io/", s.ID, port)
		}
		p := M{"port": port, "url": url, "access": access, "isProtected": access == "private", "localhost": b["localhost"] == true, "title": str(b, "title")}
		s.Ports = append(without(s.Ports, "port", port), p)
		ok(w, 200, "host.created", p)
	}))
	A("DELETE /sandboxes/{id}/host/{port}", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		n, _ := strconv.Atoi(r.PathValue("port"))
		s.Ports = without(s.Ports, "port", n)
		ok(w, 200, "host.removed", nil)
	}))

	// snapshots
	A("GET /sandboxes/{id}/snapshots", withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		ok(w, 200, "snapshot.list", M{"snapshots": orEmpty(s.Snaps)})
	}))
	A("GET /snapshots/{sid}/tree", func(w http.ResponseWriter, r *http.Request, u *user) {
		entries := []M{}
		for k, n := range fsys {
			if strings.HasPrefix(k, "/home/user") {
				kind := "file"
				if n.dir {
					kind = "dir"
				}
				entries = append(entries, M{"path": k, "kind": kind, "size": len(n.content)})
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i]["path"].(string) < entries[j]["path"].(string) })
		ok(w, 200, "snapshot.tree", M{"snapshotId": r.PathValue("sid"), "treeAvailable": true, "truncated": false, "fileCount": len(entries), "totalSizeBytes": 18_204_000, "entries": entries})
	})
	A("GET /saved-snapshots", func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "snapshot.saved.list", M{"snapshots": saved, "allowance": M{"included": 10, "used": len(saved)}})
	})
	A("POST /saved-snapshots", func(w http.ResponseWriter, r *http.Request, u *user) {
		b := body(r)
		snap := M{"name": str(b, "name"), "status": "saving", "sourceSandboxId": str(b, "sandboxId"), "type": "default", "createdAt": ts(time.Now())}
		saved = append([]M{snap}, without(saved, "name", str(b, "name"))...)
		go func() {
			time.Sleep(5 * time.Second)
			mu.Lock()
			snap["status"], snap["sizeBytes"], snap["snapshotId"] = "ready", 5_400_000_000, "6f1c2b7a-3d4e-4f5a-9b0c-1d2e3f4a5b02"
			mu.Unlock()
		}()
		ok(w, 202, "snapshot.saved.saving", M{"status": "saving", "snapshot": snap})
	})
	A("DELETE /saved-snapshots/{name}", func(w http.ResponseWriter, r *http.Request, u *user) {
		saved = without(saved, "name", r.PathValue("name"))
		ok(w, 200, "snapshot.saved.deleted", M{"name": r.PathValue("name"), "status": "deleted"})
	})
	A("GET /saved-snapshots/{name}/shares", func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "snapshot.share.list", M{"name": r.PathValue("name"), "shares": orEmpty(shares[r.PathValue("name")])})
	})
	A("POST /saved-snapshots/{name}/shares", func(w http.ResponseWriter, r *http.Request, u *user) {
		code := fmt.Sprintf("bsh_%016x%08x", rand.Uint64(), rand.Uint32())
		sh := M{"id": fmt.Sprintf("shr_%x", rand.Uint32()), "codePrefix": code[:8], "status": "active", "useCount": 0, "lastUsedAt": nil, "revokedAt": nil, "createdAt": ts(time.Now())}
		shares[r.PathValue("name")] = append(shares[r.PathValue("name")], sh)
		ok(w, 201, "snapshot.share.created", M{"name": r.PathValue("name"), "shareCode": code, "share": sh})
	})
	A("DELETE /saved-snapshots/{name}/shares/{sid}", func(w http.ResponseWriter, r *http.Request, u *user) {
		for _, sh := range shares[r.PathValue("name")] {
			if sh["id"] == r.PathValue("sid") {
				sh["status"], sh["revokedAt"] = "revoked", ts(time.Now())
			}
		}
		ok(w, 200, "snapshot.share.revoked", nil)
	})

	// environments
	A("GET /environments", func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "environments.list", M{"environments": envs})
	})
	A("GET /environments/{eid}", func(w http.ResponseWriter, r *http.Request, u *user) {
		if e := envByID(r.PathValue("eid")); e != nil {
			ok(w, 200, "environment.info", M{"environment": e})
			return
		}
		fail(w, 404, "environment_not_found", "No such environment.")
	})
	A("POST /environments", func(w http.ResponseWriter, r *http.Request, u *user) {
		e := M{"id": fmt.Sprintf("0b6f7d1e-0000-4000-8000-%012x", rand.Uint64()&0xffffffffffff), "name": str(body(r), "name"), "isDefault": false, "latestVersionNumber": 1,
			"passSecrets": true, "passGithub": true, "passAgentsCredentials": true, "envContents": "", "secretFiles": []M{}, "selectedRepositories": []M{},
			"versions": []M{{"id": "v1n", "versionNumber": 1, "sandboxCount": 0, "createdAt": ts(time.Now())}}}
		envs = append(envs, e)
		ok(w, 201, "environment.created", M{"success": true, "environment": e})
	})
	A("PUT /environments/{eid}", envMut(func(e M, r *http.Request) {
		b := body(r)
		for k, v := range b {
			if k == "isDefault" && v == true {
				for _, x := range envs {
					x["isDefault"] = false
				}
			}
			e[k] = v
		}
	}))
	A("DELETE /environments/{eid}", func(w http.ResponseWriter, r *http.Request, u *user) {
		envs = without(envs, "id", r.PathValue("eid"))
		ok(w, 200, "environment.deleted", M{"success": true})
	})
	A("POST /environments/{eid}/upgrade", envMut(func(e M, r *http.Request) {
		vs := e["versions"].([]M)
		total := 0
		for _, v := range vs[:len(vs)-1] {
			total += v["sandboxCount"].(int)
			v["sandboxCount"] = 0
		}
		vs[len(vs)-1]["sandboxCount"] = vs[len(vs)-1]["sandboxCount"].(int) + total
	}))
	A("PUT /environments/{eid}/vars/{key}", envMut(func(e M, r *http.Request) {
		k, v := r.PathValue("key"), str(body(r), "value")
		e["envContents"] = setEnvLine(e["envContents"].(string), k, &v)
	}))
	A("DELETE /environments/{eid}/vars/{key}", envMut(func(e M, r *http.Request) {
		e["envContents"] = setEnvLine(e["envContents"].(string), r.PathValue("key"), nil)
	}))
	A("PUT /environments/{eid}/secret-files", envMut(func(e M, r *http.Request) {
		b := body(r)
		e["secretFiles"] = append(without(e["secretFiles"].([]M), "path", str(b, "path")), M{"path": str(b, "path"), "contents": str(b, "contents")})
	}))
	A("DELETE /environments/{eid}/secret-files", envMut(func(e M, r *http.Request) {
		e["secretFiles"] = without(e["secretFiles"].([]M), "path", r.URL.Query().Get("path"))
	}))
	A("POST /environments/{eid}/repos", envMut(func(e M, r *http.Request) {
		b := body(r)
		e["selectedRepositories"] = append(e["selectedRepositories"].([]M), M{"id": fmt.Sprintf("repo_%x", rand.Uint32()), "url": str(b, "url"), "baseBranch": str(b, "branch"), "path": str(b, "path"), "setupScript": str(b, "setupScript")})
	}))
	A("DELETE /environments/{eid}/repos/{rid}", envMut(func(e M, r *http.Request) {
		e["selectedRepositories"] = without(e["selectedRepositories"].([]M), "id", r.PathValue("rid"))
	}))

	// AI credentials
	A("GET /credential-profiles", func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "credential_profile.list", M{"profiles": profiles})
	})
	A("POST /credential-profiles", func(w http.ResponseWriter, r *http.Request, u *user) {
		b := body(r)
		p := M{"id": fmt.Sprintf("prof_%x", rand.Uint32()), "name": str(b, "name"), "isDefault": false, "owner": M{"id": "acct_demo", "kind": "personal", "name": "demo"}, "canManage": true, "connected": []string{}, "customApis": []string{}}
		profiles = append(profiles, p)
		ok(w, 201, "credential_profile.created", M{"success": true, "profile": p})
	})
	A("PUT /credential-profiles/{pid}", profMut(func(p M, r *http.Request) {
		if body(r)["isDefault"] == true {
			for _, x := range profiles {
				x["isDefault"] = false
			}
			p["isDefault"] = true
		}
	}))
	A("DELETE /credential-profiles/{pid}", func(w http.ResponseWriter, r *http.Request, u *user) {
		profiles = without(profiles, "id", r.PathValue("pid"))
		ok(w, 200, "credential_profile.deleted", M{"success": true})
	})
	A("PUT /credential-profiles/{pid}/keys/{cred}", profMut(func(p M, r *http.Request) {
		c := r.PathValue("cred")
		p["connected"] = append(removeStr(p["connected"].([]string), c), c)
		v := str(body(r), "value")
		if p["accounts"] == nil {
			p["accounts"] = M{}
		}
		p["accounts"].(M)[c] = []M{{"id": "acc", "label": "…" + v[max(0, len(v)-4):], "active": true}}
	}))
	A("DELETE /credential-profiles/{pid}/keys/{cred}", profMut(func(p M, r *http.Request) {
		p["connected"] = removeStr(p["connected"].([]string), r.PathValue("cred"))
	}))
	A("PUT /credential-profiles/{pid}/custom-apis/{name}", profMut(func(p M, r *http.Request) {
		p["customApis"] = append(removeStr(p["customApis"].([]string), r.PathValue("name")), r.PathValue("name"))
	}))
	A("DELETE /credential-profiles/{pid}/custom-apis/{name}", profMut(func(p M, r *http.Request) {
		p["customApis"] = removeStr(p["customApis"].([]string), r.PathValue("name"))
	}))
	A("GET /ai-proxy", func(w http.ResponseWriter, r *http.Request, u *user) { ok(w, 200, "ai_proxy.settings", aiProxy) })
	A("PUT /ai-proxy", func(w http.ResponseWriter, r *http.Request, u *user) {
		aiProxy["enabled"] = body(r)["enabled"] == true
		ok(w, 200, "ai_proxy.settings", aiProxy)
	})
	A("GET /ai-usage", func(w http.ResponseWriter, r *http.Request, u *user) {
		tot := func(req, in, out int) M {
			return M{"requests": req, "errors": req / 90, "inputTokens": in, "outputTokens": out, "cacheReadTokens": in * 3, "cacheWriteTokens": in / 4}
		}
		ok(w, 200, "ai_usage", M{"since": ago(30 * 24 * time.Hour).Format("2006-01-02"), "totals": tot(4210, 61_300_000, 2_940_000), "dollars": 0, "usage": []M{},
			"sandboxes": []M{{"sandboxId": "bx_k7m2p9qd", "name": "checkout-refactor", "totals": tot(2870, 0, 0)}, {"sandboxId": "bx_w3x8n1tv", "name": "flaky-test-hunt", "totals": tot(911, 0, 0)}, {"sandboxId": "bx_h9j3l5nx", "name": "bench-nightly", "totals": tot(429, 0, 0)}}})
	})

	// API keys
	A("GET /api-keys", func(w http.ResponseWriter, r *http.Request, u *user) {
		sb, env := []M{}, []M{}
		for _, s := range sandboxes {
			sb = append(sb, M{"id": s.ID, "name": s.Name, "organizationId": "", "organizationName": ""})
		}
		for _, e := range envs {
			env = append(env, M{"id": e["id"], "name": e["name"], "organizationId": "", "organizationName": ""})
		}
		ok(w, 200, "api_key.list", M{"apiKeys": keys, "catalog": M{
			"actions":    []string{"sandbox.create", "sandbox.read", "sandbox.update", "sandbox.stop", "sandbox.resume", "sandbox.fork", "sandbox.delete", "sandbox.delete-own", "agent.prompt", "exec", "file.read", "file.write", "ssh", "desktop", "host", "snapshot.read", "snapshot.write", "environment.read", "environment.write", "account.read", "account.admin", "*"},
			"presets":    M{"read-only": []string{"sandbox.read"}, "admin": []string{"*"}},
			"defaultTtl": "90d", "maxTtl": "365d", "scopedCreationEnabled": true, "scopeResources": M{"sandboxes": sb, "environments": env}}})
	})
	A("POST /api-keys/scoped", func(w http.ResponseWriter, r *http.Request, u *user) {
		b := body(r)
		preset := str(b, "preset")
		acts := []string{}
		if a, _ := b["actions"].([]any); a != nil {
			for _, x := range a {
				acts = append(acts, fmt.Sprint(x))
			}
		}
		k := apiKey(fmt.Sprintf("key_%x", rand.Uint32()), str(b, "name"), preset, acts, time.Time{}, 0, time.Now().Add(90*24*time.Hour), false)
		k["createdAt"] = ts(time.Now())
		keys = append(keys, k)
		ok(w, 201, "api_key.created", M{"apiKey": k, "secret": fmt.Sprintf("cvm_%016x%016x", rand.Uint64(), rand.Uint64()), "apiKeys": keys})
	})
	A("POST /api-keys/{kid}/rotate", func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "api_key.rotated", M{"secret": fmt.Sprintf("cvm_%016x%016x", rand.Uint64(), rand.Uint64()), "apiKeys": keys})
	})
	A("DELETE /api-keys/{kid}", func(w http.ResponseWriter, r *http.Request, u *user) {
		keys = without(keys, "id", r.PathValue("kid"))
		ok(w, 200, "api_key.revoked", M{"apiKeys": keys})
	})
	A("GET /api-keys/{kid}/usage", func(w http.ResponseWriter, r *http.Request, u *user) {
		for _, k := range keys {
			if k["id"] == r.PathValue("kid") {
				m := M{}
				for a, b := range k {
					m[a] = b
				}
				m["createdResources"] = []M{{"kind": "sandbox", "id": "bx_w3x8n1tv", "name": "flaky-test-hunt", "state": "ready", "createdAt": ts(ago(50 * time.Minute))}, {"kind": "sandbox", "id": "bx_b8e3k5ms", "name": "api-migration", "state": "archived", "createdAt": ts(ago(5 * 24 * time.Hour))}}
				ok(w, 200, "api_key.usage", m)
				return
			}
		}
		fail(w, 404, "not_found", "No such key.")
	})

	// webhooks
	A("GET /webhooks", func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "webhook.list", M{"webhooks": hooks})
	})
	A("POST /webhooks", func(w http.ResponseWriter, r *http.Request, u *user) {
		b := body(r)
		h := M{"id": fmt.Sprintf("wh_%024x", rand.Uint64()), "name": b["name"], "url": str(b, "url"), "events": b["events"], "createdAt": ts(time.Now()), "updatedAt": ts(time.Now())}
		hooks = append(hooks, h)
		ok(w, 201, "webhook.created", M{"webhook": h, "secret": fmt.Sprintf("whsec_%016x%016x", rand.Uint64(), rand.Uint64())})
	})
	A("PATCH /webhooks/{hid}", func(w http.ResponseWriter, r *http.Request, u *user) {
		b := body(r)
		for _, h := range hooks {
			if h["id"] == r.PathValue("hid") {
				for k, v := range b {
					h[k] = v
				}
				h["updatedAt"] = ts(time.Now())
			}
		}
		ok(w, 200, "webhook.updated", nil)
	})
	A("POST /webhooks/{hid}/rotate", func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "webhook.rotated", M{"secret": fmt.Sprintf("whsec_%016x%016x", rand.Uint64(), rand.Uint64())})
	})
	A("DELETE /webhooks/{hid}", func(w http.ResponseWriter, r *http.Request, u *user) {
		hooks = without(hooks, "id", r.PathValue("hid"))
		ok(w, 200, "webhook.deleted", M{"id": r.PathValue("hid")})
	})

	moreRoutes(mux)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, 404, "not_found", "No such endpoint in the devserver mock: "+r.Method+" "+r.URL.Path)
	})
}

func withSB(h func(http.ResponseWriter, *http.Request, *sandbox)) func(http.ResponseWriter, *http.Request, *user) {
	return func(w http.ResponseWriter, r *http.Request, u *user) {
		s := find(r.PathValue("id"))
		if s == nil {
			fail(w, 404, "sandbox_not_found", "No such sandbox.")
			return
		}
		h(w, r, s)
	}
}

func envByID(id string) M {
	for _, e := range envs {
		if e["id"] == id || e["name"] == id {
			return e
		}
	}
	return nil
}

// envMut applies f and mints a new version, like the real API.
func envMut(f func(M, *http.Request)) func(http.ResponseWriter, *http.Request, *user) {
	return func(w http.ResponseWriter, r *http.Request, u *user) {
		e := envByID(r.PathValue("eid"))
		if e == nil {
			fail(w, 404, "environment_not_found", "No such environment.")
			return
		}
		f(e, r)
		if !strings.HasSuffix(r.URL.Path, "/upgrade") {
			n := e["latestVersionNumber"].(int) + 1
			e["latestVersionNumber"] = n
			e["versions"] = append(e["versions"].([]M), M{"id": fmt.Sprintf("v%d", n), "versionNumber": n, "sandboxCount": 0, "createdAt": ts(time.Now())})
		}
		ok(w, 200, "environment.updated", M{"success": true, "versionNumber": e["latestVersionNumber"], "upgraded": 4, "failed": 0})
	}
}

func profMut(f func(M, *http.Request)) func(http.ResponseWriter, *http.Request, *user) {
	return func(w http.ResponseWriter, r *http.Request, u *user) {
		for _, p := range profiles {
			if p["id"] == r.PathValue("pid") {
				f(p, r)
				ok(w, 200, "credential_profile.updated", M{"success": true, "profile": p})
				return
			}
		}
		fail(w, 404, "credential_profile_not_found", "No such profile.")
	}
}

func setEnvLine(contents, key string, val *string) string {
	out := []string{}
	done := false
	for _, l := range strings.Split(strings.TrimRight(contents, "\n"), "\n") {
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, key+"=") {
			if val != nil && !done {
				out = append(out, key+"="+*val)
				done = true
			}
			continue
		}
		out = append(out, l)
	}
	if val != nil && !done {
		out = append(out, key+"="+*val)
	}
	return strings.Join(out, "\n") + "\n"
}

func without(list []M, k string, v any) []M {
	out := []M{}
	for _, m := range list {
		if m[k] != v {
			out = append(out, m)
		}
	}
	return out
}

func removeStr(list []string, s string) []string {
	out := []string{}
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func orEmpty(l []M) []M {
	if l == nil {
		return []M{}
	}
	return l
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func newID() string {
	const a = "23456789abcdefghjkmnpqrstvwxyz"
	b := make([]byte, 8)
	for i := range b {
		b[i] = a[rand.Intn(len(a))]
	}
	return "bx_" + string(b)
}

func transition(s *sandbox, from, to string, after time.Duration) {
	go func() {
		time.Sleep(after)
		mu.Lock()
		if s.State == from {
			s.State = to
			if s.Setup == "pending" {
				s.Setup = "running"
			}
		}
		mu.Unlock()
	}()
}

func fakeAgent(c *conv, p M, text string) {
	time.Sleep(1500 * time.Millisecond)
	mu.Lock()
	c.Events = append(c.Events, ev("response", time.Now(), M{"content": "", "model": "claude-opus-5-5", "tools": []M{{"name": "Bash", "input": M{"command": "rg -n TODO src | head -20"}}, {"name": "Read", "input": M{"path": "src/checkout/service.ts"}}}}))
	partial := ev("response", time.Now(), M{"content": "Looking into it…", "model": "claude-opus-5-5", "is_streaming": true})
	partial["taskId"] = p["id"]
	c.Events = append(c.Events, partial)
	mu.Unlock()
	time.Sleep(3 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	if !c.Running {
		return
	}
	final := ev("response", time.Now(), M{"content": "Done. (This is the devserver mock; a real sandbox runs the agent you picked.)\n\nYou asked: " + trunc(text, 200), "model": "claude-opus-5-5"})
	final["taskId"] = p["id"]
	c.Events = append(c.Events, final)
	p["data"].(M)["status"] = "finished"
	c.Running = false
}

// ---------------------------------------------------------------- commands (NDJSON)

func authedStream(h func(http.ResponseWriter, *http.Request, *sandbox)) http.HandlerFunc {
	return authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		s := find(r.PathValue("id"))
		if s == nil {
			fail(w, 404, "sandbox_not_found", "No such sandbox.")
			return
		}
		if !live(s.State) {
			fail(w, 409, "machine_not_running", "The sandbox is not running.")
			return
		}
		mu.Unlock() // streaming must not hold the lock
		defer mu.Lock()
		h(w, r, s)
	})
}

func runCommand(w http.ResponseWriter, r *http.Request, s *sandbox) {
	b := body(r)
	cmd := strings.TrimSpace(str(b, "command"))
	cwd := cleanPath(str(b, "cwd"))
	rec := cmdRec(s.ID, cmd, "running", 0, 0, 0)
	rec["cwd"] = cwd
	mu.Lock()
	s.Commands = append([]M{rec}, s.Commands...)
	mu.Unlock()
	out, errOut, code, delay := fakeShell(cmd, cwd, s)
	finish := func() {
		mu.Lock()
		rec["status"], rec["exitCode"], rec["finishedAt"], rec["out"], rec["err"] = "exited", code, ts(time.Now()), out, errOut
		rec["stdoutBytes"], rec["stderrBytes"], rec["processId"] = len(out), len(errOut), nil
		mu.Unlock()
	}
	if b["detached"] == true {
		rec["mode"], rec["processId"] = "detached", 5000+rand.Intn(4000)
		go func() { time.Sleep(delay + 2*time.Second); finish() }()
		ok(w, 200, "command.started", M{"success": true, "processId": rec["processId"], "pid": rec["processId"], "commandId": rec["commandId"], "command": cmd, "startedAt": rec["startedAt"]})
		return
	}
	if b["stream"] != true {
		time.Sleep(delay)
		finish()
		ok(w, 200, "command.finished", M{"success": code == 0, "commandId": rec["commandId"], "exitCode": code, "stdout": out, "stderr": errOut, "timedOut": false})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	send := func(m M) {
		enc.Encode(m)
		if fl != nil {
			fl.Flush()
		}
	}
	send(M{"type": "started", "commandId": rec["commandId"]})
	lines := strings.SplitAfter(out, "\n")
	for _, l := range lines {
		if l == "" {
			continue
		}
		select {
		case <-r.Context().Done():
			mu.Lock()
			rec["status"] = "killed"
			mu.Unlock()
			return
		case <-time.After(delay / time.Duration(len(lines)+1)):
		}
		send(M{"type": "stdout", "data": l})
	}
	if errOut != "" {
		send(M{"type": "stderr", "data": errOut})
	}
	finish()
	send(M{"type": "exit", "exitCode": code, "success": code == 0, "timedOut": false})
}

func fakeShell(cmd, cwd string, s *sandbox) (string, string, int, time.Duration) {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return "", "", 0, 0
	}
	switch f[0] {
	case "ls":
		mu.Lock()
		defer mu.Unlock()
		p := cwd
		if len(f) > 1 && !strings.HasPrefix(f[len(f)-1], "-") {
			p = cleanPath(f[len(f)-1])
		}
		var sb strings.Builder
		for _, e := range listDir(p) {
			sb.WriteString(e["name"].(string))
			if e["isDir"].(bool) {
				sb.WriteString("/")
			}
			sb.WriteString("\n")
		}
		return sb.String(), "", 0, 150 * time.Millisecond
	case "pwd":
		return cwd + "\n", "", 0, 0
	case "whoami":
		return "user\n", "", 0, 0
	case "hostname":
		return s.ID + "\n", "", 0, 0
	case "date":
		return time.Now().Format(time.UnixDate) + "\n", "", 0, 0
	case "echo":
		return strings.Join(f[1:], " ") + "\n", "", 0, 0
	case "uname":
		return "Linux " + s.ID + " 6.8.0-49-generic #49-Ubuntu SMP x86_64 GNU/Linux\n", "", 0, 0
	case "nproc":
		return strconv.Itoa(int(sizes[s.Type][0])) + "\n", "", 0, 0
	case "free":
		m := int(sizes[s.Type][1])
		return fmt.Sprintf("               total        used        free\nMem:           %dGi       2.1Gi       %dGi\n", m, m-3), "", 0, 0
	case "df":
		return "Filesystem      Size  Used Avail Use% Mounted on\n/dev/vda1        50G   14G   36G  28% /\n", "", 0, 100 * time.Millisecond
	case "cat":
		mu.Lock()
		defer mu.Unlock()
		if len(f) > 1 {
			if n := fsys[cleanPath(path.Join(cwd, f[1]))]; n != nil && !n.dir {
				return n.content, "", 0, 0
			}
			if n := fsys[cleanPath(f[1])]; n != nil && !n.dir {
				return n.content, "", 0, 0
			}
		}
		return "", "cat: " + strings.Join(f[1:], " ") + ": No such file or directory\n", 1, 0
	case "sleep":
		n, _ := strconv.Atoi(strings.Join(f[1:], ""))
		return "", "", 0, time.Duration(min(n, 30)) * time.Second
	case "npm", "pnpm":
		return "\n> acme-web@1.0.0 test\n> vitest --run\n\n ✓ src/checkout/service.test.ts (12) 412ms\n ✓ src/checkout/retry.test.ts (2) 38ms\n\n Test Files  2 passed (2)\n      Tests  14 passed (14)\n   Duration  2.31s\n", "", 0, 2300 * time.Millisecond
	case "false":
		return "", "", 1, 0
	}
	return "", "bash: " + f[0] + ": command not found (devserver mock knows ls, cat, echo, pwd, uname, nproc, free, df, date, sleep, npm)\n", 127, 50 * time.Millisecond
}

// ---------------------------------------------------------------- fake VNC (RFB 3.8 over websocket)

const vncW, vncH = 1280, 720

func fakeVNC(w http.ResponseWriter, r *http.Request) {
	if !autologin && r.URL.Query().Get("token") == "" {
		mu.Lock()
		u := current(r)
		mu.Unlock()
		if u == nil {
			fail(w, 401, "unauthorized", "Desktop token or session required.")
			return
		}
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}, InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
	defer conn.Close()
	serveRFB(conn)
}

func serveRFB(conn net.Conn) {
	br := bufio.NewReader(conn)
	conn.Write([]byte("RFB 003.008\n"))
	ver := make([]byte, 12)
	if _, err := io.ReadFull(br, ver); err != nil {
		return
	}
	conn.Write([]byte{1, 1}) // one security type: None
	if _, err := br.ReadByte(); err != nil {
		return
	}
	conn.Write([]byte{0, 0, 0, 0})           // SecurityResult OK
	if _, err := br.ReadByte(); err != nil { // ClientInit
		return
	}
	name := "crowdvm devserver desktop"
	init := make([]byte, 24+len(name))
	binary.BigEndian.PutUint16(init[0:], vncW)
	binary.BigEndian.PutUint16(init[2:], vncH)
	copy(init[4:], []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0})
	binary.BigEndian.PutUint32(init[20:], uint32(len(name)))
	copy(init[24:], name)
	conn.Write(init)

	frame := desktopPixels()
	sent := false
	for {
		t, err := br.ReadByte()
		if err != nil {
			return
		}
		skip := map[byte]int{0: 19, 4: 7, 5: 5}
		switch t {
		case 2: // SetEncodings
			hdr := make([]byte, 3)
			io.ReadFull(br, hdr)
			n := int(binary.BigEndian.Uint16(hdr[1:]))
			io.CopyN(io.Discard, br, int64(4*n))
		case 3: // FramebufferUpdateRequest
			req := make([]byte, 9)
			io.ReadFull(br, req)
			if req[0] == 1 && sent {
				continue // nothing changes; stay quiet on incremental requests
			}
			sent = true
			msg := make([]byte, 16, 16+len(frame))
			msg[0] = 0
			binary.BigEndian.PutUint16(msg[2:], 1)
			binary.BigEndian.PutUint16(msg[8:], vncW)
			binary.BigEndian.PutUint16(msg[10:], vncH)
			msg = append(msg, frame...) // encoding 0 = raw (bytes 12..15 zero)
			conn.Write(msg)
		case 6: // ClientCutText
			hdr := make([]byte, 7)
			io.ReadFull(br, hdr)
			io.CopyN(io.Discard, br, int64(binary.BigEndian.Uint32(hdr[3:])))
		default:
			if n, known := skip[t]; known {
				io.CopyN(io.Discard, br, int64(n))
			} else {
				return
			}
		}
	}
}

// desktopPixels paints an Xfce-like desktop: gradient wallpaper, top panel, two windows.
func desktopPixels() []byte {
	px := make([]byte, vncW*vncH*4)
	set := func(x, y int, r, g, b byte) {
		i := (y*vncW + x) * 4
		px[i], px[i+1], px[i+2] = r, g, b
	}
	rect := func(x0, y0, x1, y1 int, r, g, b byte) {
		for y := max(0, y0); y < min(vncH, y1); y++ {
			for x := max(0, x0); x < min(vncW, x1); x++ {
				set(x, y, r, g, b)
			}
		}
	}
	for y := 0; y < vncH; y++ {
		for x := 0; x < vncW; x++ {
			t := float64(x+y) / float64(vncW+vncH)
			set(x, y, byte(12+20*t), byte(70+60*(1-t)), byte(74+50*t))
		}
	}
	rect(0, 0, vncW, 28, 24, 26, 25)           // panel
	rect(10, 7, 24, 21, 54, 194, 168)          // menu button
	rect(vncW-120, 9, vncW-14, 19, 70, 74, 72) // clock
	// terminal window
	rect(90, 90, 690, 470, 21, 23, 26)
	rect(90, 90, 690, 118, 44, 47, 45)
	for i, c := range [][3]byte{{237, 106, 94}, {245, 191, 79}, {98, 197, 84}} {
		rect(102+i*20, 99, 114+i*20, 111, c[0], c[1], c[2])
	}
	for i := 0; i < 11; i++ { // "text" lines
		w := 160 + (i*97)%340
		col := [3]byte{139, 224, 205}
		if i%3 != 0 {
			col = [3]byte{190, 194, 186}
		}
		rect(110, 136+i*28, 110+w, 146+i*28, col[0], col[1], col[2])
	}
	// browser window
	rect(560, 200, 1190, 640, 246, 246, 243)
	rect(560, 200, 1190, 236, 228, 228, 224)
	rect(600, 208, 1100, 228, 255, 255, 255)
	rect(600, 270, 900, 300, 13, 125, 108)
	for i := 0; i < 6; i++ {
		rect(600, 330+i*32, 600+220+(i*131)%300, 342+i*32, 190, 192, 186)
	}
	rect(940, 270, 1150, 420, 226, 243, 239)
	return px
}

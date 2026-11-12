package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fakeNodeBin is a crowdvm-node that answers `version`.
func fakeNodeBin(v string) string {
	return "#!/bin/sh\necho crowdvm-node " + v + " linux/amd64 go1\n"
}

// updateServer serves GET /downloads for files ("os/arch/name" -> content);
// badSHA lists a wrong checksum for that path.
func updateServer(t *testing.T, ver string, files map[string]string, badSHA string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/downloads" {
			var list []map[string]any
			for k, body := range files {
				p := strings.Split(k, "/")
				sum := sha256.Sum256([]byte(body))
				sha := hex.EncodeToString(sum[:])
				if k == badSHA {
					sha = strings.Repeat("0", 64)
				}
				list = append(list, map[string]any{"os": p[0], "arch": p[1], "name": p[2], "size": len(body), "sha256": sha})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": ver, "downloads": list})
			return
		}
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/downloads/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The local page checks without a login; installing needs one.
func TestUIUpdate(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	Version = "v1.2.0"
	h := start(t, nil)
	name := "crowdvm-node"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	up := updateServer(t, "v1.3.0", map[string]string{runtime.GOOS + "/" + runtime.GOARCH + "/" + name: fakeNodeBin("v1.3.0")}, "")
	h.node.cfgMu.Lock()
	h.node.cfg.Server = up.URL
	h.node.cfgMu.Unlock()
	srv := httptest.NewServer(h.node.UIHandler("tok"))
	defer srv.Close()
	check := func(query, token string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+"/api/update"+query, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var chk map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&chk)
		return chk
	}
	if chk := check("", ""); chk["available"] != true || chk["latest"] != "v1.3.0" || chk["error"] != nil {
		t.Fatalf("check: %v", chk)
	}
	if resp := uiPost(t, http.DefaultClient, srv.URL+"/api/update", "", ""); resp.StatusCode != 401 {
		t.Fatalf("update without login: %d", resp.StatusCode)
	}
	// ?refresh=1 skips the cache only for a caller with control access.
	up2 := updateServer(t, "v1.4.0", map[string]string{runtime.GOOS + "/" + runtime.GOARCH + "/" + name: fakeNodeBin("v1.4.0")}, "")
	h.node.cfgMu.Lock()
	h.node.cfg.Server = up2.URL
	h.node.cfgMu.Unlock()
	if chk := check("?refresh=1", ""); chk["latest"] != "v1.3.0" {
		t.Fatalf("anonymous refresh: %v", chk)
	}
	if chk := check("?refresh=1", "tok"); chk["latest"] != "v1.4.0" {
		t.Fatalf("authorized refresh: %v", chk)
	}
	if st := getStatus(t, http.DefaultClient, srv.URL); st.AutoUpdate == nil || st.AutoUpdate.Mode != AutoUpdateRequired {
		t.Fatalf("status auto-update: %+v", st.AutoUpdate)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSelfUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binaries are shell scripts")
	}
	defer func(v string) { Version = v }(Version)
	Version = "v1.2.0"
	dir := t.TempDir()
	exe, guest, cli := filepath.Join(dir, "crowdvm-node"), filepath.Join(dir, "crowdvm-guest"), filepath.Join(dir, "crowdvm")
	oldNode := fakeNodeBin("v1.2.0")
	_ = os.WriteFile(exe, []byte(oldNode), 0o755)
	_ = os.WriteFile(guest, []byte("guest v1.2.0"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "npm-crowdvm"), []byte("npm's cli"), 0o755)
	_ = os.Symlink(filepath.Join(dir, "npm-crowdvm"), cli) // another installer's: never touched
	plat := runtime.GOOS + "/" + runtime.GOARCH + "/"
	newNode := fakeNodeBin("v1.3.0")
	files := map[string]string{plat + "crowdvm-node": newNode, "linux/amd64/crowdvm-guest": "guest v1.3.0", plat + "crowdvm": "cli v1.3.0"}
	ctx := context.Background()
	o := UpdateOptions{Exe: exe}
	update := func(srv *httptest.Server, o UpdateOptions) (UpdateResult, error) {
		return SelfUpdate(ctx, &Config{Server: srv.URL}, o)
	}
	unchanged := func(what string) {
		t.Helper()
		if readFile(t, exe) != oldNode || readFile(t, guest) != "guest v1.2.0" || readFile(t, cli) != "npm's cli" {
			t.Fatalf("%s: files changed", what)
		}
		if m, _ := filepath.Glob(filepath.Join(dir, "*.new-*")); len(m) > 0 {
			t.Fatalf("%s: left %v", what, m)
		}
	}

	srv := updateServer(t, "v1.3.0", files, "")
	o.Check = true
	if res, err := update(srv, o); err != nil || !res.Available || res.Latest != "v1.3.0" || res.Updated {
		t.Fatalf("check: %+v %v", res, err)
	}
	unchanged("check")
	o.Check = false

	// A checksum mismatch installs nothing, not even the files that matched.
	if _, err := update(updateServer(t, "v1.3.0", files, "linux/amd64/crowdvm-guest"), o); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("mismatch: %v", err)
	}
	unchanged("mismatch")

	// The manifest says newer, the binary says otherwise: refused.
	lie := map[string]string{plat + "crowdvm-node": fakeNodeBin("v1.1.0"), "linux/amd64/crowdvm-guest": "guest v1.3.0"}
	if _, err := update(updateServer(t, "v1.3.0", lie, ""), o); err == nil || !strings.Contains(err.Error(), "not newer") {
		t.Fatalf("older binary: %v", err)
	}
	unchanged("older binary")

	res, err := update(srv, o)
	if err != nil || !res.Updated || len(res.Files) != 2 || res.Latest != "v1.3.0" {
		t.Fatalf("update: %+v %v", res, err)
	}
	if readFile(t, exe) != newNode || readFile(t, guest) != "guest v1.3.0" || readFile(t, cli) != "npm's cli" ||
		readFile(t, exe+".old") != oldNode || readFile(t, guest+".old") != "guest v1.2.0" {
		t.Fatal("update: wrong files")
	}
	Version = "v1.3.0"
	if res, err := update(srv, UpdateOptions{Exe: exe, Check: true}); err != nil || res.Available {
		t.Fatalf("after update: %+v %v", res, err)
	}

	// Rollback swaps back; a second one goes forward again.
	if res, err := update(srv, UpdateOptions{Exe: exe, Rollback: true}); err != nil || !res.Updated || len(res.Files) != 2 {
		t.Fatalf("rollback: %+v %v", res, err)
	}
	if readFile(t, exe) != oldNode || readFile(t, guest) != "guest v1.2.0" || readFile(t, exe+".old") != newNode {
		t.Fatal("rollback: wrong files")
	}
	if _, err := update(srv, UpdateOptions{Exe: exe, Rollback: true}); err != nil || readFile(t, exe) != newNode {
		t.Fatalf("roll forward: %v", err)
	}

	// A dev build compares contents: the server's guest differs.
	Version = "dev"
	_ = os.WriteFile(guest, []byte("guest dev"), 0o755)
	if res, err := update(srv, UpdateOptions{Exe: exe, Check: true}); err != nil || !res.Available {
		t.Fatalf("dev: %+v %v", res, err)
	}
	if _, err := update(srv, UpdateOptions{Exe: exe}); err != nil || readFile(t, guest) != "guest v1.3.0" {
		t.Fatalf("dev update: %v", err)
	}
	// Only this update's file has a .old now, so a rollback restores exactly it.
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Fatalf("stale crowdvm-node.old: %v", err)
	}
	if res, err := update(srv, UpdateOptions{Exe: exe, Rollback: true}); err != nil || len(res.Files) != 1 || readFile(t, guest) != "guest dev" {
		t.Fatalf("guest rollback: %+v %v", res, err)
	}
	_ = os.Remove(guest + ".old")
	if _, err := update(srv, UpdateOptions{Exe: exe, Rollback: true}); err == nil {
		t.Fatal("rollback without .old files")
	}
}

// installed is a node install dir with crowdvm-node at ver; plat is this
// platform's download prefix.
func installed(t *testing.T, ver string) (dir, exe, plat string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binaries are shell scripts")
	}
	v := Version
	t.Cleanup(func() { Version = v })
	Version = ver
	dir = t.TempDir()
	exe = filepath.Join(dir, "crowdvm-node")
	if err := os.WriteFile(exe, []byte(fakeNodeBin(ver)), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, exe, runtime.GOOS + "/" + runtime.GOARCH + "/"
}

func noTempFiles(t *testing.T, dir string) {
	t.Helper()
	if m, _ := filepath.Glob(filepath.Join(dir, "*.new-*")); len(m) > 0 {
		t.Fatalf("left %v", m)
	}
}

// The CLI and the daemon never update at once: a second update is refused
// while one runs, a check does not wait for it, and the first one finishes
// intact.
func TestSelfUpdateLock(t *testing.T) {
	dir, exe, plat := installed(t, "v1.2.0")
	stale := filepath.Join(dir, "crowdvm-node.new-123") // from a killed update
	_ = os.WriteFile(stale, []byte("partial"), 0o755)
	srv := updateServer(t, "v1.3.0", map[string]string{plat + "crowdvm-node": fakeNodeBin("v1.3.0")}, "")
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	gated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/downloads/") {
			once.Do(func() { close(started) })
			<-release
		}
		srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer gated.Close()
	ctx, cfg := context.Background(), &Config{Server: gated.URL}
	done := make(chan error, 1)
	go func() { _, err := SelfUpdate(ctx, cfg, UpdateOptions{Exe: exe}); done <- err }()
	<-started
	for _, o := range []UpdateOptions{{Exe: exe}, {Exe: exe, Rollback: true}} {
		if _, err := SelfUpdate(ctx, cfg, o); !errors.Is(err, errUpdating) {
			close(release)
			t.Fatalf("second update %+v: %v", o, err)
		}
	}
	if res, err := SelfUpdate(ctx, cfg, UpdateOptions{Exe: exe, Check: true}); err != nil || !res.Available {
		close(release)
		t.Fatalf("check during an update: %+v %v", res, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if readFile(t, exe) != fakeNodeBin("v1.3.0") || readFile(t, exe+".old") != fakeNodeBin("v1.2.0") {
		t.Fatal("wrong files")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale download kept: %v", err)
	}
	noTempFiles(t, dir)
}

// The binary's path exists at every moment of a swap: .old is a hard link
// to it, then the new file is renamed over it (one atomic step).
func TestSwapKeepsPath(t *testing.T) {
	dir, p, _ := installed(t, "v1.2.0")
	dst := filepath.Join(dir, "kept")
	if renamed, err := keepAs(p, dst); err != nil || renamed || readFile(t, p) != readFile(t, dst) {
		t.Fatalf("keepAs moved the binary away: %v %v", renamed, err)
	}
	if _, err := keepAs(p, dst); !os.IsExist(err) {
		t.Fatalf("keepAs over an existing file: %v", err)
	}
	before, _ := os.Stat(p)
	next := filepath.Join(dir, "next")
	_ = os.WriteFile(next, []byte("new"), 0o755)
	if err := swapIn(p, next); err != nil {
		t.Fatal(err)
	}
	if old, _ := os.Stat(p + ".old"); !os.SameFile(before, old) || readFile(t, p) != "new" {
		t.Fatal("swapIn: .old is not the previous file")
	}
	if err := swapOld(p); err != nil || readFile(t, p) != fakeNodeBin("v1.2.0") || readFile(t, p+".old") != "new" {
		t.Fatalf("swapOld: %v", err)
	}
	if _, err := os.Stat(p + ".swap"); !os.IsNotExist(err) {
		t.Fatalf("swapOld left its temp: %v", err)
	}
}

// A new build that never reaches the server is rolled back by its fourth
// start, and auto-update skips it from then on.
func TestUpdateRollsBackFailingBuild(t *testing.T) {
	_, exe, plat := installed(t, "v1.2.0")
	data := t.TempDir()
	srv := updateServer(t, "v1.3.0", map[string]string{plat + "crowdvm-node": fakeNodeBin("v1.3.0")}, "")
	ctx, cfg := context.Background(), &Config{Server: srv.URL, DataDir: data}
	if _, err := SelfUpdate(ctx, cfg, UpdateOptions{Exe: exe}); err != nil {
		t.Fatal(err)
	}
	if p, ok := loadPending(data); !ok || p != (pendingUpdate{From: "v1.2.0", To: "v1.3.0"}) {
		t.Fatalf("marker: %+v %v", p, ok)
	}
	confirmUpdate(data) // the old daemon's heartbeat confirms nothing
	Version = "v1.3.0"  // the new build starts, and fails
	for i := range maxUpdateStarts {
		if back, err := CheckUpdateStart(ctx, cfg, exe); back || err != nil {
			t.Fatalf("start %d: %v %v", i+1, back, err)
		}
	}
	if back, err := CheckUpdateStart(ctx, cfg, exe); !back || err != nil {
		t.Fatalf("no rollback: %v %v", back, err)
	}
	if readFile(t, exe) != fakeNodeBin("v1.2.0") || !isSkipped(data, "v1.3.0") {
		t.Fatal("not rolled back, or not skipped")
	}
	if _, ok := loadPending(data); ok {
		t.Fatal("marker kept")
	}

	Version = "v1.2.0"
	if _, err := SelfUpdate(ctx, cfg, UpdateOptions{Exe: exe}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("skipped release installed: %v", err)
	}
	n := New(&Config{DataDir: data, Server: "https://crowd.example"}, nil, nil)
	n.SetRestarter(func() {})
	if s := n.autoUpdateStatus("v1.3.0"); !strings.Contains(s.Unavailable, "rolled back") {
		t.Fatalf("auto-update: %+v", s)
	}
	if res, err := SelfUpdate(ctx, cfg, UpdateOptions{Exe: exe, Force: true}); err != nil || !res.Updated {
		t.Fatalf("forced: %+v %v", res, err)
	}
	Version = "v1.3.0" // this time it reaches the server
	if back, err := CheckUpdateStart(ctx, cfg, exe); back || err != nil {
		t.Fatal(back, err)
	}
	confirmUpdate(data)
	if _, ok := loadPending(data); ok {
		t.Fatal("marker kept after a heartbeat")
	}

	// --rollback skips the release it leaves.
	cfg2 := &Config{DataDir: t.TempDir()}
	if _, err := SelfUpdate(ctx, cfg2, UpdateOptions{Exe: exe, Rollback: true}); err != nil || !isSkipped(cfg2.DataDir, "v1.3.0") {
		t.Fatalf("rollback: %v %v", err, loadSkipped(cfg2.DataDir))
	}
}

// A download longer than the server listed fails without filling the disk.
func TestUpdateOversizedDownload(t *testing.T) {
	dir, exe, plat := installed(t, "v1.2.0")
	srv := updateServer(t, "v1.3.0", map[string]string{plat + "crowdvm-node": fakeNodeBin("v1.3.0")}, "")
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Config.Handler.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/downloads/") {
			_, _ = w.Write(make([]byte, 1<<20))
		}
	}))
	defer big.Close()
	if _, err := SelfUpdate(context.Background(), &Config{Server: big.URL}, UpdateOptions{Exe: exe}); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("oversized: %v", err)
	}
	if readFile(t, exe) != fakeNodeBin("v1.2.0") {
		t.Fatal("installed")
	}
	noTempFiles(t, dir)
}

// The companions follow the node: alone when the node already is the
// server's (a crash between two swaps heals), never toward an older node.
// The crowdvm CLI is left to its own installer.
func TestUpdateCompanions(t *testing.T) {
	dir, exe, plat := installed(t, "v1.3.0")
	guest, cli := filepath.Join(dir, "crowdvm-guest"), filepath.Join(dir, "crowdvm")
	_ = os.WriteFile(guest, []byte("guest v1.2.0"), 0o755)
	_ = os.WriteFile(cli, []byte("cli v1.4.0"), 0o755)
	ctx := context.Background()
	srv := updateServer(t, "v1.3.0", map[string]string{plat + "crowdvm-node": fakeNodeBin("v1.3.0"),
		"linux/amd64/crowdvm-guest": "guest v1.3.0", plat + "crowdvm": "cli v1.3.0"}, "")
	if res, err := SelfUpdate(ctx, &Config{Server: srv.URL}, UpdateOptions{Exe: exe}); err != nil || !res.Updated || len(res.Files) != 1 || res.Files[0] != guest {
		t.Fatalf("heal: %+v %v", res, err)
	}
	if readFile(t, guest) != "guest v1.3.0" || readFile(t, cli) != "cli v1.4.0" {
		t.Fatal("wrong files")
	}
	older := updateServer(t, "v1.2.0", map[string]string{plat + "crowdvm-node": fakeNodeBin("v1.2.0"),
		"linux/amd64/crowdvm-guest": "guest v1.2.0"}, "")
	if res, err := SelfUpdate(ctx, &Config{Server: older.URL}, UpdateOptions{Exe: exe}); err != nil || res.Available || res.Updated {
		t.Fatalf("older server: %+v %v", res, err)
	}
	if readFile(t, guest) != "guest v1.3.0" {
		t.Fatal("guest downgraded")
	}
}

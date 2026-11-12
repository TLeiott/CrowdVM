package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	resp, err := http.Get(srv.URL + "/api/update")
	if err != nil {
		t.Fatal(err)
	}
	var chk map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&chk)
	resp.Body.Close()
	if chk["available"] != true || chk["latest"] != "v1.3.0" || chk["error"] != nil {
		t.Fatalf("check: %v", chk)
	}
	if resp := uiPost(t, http.DefaultClient, srv.URL+"/api/update", "", ""); resp.StatusCode != 401 {
		t.Fatalf("update without login: %d", resp.StatusCode)
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
		if m, _ := filepath.Glob(filepath.Join(dir, "*.new")); len(m) > 0 {
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

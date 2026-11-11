package server_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// fakeInspector implements the node's /v1/inspect API over an in-memory
// filesystem (paths as the guest sees them).
type fakeInspector struct {
	mu       sync.Mutex
	creates  []map[string]any
	commands []string
	goneOnce bool
	files    map[string]string // guest path -> content ("" + dir=true for folders)
	dirs     map[string]bool
	links    map[string]bool
}

func newFakeInspector() *fakeInspector {
	return &fakeInspector{
		files: map[string]string{
			"/home/user/proj/a.txt":                                  "hello",
			"/home/user/proj/b.txt":                                  "xyz",
			"/tmp/.crowdvm-root/etc/hosts":                           "127.0.0.1 localhost\n",
			"/tmp/.crowdvm-root/var/lib/docker/volumes/data/_data/x": "vol",
		},
		dirs:  map[string]bool{"/home/user": true, "/home/user/proj": true, "/tmp/.crowdvm-root/var/lib/docker/volumes/data/_data": true},
		links: map[string]bool{"/home/user/link": true},
	}
}

func (f *fakeInspector) routes(m *http.ServeMux) {
	m.HandleFunc("POST /v1/inspect", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.creates = append(f.creates, req)
		n := len(f.creates)
		f.mu.Unlock()
		api.WriteJSON(w, 200, map[string]any{"id": "insp_" + string(rune('a'+n)), "snapshotId": req["snapshotId"], "ready": true})
	})
	m.HandleFunc("/v1/inspect/{id}/guest/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		gone := f.goneOnce
		f.goneOnce = false
		f.mu.Unlock()
		if gone {
			api.WriteError(w, api.NewError(404, "inspector_not_found", "no such inspector", nil))
			return
		}
		f.guest(w, r, r.PathValue("rest"))
	})
}

func (f *fakeInspector) guest(w http.ResponseWriter, r *http.Request, rest string) {
	p := r.URL.Query().Get("path")
	switch rest {
	case "commands":
		var req struct{ Command string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.commands = append(f.commands, req.Command)
		f.mu.Unlock()
		out := ""
		switch {
		case strings.Contains(req.Command, "mount --bind"):
		case strings.Contains(req.Command, "find . -mindepth 1"):
			out = "d\t4096\tproj\nf\t5\tproj/a.txt\nf\t3\tproj/b.txt\nl\t9\tlink\nd\t0\t__dockervol__/data\nf\t3\t__dockervol__/data/x\npartial-line"
		case strings.Contains(req.Command, "awk"):
			out = "2 8\n"
		case strings.Contains(req.Command, "ls -1"):
			out = "data\n"
		}
		api.WriteOK(w, 200, "command.result", map[string]any{"exitCode": 0, "stdout": out, "stderr": ""})
	case "files/info":
		switch {
		case f.links[p]:
			api.WriteOK(w, 200, "file.info", map[string]any{"path": p, "isSymlink": true})
		case f.dirs[p]:
			api.WriteOK(w, 200, "file.info", map[string]any{"path": p, "isDir": true})
		default:
			c, ok := f.files[p]
			if !ok {
				api.WriteError(w, api.NewError(404, "not_found", "no such file", nil))
				return
			}
			api.WriteOK(w, 200, "file.info", map[string]any{"path": p, "size": len(c)})
		}
	case "files/raw":
		c, ok := f.files[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(c))
	case "artifacts":
		// like the guest: a folder as .tar.gz rooted at its base name
		w.Header().Set("Content-Type", "application/gzip")
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		base := path.Base(p)
		_ = tw.WriteHeader(&tar.Header{Name: base + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		var names []string
		for name := range f.files {
			if strings.HasPrefix(name, p+"/") {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			c := f.files[name]
			_ = tw.WriteHeader(&tar.Header{Name: base + "/" + strings.TrimPrefix(name, p+"/"), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(c))})
			_, _ = tw.Write([]byte(c))
		}
		_ = tw.Close()
		_ = gz.Close()
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeInspector) count(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.commands {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func tarNames(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		c, _ := io.ReadAll(tr)
		out[h.Name] = string(c)
	}
}

func TestSnapshotFiles(t *testing.T) {
	env := servertest.New(t)
	fi := newFakeInspector()
	node := env.StartNode(servertest.NodeOptions{Routes: fi.routes})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	snap := env.WaitFor(key, "/api/v1/sandboxes/"+id+"/snapshots/latest", "snapshot.uploaded", 5*time.Second, "true").Str("snapshot.id")
	base := "/api/v1/snapshots/" + snap

	// Tree: home entries + docker volumes, sorted, counted, cached.
	r := env.Do(key, "GET", base+"/tree", nil)
	if r.Status != 200 || r.Str("type") != "snapshot.tree" || r.Str("treeAvailable") != "true" || r.Str("fileCount") != "3" ||
		r.Str("totalSizeBytes") != "11" || r.Str("sandboxId") != id || r.Str("truncated") != "false" {
		t.Fatalf("tree: %s", r)
	}
	var paths []string
	for _, e := range r.Get("entries").([]any) {
		m := e.(map[string]any)
		paths = append(paths, m["path"].(string)+":"+m["kind"].(string))
		if m["kind"] == "dir" && m["size"] != nil {
			t.Fatalf("dir with size: %v", m)
		}
	}
	want := "__dockervol__:dir __dockervol__/data:dir __dockervol__/data/x:file link:symlink proj:dir proj/a.txt:file proj/b.txt:file"
	if strings.Join(paths, " ") != want {
		t.Fatalf("entries: %v", paths)
	}
	fi.mu.Lock()
	create := fi.creates[0]
	fi.mu.Unlock()
	if create["snapshotId"] != snap || create["token"] == "" || !node.Called("POST /v1/inspect") {
		t.Fatalf("inspect request: %v", create)
	}
	// Dashboard filter (extension): below a path, limited depth; served from the cache.
	r = env.Do(key, "GET", base+"/tree?path=/home/user/proj&depth=1", nil)
	if es := r.Get("entries").([]any); len(es) != 2 || r.Str("entries.0.path") != "proj/a.txt" || r.Str("fileCount") != "3" {
		t.Fatalf("filtered tree: %s", r)
	}
	if r := env.Do(key, "GET", base+"/tree?path=/home/user&depth=1", nil); len(r.Get("entries").([]any)) != 3 {
		t.Fatalf("depth 1: %s", r)
	}
	if fi.count("find . -mindepth 1") != 1 {
		t.Fatal("tree not cached")
	}

	// Files: relative to /home/user, "home/user/..." too, ranges.
	for _, p := range []string{"proj/a.txt", "home/user/proj/a.txt", "/home/user/proj/a.txt"} {
		fr := hget(t, env, "", base+"/files?path="+p, "Authorization: Bearer "+key)
		if fr.StatusCode != 200 || fr.body != "hello" || fr.Header.Get("X-Snapshot-Entry-Kind") != "file" || fr.Header.Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("file %s: %d %q %v", p, fr.StatusCode, fr.body, fr.Header)
		}
	}
	fr := hget(t, env, "", base+"/files?path=proj/a.txt", "Authorization: Bearer "+key, "Range: bytes=1-2")
	if fr.StatusCode != 206 || fr.body != "el" || fr.Header.Get("Content-Range") != "bytes 1-2/5" {
		t.Fatalf("range: %d %q %v", fr.StatusCode, fr.body, fr.Header)
	}
	// A folder as tar rooted at its name.
	fr = hget(t, env, "", base+"/files?path=proj", "Authorization: Bearer "+key)
	names := tarNames(t, []byte(fr.body))
	if fr.StatusCode != 200 || fr.Header.Get("Content-Type") != "application/x-tar" || fr.Header.Get("X-Snapshot-Entry-Kind") != "dir" ||
		fr.Header.Get("X-Snapshot-File-Count") != "2" || fr.Header.Get("X-Snapshot-Total-Size-Bytes") != "8" || names["proj/a.txt"] != "hello" {
		t.Fatalf("folder: %d %v %v", fr.StatusCode, fr.Header, names)
	}
	// System files through the read-only bind mount; docker volumes.
	fr = hget(t, env, "", base+"/files?path=/etc/hosts", "Authorization: Bearer "+key)
	if fr.StatusCode != 200 || !strings.HasPrefix(fr.body, "127.0.0.1") || fi.count("mount --bind") != 1 {
		t.Fatalf("system file: %d %q", fr.StatusCode, fr.body)
	}
	if fr := hget(t, env, "", base+"/files?path=__dockervol__/data/x", "Authorization: Bearer "+key); fr.body != "vol" {
		t.Fatalf("volume file: %d %q", fr.StatusCode, fr.body)
	}
	// Whole snapshot: home at the root, volumes under __dockervol__/.
	fr = hget(t, env, "", base+"/files", "Authorization: Bearer "+key)
	names = tarNames(t, []byte(fr.body))
	if fr.StatusCode != 200 || names["proj/a.txt"] != "hello" || names["__dockervol__/data/x"] != "vol" {
		t.Fatalf("whole: %d %v", fr.StatusCode, names)
	}
	if _, ok := names["user/"]; ok {
		t.Fatalf("home root entry leaked: %v", names)
	}
	// Errors.
	for p, code := range map[string]string{"link": "is_symlink", "nope": "not_found", "../etc/passwd": "invalid_path"} {
		if r := env.Do(key, "GET", base+"/files?path="+p, nil); r.Code() != code {
			t.Fatalf("%s: %s", p, r)
		}
	}

	// The node destroyed the idle inspector: the server boots a new one.
	fi.mu.Lock()
	fi.goneOnce = true
	fi.mu.Unlock()
	if fr := hget(t, env, "", base+"/files?path=proj/b.txt", "Authorization: Bearer "+key); fr.body != "xyz" {
		t.Fatalf("after inspector loss: %d %q", fr.StatusCode, fr.body)
	}
	fi.mu.Lock()
	n := len(fi.creates)
	fi.mu.Unlock()
	if n != 2 {
		t.Fatalf("inspector not re-created: %d", n)
	}

	// Download: signed layer URLs (+ inventory).
	r = env.Do(key, "GET", base+"/download", nil)
	// The stop's pre-flight live snapshot is the base layer; the stop
	// snapshot is the newest chunk.
	chunks, _ := r.Get("chunks").([]any)
	if r.Status != 200 || r.Str("type") != "snapshot.download" || r.Str("expiresInSeconds") != "3600" || len(chunks) == 0 ||
		r.Str(fmt.Sprintf("chunks.%d.snapshotId", len(chunks)-1)) != snap || (len(chunks) == 1) != (r.Str("kind") == "base") {
		t.Fatalf("download: %s", r)
	}
	last := fmt.Sprintf("chunks.%d.", len(chunks)-1)
	blobURL := r.Str(last + "signedUrl")
	resp, err := http.Get(blobURL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(string(b), "fake-snapshot:"+snap) || resp.Header.Get("X-Crowdvm-Sha256") != r.Str(last+"sha256") {
		t.Fatalf("blob: %d %q", resp.StatusCode, b)
	}
	if resp, _ := http.Get(strings.Replace(blobURL, "sig=", "sig=0", 1)); resp.StatusCode != 403 {
		t.Fatalf("tampered signature: %d", resp.StatusCode)
	}
	resp, _ = http.Get(r.Str("inventory.signedUrl"))
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"proj/a.txt"`) {
		t.Fatalf("inventory: %d %s", resp.StatusCode, b)
	}

	// Other users do not see the snapshot; keys need snapshot.read.
	_, otherKey := env.NewUser("other", false)
	if r := env.Do(otherKey, "GET", base+"/tree", nil); r.Status != 404 {
		t.Fatalf("other user: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/snapshots/snap_nope/tree", nil); r.Status != 404 {
		t.Fatalf("unknown snapshot: %s", r)
	}
}

func TestSnapshotFilesOldNode(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{}) // no /v1/inspect
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	snap := env.WaitFor(key, "/api/v1/sandboxes/"+id+"/snapshots/latest", "snapshot.uploaded", 5*time.Second, "true").Str("snapshot.id")
	if r := env.Do(key, "GET", "/api/v1/snapshots/"+snap+"/tree", nil); r.Code() != "inspector_unsupported" {
		t.Fatalf("old node: %s", r)
	}
}

package cli

import (
	"archive/tar"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

// The sandbox swaps the archive: the root folder is a symlink to a host
// folder, then a file goes "through" it. The root entry must be refused.
func TestExtractTarRootSymlinkRefused(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	os.Mkdir(victim, 0o755)
	os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("ORIGINAL"), 0o644)
	for _, link := range []string{victim, "../victim"} {
		dest := filepath.Join(root, "xfer", "evil")
		archive := tarOf(t, []tar.Header{
			{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: link},
			{Name: "evil/pwned.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"evil/pwned.txt": "PWNED"})
		_, err := extractTar(archive, dest, "evil", nil)
		if err == nil || !strings.Contains(err.Error(), "unsafe path") {
			t.Fatalf("link %q: want unsafe path error, got %v", link, err)
		}
		if _, err := os.Stat(filepath.Join(victim, "pwned.txt")); err == nil {
			t.Fatal("wrote outside the destination")
		}
		if st, err := os.Lstat(dest); err == nil && st.Mode()&os.ModeSymlink != 0 {
			t.Fatal("destination was replaced by a symlink")
		}
	}
}

// Dir entries for the root itself ("./", "d/") are harmless and ignored.
func TestExtractTarRootDirIgnored(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dl")
	archive := tarOf(t, []tar.Header{
		{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o700},
		{Name: "d/a.txt", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"d/a.txt": "a"})
	if n, err := extractTar(archive, dest, "d", nil); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	archive = tarOf(t, []tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./b.txt", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"./b.txt": "b"})
	if n, err := extractTar(archive, filepath.Join(t.TempDir(), "snap"), "", nil); err != nil || n != 1 {
		t.Fatalf("snapshot-style root: n=%d err=%v", n, err)
	}
}

// A chain: l1 -> sub (inside), then l1/l2 -> .. would climb out through l1, and
// files under it must never be written.
func TestExtractTarChainedSymlinks(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dl")
	archive := tarOf(t, []tar.Header{
		{Name: "d/sub", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "d/l1", Typeflag: tar.TypeSymlink, Linkname: "sub"},
		{Name: "d/l1/l2", Typeflag: tar.TypeSymlink, Linkname: "../.."},
		{Name: "d/sub/l2/pwned.txt", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"d/sub/l2/pwned.txt": "PWNED"})
	if _, err := extractTar(archive, dest, "d", nil); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("want unsafe path error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "pwned.txt")); err == nil {
		t.Fatal("wrote outside the destination")
	}
	// A symlink that goes up through another symlink is not created either.
	dest = filepath.Join(root, "dl2")
	archive = tarOf(t, []tar.Header{
		{Name: "d/s", Typeflag: tar.TypeSymlink, Linkname: "."},
		{Name: "d/up", Typeflag: tar.TypeSymlink, Linkname: "s/.."},
	}, nil)
	if _, err := extractTar(archive, dest, "d", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "up")); err == nil {
		t.Fatal("symlink through a symlink was created")
	}
}

// A destination that is replaced by a symlink during extraction is caught by
// the Lstat of dest, not only of the folders below it.
func TestExtractTarDestReplacedBySymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	os.Mkdir(outside, 0o755)
	dest := filepath.Join(root, "dl")
	os.Mkdir(dest, 0o755)
	if err := checkNoSymlinkParents(dest, filepath.Join(dest, "a.txt")); err != nil {
		t.Fatal(err)
	}
	os.Remove(dest)
	if err := os.Symlink(outside, dest); err != nil {
		t.Skip(err)
	}
	if err := checkNoSymlinkParents(dest, filepath.Join(dest, "a.txt")); err == nil {
		t.Fatal("dest symlink not detected")
	}
	if err := checkNoSymlinkParents(dest, filepath.Join(dest, "x", "a.txt")); err == nil {
		t.Fatal("dest symlink not detected below a folder")
	}
}

// Absolute and out-of-tree symlink targets are skipped; in-tree ones are kept.
func TestExtractTarUnsafeLinkTargetsSkipped(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dl")
	archive := tarOf(t, []tar.Header{
		{Name: "d/abs", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
		{Name: "d/up", Typeflag: tar.TypeSymlink, Linkname: "../../x"},
		{Name: "d/a.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "d/ok", Typeflag: tar.TypeSymlink, Linkname: "a.txt"},
	}, map[string]string{"d/a.txt": "a"})
	if _, err := extractTar(archive, dest, "d", nil); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"abs", "up"} {
		if _, err := os.Lstat(filepath.Join(dest, n)); err == nil {
			t.Fatalf("%s was created", n)
		}
	}
	if l, err := os.Readlink(filepath.Join(dest, "ok")); err != nil || l != "a.txt" {
		t.Fatalf("in-tree link: %q %v", l, err)
	}
}

// snapshot pull extracts with the same function (strip ""): a root symlink
// entry there is refused too.
func TestExtractTarSnapshotRootSymlinkRefused(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "snap")
	archive := tarOf(t, []tar.Header{
		{Name: ".", Typeflag: tar.TypeSymlink, Linkname: root},
		{Name: "pwned.txt", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"pwned.txt": "PWNED"})
	if _, err := extractTar(archive, dest, "", nil); err == nil {
		t.Fatal("root symlink accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "pwned.txt")); err == nil {
		t.Fatal("wrote outside the destination")
	}
}

// sandboxStates serves the given sandbox JSON in turn (the last one repeats).
func sandboxStates(e *env, id string, states ...map[string]any) {
	n := 0
	e.f.On("GET /api/v1/sandboxes/"+id, func(w http.ResponseWriter, r *http.Request) {
		sb := states[min(n, len(states)-1)]
		n++
		sb["id"] = id
		clienttest.OK(w, "sandbox", map[string]any{"sandbox": sb})
	})
}

// A node restart marks the sandbox error/node_offline for a few seconds; it
// comes back ready. The waiter keeps waiting instead of failing.
func TestResumeWaitsThroughNodeOffline(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("archived")
	sandboxStates(e, id,
		map[string]any{"state": "provisioning"},
		map[string]any{"state": "error", "error": "node_offline"},
		map[string]any{"state": "error", "error": "node_offline: node r6n2 disconnected"},
		map[string]any{"state": "ready"})
	r := e.run("resume", id, "--json=false")
	if r.code != 0 || !strings.Contains(r.out, "ready") {
		t.Fatalf("%d %q %q", r.code, r.out, r.err)
	}
}

// Any other error is final at once.
func TestResumeFailsOnOtherError(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("archived")
	sandboxStates(e, id,
		map[string]any{"state": "error", "error": "boot_failed"},
		map[string]any{"state": "ready"})
	r := e.run("resume", id, "--json=false")
	if r.code == 0 || !strings.Contains(r.err, "boot_failed") {
		t.Fatalf("%d %q %q", r.code, r.out, r.err)
	}
}

// A node that never comes back: after the normal timeout the wait fails with
// the node_offline error.
func TestResumeNodeOfflineTimesOut(t *testing.T) {
	old := readyTimeout
	readyTimeout = 100 * time.Millisecond
	defer func() { readyTimeout = old }()
	e := newEnv(t)
	id := e.f.AddSandbox("archived")
	sandboxStates(e, id, map[string]any{"state": "error", "error": "node_offline"})
	r := e.run("resume", id, "--json=false")
	if r.code == 0 || !strings.Contains(r.err, "node_offline") {
		t.Fatalf("%d %q %q", r.code, r.out, r.err)
	}
}

// Ctrl-C on a default-timeout text-mode exec kills the remote command: it runs
// detached (only those have a process id) and the interrupt calls the kill API.
func TestExecInterruptKillsDefaultTimeoutCommand(t *testing.T) {
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
	for _, args := range [][]string{
		{"exec", id, "--json=false", "exec sleep 777"},                      // default --timeout 30
		{"exec", id, "--timeout", "60", "--json=false", "exec sleep 777"},   // explicit 1..600
		{"exec", id, "--stream", "--json=false", "--timeout", "5", "sleep"}, // --stream text
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		before := e.f.Count("DELETE /api/v1/sandboxes/" + id + "/commands/4242")
		r := e.runCtx(ctx, "", args...)
		cancel()
		if r.code != 130 {
			t.Fatalf("%v: interrupt: %d %q", args, r.code, r.err)
		}
		if e.f.Count("DELETE /api/v1/sandboxes/"+id+"/commands/4242") != before+1 {
			t.Fatalf("%v: interrupted command was not killed", args)
		}
	}
}

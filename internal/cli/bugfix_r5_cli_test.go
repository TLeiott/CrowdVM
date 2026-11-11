package cli

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

func tarOf(t *testing.T, hs []tar.Header, bodies map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hs {
		h := h
		h.Size = int64(len(bodies[h.Name]))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(bodies[h.Name]))
	}
	tw.Close()
	return &buf
}

// A sandbox-made symlink `c.txt.crowdvm-partial` -> victim sat in the archive
// before `c.txt`: the temp file was opened through it and overwrote the victim.
func TestScpDownloadDirPartialSymlinkNotFollowed(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim.txt")
	os.WriteFile(victim, []byte("ORIGINAL"), 0o644)
	dest := filepath.Join(root, "xfer", "evil")
	archive := tarOf(t, []tar.Header{
		{Name: "evil/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "evil/c.txt.crowdvm-partial", Typeflag: tar.TypeSymlink, Linkname: victim},
		{Name: "evil/c.txt", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"evil/c.txt": "PWNED-BY-SANDBOX"})
	if _, err := extractTar(archive, dest, "evil", nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "ORIGINAL" {
		t.Fatalf("victim overwritten: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "c.txt")); string(b) != "PWNED-BY-SANDBOX" {
		t.Fatalf("c.txt: %q", b)
	}
}

// A symlink the archive made earlier must not be a parent of later entries,
// and a file entry replaces a symlink of the same name instead of following it.
func TestScpDownloadDirSymlinkParentRefused(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	os.Mkdir(outside, 0o755)
	victim := filepath.Join(outside, "f")
	os.WriteFile(victim, []byte("ORIGINAL"), 0o644)
	dest := filepath.Join(root, "dl")
	archive := tarOf(t, []tar.Header{
		{Name: "d/link", Typeflag: tar.TypeSymlink, Linkname: "."}, // inside dest, so it is created
		{Name: "d/link/x", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"d/link/x": "PWNED"})
	_, err := extractTar(archive, dest, "d", nil)
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("want unsafe path error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Fatal("wrote through a symlinked parent")
	}

	archive = tarOf(t, []tar.Header{
		{Name: "d/f", Typeflag: tar.TypeSymlink, Linkname: victim},
		{Name: "d/f", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"d/f": "PWNED"})
	if _, err := extractTar(archive, filepath.Join(root, "dl2"), "d", nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "ORIGINAL" {
		t.Fatalf("victim overwritten: %q", b)
	}
}

// `crowdvm info` shows the capacity wait that docs/guide/troubleshooting.md
// tells users to look for, in --json and in text.
func TestInfoShowsWaitingForCapacity(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("starting")
	sb := e.f.Sandboxes[id]
	sb["waitingForCapacity"] = true
	sb["statusMessage"] = "Waiting for a node with room for a default sandbox."
	r := e.mustRun("info", id, "--json")
	var out struct {
		Sandbox map[string]any `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(r.out), &out); err != nil {
		t.Fatal(err)
	}
	if out.Sandbox["waitingForCapacity"] != true || !strings.Contains(fmt.Sprint(out.Sandbox["statusMessage"]), "Waiting for a node") {
		t.Fatalf("json: %s", r.out)
	}
	r = e.mustRun("info", id, "--json=false")
	if !strings.Contains(r.out, "waitingForCapacity: true") || !strings.Contains(r.out, "Waiting for a node with room") {
		t.Fatalf("text: %s", r.out)
	}
}

// The sync endpoint cuts stdout at 8 MiB with exit 0. Text mode follows a
// detached command, so nothing is lost; --json keeps the sync result and its stdoutTruncated flag.
func TestExecTextModeStreamsPastSyncCap(t *testing.T) {
	const capBytes = 8 << 20
	big := strings.Repeat("x", capBytes+4096)
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	e.f.On("POST /api/v1/sandboxes/{id}/commands", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["detached"] == true {
			clienttest.OK(w, "command.started", map[string]any{"processId": 4242})
			return
		}
		clienttest.OK(w, "command.result", map[string]any{"exitCode": 0, "stdout": big[:capBytes], "stdoutTruncated": true})
	})
	e.f.On("GET /api/v1/sandboxes/{id}/commands/{pid}/stream", func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		for i := 0; i < len(big); i += 1 << 20 {
			enc.Encode(map[string]any{"type": "stdout", "data": big[i:min(i+1<<20, len(big))], "nextOffset": i})
		}
		enc.Encode(map[string]any{"type": "exit", "exitCode": 0})
	})
	r := e.mustRun("exec", id, "yes")
	if len(r.out) != len(big) {
		t.Fatalf("text mode got %d bytes, want %d", len(r.out), len(big))
	}
	r = e.mustRun("exec", id, "--json", "yes")
	if !strings.Contains(r.out, `"stdoutTruncated": true`) && !strings.Contains(r.out, `"stdoutTruncated":true`) {
		t.Fatalf("json lost the truncation flag: %.200s", r.out)
	}
}

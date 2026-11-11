package cli

import (
	"archive/tar"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// GNU tar stores a second name of an already archived file as a hardlink
// entry. extractTar ignores TypeLink, so `scp -r` / download silently drops
// those files.
func TestBughuntR6ExtractTarHardlinkDropped(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dl")
	archive := tarOf(t, []tar.Header{
		{Name: "d/a.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "d/b.txt", Typeflag: tar.TypeLink, Linkname: "d/a.txt"},
	}, map[string]string{"d/a.txt": "hello"})
	if _, err := extractTar(archive, dest, "d", nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "b.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("hardlinked file b.txt missing after extract: %q, %v", b, err)
	}
}

// The temp file name appends ".crowdvm-partial-<rand>" to the target's base
// name, so a legal 250-byte file name overflows NAME_MAX and the whole
// download fails.
func TestBughuntR6ExtractTarLongFileName(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dl")
	name := strings.Repeat("n", 250)
	archive := tarOf(t, []tar.Header{
		{Name: "d/" + name, Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"d/" + name: "x"})
	if _, err := extractTar(archive, dest, "d", nil); err != nil {
		t.Fatalf("extract of a 250-byte file name failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
		t.Fatal(err)
	}
}

// One unix socket in a pushed/copied directory aborts the whole upload:
// tar.FileInfoHeader refuses sockets.
func TestBughuntR6WriteTarGzSocketAbortsUpload(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	l, err := net.Listen("unix", filepath.Join(root, "s.sock"))
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	var buf bytes.Buffer
	n, _, err := writeTarGz(&buf, root, []pushFile{{rel: "."}}, "")
	if err != nil {
		t.Fatalf("a socket in the tree aborts the whole archive: %v (files so far: %d)", err, n)
	}
}

// A holder whose lock was taken over (it stalled for over 30 s) removes the
// new owner's lock file on unlock, so a third process enters the critical
// section next to the new owner.
func TestBughuntR6ConfigUnlockRemovesTakenOverLock(t *testing.T) {
	dir := t.TempDir()
	unlockA, err := lockConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "config.lock")
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	unlockB, err := lockConfig(dir) // takes A's stale lock over
	if err != nil {
		t.Fatal(err)
	}
	defer unlockB()
	unlockA() // A wakes up and finishes
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("B still holds the lock, but A's unlock removed it: %v", err)
	}
}

// A config.json written by a newer crowdvm has fields this version does not
// know. Every save goes through the typed struct, so an older binary drops
// them silently (also when it changed something else entirely).
func TestBughuntR6ConfigSaveDropsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	in := `{"profile":"work","futureTopLevel":{"x":1},"profiles":{"work":{"server":"https://a.example","apiKey":"k","futureProfileField":"keep-me"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	err := updateConfig(dir, func(cur *Config) error { cur.Profiles["work"].Current = "bx_1"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if !strings.Contains(string(b), "futureProfileField") || !strings.Contains(string(b), "futureTopLevel") {
		t.Fatalf("fields written by a newer version were dropped by a save:\n%s", b)
	}
}

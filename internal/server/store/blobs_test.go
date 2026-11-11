package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Partial uploads a server crash left behind are removed (sonnet-18 B2);
// complete blobs and fresh partial uploads stay.
func TestCleanTemp(t *testing.T) {
	b := &Blobs{Dir: t.TempDir()}
	if _, err := b.Put(BlobSnapshot, "snap_keep", strings.NewReader("data"), ""); err != nil {
		t.Fatal(err)
	}
	p, _ := b.path(BlobSnapshot, "x")
	dir := filepath.Dir(p)
	old, fresh := filepath.Join(dir, ".upload-old"), filepath.Join(dir, ".upload-fresh")
	os.WriteFile(old, []byte("partial"), 0o600)
	os.WriteFile(fresh, []byte("partial"), 0o600)
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(old, past, past)
	if n := b.CleanTemp(time.Hour); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale partial upload kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh partial upload removed")
	}
	if b.CleanTemp(0) != 1 {
		t.Fatal("CleanTemp(0) should remove every partial upload")
	}
	if _, err := b.Stat(BlobSnapshot, "snap_keep"); err != nil {
		t.Fatal("a complete blob was removed")
	}
}

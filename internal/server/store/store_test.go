package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationsAndBlobs(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ms := []Migration{{Version: 2, SQL: `ALTER TABLE t ADD COLUMN b TEXT`}, {Version: 1, SQL: `CREATE TABLE t (a TEXT)`}}
	for i := 0; i < 2; i++ { // idempotent
		if err := db.Migrate(context.Background(), "feat", ms...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO t(a,b) VALUES('x','y')`); err != nil {
		t.Fatal(err)
	}

	b := &Blobs{Dir: filepath.Join(dir, "blobs")}
	if _, err := b.Put(BlobSnapshot, "../evil", strings.NewReader("x"), ""); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err := b.Put(BlobSnapshot, "s1", strings.NewReader("data"), "00"); !errors.Is(err, ErrChecksum) {
		t.Fatalf("checksum: %v", err)
	}
	bi, err := b.Put(BlobSnapshot, "s1", strings.NewReader("data"), "")
	if err != nil || bi.Size != 4 || len(bi.SHA256) != 64 {
		t.Fatalf("put: %+v %v", bi, err)
	}
	if st, err := b.Stat(BlobSnapshot, "s1"); err != nil || st.SHA256 != bi.SHA256 {
		t.Fatalf("stat: %+v %v", st, err)
	}
	if list, _ := b.List(BlobSnapshot); len(list) != 1 {
		t.Fatalf("list: %v", list)
	}
	if err := b.Delete(BlobSnapshot, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stat(BlobSnapshot, "s1"); !errors.Is(err, ErrBlobNotFound) {
		t.Fatal("still there")
	}
}

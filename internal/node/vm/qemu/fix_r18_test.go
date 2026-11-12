package qemu

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// bugs-r18-opus 3: the boot timeout killed a copy's re-encryption that was
// still making progress. The node now waits longer while the console shows
// it progressing (recently written, with preinit's progress line).
func TestR18RekeyProgressing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "console.log")
	if rekeyProgressing(p) {
		t.Fatal("no console counts as progress")
	}
	os.WriteFile(p, []byte("crowdvm-preinit: re-encrypting: 76% of 41.9 GB, 184 MB/s, 55s left\n"), 0o644)
	if !rekeyProgressing(p) {
		t.Fatal("a fresh progress line is not seen")
	}
	old := time.Now().Add(-2 * time.Minute)
	os.Chtimes(p, old, old)
	if rekeyProgressing(p) {
		t.Fatal("a console silent for two minutes counts as progress")
	}
	os.WriteFile(p, []byte("[  OK  ] Started something else\n"), 0o644)
	if rekeyProgressing(p) {
		t.Fatal("an ordinary boot counts as re-encryption")
	}
}

//go:build linux

package guest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// bugs-r15-opus 4: a disk that already booted unencrypted (its image root
// has a machine-id) must not get a data partition hiding its data.
func TestR15BootedPlain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "machine-id")
	if bootedPlain(p) {
		t.Fatal("missing machine-id counts as booted")
	}
	os.WriteFile(p, []byte("\n"), 0o644) // the image ships it empty
	if bootedPlain(p) {
		t.Fatal("an empty machine-id (fresh image) counts as booted")
	}
	os.WriteFile(p, []byte("0123456789abcdef0123456789abcdef\n"), 0o644)
	if !bootedPlain(p) {
		t.Fatal("a root that ran as a plain system is not recognized")
	}
}

// bugs-r16-opus 1: a grown data partition ended with an odd number of
// sectors; LUKS (4 KiB sectors) then refused to open it on every later
// boot. alignPartition rounds it down (and repairs such a partition).
func TestR16AlignPartition(t *testing.T) {
	if _, err := exec.LookPath("sfdisk"); err != nil {
		t.Skip("no sfdisk")
	}
	img := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(img, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, 64<<20); err != nil {
		t.Fatal(err)
	}
	layout := "label: gpt\nstart=2048, size=8192, name=root\nstart=10240, size=20001, name=" + dataPartName + "\n"
	if err := run([]byte(layout), "sfdisk", "--no-reread", "--no-tell-kernel", img); err != nil {
		t.Fatal(err)
	}
	part, err := dataPartition(img)
	if err != nil || part == "" {
		t.Fatalf("data partition: %q %v", part, err)
	}
	if err := alignPartition(img, part, "2"); err != nil {
		t.Fatal(err)
	}
	tb, _ := readTable(img)
	for _, p := range tb.PartitionTable.Partitions {
		if p.Node == part && (p.Start != 10240 || p.Size != 20000) {
			t.Fatalf("data partition after align: start %d size %d, want 10240 and 20000", p.Start, p.Size)
		}
	}
	if err := alignPartition(img, part, "2"); err != nil { // aligned: nothing to do
		t.Fatal(err)
	}
}

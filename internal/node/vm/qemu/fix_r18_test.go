package qemu

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
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

// bugs-r18-opus 2: a layer whose clusters the guest discarded (fstrim after
// a --new-key re-encryption) was uploaded at its full apparent size. The
// upload copy holds the allocated data only, on the same backing file,
// with the same guest view, and is the same file on every try.
func TestR18CompactSparseSnapshot(t *testing.T) {
	for _, b := range []string{"qemu-img", "qemu-io"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
	dir := t.TempDir()
	s, err := openStore(dir, "qemu-img")
	if err != nil {
		t.Fatal(err)
	}
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v: %s", name, args, err, out)
		}
	}
	base := s.abs("disks/base.qcow2")
	run("qemu-img", "create", "-q", "-f", "qcow2", base, "1G")
	run("qemu-io", "-c", "write -P 0x11 0 128M", base)
	layer := func(id string, io ...string) string {
		rel := "disks/" + id + ".qcow2"
		run("qemu-img", "create", "-q", "-f", "qcow2", "-b", base, "-F", "qcow2", s.abs(rel), "1G")
		run("qemu-io", append([]string{"--discard=unmap"}, append(io, s.abs(rel))...)...)
		if err := s.addSnapshot(proto.SnapshotRef{ID: id, ImageID: "img"}, rel, 1<<30, 0); err != nil {
			t.Fatal(err)
		}
		return s.abs(rel)
	}
	// Written, then mostly discarded: zeros over the base's data, plus a
	// bit of data kept.
	src := layer("sparse", "-c", "write -P 0xab 0 96M", "-c", "discard 0 90M")
	if info, _ := s.info(src); info.ActualSize >= fileSize(src)/2 {
		t.Skipf("this filesystem does not punch holes (%d of %d bytes allocated)", info.ActualSize, fileSize(src))
	}
	tmp := filepath.Join(dir, "tmp")
	cp, err := s.CompactSnapshot("sparse", tmp)
	if err != nil || cp == "" {
		t.Fatalf("compact: %q, %v", cp, err)
	}
	if fileSize(cp) > fileSize(src)/4 {
		t.Fatalf("copy is %d bytes, layer %d", fileSize(cp), fileSize(src))
	}
	info, err := s.info(cp)
	if err != nil || info.FullBackingFile != base || info.BackingFormat != "qcow2" {
		t.Fatalf("copy's backing: %+v, %v", info, err)
	}
	run("qemu-img", "compare", "-U", src, cp)
	run("qemu-io", "-r", "-c", "read -P 0 0 90M", "-c", "read -P 0xab 90M 6M", "-c", "read -P 0x11 96M 32M", cp)
	again, err := s.CompactSnapshot("sparse", tmp)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := sum(t, cp), sum(t, again); a != b {
		t.Fatalf("a second copy differs: %s vs %s", a, b)
	}
	// An ordinary layer is uploaded as it is.
	layer("dense", "-c", "write -P 0xcd 0 32M")
	if cp, err := s.CompactSnapshot("dense", tmp); cp != "" || err != nil {
		t.Fatalf("dense layer compacted: %q, %v", cp, err)
	}
}

func sum(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

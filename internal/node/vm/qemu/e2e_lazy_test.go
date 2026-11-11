//go:build linux

package qemu

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// TestE2ELazy (LIFE-013): a VM boots on a chain whose top layer is only
// reachable over HTTP, takes a live snapshot while lazy, switches to the
// local file after the "download" (Hydrated), and its snapshots resume
// from plain local chains.
//
//	CROWDVM_E2E=1 [CROWDVM_E2E_IMAGE=...] go test -v -run E2ELazy -timeout 20m ./internal/node/vm/qemu/
func TestE2ELazy(t *testing.T) {
	if os.Getenv("CROWDVM_E2E") != "1" {
		t.Skip("set CROWDVM_E2E=1")
	}
	if defaultAccel() != "kvm" {
		t.Skip("needs /dev/kvm")
	}
	bin, _ := buildGuest(t)
	dataDir := filepath.Join(devDir(t), fmt.Sprintf("e2e-lazy-%d", time.Now().UnixNano()))
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	sshPub, _ := ssh.NewPublicKey(pub)
	e := &e2e{t: t, signer: signer, pub: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))}
	e.opt = Options{DataDir: dataDir, GuestBinary: bin, BootTimeout: 3 * time.Minute, Logf: t.Logf}
	var err error
	if e.d, err = New(e.opt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, s := range e.d.List() {
			e.d.Destroy(context.Background(), s.SandboxID)
		}
		e.d.Close()
		os.RemoveAll(dataDir)
	})
	if !e.d.LazyChain() {
		t.Skip("qemu lacks the http/copy-on-read block drivers")
	}
	ctx := context.Background()
	imp := filepath.Join(dataDir, "import.qcow2")
	copyFile(t, testImage(t), imp)
	if err := e.d.Store().ImportImage("test-img", imp); err != nil {
		t.Fatal(err)
	}
	e.start(e.spec("a", "", proto.BootCreate))
	e.sh("a", "head -c 64M /dev/urandom > big.bin && sha256sum big.bin > big.sum && echo one > one.txt && sync")
	sum := e.sh("a", "cat big.sum")
	s1, err := e.d.Stop(ctx, "a", "s1", false)
	if err != nil {
		t.Fatal(err)
	}
	e.d.Destroy(ctx, "a")
	// "Upload" s1 and drop it from the node.
	srvDir := t.TempDir()
	p, _ := e.d.Store().SnapshotPath("s1")
	copyFile(t, p, filepath.Join(srvDir, "s1"))
	var ranges atomic.Int64
	fs := http.FileServer(http.Dir(srvDir))
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			ranges.Add(1)
		}
		fs.ServeHTTP(w, r)
	}))
	defer hs.Close()
	e.d.Store().DeleteSnapshot("s1")
	if e.d.Store().HasSnapshot("s1") {
		t.Fatal("s1 still local")
	}

	sp := e.spec("b", "s1", proto.BootResume)
	t0 := time.Now()
	if err := e.d.StartLazy(ctx, sp, []proto.SnapshotRef{s1}, map[string]string{"s1": hs.URL + "/s1"}); err != nil {
		t.Fatal(err)
	}
	e.wait("b")
	t.Logf("TIMING lazy resume: start -> guest healthy %s (%d range reads)", time.Since(t0).Round(10*time.Millisecond), ranges.Load())
	e.want("b", "cat one.txt; sha256sum -c big.sum >/dev/null && echo ok", "one\nok")
	if ranges.Load() == 0 {
		t.Fatal("no lazy reads")
	}
	// Live snapshot while lazy, then more writes.
	if s2, err := e.d.Snapshot(ctx, "b", "s2"); err != nil || s2.ParentID != "s1" {
		t.Fatalf("lazy live snapshot: %+v %v", s2, err)
	}
	e.sh("b", "echo two > two.txt && sync")
	// The download ends: import s1, switch.
	dl := filepath.Join(dataDir, "dl-s1")
	copyFile(t, filepath.Join(srvDir, "s1"), dl)
	os.Chmod(dl, 0o644) // a download is writable (the store rebases it)
	if err := e.d.Store().ImportSnapshot(s1, dl); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Hydrated(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	before := ranges.Load()
	e.sh("b", "sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches' && sha256sum -c big.sum >/dev/null && echo ok")
	if after := ranges.Load(); after != before {
		t.Fatalf("reads still go over http after Hydrated: %d -> %d", before, after)
	}
	// Recover (relaunch) and stop: plain local chain from the headers.
	if err := e.d.Restart(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	e.wait("b")
	e.want("b", "cat one.txt two.txt", "one\ntwo")
	s3, err := e.d.Stop(ctx, "b", "s3", false)
	if err != nil || s3.ParentID != "s2" {
		t.Fatalf("stop: %+v %v", s3, err)
	}
	e.start(e.spec("c", "s3", proto.BootFork))
	e.want("c", "cat one.txt two.txt; sha256sum -c big.sum >/dev/null && echo ok", "one\ntwo\nok")
	_ = sum
}

//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/crowdvm/crowdvm/internal/node"
)

// bugs-r13-sonnet (code only): with no verified daemon, `config set` wrote
// config.json itself even while a daemon held the data dir (it did not
// answer the hello in time, or is too old), rewriting the file under it.
func TestR13ConfigSetRefusesWhileDataDirLocked(t *testing.T) {
	dir := t.TempDir()
	cfg, _ := node.LoadConfig(dir)
	cfg.UIAddr = "off"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	holdLock(t, dir) // another process's daemon
	err := run(t, "--data-dir", dir, "config", "set", "cpus", "3")
	if err == nil || !strings.Contains(err.Error(), "another crowdvm-node daemon") {
		t.Fatalf("config set on a locked data dir: err = %v", err)
	}
	if c, _ := node.LoadConfig(dir); c.CPUs == 3 {
		t.Fatal("config set rewrote config.json while a daemon held the data dir")
	}
	// bugs-r13-sonnet 1: image import builds a driver only with the lock.
	if err := run(t, "--data-dir", dir, "image", "import", "/dev/null", "--id", "x"); err == nil || !strings.Contains(err.Error(), "another crowdvm-node daemon") {
		t.Fatalf("image import on a locked data dir: err = %v", err)
	}
}

// holdLock takes dir's daemon lock through its own open file, as another
// process would, until the test ends.
func holdLock(t *testing.T, dir string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "node.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

// The lock comes before newDriver now, and join runs on a data dir that does
// not exist yet: taking the lock creates it.
func TestR13LockCreatesMissingDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh", "node1")
	release, err := node.LockDataDir(dir)
	if err != nil {
		t.Fatalf("lock on a fresh data dir: %v", err)
	}
	release()
}

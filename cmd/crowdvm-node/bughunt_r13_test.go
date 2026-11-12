//go:build !windows

package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/crowdvm/crowdvm/internal/node"
	"github.com/crowdvm/crowdvm/internal/node/vm"
)

// bugs-r13-sonnet 1: the r12 data-dir lock is taken in node.Run, but runCmd
// builds the VM driver before that. qemu.New (the real driver) adopts every
// VM under <data>/vm/vms, dials each VM's single-client QMP socket, KILLS a
// qemu it believes is "ours but hung" when that dial fails (it fails when
// the first daemon holds the connection), removes vm dirs whose vm.json it
// cannot read, and runs the store's orphan GC. A second `crowdvm-node run`
// on a live data dir therefore damages the first daemon's VMs before it
// finds out that it must not run. The lock has to be held before newDriver.
func TestBughuntR13SecondDaemonBuildsDriverBeforeLockRefuses(t *testing.T) {
	dir := t.TempDir()
	cfg, _ := node.LoadConfig(dir)
	cfg.Server, cfg.NodeID, cfg.NodeToken = "http://127.0.0.1:1", "n_r13", "tok"
	cfg.Driver, cfg.UIAddr = "r13probe", "off"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	var built atomic.Int32
	drivers["r13probe"] = func(*node.Config, *slog.Logger) (vm.Driver, error) {
		built.Add(1)
		return nil, errors.New("probe: driver constructed")
	}
	t.Cleanup(func() { delete(drivers, "r13probe") })

	// The first daemon: holds the lock on <data dir>/node.lock.
	f, err := os.OpenFile(filepath.Join(dir, "node.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	err = run(t, "--data-dir", dir, "run")
	if err == nil || !strings.Contains(err.Error(), "another crowdvm-node daemon") {
		t.Fatalf("second daemon on a locked data dir: err = %v", err)
	}
	if built.Load() != 0 {
		t.Fatalf("the second daemon built its VM driver %d time(s) before the lock refused it (the real driver adopts, kills and garbage-collects the first daemon's VMs)", built.Load())
	}
}

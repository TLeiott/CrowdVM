package qemu

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestOrphanSweepKeepsRecentAndReferencedDisks(t *testing.T) {
	s, err := openStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"orphan", "recent", "image", "snapshot", "vm"} {
		path := s.abs("disks/" + name)
		if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
			t.Fatal(err)
		}
		if name != "recent" {
			old := time.Now().Add(-2 * time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.images["img"] = &imageMeta{File: "disks/image"}
	s.snaps["snap"] = &snapMeta{Ref: proto.SnapshotRef{ID: "snap"}, File: "disks/snapshot"}
	s.inUse = func() ([]string, []string) { return nil, []string{"disks/vm"} }
	s.gcOrphans(true)
	for _, name := range []string{"orphan", "recent", "image", "snapshot", "vm"} {
		_, err := os.Stat(s.abs("disks/" + name))
		if name == "orphan" {
			if !os.IsNotExist(err) {
				t.Errorf("old orphan remains: %v", err)
			}
		} else if err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
}

// A nonempty directory makes removal fail on every OS, even when tests run as root.
func blockedDisk(t *testing.T, s *store, rel string) {
	t.Helper()
	path := s.abs(rel)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "busy"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
}

func unblockDisk(t *testing.T, s *store, rel string) {
	t.Helper()
	path := s.abs(rel)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("disk"), 0o400); err != nil {
		t.Fatal(err)
	}
}

func TestGCFailedRemovalKeepsSnapshotForRetry(t *testing.T) {
	s, err := openStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	const rel = "disks/locked"
	blockedDisk(t, s, rel)
	var logs []string
	s.logf = func(f string, args ...any) { logs = append(logs, fmt.Sprintf(f, args...)) }
	if err := s.addSnapshot(proto.SnapshotRef{ID: "snap_locked"}, rel, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSnapshot("snap_locked"); err != nil {
		t.Fatal(err)
	}
	if s.snaps["snap_locked"] == nil {
		t.Fatal("failed removal lost its snapshot record")
	}
	if !strings.Contains(strings.Join(logs, "\n"), rel) {
		t.Fatalf("failed snapshot removal was not logged: %v", logs)
	}
	// The retry must also survive a daemon restart.
	s, err = openStore(s.dir, "")
	if err != nil || s.snaps["snap_locked"] == nil {
		t.Fatalf("failed removal lost its persisted record: %v", err)
	}
	unblockDisk(t, s, rel)
	s.gc()
	if s.snaps["snap_locked"] != nil {
		t.Fatal("successful retry kept the snapshot record")
	}
	if _, err := os.Stat(s.abs(rel)); !os.IsNotExist(err) {
		t.Fatalf("successful retry kept the file: %v", err)
	}
}

func TestRemoveVMKeepsFailedOverlayForRetry(t *testing.T) {
	s, err := openStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	const rel = "disks/overlay"
	blockedDisk(t, s, rel)
	var logs []string
	d := &Driver{store: s, vms: map[string]*machine{}, opt: Options{DataDir: s.dir, Logf: func(f string, args ...any) {
		logs = append(logs, fmt.Sprintf(f, args...))
	}}}
	m := &machine{id: "bx_locked", rec: vmRecord{Disk: rel}}
	d.vms[m.id] = m
	d.remove(m)
	if len(s.snaps) != 1 {
		t.Fatal("failed overlay removal left no record for GC to retry")
	}
	if !strings.Contains(strings.Join(logs, "\n"), rel) {
		t.Fatalf("failed removal was not logged: %v", logs)
	}
	s, err = openStore(s.dir, "")
	if err != nil || len(s.snaps) != 1 {
		t.Fatalf("overlay retry record did not survive a restart: %v", err)
	}
	unblockDisk(t, s, rel)
	s.gc()
	if len(s.snaps) != 0 {
		t.Fatal("GC did not retry the overlay removal")
	}
	if _, err := os.Stat(s.abs(rel)); !os.IsNotExist(err) {
		t.Fatalf("overlay file remains after retry: %v", err)
	}
}

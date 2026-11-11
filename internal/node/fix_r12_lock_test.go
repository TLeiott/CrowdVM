package node

import (
	"strings"
	"testing"
)

// bugs-r12-opus 3: a second daemon on the same data dir must not start.
func TestR12DataDirLockRefusesSecondDaemon(t *testing.T) {
	dir := t.TempDir()
	first, err := lockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := lockDataDir(dir); err == nil || !strings.Contains(err.Error(), "another crowdvm-node daemon") {
		if f != nil {
			f.Close()
		}
		t.Fatalf("second lock on one data dir: %v", err)
	}
	first.Close()
	again, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("lock after the first daemon ended: %v", err)
	}
	again.Close()
}

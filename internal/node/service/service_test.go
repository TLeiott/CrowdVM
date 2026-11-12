//go:build linux

package service

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Refresh keeps the installed ExecStart, writes and reloads only when the
// template changed, and does nothing without a unit.
func TestRefresh(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var reloads int
	orig := systemctl
	systemctl = func(_ io.Writer, args ...string) error {
		if strings.Join(args, " ") == "daemon-reload" {
			reloads++
		}
		return nil
	}
	defer func() { systemctl = orig }()
	if changed, err := Refresh(io.Discard); changed || err != nil {
		t.Fatal("no unit:", changed, err)
	}
	cur := Unit(Options{Exe: "/opt/crowd vm/crowdvm-node", Args: []string{"run", "--data-dir", "/d"}, FDStore: true})
	stale := strings.Replace(cur, "KillMode=process", "KillMode=mixed", 1)
	_ = os.MkdirAll(filepath.Dir(UnitPath()), 0o755)
	if err := os.WriteFile(UnitPath(), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := Refresh(io.Discard); !changed || err != nil || reloads != 1 {
		t.Fatal("stale unit:", changed, err, reloads)
	}
	if b, _ := os.ReadFile(UnitPath()); string(b) != cur {
		t.Fatalf("refreshed unit:\n%s", b)
	}
	if changed, err := Refresh(io.Discard); changed || err != nil || reloads != 1 {
		t.Fatal("current unit rewritten:", changed, err, reloads)
	}
}

func TestUnit(t *testing.T) {
	u := Unit(Options{Exe: "/opt/crowd vm/crowdvm-node", Args: []string{"run", "--data-dir", "/d"}})
	if !strings.Contains(u, `ExecStart="/opt/crowd vm/crowdvm-node" run --data-dir /d`) || !strings.Contains(u, "WantedBy=default.target") {
		t.Fatal(u)
	}
	var out bytes.Buffer
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Install(Options{Exe: "/x", Args: []string{"run"}, DryRun: true}, &out); err != nil || !strings.Contains(out.String(), "would write") {
		t.Fatal(err, out.String())
	}
}

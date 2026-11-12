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

	// Older releases wrote the template without the managed line: replaced.
	legacy := strings.Replace(cur, managedLine+"\n", "", 1)
	_ = os.WriteFile(UnitPath(), []byte(legacy), 0o644)
	if changed, err := Refresh(io.Discard); !changed || err != nil {
		t.Fatal("legacy unit:", changed, err)
	}
	if b, _ := os.ReadFile(UnitPath()); string(b) != cur {
		t.Fatalf("refreshed legacy unit:\n%s", b)
	}
	// An owner's edit (here: to an older unit) stays, and Refresh says so.
	edited := strings.Replace(legacy, "RestartSec=5", "RestartSec=5\nNice=10", 1)
	_ = os.WriteFile(UnitPath(), []byte(edited), 0o644)
	if changed, err := Refresh(io.Discard); changed || err == nil || !strings.Contains(err.Error(), "edited") {
		t.Fatal("edited unit:", changed, err)
	}
	if b, _ := os.ReadFile(UnitPath()); string(b) != edited {
		t.Fatal("edited unit rewritten")
	}
}

// Only a unit whose Restart= starts the node after a failed exit restarts it.
func TestRestartsOnExit(t *testing.T) {
	orig := restartPolicy
	defer func() { restartPolicy = orig }()
	const user = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/crowdvm-node.service\n"
	const system = "0::/system.slice/crowdvm-node.service\n"
	for _, c := range []struct {
		cgroup, policy string
		wantUser, want bool
	}{
		{user, "always", true, true},
		{system, "on-failure", false, true},
		{user, "on-abnormal", true, false}, // exit status 1 is not abnormal
		{system, "no", false, false},
		{"0::/user.slice/user-1000.slice/session-2.scope\n", "always", false, false},
	} {
		var gotUser bool
		restartPolicy = func(u bool) string { gotUser = u; return c.policy }
		if got := restartsOnExit(c.cgroup); got != c.want || (c.want && gotUser != c.wantUser) {
			t.Errorf("%q %s: %v (user %v)", c.cgroup, c.policy, got, gotUser)
		}
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

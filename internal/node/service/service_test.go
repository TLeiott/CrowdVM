//go:build linux

package service

import (
	"bytes"
	"strings"
	"testing"
)

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

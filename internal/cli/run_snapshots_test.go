package cli

import "testing"

// Issue #15: run's sandbox is deleted at the end, so it skips periodic
// snapshots; with --keep it outlives the command and keeps the default.
func TestRunSnapshotsOffUnlessKept(t *testing.T) {
	e := newEnv(t)
	e.run("run", "--json=false", "--", "true")
	if b := reqBody(t, e.f.Find("POST", "/api/v1/sandboxes")); b["snapshots"] != false {
		t.Fatalf("run: %v", b)
	}
	e = newEnv(t)
	e.run("run", "--json=false", "--keep", "--", "true")
	if b := reqBody(t, e.f.Find("POST", "/api/v1/sandboxes")); b["snapshots"] != nil {
		t.Fatalf("run --keep should leave snapshots to the default: %v", b)
	}
}

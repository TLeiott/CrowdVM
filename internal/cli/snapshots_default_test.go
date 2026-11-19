package cli

import (
	"net/http"
	"testing"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

// Issue #18: `config set snapshots off` stores the account default on the
// server; new --snapshots / --no-snapshots override it per sandbox.
func TestSnapshotsDefault(t *testing.T) {
	e := newEnv(t)
	on := true
	e.f.On("GET /api/v1/account-settings", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "account.settings", map[string]any{"newSandboxSnapshots": on})
	})
	e.f.On("PATCH /api/v1/account-settings", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "account.settings", map[string]any{})
	})
	e.mustRun("config", "set", "snapshots", "off")
	if b := reqBody(t, e.f.Find("PATCH", "/api/v1/account-settings")); b["newSandboxSnapshots"] != false {
		t.Fatalf("set off: %v", b)
	}
	e.mustRun("config", "unset", "snapshots")
	if b := reqBody(t, e.f.Find("PATCH", "/api/v1/account-settings")); b["newSandboxSnapshots"] != true {
		t.Fatalf("unset: %v", b)
	}
	on = false
	if r := e.mustRun("--json=false", "config", "get", "snapshots"); r.out != "off\n" {
		t.Fatalf("get: %q", r.out)
	}
	if r := e.run("config", "set", "snapshots", "maybe"); r.code == 0 {
		t.Fatal("snapshots maybe accepted")
	}

	for flag, want := range map[string]any{"": nil, "--snapshots": true, "--no-snapshots": false} {
		args := []string{"new", "--json=false"}
		if flag != "" {
			args = append(args, flag)
		}
		e.mustRun(args...)
		if b := reqBody(t, e.f.Find("POST", "/api/v1/sandboxes")); b["snapshots"] != want {
			t.Errorf("new %s: snapshots %v, want %v", flag, b["snapshots"], want)
		}
	}
	if r := e.run("new", "--snapshots", "--no-snapshots"); r.code == 0 {
		t.Fatal("--snapshots with --no-snapshots accepted")
	}
}

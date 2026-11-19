package server_test

import (
	"testing"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// Issue #18: an account-wide default for periodic snapshots that a
// request's own snapshots field overrides.
func TestNewSandboxSnapshotsDefault(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	_, key := env.NewUser("throwaway", false)
	snaps := func(body map[string]any) any {
		t.Helper()
		id := createSandbox(t, env, key, body)
		return env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil).Get("sandbox.snapshots")
	}
	if r := env.Do(key, "GET", "/api/v1/account-settings", nil); r.Get("newSandboxSnapshots") != true {
		t.Fatalf("default should be on: %s", r)
	}
	if r := env.Do(key, "PATCH", "/api/v1/account-settings", map[string]any{"newSandboxSnapshots": false}); r.Status != 200 || r.Get("newSandboxSnapshots") != false {
		t.Fatalf("patch: %s", r)
	}
	if v := snaps(nil); v != false {
		t.Fatalf("no field with the default off: snapshots %v", v)
	}
	if v := snaps(map[string]any{"snapshots": true}); v != true {
		t.Fatalf("snapshots: true must override the default: %v", v)
	}
	env.Do(key, "PATCH", "/api/v1/account-settings", map[string]any{"newSandboxSnapshots": true})
	if v := snaps(nil); v != true {
		t.Fatalf("default back on: snapshots %v", v)
	}
}

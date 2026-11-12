package server_test

import (
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// bugs-r19-opus 1: a subdomain could be another sandbox's default URL
// label (bx-<id>), and took over its URLs once that sandbox got a subdomain.
func TestR19SubdomainCannotTakeAnotherSandboxsIDLabel(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	_, bob := env.NewUser("r19bob", false)
	_, carol := env.NewUser("r19carol", false)
	victim := createSandbox(t, env, bob, nil)
	mine := createSandbox(t, env, carol, nil)
	env.WaitState(bob, victim, "ready")
	env.WaitState(carol, mine, "ready")
	r := env.Do(carol, "PATCH", "/api/v1/sandboxes/"+mine, map[string]any{"subdomain": strings.ReplaceAll(victim, "_", "-")})
	if r.Status != 400 {
		t.Fatalf("another sandbox's id label as subdomain: %s", r)
	}
	// Its own id label stays allowed (a no-op).
	if r := env.Do(carol, "PATCH", "/api/v1/sandboxes/"+mine, map[string]any{"subdomain": strings.ReplaceAll(mine, "_", "-")}); r.Status != 200 {
		t.Fatalf("own id label: %s", r)
	}
}

// bugs-r19-opus 2: a failed first save (no disk stored) counted toward the
// saved-snapshot allowance and the daily charge.
func TestR19FailedSaveDoesNotCountTowardAllowance(t *testing.T) {
	env := servertest.New(t)
	u, key := env.NewUser("r19saver", false)
	env.Do(env.AdminKey, "PUT", "/api/v1/admin/policies/r19saver", map[string]any{"savedSnapshotAllowance": 1})
	if _, err := env.S.DB.Exec(`INSERT INTO saved_snapshots(id,owner_id,name,status,source_sandbox_id,created_at,updated_at) VALUES('ss_r19failed',?,'broken','failed','bx_gone',1,1)`, u.ID); err != nil {
		t.Fatal(err)
	}
	r := env.Do(key, "GET", "/api/v1/saved-snapshots", nil)
	if used, _ := r.Get("allowance.used").(float64); used != 0 {
		t.Fatalf("a failed save with no disk counts as used: %s", r)
	}
}

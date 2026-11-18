package server_test

import (
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// Custom shapes ("8c10g") are placed with their own vCPU/RAM, billed by the
// per-resource formula and allowed by a size policy they fit under.
func TestCustomSandboxSize(t *testing.T) {
	env := servertest.New(t)
	// 12 GiB of RAM: large (16 GiB) never fits, 8c10g does.
	n := env.StartNode(servertest.NodeOptions{Capacity: proto.Capacity{VCPU: 8, MemoryMiB: 12 << 10, DiskGiB: 500}})
	u, key := env.NewUser("custom", false)

	r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "8C10G"})
	if r.Status != 202 || r.Str("sandbox.type") != "8c10g" || r.Str("sandbox.vcpu") != "8" || r.Str("sandbox.memoryGB") != "10" ||
		r.Str("sandbox.billingMultiplier") != "1.625" || r.Str("sandbox.storage.diskGB") != "111" || r.Str("sandbox.nodeId") != n.ID {
		t.Fatalf("create 8c10g: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "8c999g"}); r.Status != 400 || r.Code() != "invalid_type" {
		t.Fatalf("out of bounds: %s", r)
	}
	if r := env.Do(env.AdminKey, "PUT", "/api/v1/admin/policies/"+u.Username, map[string]any{"allowedSizes": []string{"small", "large"}}); r.Status != 200 {
		t.Fatalf("policy: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "default"}); r.Status != 403 {
		t.Fatalf("a listed-only policy let default through: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "6c12g", "failFast": false}); r.Status != 202 {
		t.Fatalf("6c12g fits under large: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "12c8g"}); r.Status != 403 {
		t.Fatalf("12c8g exceeds every allowed size: %s", r)
	}
}

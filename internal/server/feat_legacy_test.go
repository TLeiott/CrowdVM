package server_test

import (
	"testing"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func TestLegacyBoxAPI(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey

	r := env.Do(key, "POST", "/api/box/v1/boxes", map[string]any{"name": "legacy"})
	if r.Status != 202 || r.Str("type") != "box.created" || r.Str("box.id") == "" || r.Get("sandbox") != nil {
		t.Fatalf("create: %s", r)
	}
	if r.Header.Get("Deprecation") == "" || r.Header.Get("Sunset") == "" || r.Header.Get("Link") == "" {
		t.Fatalf("headers: %v", r.Header)
	}
	id := r.Str("box.id")
	env.WaitFor(key, "/api/box/v1/boxes/"+id, "box.state", 10e9, "ready")

	r = env.Do(key, "GET", "/api/box/v1/boxes", nil)
	if r.Status != 200 || r.Str("type") != "box.list" || r.Str("boxes.0.id") != id {
		t.Fatalf("list: %s", r)
	}
	// Pass-through under the alias.
	if r := env.Do(key, "POST", "/api/box/v1/boxes/"+id+"/commands", map[string]any{"command": "echo hi"}); r.Status != 200 || r.Str("stdout") != "echo hi\n" {
		t.Fatalf("command: %s", r)
	}
	// Non-box routes work too.
	if r := env.Do(key, "GET", "/api/box/v1/me", nil); r.Status != 200 || r.Str("user.id") == "" {
		t.Fatalf("me: %s", r)
	}
	// Delete with the confirmation header.
	r = env.Do(key, "DELETE", "/api/box/v1/boxes/"+id, nil, "X-Ascii-Confirm-Delete: "+id)
	if r.Status != 202 || r.Str("type") != "box.deleting" {
		t.Fatalf("delete: %s", r)
	}
	if r := env.Do(key, "GET", "/api/box/v1/boxes/"+id, nil); r.Status != 404 {
		t.Fatalf("after delete: %s", r)
	}
}

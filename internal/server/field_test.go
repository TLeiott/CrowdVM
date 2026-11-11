package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// Field test (two machines): `crowdvm stop` then `crowdvm resume --node
// other` cancelled the stop and kept the sandbox on its node, dropping the
// placement. A new placement must wait for the stop like a new size does,
// and a running sandbox must be stopped first.
func TestResumeWithPlacementDoesNotCancelStop(t *testing.T) {
	env := servertest.New(t)
	n1 := env.StartNode(servertest.NodeOptions{})
	n2 := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	r := env.WaitState(key, id, "ready")
	node, other := n1, n2
	if r.Str("sandbox.nodeId") == n2.ID {
		node, other = n2, n1
	}
	move := map[string]any{"placement": map[string]any{"node": other.ID}}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", move); r.Status != 409 || r.Code() != "sandbox_running_stop_first" {
		t.Fatalf("move a running sandbox: %s", r)
	}
	// The same placement it already has is no change.
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"placement": map[string]any{}}); r.Status != 202 {
		t.Fatalf("empty placement on a running sandbox: %s", r)
	}
	node.FailNext("POST /v1/vms/" + id + "/stop")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.stop.status", 5*time.Second, "failing")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", move); r.Status != 409 || r.Code() != "sandbox_stopping" || r.Get("error.details.retryable") != true {
		t.Fatalf("move while stopping: %s", r)
	}
	r = env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", 15*time.Second, "archived")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", move); r.Status != 202 {
		t.Fatalf("move after the stop: %s", r)
	}
	r = env.WaitState(key, id, "ready")
	if r.Str("sandbox.nodeId") != other.ID {
		t.Fatalf("resumed on %s, want %s", r.Str("sandbox.nodeId"), other.ID)
	}
}

// Field test: the server restarted while a laptop node was asleep. The node
// never reconnected and never "disconnected" from the new server, so its
// sandbox stayed ready/ok for good and was never rebuilt. A node still
// missing after the reconnect window is offline.
func TestNodeLostDuringServerRestartGoesOffline(t *testing.T) {
	t.Cleanup(server.SetNodeReconnectWindow(0))
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.S.ForgetNodeConn(node.ID)
	r := env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.health", 5*time.Second, "degraded")
	if r.Str("sandbox.state") != "error" || r.Str("sandbox.healthReason") != "node_offline" {
		t.Fatalf("after the node went missing: %s", r)
	}
}

// Field test: a sandbox created with --node on a laptop that fell asleep sat
// in node_offline for good with no word why. It still waits (the pin is a
// hard requirement), but its owner is told how to move it.
func TestPinnedSandboxOnLostNodeTellsOwner(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.NodeLostGrace = 200 * time.Millisecond; c.NodeTimeout = time.Second })
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	env.StartNode(servertest.NodeOptions{Name: "b"})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
	env.WaitState(key, id, "ready")
	if _, err := a.PeriodicSnapshot(id); err != nil {
		t.Fatal(err)
	}
	env.WaitFor(key, "/api/v1/sandboxes/"+id+"/snapshots/latest", "snapshot.uploaded", 5*time.Second, "true")
	a.Close()
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", 10*time.Second, "error")
	env.Eventually("rebuild_blocked notice", func() bool {
		return strings.Contains(string(env.Do(key, "GET", "/api/v1/notices", nil).Body), "rebuild_blocked")
	})
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.state") != "error" {
		t.Fatalf("a pinned sandbox moved: %s", r)
	}
}

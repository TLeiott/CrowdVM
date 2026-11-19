package server_test

import (
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// Issue 7: a node restarting for an update was away 5 s, yet its sandboxes
// turned error node_offline at once and an agent deleted one. A node back
// within the grace period leaves them alone; one that stays away does not.
func TestNodeOfflineGrace(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.NodeOfflineGrace = time.Second })
	var mu sync.Mutex
	var errs int
	env.S.Bus.Subscribe(func(e server.Event) {
		if e.Type == server.EvSandboxError {
			mu.Lock()
			errs++
			mu.Unlock()
		}
	})
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")

	// Back in time, then away again: the first timer must not fire for the
	// second absence.
	node.Disconnect()
	time.Sleep(200 * time.Millisecond)
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	node.Disconnect()
	time.Sleep(700 * time.Millisecond) // 1.3 s after the first disconnect
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.state") != "ready" {
		t.Fatalf("a node within its grace period failed its sandbox: %s", r)
	}
	mu.Lock()
	n := errs
	mu.Unlock()
	if n != 0 {
		t.Fatalf("%d sandbox.error events during a restart", n)
	}

	// Away for good: today's node_offline handling.
	r := env.WaitState(key, id, "error")
	if r.Str("sandbox.error") != "node_offline" {
		t.Fatalf("after the grace period: %s", r)
	}
}

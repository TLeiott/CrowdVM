package server_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/compat"
	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func setGen(t *testing.T, p *int, v int) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// A node below Required gets no new sandboxes, its owner is told, its
// sandboxes get an evacuation deadline, and all of it ends when the node
// comes back updated.
func TestOutdatedNode(t *testing.T) {
	setGen(t, &compat.RequiredNodeGen, compat.NodeGen)
	env := servertest.New(t)
	alice, aliceKey := env.NewUser("alice", false)
	_, tok, err := env.S.CreateJoinToken(server.JoinTokenParams{Name: "a", CreatedBy: alice.ID, OwnerID: alice.ID, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	old := compat.NodeGen - 1
	node := env.StartNode(servertest.NodeOptions{JoinToken: tok, Gen: &old})
	path := "/api/v1/nodes/" + node.ID
	env.Eventually("outdated with a deadline", func() bool {
		r := env.Do(aliceKey, "GET", path, nil)
		return r.Str("node.update.state") == compat.StateOutdated && r.Str("node.evacuation.reason") == "outdated" && r.Str("node.update.deadline") != ""
	})
	notices := func() (n int) {
		_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM notices WHERE user_id=? AND kind='node_outdated'`, alice.ID).Scan(&n)
		return n
	}
	// Told only when it stays outdated (most nodes update themselves at once).
	time.Sleep(200 * time.Millisecond)
	if n := notices(); n != 0 {
		t.Fatalf("owner told at once: %d notices", n)
	}
	server.OutdatedNoticeDelay = 0
	t.Cleanup(func() { server.OutdatedNoticeDelay = 10 * time.Minute })
	env.Eventually("owner told once", func() bool { return notices() == 1 })
	time.Sleep(200 * time.Millisecond)
	if n := notices(); n != 1 {
		t.Fatalf("owner notices: %d", n)
	}
	env.Eventually("heartbeat says outdated", func() bool {
		u := node.LastHeartbeatResponse().Update
		return u != nil && u.State == compat.StateOutdated && u.Command == server.NodeUpdateCommand
	})
	// No placement on the only node, which is outdated: a create waits.
	waiting := createSandbox(t, env, env.AdminKey, nil)
	time.Sleep(300 * time.Millisecond)
	if r := env.Do(env.AdminKey, "GET", "/api/v1/sandboxes/"+waiting, nil); r.Str("sandbox.waitingForCapacity") != "true" {
		t.Fatalf("create on an outdated node: %s", r)
	}
	// Its owner cannot cancel the evacuation; updating ends it.
	if r := env.Do(aliceKey, "DELETE", path+"/evacuate", nil); r.Status != 409 || r.Code() != "node_outdated" {
		t.Fatalf("cancel outdated evacuation: %s", r)
	}
	node.Disconnect()
	node.SetGen(compat.NodeGen)
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	env.Eventually("updated node is current", func() bool {
		r := env.Do(aliceKey, "GET", path, nil)
		return r.Str("node.update.state") == compat.StateCurrent && r.Get("node.evacuation") == nil && r.Str("node.disabled") == "false"
	})
	env.WaitState(env.AdminKey, waiting, "ready") // the waiting create boots there now
}

// An owner's evacuation of an outdated node stays the owner's after the update.
func TestOwnerEvacuationSurvivesUpdate(t *testing.T) {
	setGen(t, &compat.RequiredNodeGen, compat.NodeGen)
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	env.WaitState(env.AdminKey, createSandbox(t, env, env.AdminKey, nil), "ready") // keeps the evacuation going
	reconnect := func(gen int) {
		node.Disconnect()
		node.SetGen(gen)
		if err := node.Connect(); err != nil {
			t.Fatal(err)
		}
	}
	path := "/api/v1/nodes/" + node.ID
	reconnect(compat.NodeGen - 1)
	env.Eventually("outdated", func() bool { return env.Do(env.AdminKey, "GET", path, nil).Str("node.evacuation.reason") == "outdated" })
	if r := env.Do(env.AdminKey, "POST", path+"/evacuate", map[string]any{"graceSec": 3600}); r.Status != 200 || r.Str("node.evacuation.reason") != "owner" {
		t.Fatalf("evacuate: %s", r)
	}
	reconnect(compat.NodeGen)
	env.Eventually("current", func() bool {
		return env.Do(env.AdminKey, "GET", path, nil).Str("node.update.state") == compat.StateCurrent
	})
	if r := env.Do(env.AdminKey, "GET", path, nil); r.Str("node.evacuation.reason") != "owner" {
		t.Fatalf("owner evacuation after update: %s", r)
	}
}

// Below Min the connection is refused with 426 and the body says what to run.
func TestIncompatibleNodeRefused(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	node.Disconnect()
	setGen(t, &compat.MinNodeGen, compat.NodeGen+1)
	err := node.Connect()
	var re *mux.RefusedError
	if !errors.As(err, &re) || re.Status != http.StatusUpgradeRequired || !strings.Contains(string(re.Body), server.NodeUpdateCommand) {
		t.Fatalf("connect: %v", err)
	}
}

func TestClientGate(t *testing.T) {
	env := servertest.New(t)
	setGen(t, &compat.MinAPIGen, 2)
	r := env.Do(env.AdminKey, "GET", "/api/v1/me", nil, compat.Header+": 1")
	if r.Status != 426 || r.Code() != "client_outdated" {
		t.Fatalf("below min: %s", r)
	}
	r = env.Do(env.AdminKey, "GET", "/api/v1/me", nil, "User-Agent: crowdvm-cli/0.0.1")
	if r.Status != 426 {
		t.Fatalf("old CLI without header: %s", r)
	}
	// Not ours (curl, browsers): not judged.
	if r := env.Do(env.AdminKey, "GET", "/api/v1/me", nil, "User-Agent: curl/8"); r.Status != 200 {
		t.Fatalf("curl: %s", r)
	}
	setGen(t, &compat.MinAPIGen, 0)
	setGen(t, &compat.RequiredAPIGen, 2)
	r = env.Do(env.AdminKey, "GET", "/api/v1/me", nil, compat.Header+": 1")
	if r.Status != 200 || !strings.HasPrefix(r.Header.Get(compat.ClientStatusHeader), "outdated") {
		t.Fatalf("below required: %s %v", r, r.Header)
	}
	r = env.Do(env.AdminKey, "GET", "/api/v1/me", nil, compat.Header+": 2")
	if r.Header.Get(compat.ClientStatusHeader) != "" {
		t.Fatal("current client marked outdated")
	}
	if r := env.Do(env.AdminKey, "GET", "/api/v1/server", nil); r.Str("apiGen") == "" || r.Str("requiredApiGen") != "2" {
		t.Fatalf("server info: %s", r)
	}
}

// A node that stays outdated past the grace period loses its sandboxes to
// current nodes (stopped with their disk saved, resumed there).
func TestOutdatedNodeGraceMovesSandboxes(t *testing.T) {
	setGen(t, &compat.RequiredNodeGen, compat.NodeGen)
	env := servertest.New(t, func(c *server.Config) { c.NodeUpdateGrace = 500 * time.Millisecond })
	a := env.StartNode(servertest.NodeOptions{})
	sb := createSandbox(t, env, env.AdminKey, nil)
	env.WaitState(env.AdminKey, sb, "ready")
	b := env.StartNode(servertest.NodeOptions{})
	a.Disconnect()
	a.SetGen(compat.NodeGen - 1)
	if err := a.Connect(); err != nil {
		t.Fatal(err)
	}
	env.Eventually("moved to the current node", func() bool {
		r := env.Do(env.AdminKey, "GET", "/api/v1/sandboxes/"+sb, nil)
		return r.Str("sandbox.state") == "ready" && r.Str("sandbox.nodeId") == b.ID
	})
}

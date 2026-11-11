package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func TestNodeDisconnectAndRecover(t *testing.T) {
	env := servertest.New(t)
	var mu sync.Mutex
	var events []string
	env.S.Bus.Subscribe(func(e server.Event) {
		mu.Lock()
		events = append(events, e.Type+":"+e.Reason)
		mu.Unlock()
	})
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")

	node.Disconnect()
	r := env.WaitState(key, id, "error")
	if r.Str("sandbox.error") != "node_offline" || r.Str("sandbox.health") != "degraded" || r.Str("sandbox.healthReason") != "node_offline" {
		t.Fatalf("offline: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/nodes/"+node.ID, nil); r.Str("node.online") != "false" {
		t.Fatalf("node still online: %s", r)
	}
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	r = env.WaitState(key, id, "ready")
	if r.Str("sandbox.health") != "ok" || r.Get("sandbox.error") != nil {
		t.Fatalf("recovered: %s", r)
	}
	env.Eventually("degraded and recovered events", func() bool {
		mu.Lock()
		defer mu.Unlock()
		var d, rec bool
		for _, e := range events {
			d = d || e == server.EvSandboxDegraded+":node_offline"
			rec = rec || strings.HasPrefix(e, server.EvSandboxRecovered)
		}
		return d && rec
	})

	// A VM that died while the node was away stays in error. It never
	// completed a snapshot, so a resume (an empty disk) is refused and
	// recover power-cycles it on its node instead.
	node.Disconnect()
	env.WaitState(key, id, "error")
	node.SetVMState(id, proto.VMStopped, false, "") // no session: dropped
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.state") != "error" {
		t.Fatalf("dead VM should stay error: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status != 409 || r.Code() != "resume_failed" {
		t.Fatalf("resume from error without a snapshot: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/recover", nil); r.Status != 200 {
		t.Fatalf("recover from error: %s", r)
	}
	env.WaitState(key, id, "ready")
}

func TestNodeRegistrationAndBlobs(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.SnapshotInterval = 90 * time.Second })
	// Bad join token.
	resp, err := http.Post(env.URL+"/node/v1/register", "application/json", strings.NewReader(`{"joinToken":"cvj_nope","info":{"name":"x"}}`))
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("bad join token: %v %v", err, resp.StatusCode)
	}
	resp.Body.Close()
	// Single-use token.
	_, tok, err := env.S.CreateJoinToken(server.JoinTokenParams{CreatedBy: env.Admin.ID, MaxUses: 1, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	n1, err := servertest.StartFakeNode(env.URL, servertest.NodeOptions{JoinToken: tok})
	if err != nil {
		t.Fatal(err)
	}
	defer n1.Close()
	if _, err := servertest.StartFakeNode(env.URL, servertest.NodeOptions{JoinToken: tok}); err == nil {
		t.Fatal("join token reused")
	}
	env.Eventually("heartbeat reply", func() bool { return n1.LastHeartbeatResponse().SnapshotIntervalSec == 90 })

	// Blob API on the main listener with the node token.
	blob := []byte("image bytes")
	sum := sha256.Sum256(blob)
	put := func(sha string) int {
		req, _ := http.NewRequest("PUT", env.URL+"/node/v1/blobs/image/ubuntu-test", bytes.NewReader(blob))
		req.Header.Set("Authorization", "Bearer "+n1.Token)
		req.Header.Set("X-Crowdvm-Sha256", sha)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := put(strings.Repeat("0", 64)); st != 422 {
		t.Fatalf("checksum mismatch accepted: %d", st)
	}
	if st := put(hex.EncodeToString(sum[:])); st != 200 {
		t.Fatalf("put: %d", st)
	}
	req, _ := http.NewRequest("GET", env.URL+"/node/v1/blobs/image/ubuntu-test", nil)
	req.Header.Set("Authorization", "Bearer "+n1.Token)
	req.Header.Set("Range", "bytes=6-")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(b) != "bytes" || resp.Header.Get("X-Crowdvm-Sha256") != hex.EncodeToString(sum[:]) {
		t.Fatalf("range get: %d %q", resp.StatusCode, b)
	}
	// Without a token.
	if r := env.Do("", "GET", "/node/v1/blobs/image/ubuntu-test", nil); r.Status != 401 {
		t.Fatalf("anonymous blob: %s", r)
	}
	// The uploaded image becomes the default; new sandboxes fetch it.
	if r := env.Do(env.AdminKey, "GET", "/api/v1/server", nil); r.Str("defaultImage") != "ubuntu-test" {
		t.Fatalf("default image: %s", r)
	}
	env.Eventually("node online", func() bool { return env.S.NodeConn(n1.ID) != nil })
	id := createSandbox(t, env, env.AdminKey, nil)
	env.WaitState(env.AdminKey, id, "ready")
	if !n1.Called("POST /v1/images/ubuntu-test/fetch") {
		t.Fatalf("image not fetched: %v", n1.Calls())
	}

	// Bench endpoints.
	req, _ = http.NewRequest("GET", env.URL+"/node/v1/bench/download?bytes=1000000", nil)
	req.Header.Set("Authorization", "Bearer "+n1.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if n != 1000000 {
		t.Fatalf("bench download %d", n)
	}
	// Join tokens via the API (admin only).
	_, userKey := env.NewUser("plainuser", false)
	if r := env.Do(userKey, "POST", "/api/v1/join-tokens", nil); r.Status != 403 {
		t.Fatalf("user join token: %s", r)
	}
	if r := env.Do(env.AdminKey, "POST", "/api/v1/join-tokens", map[string]any{"maxUses": 0}); r.Status != 201 || !strings.HasPrefix(r.Str("token"), "cvj_") {
		t.Fatalf("admin join token: %s", r)
	}
}

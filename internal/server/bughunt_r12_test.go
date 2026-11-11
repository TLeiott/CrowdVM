package server_test

import (
	"bytes"
	"encoding/json"
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

// bugs-r12-sonnet 1: handleEvacuateNode checks the grace with
// `time.Duration(grace)*time.Second > MaxEvacuationGrace`. Duration is an
// int64 of nanoseconds, so a huge graceSec wraps: 2^55 s * 1e9 = 2^64 * 5^9,
// which is exactly 0 mod 2^64. The cap is skipped, and the deadline
// (`now + grace*1000`, also wrapped) becomes a nonsense value (here before
// now: the evacuation is overdue at once and can no longer be cancelled).
func TestBughuntR12EvacuateGraceOverflowSkipsCap(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	r := env.Do(env.AdminKey, "POST", "/api/v1/nodes/"+node.ID+"/evacuate", map[string]any{"graceSec": int64(1) << 55})
	if r.Status != 400 {
		t.Fatalf("graceSec 2^55 (over the %s cap) was accepted: status %d", server.MaxEvacuationGrace, r.Status)
	}
}

// bugs-r12-sonnet 2: uniqueNodeName (nodes.go:431) is a SELECT COUNT followed
// by a separate INSERT/UPDATE, with no unique index. Nodes that register at
// the same time with the same name all see the name free and all keep it, so
// pins by name (placement.node, nodeByRef) match several nodes again: the
// r11 fix only holds for sequential registrations.
func TestBughuntR12ParallelRegistrationsShareNodeName(t *testing.T) {
	env := servertest.New(t)
	for round := 0; round < 10; round++ {
		_, tok, err := env.S.CreateJoinToken(server.JoinTokenParams{Name: "many", CreatedBy: env.Admin.ID, TTL: time.Hour, MaxUses: 0})
		if err != nil {
			t.Fatal(err)
		}
		name := "twin-" + string(rune('a'+round))
		const n = 12
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				body, _ := json.Marshal(proto.RegisterRequest{JoinToken: tok, Info: proto.NodeInfo{Name: name}})
				<-start
				resp, err := http.Post(env.URL+"/node/v1/register", "application/json", bytes.NewReader(body))
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}()
		}
		close(start)
		wg.Wait()
		same := 0
		for _, x := range env.Do(env.AdminKey, "GET", "/api/v1/nodes", nil).Get("nodes").([]any) {
			if strings.EqualFold(x.(map[string]any)["name"].(string), name) {
				same++
			}
		}
		if same > 1 {
			t.Fatalf("%d nodes registered in parallel all hold the name %q (round %d)", same, name, round)
		}
	}
}

// bugs-r11 fix 9 / bugs-r12-sonnet 3: ServeHTTP picks the rate-limit bucket
// from any credential that authenticates, also on public routes that ignore
// it (/auth/login, email codes, signup). One request to the anonymous
// POST /agent/identity yields a fresh account and key, and every such key
// brings a bucket of its own: one IP gets IPRateLimit requests per minute per
// identity on the password and code guessing endpoints instead of per IP.
func TestBughuntR12CredentialOnPublicRouteEscapesIPLimit(t *testing.T) {
	const limit = 6
	env := servertest.New(t, func(c *server.Config) { c.IPRateLimit = limit })
	var toks []string
	for i := 0; i < 3; i++ {
		r := env.Do("", "POST", "/agent/identity", map[string]any{"method": "anonymous", "name": "bot"})
		if r.Str("access_token") == "" {
			t.Fatalf("identity: %s", r)
		}
		toks = append(toks, r.Str("access_token"))
	}
	passed := 0 // wrong-password logins that were answered rather than limited
	for i := 0; i < 5*limit; i++ {
		r := env.Do(toks[i%len(toks)], "POST", "/auth/login", map[string]any{"username": "admin", "password": "wrong-guess"})
		if r.Status != 429 {
			passed++
		}
	}
	if passed > limit {
		t.Fatalf("%d password guesses from one IP got through with IPRateLimit=%d (a valid but unrelated key picks another bucket)", passed, limit)
	}
}

// bugs-r12-sonnet 4: every heartbeat that carries Info (the first one after
// each connect, and since the schedule feature every time the schedule
// crosses a boundary or the capacity changes) and afterNodeConnect write the
// node's configured name over nodes.name (nodes.go:~583, ~897). A name set
// through PATCH /nodes/{id} (the r11 rename path) is reverted the next time
// the node reconnects, though the node's own name did not change.
func TestBughuntR12AdminRenameRevertedByNextInfoHeartbeat(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Name: "laptop-orig"})
	if r := env.Do(env.AdminKey, "PATCH", "/api/v1/nodes/"+node.ID, map[string]any{"name": "chosen-by-admin"}); r.Status != 200 {
		t.Fatalf("rename: %s", r)
	}
	// What the real node sends after a reconnect or a schedule boundary: the
	// same info as before (same configured name).
	hb, _ := json.Marshal(proto.Heartbeat{Info: &proto.NodeInfo{Name: "laptop-orig", Paused: true, PauseReason: "schedule"}})
	if code, body := r11NodeReq(t, env, node.Token, "POST", "/node/v1/heartbeat", hb); code != 200 {
		t.Fatalf("heartbeat: %d %s", code, body)
	}
	r := env.Do(env.AdminKey, "GET", "/api/v1/nodes/"+node.ID, nil)
	if got := r.Str("node.name"); got != "chosen-by-admin" {
		t.Fatalf("the admin's rename was undone by a heartbeat that did not change the node's name: now %q", got)
	}
}

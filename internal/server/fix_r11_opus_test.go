package server_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// r11HistoryEnv starts a node with a fake guest and one ready sandbox.
func r11HistoryEnv(t *testing.T, cfg func(*server.Config)) (*servertest.Env, *servertest.FakeNode, *servertest.FakeGuest, string) {
	t.Helper()
	g := servertest.NewFakeGuest()
	var env *servertest.Env
	if cfg != nil {
		env = servertest.New(t, cfg)
	} else {
		env = servertest.New(t)
	}
	node := env.StartNode(servertest.NodeOptions{Guest: g})
	id := createSandbox(t, env, env.AdminKey, nil)
	env.WaitState(env.AdminKey, id, "ready")
	return env, node, g, id
}

// bugs-r11-opus 4: a record's command text is capped, the records and samples
// of one pull are capped.
func TestR11OpusHistoryExportIsCapped(t *testing.T) {
	env, _, g, id := r11HistoryEnv(t, nil)
	now := time.Now().UTC()
	recs := []proto.CommandRecord{{CommandID: "cmd_big", Command: strings.Repeat("x", 300<<10), Mode: "sync", Status: "exited", StartedAt: now}}
	for i := 0; i < 700; i++ {
		recs = append(recs, proto.CommandRecord{CommandID: "cmd_" + time.Duration(i).String(), Command: "ls", Mode: "sync", Status: "exited", StartedAt: now})
	}
	var smp []proto.GuestSample
	for i := 0; i < 300; i++ {
		smp = append(smp, proto.GuestSample{Time: now.Add(-time.Duration(i) * 30 * time.Second)})
	}
	g.AddHistory(id, proto.HistoryExport{Records: recs, Samples: smp})
	env.Eventually("pulled", func() bool { return g.HistoryAcked(id) > 0 })
	var n, longest, rec, samples int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*), MAX(length(command)), MAX(length(record)) FROM command_history WHERE sandbox_id=?`, id).Scan(&n, &longest, &rec)
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM process_samples WHERE sandbox_id=?`, id).Scan(&samples)
	if longest > 64<<10 || rec > 80<<10 {
		t.Errorf("command not capped: command %d bytes, record %d bytes", longest, rec)
	}
	if n > 500 {
		t.Errorf("%d records stored from one pull", n)
	}
	if samples > 200 {
		t.Errorf("%d samples stored from one pull", samples)
	}
}

// bugs-r11-opus 4: an export longer than the server reads is refused as a
// whole, nothing of it is stored, and it is not acked.
func TestR11OpusHistoryExportIsBounded(t *testing.T) {
	env, node, g, id := r11HistoryEnv(t, nil)
	g.AddHistory(id, proto.HistoryExport{Records: []proto.CommandRecord{{CommandID: "cmd_huge", Command: strings.Repeat("y", 20<<20), Mode: "sync", Status: "exited", StartedAt: time.Now().UTC()}}})
	env.Eventually("pulled", func() bool { return node.Called("GET /v1/vms/" + id + "/guest/internal/history/export") })
	time.Sleep(time.Second)
	var n int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM command_history WHERE sandbox_id=?`, id).Scan(&n)
	if n != 0 || g.HistoryAcked(id) != 0 {
		t.Fatalf("a 20 MiB export was stored (%d records) or acked (%d)", n, g.HistoryAcked(id))
	}
}

// bugs-r11-opus 4: replacing a stored output chunk with a bigger one is
// charged for its growth; a chunk that does not fit leaves the old copy.
func TestR11OpusReplacedChunkIsCharged(t *testing.T) {
	env, _, g, id := r11HistoryEnv(t, func(c *server.Config) { c.OutputCapPerHour = 2000 })
	now := time.Now().UTC()
	rec := proto.CommandRecord{CommandID: "cmd_x", Command: "cat", Mode: "sync", Status: "exited", StartedAt: now}
	g.AddHistory(id, proto.HistoryExport{Records: []proto.CommandRecord{rec}, Chunks: []proto.HistoryChunk{{CommandID: "cmd_x", Stream: "stdout", Offset: 0, Data: make([]byte, 1000)}}})
	env.Eventually("first pull", func() bool { return g.HistoryAcked(id) >= 1 })
	g.AddHistory(id, proto.HistoryExport{Records: []proto.CommandRecord{rec}, Chunks: []proto.HistoryChunk{{CommandID: "cmd_x", Stream: "stdout", Offset: 0, Data: make([]byte, 40000)}}})
	env.Eventually("second pull", func() bool { return g.HistoryAcked(id) >= 2 })
	var size int
	_ = env.S.DB.QueryRow(`SELECT COALESCE(SUM(length(data)),0) FROM command_logs WHERE sandbox_id=?`, id).Scan(&size)
	if size > 2000 {
		t.Fatalf("command_logs holds %d bytes with a 2000 byte quota", size)
	}
}

// bugs-r11-opus 6: a foreign deploy also removes OpenCode's SQLite store,
// Prime's session artifacts and Codex's SQLite stores and session index.
func TestR11OpusForeignScrubListsOpenCodeAndCodexStores(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "tpl11"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/tpl11", "snapshot.status", 5*time.Second, "ready")
	code := env.Do(key, "POST", "/api/v1/saved-snapshots/tpl11/shares", nil).Str("shareCode")
	_, bobKey := env.NewUser("r11obob", false)
	bobs := createSandbox(t, env, bobKey, map[string]any{"from": code})
	env.WaitState(bobKey, bobs, "ready")
	sc := guestSpec(t, node, bobs).Scrub
	if sc == nil {
		t.Fatal("no scrub")
	}
	for _, f := range []string{".local/share/opencode/opencode.db", ".local/share/opencode/opencode.db-wal", ".local/share/opencode/opencode.db-shm",
		".codex/session_index.jsonl", ".codex/state_*.sqlite*", ".codex/logs_*.sqlite*", ".codex/history*.sqlite*",
		".codex/state_5.sqlite", ".codex/logs_2.sqlite", ".codex/history.sqlite"} {
		if !slices.Contains(sc.Files, f) {
			t.Errorf("scrub files miss %s: %v", f, sc.Files)
		}
	}
	if !slices.Contains(sc.Dirs, ".prime/agent/session-artifacts") {
		t.Errorf("scrub dirs miss session-artifacts: %v", sc.Dirs)
	}
}

// bugs-r11-opus 9: requests that authenticate count per user, not per IP, so
// one guest flooding unauthenticated requests through its node's address does
// not lock out the others. Unauthenticated requests, valid-looking tokens
// that do not exist included, still share the IP bucket.
func TestR11OpusRateLimitIsPerPrincipal(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.IPRateLimit = 6 })
	_, bobKey := env.NewUser("r11olim1", false)
	_, carolKey := env.NewUser("r11olim2", false)
	for i := 0; i < 10; i++ { // the flood: no credentials
		env.Do("", "GET", "/api/v1/me", nil)
	}
	if r := env.Do("", "GET", "/api/v1/me", nil); r.Status != 429 {
		t.Fatalf("unauthenticated flood not limited: %s", r)
	}
	if r := env.Do("asc_"+strings.Repeat("a", 40), "GET", "/api/v1/me", nil); r.Status != 429 {
		t.Fatalf("an unknown token got a bucket of its own: %s", r)
	}
	for i := 0; i < 5; i++ {
		if r := env.Do(bobKey, "GET", "/api/v1/me", nil); r.Status != 200 {
			t.Fatalf("bob locked out by the IP's flood (%d): %s", i, r)
		}
	}
	if r := env.Do(carolKey, "GET", "/api/v1/me", nil); r.Status != 200 {
		t.Fatalf("carol locked out: %s", r)
	}
	// A user's own bucket still fills.
	for i := 0; i < 6; i++ {
		env.Do(bobKey, "GET", "/api/v1/me", nil)
	}
	if r := env.Do(bobKey, "GET", "/api/v1/me", nil); r.Status != 429 || r.Code() != "IP_RATE_LIMIT_EXCEEDED" {
		t.Fatalf("a user is not limited: %s", r)
	}
	if r := env.Do(carolKey, "GET", "/api/v1/me", nil); r.Status != 200 {
		t.Fatalf("carol limited by bob: %s", r)
	}
}

// bugs-r11-opus 12: without the allow-private opt-in, a custom AI API whose
// host resolves to a LAN address is refused at dial time (the DB row stands in
// for a DNS name: the save check only reads the URL's text).
func TestR11OpusCustomAPIRefusesLANAtDial(t *testing.T) {
	var lan string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.IsPrivate() {
			lan = ipn.IP.String()
			break
		}
	}
	if lan == "" {
		t.Skip("no private IPv4 address on this machine")
	}
	ln, err := net.Listen("tcp", lan+":0")
	if err != nil {
		t.Skip("cannot listen on", lan, err)
	}
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("reached"))
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	call := func(allow string) (int, string) {
		t.Setenv("CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE", allow)
		env := servertest.New(t, func(c *server.Config) { c.WebhookAllowLocal = false })
		node := env.StartNode(servertest.NodeOptions{})
		key := env.AdminKey
		env.Do(key, "PUT", "/api/v1/credential-profiles/default/custom-apis/LAN1", map[string]any{"value": "CUSTOMSECRET-R11", "baseUrl": "https://api.example.com"})
		if _, err := env.S.DB.Exec(`UPDATE credential_custom_apis SET base_url=?`, srv.URL); err != nil {
			t.Fatal(err)
		}
		env.Do(key, "PUT", "/api/v1/ai-proxy", map[string]any{"enabled": true})
		id := createSandbox(t, env, key, nil)
		env.WaitState(key, id, "ready")
		tok := guestSpec(t, node, id).AIProxy.Token
		resp := proxyReq(t, env, "GET", "/ai-proxy/custom/LAN1/x", "", "Authorization", "Bearer "+tok)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	if st, b := call(""); st != 502 || hits.Load() != 0 {
		t.Fatalf("LAN custom API without the opt-in: status %d, target hits %d: %s", st, hits.Load(), b)
	}
	if st, b := call("1"); st != 200 || hits.Load() != 1 {
		t.Fatalf("with the opt-in: status %d, target hits %d: %s", st, hits.Load(), b)
	}
}

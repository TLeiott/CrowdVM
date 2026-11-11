package server_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func TestTTLAutoStop(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"ttlSeconds": 1})
	r := env.WaitState(key, id, "archived") // ready lasts only ~1 s: wait for the end state
	if r.Str("sandbox.ttlSeconds") != "1" {
		t.Fatalf("ttl kept: %s", r)
	}

	// A running agent prompt moves the deadline. Start with a long TTL so
	// the prompt is running before the deadline passes, then expire it.
	id2 := createSandbox(t, env, key, map[string]any{"ttlSeconds": 600})
	env.WaitState(key, id2, "ready")
	node.SendGuestEvent(id2, "prompt.event", map[string]any{"type": "prompt", "taskId": "p1", "data": map[string]any{"status": "running"}})
	env.WaitState(key, id2, "running")
	if _, err := env.S.DB.Exec(`UPDATE sandboxes SET ttl_seconds=1, archive_after=? WHERE id=?`, time.Now().Add(-time.Second).UnixMilli(), id2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // many reaper passes (50 ms)
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id2, nil); r.Str("sandbox.state") != "running" {
		t.Fatalf("stopped while the agent works: %s", r)
	}
	node.SendGuestEvent(id2, "prompt.event", map[string]any{"type": "prompt.status", "taskId": "p1", "data": map[string]any{"status": "finished"}})
	env.WaitState(key, id2, "idle", "archiving", "archived")
	env.WaitState(key, id2, "archived")

	// No auto-stop.
	id3 := createSandbox(t, env, key, map[string]any{"ttlSeconds": nil})
	r = env.WaitState(key, id3, "ready")
	if r.Get("sandbox.archiveAfter") != nil || r.Get("sandbox.ttlSeconds") != nil {
		t.Fatalf("no auto-stop: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"ttlSeconds": 0}); r.Code() != "invalid_ttl" {
		t.Fatalf("bad ttl: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"ttlSeconds": "infinite"}); r.Status != 202 || r.Get("ttlSeconds") != nil {
		t.Fatalf("legacy infinite: %s", r)
	}
}

func TestIdempotencyAndPagination(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Capacity: proto.Capacity{VCPU: 256, MemoryMiB: 1 << 20, DiskGiB: 100000}})
	key := env.AdminKey
	body := map[string]any{"type": "small", "labels": map[string]string{"batch": "x"}}
	r1 := env.Do(key, "POST", "/api/v1/sandboxes", body, "Idempotency-Key: k1")
	r2 := env.Do(key, "POST", "/api/v1/sandboxes", body, "Idempotency-Key: k1")
	if r1.Status != 202 || r2.Status != 202 || r1.Str("sandbox.id") != r2.Str("sandbox.id") || r2.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %s / %s", r1, r2)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "large"}, "Idempotency-Key: k1"); r.Status != 409 || r.Code() != "idempotency_key_reused" {
		t.Fatalf("reused: %s", r)
	}
	// A failed create releases the key.
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"placement": map[string]any{"node": "nope"}}, "Idempotency-Key: k2"); r.Status != 503 {
		t.Fatalf("failing create: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"placement": map[string]any{}}, "Idempotency-Key: k2"); r.Status != 202 {
		t.Fatalf("key released: %s", r)
	}
	// Other users can use the same key value.
	_, otherKey := env.NewUser("other", false)
	if r := env.Do(otherKey, "POST", "/api/v1/sandboxes", body, "Idempotency-Key: k1"); r.Status != 202 || r.Str("sandbox.id") == r1.Str("sandbox.id") {
		t.Fatalf("per-account keys: %s", r)
	}
	fid := r1.Str("sandbox.id")
	env.WaitState(key, fid, "ready")
	f1 := env.Do(key, "POST", "/api/v1/sandboxes/"+fid+"/fork", nil, "Idempotency-Key: f1")
	f2 := env.Do(key, "POST", "/api/v1/sandboxes/"+fid+"/fork", nil, "Idempotency-Key: f1")
	if f1.Str("id") == "" || f1.Str("id") != f2.Str("id") {
		t.Fatalf("fork replay: %s / %s", f1, f2)
	}

	// Pagination over 5 more sandboxes.
	for i := 0; i < 5; i++ {
		createSandbox(t, env, key, map[string]any{"name": fmt.Sprintf("page-%d", i), "type": "small"})
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		r := env.Do(key, "GET", "/api/v1/sandboxes?limit=3&cursor="+cursor, nil)
		if r.Status != 200 || r.Str("pageInfo.limit") != "3" {
			t.Fatalf("page: %s", r)
		}
		for _, s := range r.Get("sandboxes").([]any) {
			id := s.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("duplicate %s", id)
			}
			seen[id] = true
		}
		pages++
		if r.Str("pageInfo.hasMore") != "true" {
			if r.Get("pageInfo.nextCursor") != nil {
				t.Fatal("nextCursor on last page")
			}
			break
		}
		cursor = r.Str("pageInfo.nextCursor")
	}
	if len(seen) != 8 || pages != 3 {
		t.Fatalf("paged %d sandboxes in %d pages", len(seen), pages)
	}
	r := env.Do(key, "GET", "/api/v1/sandboxes?namePrefix=page-&sort=asc", nil)
	list, _ := r.Get("sandboxes").([]any)
	if len(list) != 5 {
		t.Fatalf("namePrefix: %s", r)
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].(map[string]any)["createdAt"].(string) > list[i].(map[string]any)["createdAt"].(string) {
			t.Fatalf("not ascending: %s", r)
		}
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes?label=batch%3Dx", nil); len(r.Get("sandboxes").([]any)) != 1 {
		t.Fatalf("label filter: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes?state=archived", nil); len(r.Get("sandboxes").([]any)) != 0 {
		t.Fatalf("state filter: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes?limit=500", nil); r.Code() != "invalid_limit" {
		t.Fatalf("limit: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes?include=cost", nil); r.Get("sandboxes.0.cost.seconds") == nil {
		t.Fatalf("cost: %s", r)
	}
}

func TestRateLimits(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) {
		c.StartsPerMinute, c.StartsPerHour, c.MaxActive, c.IPRateLimit = 2, 10, 3, 100000
	})
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	createSandbox(t, env, key, map[string]any{"type": "small"})
	createSandbox(t, env, key, map[string]any{"type": "small"})
	r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "small"})
	if r.Status != 429 || r.Code() != "rate_limited" || r.Str("error.details.window") != "minute" || r.Str("error.details.limit") != "2" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limited: %s", r)
	}
	if r.Header.Get("RateLimit-Policy") != `"ip";q=100000;w=60` || !strings.HasPrefix(r.Header.Get("RateLimit"), `"ip";r=`) {
		t.Fatalf("RateLimit headers: %v", r.Header)
	}
	if r := env.Do(key, "GET", "/api/v1/limits", nil); r.Str("starts.minute.remaining") != "0" || r.Str("canStart") != "false" || r.Str("activeSandboxes") != "2" {
		t.Fatalf("limits: %s", r)
	}
	// Concurrency.
	env2 := servertest.New(t, func(c *server.Config) { c.MaxActive = 1 })
	env2.StartNode(servertest.NodeOptions{})
	createSandbox(t, env2, env2.AdminKey, nil)
	if r := env2.Do(env2.AdminKey, "POST", "/api/v1/sandboxes", nil); r.Status != 429 || r.Code() != "limit_reached" {
		t.Fatalf("limit_reached: %s", r)
	}
	// IP limit.
	env3 := servertest.New(t, func(c *server.Config) { c.IPRateLimit = 3 })
	for i := 0; i < 3; i++ {
		env3.Do(env3.AdminKey, "GET", "/api/v1/me", nil)
	}
	r = env3.Do(env3.AdminKey, "GET", "/api/v1/me", nil)
	if r.Status != 429 || r.Code() != "IP_RATE_LIMIT_EXCEEDED" || r.Get("retryAfter") == nil || r.Header.Get("Retry-After") == "" {
		t.Fatalf("ip limit: %s", r)
	}
}

func TestTunnel(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Tunnel: func(id string, port int, c net.Conn) {
		defer c.Close()
		fmt.Fprintf(c, "hello %s:%d\n", id, port)
		_, _ = io.Copy(c, c)
	}})
	id := createSandbox(t, env, env.AdminKey, nil)
	env.WaitState(env.AdminKey, id, "ready")

	// Websocket.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(env.URL, "http")+"/api/v1/sandboxes/"+id+"/tunnel?port=22",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + env.AdminKey}}})
	if err != nil {
		t.Fatal(err)
	}
	c := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	br := bufio.NewReader(c)
	line, _ := br.ReadString('\n')
	if line != "hello "+id+":22\n" {
		t.Fatalf("greeting %q", line)
	}
	fmt.Fprint(c, "ping\n")
	if line, _ := br.ReadString('\n'); line != "ping\n" {
		t.Fatalf("echo %q", line)
	}
	c.Close()

	// HTTP Upgrade.
	conn, err := net.Dial("tcp", strings.TrimPrefix(env.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	tc, err := mux.DialUpgrade(conn, strings.TrimPrefix(env.URL, "http://"), "/api/v1/sandboxes/"+id+"/tunnel?port=8080", http.Header{"Authorization": {"Bearer " + env.AdminKey}})
	if err != nil {
		t.Fatal(err)
	}
	br = bufio.NewReader(tc)
	if line, _ := br.ReadString('\n'); line != "hello "+id+":8080\n" {
		t.Fatalf("upgrade greeting %q", line)
	}
	tc.Close()
	// Errors before the upgrade are JSON.
	if r := env.Do(env.AdminKey, "GET", "/api/v1/sandboxes/"+id+"/tunnel?port=0", nil); r.Code() != "invalid_port" {
		t.Fatalf("bad port: %s", r)
	}
	if r := env.Do(env.AdminKey, "GET", "/api/v1/sandboxes/"+id+"/tunnel?port=22", nil); r.Code() != "upgrade_required" {
		t.Fatalf("no upgrade: %s", r)
	}
}

func TestCreditsAndUsage(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.EnforceCredits = true })
	node := env.StartNode(servertest.NodeOptions{})
	_ = node
	u, key := env.NewUser("spender", false)
	if b := env.S.Balance(u.ID); b != 100 {
		t.Fatalf("signup credits %v", b)
	}
	id := createSandbox(t, env, key, map[string]any{"type": "large"}) // multiplier 2
	env.WaitState(key, id, "ready")
	// The metered run opens right after the ready transition.
	env.Eventually("run opened", func() bool {
		var n int
		_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM sandbox_runs WHERE sandbox_id=? AND ended_at IS NULL`, id).Scan(&n)
		return n == 1
	})
	// Pretend the run started an hour ago.
	if _, err := env.S.DB.Exec(`UPDATE sandbox_runs SET started_at=started_at-3600000, billed_until=billed_until-3600000 WHERE sandbox_id=?`, id); err != nil {
		t.Fatal(err)
	}
	env.S.BillNow()
	if b := env.S.Balance(u.ID); b > 98.01 || b < 97.9 {
		t.Fatalf("after one large hour: %v", b)
	}
	// The node owner (admin) earned it.
	if b := env.S.Balance(env.Admin.ID); b < 101.9 {
		t.Fatalf("node owner earned %v", b)
	}
	r := env.Do(key, "GET", fmt.Sprintf("/api/v1/sandboxes/%s/usage?since=%d", id, time.Now().Add(-2*time.Hour).Unix()), nil)
	if r.Status != 200 || r.Str("sandboxType") != "large" || r.Str("billingMultiplier") != "2" || r.Get("seconds").(float64) < 7200 || r.Str("running") != "true" {
		t.Fatalf("usage: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/credits", nil); r.Get("wallet.balanceCredits") == nil || len(r.Get("ledger").([]any)) < 2 {
		t.Fatalf("credits: %s", r)
	}
	// Empty wallet: 402 and running sandboxes stop.
	if err := env.S.GrantCredits(u.ID, -1000, "test", ""); err != nil {
		t.Fatal(err)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", nil); r.Status != 402 || r.Code() != "billing_required" {
		t.Fatalf("billing_required: %s", r)
	}
	// 24 h grace: still running and notified; stopped once the grace ends.
	env.S.BillNow()
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.state") != "ready" {
		t.Fatalf("stopped during grace: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/billing", nil); r.Str("grace.active") != "true" {
		t.Fatalf("grace: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/notices", nil); !strings.Contains(r.String(), "grace_started") {
		t.Fatalf("grace notice: %s", r)
	}
	if _, err := env.S.DB.Exec(`UPDATE wallet_state SET grace_since=grace_since-90000000 WHERE wallet_id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	env.S.BillNow()
	env.WaitState(key, id, "archived")
	// Admin grants.
	if r := env.Do(env.AdminKey, "POST", "/api/v1/admin/users/spender/credits", map[string]any{"amount": 2000}); r.Status != 200 {
		t.Fatalf("grant: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/admin/credits", map[string]any{"user": "spender", "amount": 5}); r.Status != 403 {
		t.Fatalf("non-admin grant: %s", r)
	}
}

func TestSignedFileURLs(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	id := createSandbox(t, env, env.AdminKey, nil)
	env.WaitState(env.AdminKey, id, "ready")
	r := env.Do(env.AdminKey, "POST", "/api/v1/sandboxes/"+id+"/files/upload-url", map[string]any{"path": "data/x.bin"})
	if r.Status != 200 || r.Str("method") != "PUT" || r.Str("path") != "/home/user/data/x.bin" {
		t.Fatalf("upload-url: %s", r)
	}
	req, _ := http.NewRequest("PUT", r.Str("url"), strings.NewReader("payload"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("upload: %v %v", err, resp)
	}
	resp.Body.Close()
	r = env.Do(env.AdminKey, "POST", "/api/v1/sandboxes/"+id+"/files/download-url", map[string]any{"path": "/home/user/data/x.bin", "ttlSeconds": 60})
	resp, err = http.Get(r.Str("url"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "payload" {
		t.Fatalf("download: %q", b)
	}
	// Tampered / wrong method.
	if resp, _ := http.Get(strings.Replace(r.Str("url"), "x.bin", "y.bin", 1)); resp.StatusCode != 403 {
		t.Fatalf("tampered: %d", resp.StatusCode)
	}
	if r := env.Do(env.AdminKey, "POST", "/api/v1/sandboxes/"+id+"/files/download-url", map[string]any{"path": "/etc/passwd"}); r.Code() != "invalid_path" {
		t.Fatalf("outside home: %s", r)
	}
}

func TestMiscEndpoints(t *testing.T) {
	webFS := fstest.MapFS{"index.html": {Data: []byte("<html>dash</html>")}, "css/app.css": {Data: []byte("body{}")}}
	env := servertest.New(t, func(c *server.Config) { c.WebFS = webFS })
	key := env.AdminKey
	r := env.Do(key, "GET", "/api/v1/limits", nil)
	if r.Status != 200 || r.Str("type") != "limits.info" || r.Get("machineTypes") == nil || r.Str("billingStatus") != "active" {
		t.Fatalf("limits: %s", r)
	}
	if r.Header.Get("X-Request-Id") == "" {
		t.Fatal("no request id")
	}
	if r := env.Do(key, "GET", "/api/v1/nope", nil); r.Status != 404 || r.Code() != "not_found" || r.Str("requestId") == "" {
		t.Fatalf("unknown route: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/environments", nil); r.Status != 200 || r.Str("environments.0.name") != "base" {
		t.Fatalf("environments: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/provider-models", nil); r.Status != 200 || len(r.Get("providers").([]any)) == 0 {
		t.Fatalf("provider models: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", "{bad"); r.Code() != "invalid_json" {
		t.Fatalf("invalid json: %s", r)
	}
	for path, want := range map[string]string{"/": "dash", "/sandboxes/bx_123": "dash", "/css/app.css": "body{}"} {
		resp, err := http.Get(env.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s: %q", path, b)
		}
	}
	if resp, _ := http.Get(env.URL + "/missing.js"); resp.StatusCode != 404 {
		t.Fatalf("missing asset: %d", resp.StatusCode)
	}
}

func TestRecover(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/recover", nil); r.Status != 409 || r.Code() != "sandbox_is_reachable" {
		t.Fatalf("reachable: %s", r)
	}
	// Guest stops answering (re-sent: the boot's own ready event may arrive
	// after a heartbeat already made the sandbox ready).
	env.Eventually("degraded", func() bool {
		node.SetVMState(id, "running", false, "")
		time.Sleep(50 * time.Millisecond)
		return env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil).Str("sandbox.health") == "degraded"
	})
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/recover", nil)
	if r.Status != 200 || r.Str("status") != "recovering" {
		t.Fatalf("recover: %s", r)
	}
	r = env.WaitState(key, id, "ready")
	if r.Str("sandbox.health") != "ok" || r.Get("sandbox.lastReboot") == nil {
		t.Fatalf("after recover: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/recover", nil); r.Code() != "too_soon" {
		t.Fatalf("too soon: %s", r)
	}
}

var registerTreeOnce sync.Once

func TestFeatureReplacesStub(t *testing.T) {
	registerTreeOnce.Do(func() { // features are process-global
		server.RegisterFeature("test-tree", func(s *server.Server) error {
			// Real stubs get implemented by features; test the mechanism on a private route.
			s.Router().Stub("GET /api/v1/test-stub/{snapshotId}/tree", 501, "not_implemented", "stub")
			s.API("GET /test-stub/{id}/tree", "snapshot.read", func(c *server.Ctx) error {
				return c.OK(200, "snapshot.tree", map[string]any{"snapshotId": c.Param("id")})
			})
			return nil
		})
	})
	env := servertest.New(t)
	if r := env.Do(env.AdminKey, "GET", "/api/v1/test-stub/snap_x/tree", nil); r.Status != 200 || r.Str("snapshotId") != "snap_x" {
		t.Fatalf("feature route: %s", r)
	}
}

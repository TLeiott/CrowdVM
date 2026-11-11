package server_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Two openers of one database (rolling restart, a CLI `migrate` next to the
// server) must not make one of them fail: the "already applied" check runs
// outside the migration transaction, so both run the ALTER TABLE.
func TestBughuntR6bConcurrentMigrate(t *testing.T) {
	for i := 0; i < 20; i++ {
		path := filepath.Join(t.TempDir(), "c.db")
		a, _ := store.Open(path)
		b, _ := store.Open(path)
		errs := make(chan error, 2)
		start := make(chan struct{})
		for _, d := range []*store.DB{a, b} {
			go func() { <-start; errs <- d.MigrateAll(context.Background()) }()
		}
		close(start)
		for j := 0; j < 2; j++ {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: concurrent MigrateAll failed: %v", i, err)
			}
		}
		a.Close()
		b.Close()
	}
}

// A delivery whose webhook secret cannot be opened (secret.key replaced or
// restored from another install, corrupt row) is never counted, never marked
// failed and never shows an error: attempt() returns before touching the row,
// so it stays "pending" with attempts=0 and is re-leased every 15 s forever.
func TestBughuntR6bWebhookUnreadableSecretStuckPending(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.WebhookBackoff = []time.Duration{50 * time.Millisecond} })
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	recv := newHookReceiver(t)
	r := env.Do(key, "POST", "/api/v1/webhooks", map[string]any{"url": recv.srv.URL, "events": []string{"sandbox.ready"}})
	if r.Status != 201 {
		t.Fatal(r)
	}
	whID := r.Str("webhook.id")
	env.S.DB.Exec(`UPDATE webhooks SET secret='v1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' WHERE id=?`, whID)
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	time.Sleep(3 * time.Second)
	var status string
	var attempts int
	var lastErr *string
	if err := env.S.DB.QueryRow(`SELECT status, attempts, last_error FROM webhook_deliveries WHERE webhook_id=?`, whID).Scan(&status, &attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	if status == "pending" && attempts == 0 && lastErr == nil {
		t.Fatalf("delivery is pending with attempts=0 and no error after its attempt was dropped (receiver hits: %d)", len(recv.all()))
	}
}

// namePrefix is compared with substr(name,1,len(v)) where len(v) counts BYTES
// of the Go string but substr counts CHARACTERS: a non-ASCII prefix never
// matches (sandboxes.go:531-533).
func TestBughuntR6bNamePrefixNonASCII(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	createSandbox(t, env, key, map[string]any{"name": "élan-vital"})
	createSandbox(t, env, key, map[string]any{"name": "plain"})
	for _, prefix := range []string{"plain", "p", "%C3%A9lan", "%C3%A9"} {
		r := env.Do(key, "GET", "/api/v1/sandboxes?namePrefix="+prefix, nil)
		if r.Status != 200 || len(r.Get("sandboxes").([]any)) != 1 {
			t.Errorf("namePrefix=%s: want 1 sandbox, got %s", prefix, r)
		}
	}
}

// The delivery log accepts limit 1..500, but any other value (0, -1, abc and
// 501+) is silently replaced by 100: a LARGER limit returns FEWER rows, and
// every other list endpoint answers 400 invalid_limit (feat_webhooks.go:828).
func TestBughuntR6bWebhookDeliveriesLimit(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	recv := newHookReceiver(t)
	r := env.Do(key, "POST", "/api/v1/webhooks", map[string]any{"url": recv.srv.URL, "events": []string{"sandbox.ready"}})
	whID := r.Str("webhook.id")
	now := time.Now().UnixMilli()
	for i := 0; i < 150; i++ {
		env.S.DB.Exec(`INSERT INTO webhook_deliveries(webhook_id,event_id,event_type,payload,status,attempts,created_at,completed_at) VALUES(?,?,?,?,?,?,?,?)`,
			whID, fmt.Sprintf("evt_%03d", i), "sandbox.ready", "{}", "delivered", 1, now+int64(i), now)
	}
	n := func(q string) (int, int) {
		r := env.Do(key, "GET", "/api/v1/webhooks/"+whID+"/deliveries"+q, nil)
		ds, _ := r.Get("deliveries").([]any)
		return r.Status, len(ds)
	}
	if st, got := n("?limit=500"); st != 200 || got != 150 {
		t.Fatalf("limit=500: %d, %d rows", st, got)
	}
	st, got := n("?limit=1000")
	if st == 200 && got < 150 {
		t.Errorf("limit=1000 returned %d rows while limit=500 returns 150 (silently reset to 100)", got)
	}
	for _, q := range []string{"?limit=0", "?limit=-5", "?limit=abc"} {
		if st, _ := n(q); st == 200 {
			t.Errorf("%s: 200, every other list endpoint answers 400 invalid_limit", q)
		}
	}
}

// An unknown or deleted command-history cursor is turned into "AND 0": the
// request succeeds with an empty page and no nextCursor, so a client pages
// to the end and silently misses everything after the cursor
// (feat_history.go:455-462). Every other list answers 400 invalid_cursor.
func TestBughuntR6bCommandHistoryBadCursor(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	stopUntilArchived(t, env, key, id)
	for i := 0; i < 3; i++ {
		env.S.DB.Exec(`INSERT INTO command_history(sandbox_id,command_id,mode,command,started_at,record,updated_at) VALUES(?,?,?,?,?,?,?)`,
			id, fmt.Sprintf("cmd_%d", i), "sync", "echo", time.Now().UnixMilli()+int64(i), `{"id":"x"}`, 1)
	}
	r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/command-history?limit=1", nil)
	if r.Status != 200 || len(r.Get("commands").([]any)) != 1 {
		t.Fatalf("setup: %s", r)
	}
	r = env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/command-history?cursor=garbage", nil)
	if r.Status == 200 {
		t.Errorf("garbage cursor accepted: %s", r)
	}
}

// Stored events: limit=1000 is clamped to 200 although the error text says
// "limit must be 1..200", and sort=bogus silently means desc, where
// GET /sandboxes and /snapshots answer 400 (feat_history.go:667-673, 674).
func TestBughuntR6bEventsLimitSortValidation(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	stopUntilArchived(t, env, key, id)
	for _, q := range []string{"limit=1000", "sort=bogus"} {
		if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/events?"+q, nil); r.Status == 200 {
			t.Errorf("events?%s accepted (200); sandboxes/snapshots lists reject it with 400", q)
		}
	}
}

// /admin/sandboxes ignores limit/cursor/sort, caps the answer at 500 rows
// without a pageInfo, and does not know the "stopped" alias that
// GET /sandboxes?state=stopped maps to archived (account.go:331-346).
func TestBughuntR6bAdminSandboxesList(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	stopUntilArchived(t, env, key, id)
	a := env.Do(key, "GET", "/api/v1/sandboxes?state=stopped", nil)
	b := env.Do(key, "GET", "/api/v1/admin/sandboxes?state=stopped", nil)
	if len(a.Get("sandboxes").([]any)) != 1 || len(b.Get("sandboxes").([]any)) != 1 {
		t.Errorf("state=stopped: /sandboxes -> %d rows, /admin/sandboxes -> %d rows", len(a.Get("sandboxes").([]any)), len(b.Get("sandboxes").([]any)))
	}
}

// One account with 16 hanging endpoints occupies every delivery slot
// (whParallel) for the whole endpoint timeout, so a healthy webhook of the
// same or ANOTHER account waits behind it: the due-list is picked oldest
// first with no per-webhook or per-owner limit (feat_webhooks.go:331-362).
func TestBughuntR6bWebhookSlowEndpointBlocksOthers(t *testing.T) {
	old := server.WebhookTimeout
	server.WebhookTimeout = 4 * time.Second
	t.Cleanup(func() { server.WebhookTimeout = old })
	env := servertest.New(t)
	key := env.AdminKey
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	fast := newHookReceiver(t)

	other, otherKey := env.NewUser("r6bother", false)
	_ = other
	mk := func(k, url string) {
		if r := env.Do(k, "POST", "/api/v1/webhooks", map[string]any{"url": url, "events": []string{"sandbox.ready"}}); r.Status != 201 {
			t.Fatal(r)
		}
	}
	mk(key, slow.URL)
	create := func(owner string, n, from int) {
		for i := 0; i < n; i++ {
			env.S.DB.Exec(`INSERT INTO hook_events(id,type,owner_id,sandbox_id,sandbox_name,state,created_at) VALUES(?,?,?,?,?,?,?)`,
				fmt.Sprintf("evt_%s_%d", owner, from+i), "sandbox.ready", owner, "sb_x", "x", "ready", time.Now().UnixMilli())
		}
	}
	create(env.Admin.ID, 20, 0)
	time.Sleep(1500 * time.Millisecond) // fanned out; 16 attempts now hang
	mk(otherKey, fast.srv.URL)
	create(other.ID, 1, 100)
	start := time.Now()
	for len(fast.all()) == 0 && time.Since(start) < 12*time.Second {
		time.Sleep(50 * time.Millisecond)
	}
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("a healthy webhook of another account was delivered only after %s (all 16 slots held by hanging endpoints of one account)", d)
	}
}

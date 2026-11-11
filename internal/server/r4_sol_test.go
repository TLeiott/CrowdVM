package server_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func TestR4TelemetrySlowSandboxIsolation(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.OTelMetricsInterval = 500 * time.Millisecond })
	slow, slowKey := env.NewUser("slow", false)
	fast, fastKey := env.NewUser("fast", false)
	release := make(chan struct{})
	defer close(release)
	started := make(chan string, 16)
	guest := servertest.NewFakeGuest()
	var mu sync.Mutex
	blocked := map[string]bool{}
	env.StartNode(servertest.NodeOptions{Guest: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/files" {
			id := r.Header.Get("X-Sandbox-Id")
			mu.Lock()
			block := blocked[id]
			mu.Unlock()
			if block {
				select {
				case started <- id:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
				}
				return
			}
		}
		guest.ServeHTTP(w, r)
	})})
	for range 2 {
		id := createSandbox(t, env, slowKey, nil)
		env.WaitState(slowKey, id, "ready")
		mu.Lock()
		blocked[id] = true
		mu.Unlock()
	}
	fastID := createSandbox(t, env, fastKey, nil)
	env.WaitState(fastKey, fastID, "ready")
	col := newFakeCollector(t)
	for _, key := range []string{slowKey, fastKey} {
		if r := env.Do(key, "PUT", "/api/v1/telemetry", map[string]any{"endpoint": col.srv.URL}); r.Status != 200 {
			t.Fatal(r)
		}
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("sampling did not start")
	}
	// Both slow sandboxes must be sampled concurrently.
	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Error("sandbox sampling serialized")
	}
	before := time.Now()
	beforeLogs := len(col.byPath("/v1/logs"))
	if r := env.Do(fastKey, "POST", "/api/v1/sandboxes/"+fastID+"/stop", nil); r.Status != 202 && r.Status != 200 {
		t.Fatal(r)
	}
	deadline := time.Now().Add(600 * time.Millisecond)
	found := false
	for time.Now().Before(deadline) {
		for _, req := range col.byPath("/v1/logs")[beforeLogs:] {
			if strings.Contains(req.Raw, fastID) && strings.Contains(req.Raw, "sandbox.archived") {
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !found {
		t.Error("logs waited behind metric sampling")
	}
	env.Eventually("healthy account metrics during blocked sampling", func() bool {
		for _, req := range col.byPath("/v1/metrics") {
			if strings.Contains(req.Raw, fast.ID) && strings.Contains(req.Raw, "boat.account.sandboxes.active") {
				return true
			}
		}
		return false
	})
	if time.Since(before) > 900*time.Millisecond {
		t.Error("healthy account waited for slow account")
	}
	env.Eventually("slow account bounded sampling", func() bool {
		for _, req := range col.byPath("/v1/metrics") {
			if strings.Contains(req.Raw, slow.ID) {
				return true
			}
		}
		return false
	})
	if time.Since(before) > 1800*time.Millisecond {
		t.Error("sandbox sampling exceeded its timeout")
	}
}

func TestR4ServiceAccountSandboxDeletion(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	_, ownerKey := env.NewUser("owner", false)
	env.Do(ownerKey, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"})
	orgAdmin, orgAdminKey := env.NewUser("orgadmin", false)
	member, memberKey := env.NewUser("member", false)
	var orgID string
	if err := env.S.DB.QueryRow(`SELECT id FROM orgs WHERE name='Acme'`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct{ id, role string }{{orgAdmin.ID, "admin"}, {member.ID, "member"}} {
		if _, err := env.S.DB.Exec(`INSERT INTO org_members(org_id,user_id,role,joined_at) VALUES(?,?,?,0)`, orgID, u.id, u.role); err != nil {
			t.Fatal(err)
		}
	}

	for _, actor := range []struct{ name, key string }{{"owner", ownerKey}, {"orgadmin", orgAdminKey}, {"serveradmin", env.AdminKey}} {
		r := env.Do(ownerKey, "POST", "/api/v1/orgs/acme/service-accounts", map[string]any{"name": "bot" + actor.name, "preset": "admin", "ttl": "1h"})
		if r.Status != 201 {
			t.Fatal(r)
		}
		botID, botKey := r.Str("user.id"), r.Str("secret")
		id := createSandbox(t, env, botKey, nil)
		env.WaitState(botKey, id, "ready")
		if r := env.Do(ownerKey, "DELETE", "/api/v1/orgs/acme/service-accounts/"+botID, nil); r.Status != 409 {
			t.Errorf("delete with sandbox: %s", r)
		}
		// Simulate an orphan left by an older server.
		if _, err := env.S.DB.Exec(`DELETE FROM org_members WHERE user_id=?`, botID); err != nil {
			t.Fatal(err)
		}
		if _, err := env.S.DB.Exec(`UPDATE users SET disabled=1 WHERE id=?`, botID); err != nil {
			t.Fatal(err)
		}
		denied := env.Do(memberKey, "DELETE", "/api/v1/sandboxes/"+id, nil, "X-CrowdVM-Confirm-Delete: "+id)
		if denied.Status != 403 {
			t.Errorf("member may delete orphan: %s", denied)
		}
		r = env.Do(actor.key, "DELETE", "/api/v1/sandboxes/"+id, nil, "X-CrowdVM-Confirm-Delete: "+id)
		if r.Status != 202 && r.Status != 200 {
			t.Errorf("%s delete orphan: %s", actor.name, r)
		}
	}
}

func TestR4SecretFileHomeDirectoriesAndControls(t *testing.T) {
	env := servertest.New(t)
	for _, p := range []string{".ssh", "Desktop", "~/Desktop", "/home/user/.ssh", ".config", ".local", ".cache", ".cargo", ".kimi-code", ".hermes", ".openclaw", ".vibe", "Downloads", "Documents", "Pictures", "Templates", "Music", "Videos", "Public", "a\tb", "a\bb", "a\x7fb", "a\u0085b", "\tfoo"} {
		t.Run(p, func(t *testing.T) {
			r := env.Do(env.AdminKey, "PUT", "/api/v1/environments/base", map[string]any{"secretFiles": []map[string]string{{"path": p, "contents": "secret"}}})
			if r.Status != 400 {
				t.Errorf("path %q accepted: %s", p, r)
			}
			r = env.Do(env.AdminKey, "PUT", "/api/v1/environments/base/secret-files", map[string]string{"path": p, "contents": "secret"})
			if r.Status != 400 {
				t.Errorf("set file path %q accepted: %s", p, r)
			}
		})
	}
	for _, p := range []string{".ssh/config", "Desktop/secret", ".config/app/secret"} {
		r := env.Do(env.AdminKey, "PUT", "/api/v1/environments/base", map[string]any{"secretFiles": []map[string]string{{"path": p, "contents": "secret"}}})
		if r.Status != 200 {
			t.Errorf("child path %q refused: %s", p, r)
		}
	}
}

func TestR4SandboxErrorNoticeClears(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	for _, deleted := range []bool{false, true} {
		id := createSandbox(t, env, env.AdminKey, nil)
		env.WaitState(env.AdminKey, id, "ready")
		env.S.DB.Exec(`UPDATE sandboxes SET state='error',error='node_offline' WHERE id=?`, id)
		env.S.Bus.Publish(server.Event{Type: server.EvSandboxState, SandboxID: id, PreviousState: "ready", State: "error"})
		count := func() int {
			var n int
			env.S.DB.QueryRow(`SELECT COUNT(*) FROM notices WHERE sandbox_id=? AND kind='sandbox_error'`, id).Scan(&n)
			return n
		}
		env.Eventually("error notice created", func() bool { return count() == 1 })
		event := server.Event{Type: server.EvSandboxState, SandboxID: id, PreviousState: "error", State: "ready"}
		if deleted {
			env.S.DB.Exec(`UPDATE sandboxes SET deleted_at=1 WHERE id=?`, id)
			event.Type = server.EvSandboxDeleted
		} else {
			env.S.DB.Exec(`UPDATE sandboxes SET state='ready' WHERE id=?`, id)
		}
		env.S.Bus.Publish(event)
		env.Eventually("error notice cleared", func() bool { return count() == 0 })
	}
}

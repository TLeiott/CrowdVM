package server_test

import (
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func TestZeroDataRetention(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	u, key := env.NewUser("zed", false)
	sess := env.Session(u)

	r := env.Do(key, "GET", "/api/v1/account/data-retention", nil)
	if r.Status != 200 || r.Str("type") != "data_retention.info" || r.Get("enabled") != false || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("get: %s %v", r, r.Header)
	}
	// API keys cannot change it; the phrase is required.
	if r := env.Do(key, "PATCH", "/api/v1/account/data-retention", map[string]any{"enabled": true, "confirmation": "delete archived sandbox data"}); r.Status != 403 || r.Code() != "session_required" {
		t.Fatalf("key patch: %s", r)
	}
	if r := env.Do(sess, "PATCH", "/api/v1/account/data-retention", map[string]any{"enabled": true}); r.Status != 400 || r.Code() != "confirmation_required" {
		t.Fatalf("no phrase: %s", r)
	}

	// One archived sandbox (queued on enable), one running (deleted on stop), one saved snapshot.
	archived := createSandbox(t, env, key, nil)
	env.WaitState(key, archived, "ready")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": archived, "name": "keep"}); r.Status/100 != 2 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/keep", "snapshot.status", 5*time.Second, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+archived+"/stop", nil)
	env.WaitState(key, archived, "archived")
	running := createSandbox(t, env, key, nil)
	env.WaitState(key, running, "ready")

	r = env.Do(sess, "PATCH", "/api/v1/account/data-retention", map[string]any{"enabled": true, "confirmation": "delete archived sandbox data"})
	if r.Status != 200 || r.Str("type") != "data_retention.updated" || r.Get("enabled") != true || r.Get("queuedSandboxes").(float64) != 1 ||
		r.Get("acceptedDeletionOperationsIrreversible") != true || r.Get("enabledAt") == nil {
		t.Fatalf("enable: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+archived, nil); r.Status != 404 {
		t.Fatalf("archived sandbox still there: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/saved-snapshots/keep", nil); r.Status != 404 {
		t.Fatalf("saved snapshot still there: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": running, "name": "nope"}); r.Status != 409 {
		t.Fatalf("save under zdr: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/me", nil); r.Get("user.zeroDataRetention") != true || r.Get("user.zeroDataRetentionEnabledAt") == nil {
		t.Fatalf("me: %s", r)
	}
	// A stop deletes.
	env.Do(key, "POST", "/api/v1/sandboxes/"+running+"/stop", nil)
	env.Eventually("stopped sandbox deleted", func() bool {
		return env.Do(key, "GET", "/api/v1/sandboxes/"+running, nil).Status == 404
	})
	// Disable again.
	if r := env.Do(sess, "PATCH", "/api/v1/account/data-retention", map[string]any{"enabled": false}); r.Status != 200 || r.Get("enabled") != false {
		t.Fatalf("disable: %s", r)
	}
}

func TestSnapshotsAndMonitoringOff(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	u, key := env.NewUser("priv", false)
	sess := env.Session(u)

	// Per sandbox: monitoring off -> history 409, live commands still work.
	id := createSandbox(t, env, key, map[string]any{"monitoring": false})
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/command-history", nil); r.Status != 409 || r.Code() != "monitoring_off" {
		t.Fatalf("history: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/processes", nil); r.Status != 409 {
		t.Fatalf("processes: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "true"}); r.Status != 200 {
		t.Fatalf("command: %s", r)
	}

	// Account-wide.
	r := env.Do(sess, "PATCH", "/api/v1/account/data-retention", map[string]any{"snapshotsOff": true, "monitoringOff": true})
	if r.Status != 200 || r.Get("snapshotsOff") != true || r.Get("monitoringOff") != true || r.Get("snapshotsOffAt") == nil {
		t.Fatalf("patch: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"snapshots": true}); r.Status != 409 || r.Code() != "snapshots_off_for_account" {
		t.Fatalf("snapshots true: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"monitoring": true}); r.Status != 409 || r.Code() != "monitoring_off_for_account" {
		t.Fatalf("monitoring true: %s", r)
	}
	id2 := createSandbox(t, env, key, nil)
	r = env.WaitState(key, id2, "ready")
	if r.Get("sandbox.snapshots") != false || r.Get("sandbox.monitoring") != false {
		t.Fatalf("defaults: %s", r)
	}
}

func TestCloseAccount(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	u, key := env.NewUser("leaver", false)
	cli := env.Session(u)
	browser, err := env.S.NewSession(u.ID, "browser", "test")
	if err != nil {
		t.Fatal(err)
	}
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")

	if r := env.Do(cli, "POST", "/api/v1/account/close", map[string]any{"confirmation": "close my account"}); r.Status != 403 {
		t.Fatalf("cli close: %s", r)
	}
	if r := env.Do(browser, "POST", "/api/v1/account/close", map[string]any{}); r.Status != 400 {
		t.Fatalf("no phrase: %s", r)
	}
	r := env.Do(browser, "POST", "/api/v1/account/close", map[string]any{"confirmation": "close my account"})
	if r.Status != 200 || r.Get("purgeAt") == nil {
		t.Fatalf("close: %s", r)
	}
	env.WaitState(browser, id, "archived") // stopped with a save
	if r := env.Do(key, "GET", "/api/v1/me", nil); r.Status != 401 {
		t.Fatalf("key after close: %s", r)
	}
	if r := env.Do(cli, "GET", "/api/v1/me", nil); r.Status != 401 {
		t.Fatalf("other session after close: %s", r)
	}
	if r := env.Do(browser, "GET", "/api/v1/me", nil); r.Get("user.closed") != true || r.Get("user.purgeAt") == nil {
		t.Fatalf("me: %s", r)
	}
	if r := env.Do(browser, "POST", "/api/v1/sandboxes", map[string]any{}); r.Status != 403 || r.Code() != "account_closed" {
		t.Fatalf("create while closed: %s", r)
	}
	if r := env.Do(browser, "POST", "/api/v1/account/reopen", nil); r.Status != 200 {
		t.Fatalf("reopen: %s", r)
	}
	if r := env.Do(browser, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status/100 != 2 {
		t.Fatalf("resume after reopen: %s", r)
	}

	// Erasure.
	r = env.Do(browser, "POST", "/api/v1/account/erase", map[string]any{"confirmation": "erase my account"})
	if r.Status != 200 || r.Get("deletedSandboxes").(float64) != 1 {
		t.Fatalf("erase: %s", r)
	}
	if r := env.Do(browser, "GET", "/api/v1/me", nil); r.Status != 401 {
		t.Fatalf("after erase: %s", r)
	}
	if got, err := env.S.UserByID(u.ID); err != nil || got.Username == "leaver" || got.Email != "" || !got.Disabled {
		t.Fatalf("identity kept: %+v %v", got, err)
	}
}

func TestRetentionAdmin(t *testing.T) {
	env := servertest.New(t)
	r := env.Do(env.AdminKey, "PUT", "/api/v1/admin/retention", map[string]any{"mode": "enforce", "stoppedDays": 7, "runNow": true})
	if r.Status != 200 || r.Str("retention.mode") != "enforce" || r.Get("retention.stoppedDays").(float64) != 7 || r.Get("retention.lastRunAt") == nil {
		t.Fatalf("put: %s", r)
	}
	if r := env.Do(env.AdminKey, "PUT", "/api/v1/admin/retention", map[string]any{"mode": "bogus"}); r.Status != 400 {
		t.Fatalf("bad mode: %s", r)
	}
}

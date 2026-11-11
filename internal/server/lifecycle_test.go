package server_test

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func createSandbox(t *testing.T, env *servertest.Env, key string, body map[string]any) string {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	r := env.Do(key, "POST", "/api/v1/sandboxes", body)
	if r.Status != 202 {
		t.Fatalf("create: %s", r)
	}
	if r.Str("type") != "sandbox.created" || r.Str("status") != "provisioning" {
		t.Fatalf("create envelope: %s", r)
	}
	return r.Str("sandbox.id")
}

func TestSandboxLifecycle(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey

	id := createSandbox(t, env, key, map[string]any{"type": "small", "name": "life", "labels": map[string]string{"job": "42"},
		"env": map[string]string{"FOO": "bar"}, "files": []map[string]string{{"path": "a.txt", "content": "hi"}}, "setupScript": "echo hi"})
	r := env.WaitState(key, id, "ready")
	if r.Str("sandbox.type") != "small" || r.Str("sandbox.nodeId") != node.ID || r.Str("sandbox.labels.job") != "42" {
		t.Fatalf("sandbox: %s", r)
	}
	if r.Str("sandbox.setupStatus") != "pending" {
		t.Fatalf("setupStatus: %s", r.Str("sandbox.setupStatus"))
	}
	spec, _ := node.Spec(id)
	if spec.Size.VCPU != 2 || spec.Guest.Env["FOO"] != "bar" || spec.Guest.Env["CROWDVM_SANDBOX_ID"] != id || spec.Guest.Env["CROWDVM_API_KEY"] == "" {
		t.Fatalf("spec: %+v", spec)
	}
	if !spec.Guest.FirstBoot || spec.Guest.Boot != proto.BootCreate || spec.Guest.SetupScript != "echo hi" || len(spec.Guest.Files) != 1 || spec.Guest.Files[0].Path != "/home/user/a.txt" {
		t.Fatalf("guest config: %+v", spec.Guest)
	}

	// Setup status from guest events.
	node.SendGuestEvent(id, "setup.status", map[string]any{"status": "done"})
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.setupStatus", 5*time.Second, "done")

	// Pass-through.
	r = env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "echo hi"})
	if r.Status != 200 || r.Str("stdout") != "echo hi\n" || r.Str("sandbox") != id {
		t.Fatalf("command: %s", r)
	}
	r = env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/processes", nil)
	if r.Status != 200 || r.Str("auth") != "" {
		t.Fatalf("processes (Authorization must not reach the guest): %s", r)
	}

	// The in-sandbox key works on its own sandbox only.
	sbKey := spec.Guest.Env["CROWDVM_API_KEY"]
	if r := env.Do(sbKey, "GET", "/api/v1/sandboxes/"+id, nil); r.Status != 200 {
		t.Fatalf("in-sandbox key read: %s", r)
	}
	if r := env.Do(sbKey, "POST", "/api/v1/sandboxes", map[string]any{}); r.Status != 403 || r.Code() != "api_key_action_forbidden" {
		t.Fatalf("in-sandbox key create: %s", r)
	}

	// Stop.
	r = env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	if r.Status != 202 || r.Str("type") != "sandbox.stopping" {
		t.Fatalf("stop: %s", r)
	}
	stopID := r.Str("stop.id")
	r2 := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	if r2.Str("stop.id") != stopID {
		t.Fatalf("second stop should return the same id: %s vs %s", r2.Str("stop.id"), stopID)
	}
	r = env.WaitState(key, id, "archived")
	if r.Str("sandbox.stoppedAt") == "" || r.Str("sandbox.stop.status") != "completed" {
		t.Fatalf("archived: %s", r)
	}
	// snapshotAvailable: the stop layer is stored on the server (its upload
	// runs after the stop, so it may lag the archived state a moment).
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.snapshotAvailable", 10*time.Second, "true")
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/stops/"+stopID, nil); r.Str("stop.status") != "completed" || r.Str("type") != "stop.status" {
		t.Fatalf("get stop: %s", r)
	}
	// Guest calls on a stopped sandbox.
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "x"}); r.Status != 409 || r.Code() != "machine_not_running" {
		t.Fatalf("command while stopped: %s", r)
	}
	// Boat: a steer on a stopped sandbox is no_running_turn / not_running.
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/steer", map[string]any{"prompt": "x"}); r.Status != 409 || r.Code() != "no_running_turn" || r.Str("error.details.reason") != "not_running" {
		t.Fatalf("steer stopped: %s", r)
	}
	if r := env.Do("", "GET", "/healthz", nil); r.Status != 200 || r.Get("ok") != true {
		t.Fatalf("healthz: %s", r)
	}
	head := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/snapshots/latest", nil).Str("snapshot.id")
	if head == "" {
		t.Fatal("no latest snapshot")
	}
	env.Eventually("snapshot uploaded", func() bool {
		return env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/snapshots/latest", nil).Str("snapshot.uploaded") == "true"
	})

	// Resume with a new size.
	r = env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"type": "large", "ttlSeconds": nil})
	if r.Status != 202 || r.Str("type") != "sandbox.resuming" {
		t.Fatalf("resume: %s", r)
	}
	r = env.WaitState(key, id, "ready")
	if r.Str("sandbox.type") != "large" || r.Get("sandbox.ttlSeconds") != nil || r.Get("sandbox.archiveAfter") != nil {
		t.Fatalf("after resume: %s", r)
	}
	spec, _ = node.Spec(id)
	if spec.ParentID != head || spec.Guest.Boot != proto.BootResume || spec.Guest.FirstBoot || spec.Guest.SetupScript != "" {
		t.Fatalf("resume spec: parent=%s boot=%s first=%v", spec.ParentID, spec.Guest.Boot, spec.Guest.FirstBoot)
	}
	// Resume of a running sandbox with a new size.
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"type": "small"}); r.Status != 409 || r.Code() != "sandbox_running_stop_first" {
		t.Fatalf("resume running: %s", r)
	}

	// Fork a running sandbox: live snapshot on the same node.
	r = env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/fork", map[string]any{"name": "copy"})
	if r.Status != 202 || r.Str("status") != "forking" {
		t.Fatalf("fork: %s", r)
	}
	fid := r.Str("id")
	r = env.WaitState(key, fid, "ready")
	if r.Str("sandbox.nodeId") != node.ID || r.Str("sandbox.ttlSeconds") != "3600" || r.Str("sandbox.type") != "large" {
		t.Fatalf("fork: %s", r)
	}
	// fork --no-snapshots (Boat cli-reference: fork accepts --no-snapshots).
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/fork", map[string]any{"snapshots": false}); r.Status != 202 ||
		env.Do(key, "GET", "/api/v1/sandboxes/"+r.Str("id"), nil).Str("sandbox.snapshots") != "false" {
		t.Fatalf("fork without snapshots: %s", r)
	} else {
		env.Do(key, "DELETE", "/api/v1/sandboxes/"+r.Str("id"), nil, "X-Ascii-Confirm-Delete: "+r.Str("id"))
	}
	fspec, _ := node.Spec(fid)
	if fspec.Guest.Boot != proto.BootFork || fspec.ParentID == "" || fspec.ParentID == head {
		t.Fatalf("fork spec: %+v", fspec)
	}
	if !node.Called("POST /v1/vms/" + id + "/snapshot") {
		t.Fatal("fork of a running sandbox must take a live snapshot")
	}

	// Patch.
	r = env.Do(key, "PATCH", "/api/v1/sandboxes/"+fid, map[string]any{"name": "renamed", "ttlSeconds": 120})
	if r.Status != 200 || r.Str("sandbox.name") != "renamed" || r.Str("sandbox.ttlSeconds") != "120" || r.Str("sandbox.archiveAfter") == "" {
		t.Fatalf("patch: %s", r)
	}
	if r := env.Do(key, "PATCH", "/api/v1/sandboxes/"+fid, map[string]any{}); r.Code() != "no_changes" {
		t.Fatalf("empty patch: %s", r)
	}
	if r := env.Do(key, "PATCH", "/api/v1/sandboxes/"+fid, map[string]any{"name": " "}); r.Code() != "invalid_name" {
		t.Fatalf("bad name: %s", r)
	}

	// Delete needs the confirmation header.
	if r := env.Do(key, "DELETE", "/api/v1/sandboxes/"+fid, nil); r.Status != 409 {
		t.Fatalf("delete without confirm: %s", r)
	}
	r = env.Do(key, "DELETE", "/api/v1/sandboxes/"+fid, nil, "X-Ascii-Confirm-Delete: "+fid)
	if r.Status != 202 || r.Str("operation.kind") != "sandbox" || !strings.HasPrefix(r.Str("operation.id"), "bdop_") {
		t.Fatalf("delete: %s", r)
	}
	op := r.Str("operation.id")
	env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.status", 5*time.Second, "completed")
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+fid, nil); r.Status != 404 {
		t.Fatalf("deleted sandbox: %s", r)
	}
	env.Eventually("fork VM destroyed", func() bool { _, ok := node.VM(fid); return !ok })
}

func TestStreamingPassThrough(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	id := createSandbox(t, env, env.AdminKey, nil)
	env.WaitState(env.AdminKey, id, "ready")

	req, _ := http.NewRequest("POST", env.URL+"/api/v1/sandboxes/"+id+"/commands", strings.NewReader(`{"command":"a b c d e","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+env.AdminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(resp.Body)
	start := time.Now()
	var firstAt time.Duration
	lines := 0
	for sc.Scan() {
		if lines == 0 {
			firstAt = time.Since(start)
		}
		lines++
	}
	total := time.Since(start)
	if lines != 6 {
		t.Fatalf("got %d lines", lines)
	}
	// The first frame must arrive well before the stream ends (flushed per hop).
	if firstAt > total/2 {
		t.Fatalf("not streamed: first frame after %v of %v", firstAt, total)
	}
}

func TestWaitWhileStarting(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{BootDelay: 400 * time.Millisecond})
	id := createSandbox(t, env, env.AdminKey, nil)
	start := time.Now()
	r := env.Do(env.AdminKey, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "x"})
	if r.Status != 200 {
		t.Fatalf("command while starting: %s", r)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("command did not wait for the boot")
	}

	env2 := servertest.New(t, func(c *server.Config) { c.StartWait = 100 * time.Millisecond })
	env2.StartNode(servertest.NodeOptions{BootDelay: 2 * time.Second})
	id2 := createSandbox(t, env2, env2.AdminKey, nil)
	r = env2.Do(env2.AdminKey, "POST", "/api/v1/sandboxes/"+id2+"/commands", map[string]any{"command": "x"})
	if r.Status != 409 || r.Code() != "sandbox_starting" || r.Get("error.details.retryable") != true {
		t.Fatalf("expected sandbox_starting: %s", r)
	}
}

func TestBootFailure(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{BootError: "qemu exploded"})
	id := createSandbox(t, env, env.AdminKey, nil)
	r := env.WaitState(env.AdminKey, id, "error")
	if r.Str("sandbox.error") != "qemu exploded" {
		t.Fatalf("error: %s", r)
	}
	if r := env.Do(env.AdminKey, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "x"}); r.Code() != "sandbox_not_ready" {
		t.Fatalf("command on error: %s", r)
	}
}

package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func TestSnapshotsSavedAndShares(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, map[string]any{"type": "small", "env": map[string]string{"SECRET": "s"}})
	env.WaitState(key, id, "ready")

	// A node-initiated periodic snapshot is recorded and becomes the head.
	snapID, err := a.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	env.WaitFor(key, "/api/v1/sandboxes/"+id+"/snapshots/latest", "snapshot.id", 5*time.Second, snapID)
	r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/snapshots", nil)
	if r.Status != 200 || r.Str("snapshots.0.status") != "completed" || r.Str("snapshots.0.kind") != "base" || r.Str("pageInfo.hasMore") != "false" {
		t.Fatalf("list: %s", r)
	}

	// Save a running sandbox: live snapshot, then ready once uploaded.
	r = env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "web-stack"})
	if r.Status != 202 || r.Str("status") != "saving" || r.Str("type") != "snapshot.saved.saving" {
		t.Fatalf("save: %s", r)
	}
	r = env.WaitFor(key, "/api/v1/saved-snapshots/web-stack", "snapshot.status", 5*time.Second, "ready")
	savedSnap := r.Str("snapshot.snapshotId")
	if savedSnap == "" || savedSnap == snapID || r.Str("snapshot.type") != "small" {
		t.Fatalf("saved: %s", r)
	}
	// A saved snapshot downloads by its snapshotId (feat_snapfiles.go).
	if r := env.Do(key, "GET", "/api/v1/snapshots/"+savedSnap+"/download", nil); r.Status != 200 || r.Str("snapshotId") != savedSnap || r.Str("chunks.1.snapshotId") != savedSnap {
		t.Fatalf("saved snapshot download: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "Bad_Name"}); r.Code() != "invalid_name" {
		t.Fatalf("bad name: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "latest"}); r.Code() != "invalid_name" {
		t.Fatalf("reserved name: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/saved-snapshots", nil); len(r.Get("snapshots").([]any)) != 1 {
		t.Fatalf("saved list: %s", r)
	}

	// Create from the saved snapshot on another node (a is disabled): the chain is pulled.
	b := env.StartNode(servertest.NodeOptions{Name: "b"})
	env.Do(key, "PATCH", "/api/v1/nodes/"+a.ID, map[string]any{"disabled": true})
	r = env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"from": "web-stack"})
	if r.Status != 202 || r.Str("sandbox.name") != "web-stack" || r.Str("sandbox.type") != "small" || r.Str("sandbox.nodeId") != b.ID {
		t.Fatalf("from saved: %s", r)
	}
	fromID := r.Str("sandbox.id")
	env.WaitState(key, fromID, "ready")
	spec, _ := b.Spec(fromID)
	if spec.ParentID != savedSnap || spec.Guest.Boot != proto.BootCreate || !spec.Guest.FirstBoot || spec.Guest.Env["SECRET"] != "s" {
		t.Fatalf("from-saved spec: parent=%s boot=%s env=%v", spec.ParentID, spec.Guest.Boot, spec.Guest.Env)
	}
	if !b.HasSnapshot(snapID) || !b.HasSnapshot(savedSnap) {
		t.Fatal("chain not on b")
	}
	env.Do(key, "PATCH", "/api/v1/nodes/"+a.ID, map[string]any{"disabled": false})

	// Share codes: another account deploys it without the owner's env.
	r = env.Do(key, "POST", "/api/v1/saved-snapshots/web-stack/shares", nil)
	code, shareID := r.Str("shareCode"), r.Str("share.id")
	if r.Status != 201 || code[:4] != "bsh_" {
		t.Fatalf("share: %s", r)
	}
	_, otherKey := env.NewUser("other", false)
	r = env.Do(otherKey, "POST", "/api/v1/sandboxes", map[string]any{"from": code})
	if r.Status != 202 {
		t.Fatalf("from share code: %s", r)
	}
	oid := r.Str("sandbox.id")
	env.WaitState(otherKey, oid, "ready")
	var ospec proto.VMSpec
	for _, n := range []*servertest.FakeNode{a, b} {
		if s, ok := n.Spec(oid); ok {
			ospec = s
		}
	}
	if ospec.Guest.Env["SECRET"] != "" {
		t.Fatal("share code leaked the owner's env")
	}
	if r := env.Do(key, "GET", "/api/v1/saved-snapshots/web-stack/shares", nil); r.Str("shares.0.useCount") != "1" {
		t.Fatalf("share uses: %s", r)
	}
	if r := env.Do(key, "DELETE", "/api/v1/saved-snapshots/web-stack/shares/"+shareID, nil); r.Str("share.status") != "revoked" {
		t.Fatalf("revoke: %s", r)
	}
	if r := env.Do(otherKey, "POST", "/api/v1/sandboxes", map[string]any{"from": code}); r.Status != 404 || r.Code() != "snapshot_not_found" {
		t.Fatalf("revoked code: %s", r)
	}
	if r := env.Do(otherKey, "POST", "/api/v1/sandboxes", map[string]any{"from": "web-stack"}); r.Code() != "snapshot_not_found" {
		t.Fatalf("other account uses name: %s", r)
	}

	// Snapshot deletion: confirm header, hidden at once, head moves back.
	if r := env.Do(key, "DELETE", "/api/v1/snapshots/"+snapID, nil); r.Status != 409 {
		t.Fatalf("delete without confirm: %s", r)
	}
	r = env.Do(key, "DELETE", "/api/v1/snapshots/"+snapID, nil, "X-Ascii-Confirm-Delete: "+snapID)
	if r.Status != 202 || r.Str("operation.kind") != "snapshot" {
		t.Fatalf("delete snapshot: %s", r)
	}
	snapOp := r.Str("operation.id")
	for _, s := range env.Do(key, "GET", "/api/v1/snapshots", nil).Get("snapshots").([]any) {
		if s.(map[string]any)["id"] == snapID {
			t.Fatal("deleted snapshot still listed")
		}
	}
	// Its data stays while a descendant needs it.
	if !b.HasSnapshot(snapID) {
		t.Fatal("ancestor purged while still needed")
	}
	// Saved snapshot removal.
	if r := env.Do(key, "DELETE", "/api/v1/saved-snapshots/web-stack", nil); r.Status != 200 || r.Str("status") != "deleted" {
		t.Fatalf("delete saved: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/saved-snapshots/web-stack", nil); r.Status != 404 {
		t.Fatalf("saved after delete: %s", r)
	}
	// All snapshots of a sandbox.
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	r = env.Do(key, "DELETE", "/api/v1/sandboxes/"+id+"/snapshots", nil, "X-Ascii-Confirm-Delete: "+id)
	if r.Status != 202 || len(r.Get("operations").([]any)) == 0 {
		t.Fatalf("delete all: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/snapshots/latest", nil); r.Get("snapshot") != nil {
		t.Fatalf("latest after delete all: %s", r)
	}
	// Snapshot deletion operations complete once the data is purged.
	env.WaitFor(key, "/api/v1/deletion-operations/"+snapOp, "operation.status", 10*time.Second, "completed")
	// Deleted (and no longer saved): its files are gone too.
	if r := env.Do(key, "GET", "/api/v1/snapshots/"+savedSnap+"/download", nil); r.Status != 404 {
		t.Fatalf("deleted snapshot download: %s", r)
	}
}

func TestForkStoppedOnOtherNode(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	b := env.StartNode(servertest.NodeOptions{Name: "b"})
	env.Do(key, "PATCH", "/api/v1/nodes/"+a.ID, map[string]any{"disabled": true})
	snapCalls := func() int { // the stop's pre-flight save is one
		n := 0
		for _, c := range a.Calls() {
			if strings.HasPrefix(c, "POST /v1/vms/"+id+"/snapshot") {
				n++
			}
		}
		return n
	}
	before := snapCalls()
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/fork", map[string]any{"ttlSeconds": nil, "type": "small", "labels": map[string]string{"k": "v"}})
	if r.Status != 202 || r.Str("sandbox.nodeId") != b.ID || r.Get("sandbox.ttlSeconds") != nil {
		t.Fatalf("fork: %s", r)
	}
	fid := r.Str("id")
	r = env.WaitState(key, fid, "ready")
	if r.Str("sandbox.labels.k") != "v" || r.Str("sandbox.type") != "small" {
		t.Fatalf("fork state: %s", r)
	}
	if snapCalls() != before {
		t.Fatal("stopped fork must not take a live snapshot")
	}
}

// A sandbox resumed on a node that lacked its chain (lazy pull) and then
// deleted: its layers are erased on that node too, not only where it was
// made (opus-16 B1).
func TestDeleteErasesPulledChain(t *testing.T) {
	for _, lazy := range []bool{false, true} {
		env := servertest.New(t)
		key := env.AdminKey
		a := env.StartNode(servertest.NodeOptions{Name: "a"})
		id := createSandbox(t, env, key, nil)
		env.WaitState(key, id, "ready")
		env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
		env.WaitState(key, id, "archived")
		head := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/snapshots/latest", nil).Str("snapshot.id")
		b := env.StartNode(servertest.NodeOptions{Name: "b", LazyChain: lazy})
		env.Do(key, "PATCH", "/api/v1/nodes/"+a.ID, map[string]any{"disabled": true})
		env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
		env.WaitState(key, id, "ready")
		env.Do(key, "DELETE", "/api/v1/sandboxes/"+id, nil, "X-Ascii-Confirm-Delete: "+id)
		env.Eventually("pulled chain erased on b", func() bool {
			env.S.GCNow()
			for _, c := range b.Calls() {
				if strings.HasPrefix(c, "DELETE /v1/snapshots/"+head) {
					return true
				}
			}
			return false
		})
	}
}

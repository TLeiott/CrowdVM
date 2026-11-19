package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// A capacity refusal leaves a resumed disk stopped and eligible for cache eviction.
func TestResumeCapacityRefusalStaysStopped(t *testing.T) {
	for _, failFast := range []bool{true, false} {
		name := "wait_timeout"
		wantError := "out_of_capacity:"
		if failFast {
			name, wantError = "fail_fast", "no_ready_machine:"
		}
		t.Run(name, func(t *testing.T) {
			env := servertest.New(t, func(c *server.Config) { c.CapacityWait = 300 * time.Millisecond })
			node := env.StartNode(servertest.NodeOptions{Capacity: proto.Capacity{VCPU: 16, MemoryMiB: 65536, DiskGiB: 1000, MaxSandboxes: 1}})
			key := env.AdminKey
			id := createSandbox(t, env, key, nil)
			env.WaitState(key, id, "ready")
			base := "/api/v1/sandboxes/" + id
			if r := env.Do(key, "POST", base+"/stop", nil); r.Status != 202 {
				t.Fatalf("stop: %s", r)
			}
			env.WaitState(key, id, "archived")
			var head string
			env.Eventually("head stored and verified", func() bool {
				return env.S.DB.QueryRow(`SELECT s.id FROM sandboxes b JOIN snapshots s ON s.id=b.head_snapshot_id
					WHERE b.id=? AND s.uploaded_at IS NOT NULL AND s.verified_at IS NOT NULL`, id).Scan(&head) == nil
			})
			if failFast {
				node.RefuseNext("POST /v1/vms", 503, map[string]any{"code": "out_of_capacity", "message": "not enough free disk"})
			} else {
				filler := createSandbox(t, env, key, nil)
				env.WaitState(key, filler, "ready")
			}
			before := store.Now()
			if r := env.Do(key, "POST", base+"/resume", map[string]any{"failFast": failFast}); r.Status != 202 {
				t.Fatalf("resume: %s", r)
			}
			r := env.WaitState(key, id, "archived", "error")
			if r.Str("sandbox.state") != "archived" || !strings.HasPrefix(r.Str("sandbox.error"), wantError) {
				t.Fatalf("resume refusal did not leave the sandbox stopped with its reason: %s", r)
			}
			sb, err := env.S.SandboxByID(id)
			if err != nil {
				t.Fatal(err)
			}
			if sb.StoppedAt == nil || sb.StoppedAt.UnixMilli() < before || sb.ArchiveAfter != nil || sb.HeadSnapshotID != head || sb.Extra["pendingBoot"] != nil {
				t.Fatalf("refused resume did not restore stopped disk: %+v", sb)
			}
			env.S.EvictStoppedCopies()
			if !node.HasSnapshot(head) {
				t.Fatal("recently stopped disk was evicted")
			}
			if _, err := env.S.DB.Exec(`UPDATE sandboxes SET stopped_at=? WHERE id=?`, store.T(time.Now().Add(-25*time.Hour)), id); err != nil {
				t.Fatal(err)
			}
			env.S.ExpireBootGrants(node.ID, head)
			env.S.EvictStoppedCopies()
			if node.HasSnapshot(head) {
				t.Fatal("refused resume's disk was not evicted after the cache TTL")
			}
		})
	}
}

// A resume aborted after its run opened (node dropped mid-boot) closes that
// run, so an archived sandbox is not billed.
func TestRefusedResumeClosesRun(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(key, id, "archived")
	if err := env.S.AbortResumeWithOpenRun(id, "out_of_capacity: test"); err != nil {
		t.Fatal(err)
	}
	var open int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM sandbox_runs WHERE sandbox_id=? AND ended_at IS NULL`, id).Scan(&open)
	if sb, _ := env.S.SandboxByID(id); sb.State != "archived" || open != 0 {
		t.Fatalf("state %s, %d open runs", sb.State, open)
	}
}

// Nodes drop their copy of a stopped sandbox's disk once the server stores
// all of it and the node cache TTL passed; a resume downloads it again.
func TestNodeCacheEvictsStoppedDisks(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Name: "n1"})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"noEnv": true, "placement": map[string]any{"node": node.ID}})
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status != 200 && r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(key, id, "archived")
	var head string
	env.Eventually("head stored and verified", func() bool {
		return env.S.DB.QueryRow(`SELECT s.id FROM sandboxes b JOIN snapshots s ON s.id=b.head_snapshot_id
			WHERE b.id=? AND s.uploaded_at IS NOT NULL AND s.verified_at IS NOT NULL`, id).Scan(&head) == nil
	})
	if !node.HasSnapshot(head) {
		t.Fatal("control: the node has no copy of the stopped disk")
	}

	env.S.EvictStoppedCopies() // stopped just now: kept
	if !node.HasSnapshot(head) {
		t.Fatal("a recently stopped sandbox's disk was evicted")
	}

	_, _ = env.S.DB.Exec(`UPDATE sandboxes SET stopped_at=? WHERE id=?`, store.T(time.Now().Add(-25*time.Hour)), id)
	// Not verified on the server: the node's copy may be the only good one.
	var verified int64
	_ = env.S.DB.QueryRow(`SELECT verified_at FROM snapshots WHERE id=?`, head).Scan(&verified)
	_, _ = env.S.DB.Exec(`UPDATE snapshots SET verified_at=NULL WHERE id=?`, head)
	env.S.EvictStoppedCopies()
	if !node.HasSnapshot(head) {
		t.Fatal("evicted a disk the server has not verified")
	}
	_, _ = env.S.DB.Exec(`UPDATE snapshots SET verified_at=? WHERE id=?`, verified, head)
	env.S.EvictStoppedCopies()
	if node.HasSnapshot(head) {
		t.Fatal("the node still holds a disk stopped 25 h ago that the server stores")
	}
	var n int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM snapshot_locations WHERE snapshot_id=?`, head).Scan(&n)
	if n != 0 {
		t.Fatalf("snapshot location kept after eviction: %d", n)
	}

	// The disk comes back from the server.
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status != 200 && r.Status != 202 {
		t.Fatalf("resume: %s", r)
	}
	env.WaitState(key, id, "ready")
}

package server_test

import (
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

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

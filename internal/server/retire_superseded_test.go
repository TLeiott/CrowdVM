package server_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// Field report: merged-away periodic layers were retired only once the
// merged layer was uploaded. With uploads failing (a full server) they
// stayed needed, the server kept asking for their uploads and the node kept
// every one (79 GB for a 17 GB disk). They are retired when the merged
// layer is reported now, and only by the node that took them.
func TestSupersededLayersRetireOnReport(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Name: "n1"})
	other := env.StartNode(servertest.NodeOptions{Name: "n2"})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"noEnv": true, "placement": map[string]any{"node": node.ID}})
	env.WaitState(key, id, "ready")

	report := func(n *servertest.FakeNode, ref proto.SnapshotRef) {
		t.Helper()
		ref.SandboxID, ref.CreatedAt = id, time.Now()
		b, _ := json.Marshal(ref)
		if code, body := nodeCall(t, env, n, "POST", "/node/v1/snapshots", nil, string(b)); code != 200 {
			t.Fatalf("report %s: %d %s", ref.ID, code, body)
		}
	}
	deleted := func(snap string) bool {
		var d bool
		if err := env.S.DB.QueryRow(`SELECT deleted_at IS NOT NULL FROM snapshots WHERE id=?`, snap).Scan(&d); err != nil {
			t.Fatalf("%s: %v", snap, err)
		}
		return d
	}
	report(node, proto.SnapshotRef{ID: "snap_retirelayera0001"})
	report(node, proto.SnapshotRef{ID: "snap_retirelayerb0001", ParentID: "snap_retirelayera0001"})

	// Another node cannot retire them by naming them (its report of a
	// sandbox it does not run is recorded as deleted data).
	report(other, proto.SnapshotRef{ID: "snap_retireforged0001", Supersedes: []string{"snap_retirelayera0001"}})
	if deleted("snap_retirelayera0001") {
		t.Fatal("a node that did not take a layer retired it")
	}

	// B is the stored head: it stays live until the merged layer replaces it.
	if _, err := env.S.DB.Exec(`UPDATE sandboxes SET head_snapshot_id=? WHERE id=?`, "snap_retirelayerb0001", id); err != nil {
		t.Fatal(err)
	}
	report(node, proto.SnapshotRef{ID: "snap_retiremerged0001", Supersedes: []string{"snap_retirelayera0001", "snap_retirelayerb0001"}})
	if !deleted("snap_retirelayera0001") {
		t.Fatal("a merged-away layer is still live before any upload")
	}
	if deleted("snap_retirelayerb0001") {
		t.Fatal("the head was retired before the merged layer was stored")
	}
	if deleted("snap_retiremerged0001") {
		t.Fatal("the merged layer itself was retired")
	}
}

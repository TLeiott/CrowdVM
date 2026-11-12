package server

import (
	"context"
	"errors"
	"time"
)

// A layer the server recorded but does not store yet lives only on the node
// that took it. If that node restarts mid-upload, nothing on the node
// uploads it again, so the server asks for it: on connect, every
// maintenance pass, and while a boot elsewhere waits for it.

// snapshotsNeeded returns the unpurged snapshots that a live (not deleted)
// snapshot, a head, a saved snapshot or a waiting fork builds on, and the
// deleted ones (nil, nil on a database error). With gcOnly it stops early
// (an empty map) when nothing is deleted.
func (s *Server) snapshotsNeeded(gcOnly bool) (map[string]bool, []string) {
	parent := map[string]string{}
	var roots, deleted []string
	rows, err := s.DB.Query(`SELECT id,COALESCE(parent_id,''),deleted_at IS NULL FROM snapshots WHERE purged_at IS NULL`)
	if err != nil {
		return nil, nil
	}
	for rows.Next() {
		var id, p string
		var live bool
		if rows.Scan(&id, &p, &live) == nil {
			parent[id] = p
			if live {
				roots = append(roots, id)
			} else {
				deleted = append(deleted, id)
			}
		}
	}
	rows.Close()
	if gcOnly && len(deleted) == 0 {
		return map[string]bool{}, nil
	}
	for _, q := range []string{
		`SELECT snapshot_id FROM saved_snapshots WHERE snapshot_id IS NOT NULL`,
		`SELECT pending_snapshot_id FROM saved_snapshots WHERE pending_snapshot_id IS NOT NULL`,
		`SELECT head_snapshot_id FROM sandboxes WHERE head_snapshot_id IS NOT NULL AND deleted_at IS NULL`,
		// A fork still waiting for a node boots from its source's last
		// snapshot, also when the source is deleted meanwhile.
		`SELECT s.head_snapshot_id FROM sandboxes f JOIN sandboxes s ON s.id=f.source_id
			WHERE f.source_kind='fork' AND f.deleted_at IS NULL AND f.head_snapshot_id IS NULL AND s.head_snapshot_id IS NOT NULL`,
		// A boot still waiting for a node (a deploy of a saved or shared
		// snapshot, a resume, a rebuild) boots from its queued parent, also
		// when that snapshot was removed meanwhile (bugs-r18-opus 4).
		`SELECT json_extract(extra,'$.pendingBoot.parent') FROM sandboxes WHERE deleted_at IS NULL
			AND COALESCE(json_extract(extra,'$.pendingBoot.parent'),'')<>''`,
		// A running machine's disk builds on the layer it booted from and the
		// layers it took since, also when they were deleted: its next layer
		// (a stop) needs them (bugs-r3-opus 2).
		// Only its newest layer: merged-away and snapshots-off layers still go.
		`SELECT json_extract(extra,'$.bootParent') FROM sandboxes WHERE deleted_at IS NULL AND node_id IS NOT NULL AND snapshots_enabled=1
			AND state NOT IN ('archived','cancelled') AND COALESCE(json_extract(extra,'$.bootParent'),'')<>''`,
		`SELECT (SELECT s.id FROM snapshots s WHERE s.sandbox_id=b.id AND s.purged_at IS NULL AND s.created_at>=COALESCE(b.started_at,0)
			ORDER BY s.created_at DESC LIMIT 1) FROM sandboxes b WHERE b.deleted_at IS NULL AND b.node_id IS NOT NULL AND b.snapshots_enabled=1
			AND b.state NOT IN ('archived','cancelled')`,
	} {
		r, err := s.DB.Query(q)
		if err != nil {
			continue
		}
		for r.Next() {
			var id string
			if r.Scan(&id) == nil {
				roots = append(roots, id)
			}
		}
		r.Close()
	}
	needed := map[string]bool{}
	for _, id := range roots {
		for id != "" && !needed[id] {
			needed[id] = true
			id = parent[id]
		}
	}
	return needed, deleted
}

// holders returns the online nodes that hold snapshot id.
func (s *Server) holders(id string) []*NodeConn {
	rows, err := s.DB.Query(`SELECT node_id FROM snapshot_locations WHERE snapshot_id=?`, id)
	if err != nil {
		return nil
	}
	var out []*NodeConn
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			if c := s.nodes.get(n); c != nil {
				out = append(out, c)
			}
		}
	}
	rows.Close()
	return out
}

func (s *Server) holderOnline(id string) bool { return len(s.holders(id)) > 0 }

// holderExists: a node that holds snapshot id still exists (online or not).
func (s *Server) holderExists(id string) bool {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM snapshot_locations l JOIN nodes n ON n.id=l.node_id WHERE l.snapshot_id=?`, id).Scan(&n)
	return n > 0
}

// fallBackFromRemovedNode (node removal): stopped sandboxes whose head
// builds on a layer only that node held go back to their newest stored
// layer, as a forced stop of a lost node does; the rest of their chain
// stays restorable.
func (s *Server) fallBackFromRemovedNode() {
	rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE deleted_at IS NULL AND state='archived' AND head_snapshot_id IS NOT NULL`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		sb, err := s.SandboxByID(id)
		if err != nil {
			continue
		}
		for _, l := range s.snapshotChain(sb.HeadSnapshotID) {
			var stored bool
			if s.DB.QueryRow(`SELECT uploaded_at IS NOT NULL FROM snapshots WHERE id=?`, l).Scan(&stored) == nil && !stored && !s.holderExists(l) {
				s.fallBackToStoredHead(sb)
				break
			}
		}
	}
}

// requestUpload asks an online node holding snapshot id to upload it
// (POST /v1/snapshots/{id}/upload; a no-op on a node already uploading
// it).
func (s *Server) requestUpload(ctx context.Context, id string) {
	for _, c := range s.holders(id) {
		if s.askUpload(ctx, c, id) {
			return
		}
	}
}

func (s *Server) askUpload(ctx context.Context, c *NodeConn, id string) bool {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := c.Do(cctx, "POST", "/v1/snapshots/"+id+"/upload", nil, nil)
	if err != nil {
		var ne *NodeError
		if !(errors.As(err, &ne) && ne.Status == 404) {
			s.Log.Warn("could not ask a node to upload a snapshot", "node", c.ID, "snapshot", id, "err", err)
		}
		return false
	}
	return true
}

// uploadStranded asks node c (every online node when nil) to upload the
// layers it holds that the server recorded but does not store, oldest
// first, when something still needs them.
func (s *Server) uploadStranded(ctx context.Context, c *NodeConn) {
	needed, _ := s.snapshotsNeeded(false)
	q := `SELECT s.id, l.node_id FROM snapshots s JOIN snapshot_locations l ON l.snapshot_id=s.id
		WHERE s.uploaded_at IS NULL AND s.purged_at IS NULL`
	args := []any{}
	if c != nil {
		q += ` AND l.node_id=?`
		args = append(args, c.ID)
	}
	rows, err := s.DB.Query(q+` ORDER BY s.generation, s.created_at`, args...)
	if err != nil {
		return
	}
	type req struct{ id, node string }
	var list []req
	for rows.Next() {
		var r req
		if rows.Scan(&r.id, &r.node) == nil && needed[r.id] {
			list = append(list, r)
		}
	}
	rows.Close()
	for _, r := range list {
		if n := s.nodes.get(r.node); n != nil && s.askUpload(ctx, n, r.id) {
			s.Log.Info("asked a node to upload a snapshot the server does not store yet", "node", r.node, "snapshot", r.id)
		}
	}
}

// chainReachable (resume, fork of a stopped sandbox): every layer of chain
// the server does not store yet is held by an online node.
func (s *Server) chainReachable(chain []string) error {
	for _, id := range chain {
		var stored bool
		if s.DB.QueryRow(`SELECT uploaded_at IS NOT NULL FROM snapshots WHERE id=?`, id).Scan(&stored) != nil || stored {
			continue
		}
		if !s.holderOnline(id) {
			if !s.holderExists(id) { // its node was removed: it never comes back (opus-21 m1)
				return ErrD(409, "snapshot_lost", "Part of this sandbox's disk (snapshot "+id+") was only on a node that was removed, so it cannot be restored.",
					map[string]any{"retryable": false, "snapshotId": id})
			}
			return snapshotUnavailableErr(id)
		}
	}
	return nil
}

func snapshotUnavailableErr(id string) error {
	return ErrD(409, "snapshot_unavailable", "Part of this sandbox's disk (snapshot "+id+") is not on the server yet, and the node that holds it is offline. Retry once that node is back online.",
		map[string]any{"retryable": true, "retryAfterSeconds": 30, "snapshotId": id})
}

// HolderOfflineGrace: how long a boot waits for a layer whose holding
// node is offline before it fails with snapshot_unavailable (a node
// restart takes seconds; tests shorten it).
var HolderOfflineGrace = 2 * time.Minute

// fallBackToStoredHead (forced stop of a node that is gone): the head
// becomes the newest stored layer of its chain, Boat's "last good
// snapshot". Newer layers stay recorded, so the node uploads them if it
// comes back (they become the head again then, if the sandbox has not
// moved on).
func (s *Server) fallBackToStoredHead(sb *Sandbox) {
	for _, id := range s.snapshotChain(sb.HeadSnapshotID) {
		var stored bool
		if s.DB.QueryRow(`SELECT uploaded_at IS NOT NULL FROM snapshots WHERE id=? AND purged_at IS NULL`, id).Scan(&stored) != nil || !stored {
			continue
		}
		if id != sb.HeadSnapshotID {
			_, _ = s.DB.Exec(`UPDATE sandboxes SET head_snapshot_id=? WHERE id=? AND head_snapshot_id=?`, id, sb.ID, sb.HeadSnapshotID)
			s.Log.Info("forced stop of an offline node: back to the last stored snapshot", "sandbox", sb.ID, "head", id, "unstored", sb.HeadSnapshotID)
		}
		return
	}
}

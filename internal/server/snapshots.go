package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Snapshot is one immutable disk layer of a sandbox.
type Snapshot struct {
	ID, SandboxID, OwnerID, ParentID, ImageID, Kind, ChainID, SHA256, NodeID string
	Generation                                                               int
	SizeBytes                                                                int64
	CreatedAt                                                                time.Time
	UploadedAt, DeletedAt                                                    *time.Time
}

const snapCols = `id,sandbox_id,owner_id,COALESCE(parent_id,''),image_id,kind,chain_id,sha256,COALESCE(node_id,''),generation,size_bytes,created_at,uploaded_at,deleted_at`

func scanSnap(sc interface{ Scan(...any) error }) (*Snapshot, error) {
	sn := &Snapshot{}
	var created int64
	var up, del sql.NullInt64
	if err := sc.Scan(&sn.ID, &sn.SandboxID, &sn.OwnerID, &sn.ParentID, &sn.ImageID, &sn.Kind, &sn.ChainID, &sn.SHA256, &sn.NodeID,
		&sn.Generation, &sn.SizeBytes, &created, &up, &del); err != nil {
		return nil, err
	}
	sn.CreatedAt = store.Time(created)
	sn.UploadedAt, sn.DeletedAt = store.NullTime(up), store.NullTime(del)
	return sn, nil
}

// SnapshotByID loads a snapshot (also deleted ones).
func (s *Server) SnapshotByID(id string) (*Snapshot, error) {
	return scanSnap(s.DB.QueryRow(`SELECT `+snapCols+` FROM snapshots WHERE id=?`, id))
}

// snapshotChain returns id and its ancestors, newest first.
func (s *Server) snapshotChain(id string) []string {
	var out []string
	seen := map[string]bool{}
	for id != "" && !seen[id] && len(out) < 1000 {
		seen[id] = true
		out = append(out, id)
		var p sql.NullString
		if s.DB.QueryRow(`SELECT parent_id FROM snapshots WHERE id=?`, id).Scan(&p) != nil {
			break
		}
		id = p.String
	}
	return out
}

func (s *Server) hasLocation(snapID, nodeID string) bool {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM snapshot_locations WHERE snapshot_id=? AND node_id=?`, snapID, nodeID).Scan(&n)
	return n > 0
}

func (s *Server) addLocation(snapID, nodeID string) {
	_, _ = s.DB.Exec(`INSERT OR IGNORE INTO snapshot_locations(snapshot_id,node_id) VALUES(?,?)`, snapID, nodeID)
}

// snapDiskCols: a new snapshot row takes its sandbox's disk key and disk
// kind (the key follows the disk content; see ensureDiskKey).
const snapDiskCols = `COALESCE((SELECT data_key FROM sandboxes WHERE id=?),''),COALESCE((SELECT disk FROM sandboxes WHERE id=?),'')`

// recordSnapshot stores a snapshot taken by a node for sb. A stop layer is
// the sandbox's head at once (the newest state; a resume elsewhere waits
// for its upload). A live layer (save, fork, pre-flight) becomes the head
// once stored (afterSnapshotStored), like a periodic one, so a node lost
// mid-upload leaves the head on stored data.
func (s *Server) recordSnapshot(sb *Sandbox, ref proto.SnapshotRef, nodeID, kind string) {
	now := store.Now()
	created := store.T(ref.CreatedAt)
	if created == 0 {
		created = now
	}
	gen, chain := s.chainPos(ref)
	image := ref.ImageID
	if image == "" {
		image = sb.ImageID
	}
	res, err := s.DB.Exec(`INSERT OR IGNORE INTO snapshots(id,sandbox_id,owner_id,parent_id,image_id,kind,generation,chain_id,size_bytes,node_id,created_at,data_key,disk)
VALUES(?,?,?,?,?,?,?,?,?,?,?,`+snapDiskCols+`)`,
		ref.ID, sb.ID, sb.OwnerID, nz(ref.ParentID), image, kind, gen, chain, ref.SizeBytes, nz(nodeID), created, sb.ID, sb.ID)
	if err != nil {
		s.Log.Error("record snapshot", "err", err)
		return
	}
	if nodeID != "" {
		s.addLocation(ref.ID, nodeID)
	}
	// The sandbox was deleted while the node took it (e.g. a stop racing a
	// delete): the snapshot is deleted from the start, so GC erases it.
	if r, _ := s.DB.Exec(`UPDATE snapshots SET deleted_at=(SELECT deleted_at FROM sandboxes WHERE id=?) WHERE id=? AND deleted_at IS NULL
		AND EXISTS (SELECT 1 FROM sandboxes WHERE id=? AND deleted_at IS NOT NULL)`, sb.ID, ref.ID, sb.ID); r != nil {
		if n, _ := r.RowsAffected(); n > 0 {
			s.kickDeletions()
		}
	}
	// Layers the node merged into this one (tiered compaction): periodic
	// ones go; GC keeps whatever a head, saved snapshot or fork still needs.
	merged := 0
	for _, old := range ref.Supersedes {
		if old == "" || old == ref.ID {
			continue
		}
		r, _ := s.DB.Exec(`UPDATE snapshots SET deleted_at=? WHERE id=? AND sandbox_id=? AND kind='periodic' AND deleted_at IS NULL`, now, old, sb.ID)
		if n, _ := r.RowsAffected(); n > 0 {
			merged++
		}
	}
	if merged > 0 {
		s.Go(s.gcSnapshots)
	}
	// The attempt completes when the blob is stored (afterSnapshotStored);
	// until then it is in progress (uploading).
	_, _ = s.DB.Exec(`UPDATE sandboxes SET last_snapshot_at=?, last_snapshot_status='in_progress' WHERE id=?`, now, sb.ID)
	if kind == "stop" {
		_, _ = s.DB.Exec(`UPDATE sandboxes SET head_snapshot_id=?
			WHERE id=? AND (head_snapshot_id IS NULL OR head_snapshot_id=? OR NOT EXISTS (SELECT 1 FROM snapshots h WHERE h.id=sandboxes.head_snapshot_id AND h.sandbox_id=sandboxes.id AND h.created_at>?))`,
			ref.ID, sb.ID, ref.ParentID, created)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.Bus.Publish(Event{Type: EvSnapshotCreated, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, NodeID: nodeID,
			Data: map[string]any{"snapshotId": ref.ID, "kind": kind, "parentId": ref.ParentID}})
	}
	// The node may have uploaded the blob before we recorded the row.
	if bi, err := s.Blobs.Stat(store.BlobSnapshot, ref.ID); err == nil {
		if ref.SHA256 != "" && strings.EqualFold(ref.SHA256, bi.SHA256) {
			_, _ = s.DB.Exec(`UPDATE snapshots SET verified_at=COALESCE(verified_at,?) WHERE id=?`, now, ref.ID)
		}
		r2, _ := s.DB.Exec(`UPDATE snapshots SET uploaded_at=?, sha256=?, size_bytes=CASE WHEN size_bytes=0 THEN ? ELSE size_bytes END WHERE id=? AND uploaded_at IS NULL`, now, bi.SHA256, bi.Size, ref.ID)
		if n, _ := r2.RowsAffected(); n > 0 {
			s.afterSnapshotStored(ref.ID)
			return
		}
	}
	// The upload event got here first (and stored it): this attempt is
	// complete already.
	_, _ = s.DB.Exec(`UPDATE sandboxes SET snapshot_completed_at=?, last_snapshot_status='completed' WHERE id=? AND head_snapshot_id=? AND last_snapshot_status='in_progress'
		AND EXISTS (SELECT 1 FROM snapshots WHERE id=? AND uploaded_at IS NOT NULL)`, now, sb.ID, ref.ID, ref.ID)
}

// chainPos: the generation and chain id of a new snapshot ref.
func (s *Server) chainPos(ref proto.SnapshotRef) (gen int, chain string) {
	gen, chain = 0, ref.ID
	if ref.ParentID != "" {
		var pg int
		var pc string
		if s.DB.QueryRow(`SELECT generation, chain_id FROM snapshots WHERE id=?`, ref.ParentID).Scan(&pg, &pc) == nil {
			gen, chain = pg+1, pc
		}
	}
	return gen, chain
}

// recordTaken records a snapshot a node took on its own (a periodic layer)
// as soon as it is taken (POST /node/v1/snapshots), before its upload: with
// its parent and the node as its location, not stored yet (uploaded_at
// NULL). Stops, saves, forks, resumes, GC and deletions then see the whole
// chain at once. It becomes the sandbox's head once stored
// (afterSnapshotStored), so a node lost mid-upload leaves the head on
// stored data. A layer of a deleted sandbox, of one that runs on another
// node now, or with snapshots off is recorded as deleted: deletions wait
// for its upload and GC erases every copy of it (unless something built on
// it, which keeps it).
func (s *Server) recordTaken(nodeID string, ref proto.SnapshotRef) {
	if ref.ID == "" || ref.SandboxID == "" {
		return
	}
	if _, err := s.SnapshotByID(ref.ID); err == nil {
		// Only the node that took it, or that runs its sandbox, becomes a
		// holder: a holder is asked for the blob and preferred for restores
		// (bugs-r11-sonnet 12).
		_, _ = s.DB.Exec(`INSERT OR IGNORE INTO snapshot_locations(snapshot_id,node_id) SELECT s.id,? FROM snapshots s LEFT JOIN sandboxes b ON b.id=s.sandbox_id
WHERE s.id=? AND s.purged_at IS NULL AND (s.node_id=? OR b.node_id=?)`, nodeID, ref.ID, nodeID, nodeID)
		return
	}
	sb, err := s.SandboxByID(ref.SandboxID)
	if err != nil {
		return // unknown sandbox: its upload is refused (snapshotGone)
	}
	if ref.ParentID != "" && !s.onDiskOf(sb, ref.ParentID) {
		// The node names the parent, and a layer's chain is what its node
		// may fetch: a parent that is not on this sandbox's disk would let
		// it read another account's layers (bugs-r14-sonnet 2).
		s.Log.Warn("refusing a snapshot whose parent is not on its sandbox's disk", "snapshot", ref.ID, "sandbox", sb.ID, "parent", ref.ParentID, "node", nodeID)
		return
	}
	now := store.Now()
	var deleted any
	switch {
	case sb.DeletedAt != nil:
		deleted = store.T(*sb.DeletedAt)
	case sb.NodeID != "" && sb.NodeID != nodeID, !sb.SnapshotsEnabled:
		deleted = now
	}
	created := store.T(ref.CreatedAt)
	if created == 0 {
		created = now
	}
	image := ref.ImageID
	if image == "" {
		image = sb.ImageID
	}
	gen, chain := s.chainPos(ref)
	res, err := s.DB.Exec(`INSERT OR IGNORE INTO snapshots(id,sandbox_id,owner_id,parent_id,image_id,kind,generation,chain_id,size_bytes,node_id,created_at,deleted_at,data_key,disk) VALUES(?,?,?,?,?,'periodic',?,?,?,?,?,?,`+snapDiskCols+`)`,
		ref.ID, sb.ID, sb.OwnerID, nz(ref.ParentID), image, gen, chain, ref.SizeBytes, nz(nodeID), created, deleted, sb.ID, sb.ID)
	if err != nil {
		s.Log.Error("record taken snapshot", "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	s.addLocation(ref.ID, nodeID)
	if deleted != nil {
		s.Log.Info("recorded a snapshot of a deleted or moved sandbox as deleted", "snapshot", ref.ID, "sandbox", sb.ID, "node", nodeID)
		s.kickDeletions()
		return
	}
	s.Bus.Publish(Event{Type: EvSnapshotCreated, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, NodeID: nodeID,
		Data: map[string]any{"snapshotId": ref.ID, "kind": "periodic", "parentId": ref.ParentID}})
}

// onDiskOf: snapshot id is a layer of sb's disk: one of its own, or in the
// chain it booted from (or of its head).
func (s *Server) onDiskOf(sb *Sandbox, id string) bool {
	var owner string
	if s.DB.QueryRow(`SELECT sandbox_id FROM snapshots WHERE id=?`, id).Scan(&owner) != nil {
		return false
	}
	if owner == sb.ID {
		return true
	}
	bp, _ := sb.Extra["bootParent"].(string)
	for _, from := range []string{bp, sb.HeadSnapshotID} {
		if from != "" && slices.Contains(s.snapshotChain(from), id) {
			return true
		}
	}
	return false
}

// onSnapshotUploaded handles snapshot.uploaded (also for snapshots the node
// took on its own, e.g. periodic saves).
func (s *Server) onSnapshotUploaded(nodeID, sbID string, ref proto.SnapshotRef) {
	if ref.ID == "" {
		return
	}
	// Normally the row exists since the node reported the layer when it
	// took it (recordTaken); this records it if that report never arrived.
	if _, err := s.SnapshotByID(ref.ID); err != nil {
		if sbID == "" {
			return
		}
		sb, err := s.SandboxByID(sbID)
		switch {
		case (err != nil && errors.Is(err, sql.ErrNoRows)) || (err == nil && sb.DeletedAt != nil):
			// Deleted (or unknown) sandbox: nothing of it may stay (B1).
			s.Log.Info("erasing a snapshot of a deleted sandbox", "sandbox", sbID, "snapshot", ref.ID, "node", nodeID)
			s.dropNodeSnapshot(nodeID, ref.ID)
			s.kickDeletions()
			return
		case err != nil:
			return
		case !sb.SnapshotsEnabled:
			// snapshots:false: nothing of the disk may be kept (SNAP-036).
			s.Log.Warn("dropping a snapshot of a sandbox with snapshots off", "sandbox", sbID, "snapshot", ref.ID, "node", nodeID)
			s.dropNodeSnapshot(nodeID, ref.ID)
			return
		}
		// A sandbox that moved to another node meanwhile gets it as deleted
		// data held by this node, so GC and its deletion erase it here.
		ref.SandboxID = sbID
		s.recordTaken(nodeID, ref)
	}
	if ref.SizeBytes > 0 { // only the node that took or holds it (bugs-r11-sonnet 13)
		_, _ = s.DB.Exec(`UPDATE snapshots SET size_bytes=? WHERE id=? AND (node_id=? OR id IN (SELECT snapshot_id FROM snapshot_locations WHERE node_id=?))`, ref.SizeBytes, ref.ID, nodeID, nodeID)
	}
	if len(ref.Supersedes) > 0 && sbID != "" {
		now := store.Now()
		for _, old := range ref.Supersedes {
			// Only layers this node took: another node cannot retire a
			// sandbox's layers by naming them (bugs-r9-sonnet).
			_, _ = s.DB.Exec(`UPDATE snapshots SET deleted_at=? WHERE id=? AND sandbox_id=? AND kind='periodic' AND deleted_at IS NULL AND id<>? AND node_id=?`, now, old, sbID, ref.ID, nodeID)
		}
		s.Go(s.gcSnapshots)
	}
	if sn, err := s.SnapshotByID(ref.ID); err == nil && sn.DeletedAt != nil {
		s.kickDeletions() // deleted while it uploaded: erase it now
	}
	if bi, err := s.Blobs.Stat(store.BlobSnapshot, ref.ID); err == nil {
		res, _ := s.DB.Exec(`UPDATE snapshots SET uploaded_at=?, sha256=? WHERE id=? AND uploaded_at IS NULL AND purged_at IS NULL`, store.Now(), bi.SHA256, ref.ID)
		// SNAP-002: verified = the stored copy has the checksum the node computed.
		if ref.SHA256 != "" && strings.EqualFold(ref.SHA256, bi.SHA256) {
			_, _ = s.DB.Exec(`UPDATE snapshots SET verified_at=COALESCE(verified_at,?) WHERE id=?`, store.Now(), ref.ID)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			s.afterSnapshotStored(ref.ID)
		}
	} else {
		s.Log.Warn("snapshot.uploaded but no blob on the server", "snapshot", ref.ID, "node", nodeID)
	}
}

// afterSnapshotStored runs once a snapshot's blob is on the server.
func (s *Server) afterSnapshotStored(snapID string) {
	sn, err := s.SnapshotByID(snapID)
	if err != nil {
		return
	}
	// A layer becomes the head once stored, unless the head is newer or
	// builds on it (e.g. a stop), or the sandbox runs from another node's
	// disk now (a layer of a node lost mid-upload that came back after a
	// forced stop and a resume elsewhere). Compare-and-swap on the head read:
	// parent and child landing at once must not leave the parent as head, so
	// a lost race is retried with the new head.
	for try := 0; try < 5 && sn.DeletedAt == nil; try++ {
		sb, err := s.SandboxByID(sn.SandboxID)
		if err != nil || slices.Contains(s.snapshotChain(sb.HeadSnapshotID), snapID) || s.abandonedLayer(sb, sn) {
			break
		}
		res, _ := s.DB.Exec(`UPDATE sandboxes SET head_snapshot_id=?, last_snapshot_at=? WHERE id=? AND deleted_at IS NULL AND COALESCE(head_snapshot_id,'')=? AND (?='' OR node_id=?)
			AND (head_snapshot_id IS NULL OR head_snapshot_id=? OR NOT EXISTS (SELECT 1 FROM snapshots h WHERE h.id=sandboxes.head_snapshot_id AND h.sandbox_id=sandboxes.id
				AND (h.created_at>? OR (h.created_at=? AND h.parent_id=?))))`, // never back to the head's own parent (same-millisecond tie)
			snapID, store.Now(), sn.SandboxID, sb.HeadSnapshotID, sn.NodeID, sn.NodeID, sn.ParentID, store.T(sn.CreatedAt), store.T(sn.CreatedAt), snapID)
		if n, _ := res.RowsAffected(); n > 0 {
			break
		}
		var now string
		if s.DB.QueryRow(`SELECT COALESCE(head_snapshot_id,'') FROM sandboxes WHERE id=?`, sn.SandboxID).Scan(&now) != nil || now == sb.HeadSnapshotID {
			break // the head did not move under us: the rule said no
		}
	}
	sb, err := s.SandboxByID(sn.SandboxID)
	if err == nil {
		if sb.HeadSnapshotID == snapID {
			now := store.Now()
			_, _ = s.DB.Exec(`UPDATE sandboxes SET snapshot_completed_at=?, last_snapshot_status='completed' WHERE id=? AND head_snapshot_id=?`, now, sb.ID, snapID)
		}
		s.Bus.Publish(Event{Type: EvSnapshotStored, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, Data: map[string]any{"snapshotId": snapID}})
	}
	// Saved snapshots waiting for this upload.
	s.savedSnapshotStored(snapID)
}

// abandonedLayer (bugs-r2-sol 1): sn was taken before sb's current machine
// started, by a machine it did not boot from (one a forced stop gave up on,
// then the sandbox booted again from an older layer, also on the same
// node). Stored late, it never becomes the head.
func (s *Server) abandonedLayer(sb *Sandbox, sn *Snapshot) bool {
	bp, ok := sb.Extra["bootParent"].(string)
	if !ok || sb.StartedAt == nil || !sn.CreatedAt.Before(*sb.StartedAt) {
		return false
	}
	return !slices.Contains(s.snapshotChain(bp), sn.ID)
}

// headSnapshotState (SNAP-002): when the sandbox's head snapshot was stored
// on the server and verified (checksum of the stored copy = the node's).
func (s *Server) headSnapshotState(sb *Sandbox) (stored, verified *time.Time) {
	if sb.HeadSnapshotID == "" {
		return nil, nil
	}
	var up, ver sql.NullInt64
	if s.DB.QueryRow(`SELECT uploaded_at, verified_at FROM snapshots WHERE id=? AND purged_at IS NULL`, sb.HeadSnapshotID).Scan(&up, &ver) != nil {
		return nil, nil
	}
	return store.NullTime(up), store.NullTime(ver)
}

func (s *Server) chainSize(id string) int64 {
	var total int64
	for _, c := range s.snapshotChain(id) {
		var n int64
		_ = s.DB.QueryRow(`SELECT size_bytes FROM snapshots WHERE id=?`, c).Scan(&n)
		total += n
	}
	return total
}

// gcSnapshots purges deleted snapshots that nothing depends on any more.
func (s *Server) gcSnapshots(ctx context.Context) { s.gcSnapshotsNeeded(ctx, nil) }

// gcSnapshotsNeeded is gcSnapshots that also returns the snapshots still
// needed by a head, saved snapshot or fork. A snapshot counts as purged only
// once the server's blob and every node's copy are confirmed gone (nodes
// that are offline keep it pending until they reconnect). Snapshots a node
// is still uploading (up; nil = ask the nodes) wait until the upload ended:
// purging earlier would let the upload store the data afterwards.
func (s *Server) gcSnapshotsNeeded(ctx context.Context, up map[string]flight) map[string]bool {
	needed, deleted := s.snapshotsNeeded(true)
	if needed == nil {
		return nil
	}
	for _, id := range deleted {
		if needed[id] {
			continue
		}
		if up == nil {
			up = s.uploadsInFlight(ctx)
		}
		if _, ok := up[id]; ok {
			continue
		}
		var nodes []string
		r, err := s.DB.Query(`SELECT node_id FROM snapshot_locations WHERE snapshot_id=?`, id)
		if err == nil {
			for r.Next() {
				var n string
				if r.Scan(&n) == nil {
					nodes = append(nodes, n)
				}
			}
			r.Close()
		}
		left := 0
		for _, n := range nodes {
			c := s.nodes.get(n)
			if c == nil {
				left++ // offline: erased when it reconnects
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := c.Do(cctx, "DELETE", "/v1/snapshots/"+id, nil, nil)
			cancel()
			var ne *NodeError
			if err != nil && !(errors.As(err, &ne) && ne.Status == 404) {
				left++
				continue
			}
			_, _ = s.DB.Exec(`DELETE FROM snapshot_locations WHERE snapshot_id=? AND node_id=?`, id, n)
		}
		if err := s.Blobs.Delete(store.BlobSnapshot, id); err != nil && !errors.Is(err, fs.ErrNotExist) {
			left++
		}
		if left == 0 {
			_, _ = s.DB.Exec(`UPDATE snapshots SET purged_at=? WHERE id=?`, store.Now(), id)
		}
	}
	return needed
}

// ---------------------------------------------------------------------------
// Deletion operations
// ---------------------------------------------------------------------------

func (s *Server) newDeletionOp(owner, kind, target, reason string) (string, error) {
	id := "bdop_" + randHex(16)
	_, err := s.DB.Exec(`INSERT INTO deletion_ops(id,owner_id,kind,target_id,reason,status,stage,requested_at) VALUES(?,?,?,?,?,?,?,?)`,
		id, owner, kind, target, reason, "pending", "removing", store.Now())
	return id, err
}

func (s *Server) deletionOpJSON(id string) (map[string]any, error) {
	var kind, target, reason, status, stage string
	var attempts int
	var req int64
	var done sql.NullInt64
	var lastErr sql.NullString
	err := s.DB.QueryRow(`SELECT kind,target_id,reason,status,stage,attempt_count,requested_at,completed_at,last_error FROM deletion_ops WHERE id=?`, id).
		Scan(&kind, &target, &reason, &status, &stage, &attempts, &req, &done, &lastErr)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "kind": kind, "targetId": target, "reason": reason, "status": status, "stage": stage,
		"expectedBy": nil, "attemptCount": attempts, "requestedAt": tsMs(req), "completedAt": tsNull(done),
		// CrowdVM extension: why it is not completed yet (node offline,
		// uploads in flight, node removed), null once completed.
		"lastError": nz(lastErr.String)}, nil
}

func (s *Server) handleGetDeletionOp(c *Ctx) error {
	var owner string
	if s.DB.QueryRow(`SELECT owner_id FROM deletion_ops WHERE id=?`, c.Param("operationId")).Scan(&owner) != nil || owner != c.P.User.ID {
		return errNotFound("Deletion operation")
	}
	op, err := s.deletionOpJSON(c.Param("operationId"))
	if err != nil {
		return err
	}
	c.W.Header().Set("Cache-Control", "no-store")
	return c.OK(200, "deletion.operation", map[string]any{"operation": op})
}

// ---------------------------------------------------------------------------
// Snapshot endpoints
// ---------------------------------------------------------------------------

func (s *Server) snapJSON(sn *Snapshot) map[string]any {
	kind := "incremental"
	if sn.ParentID == "" {
		kind = "base"
	}
	var up any
	if sn.UploadedAt != nil {
		up = ts(*sn.UploadedAt)
	}
	return map[string]any{
		"id": sn.ID, "sandboxId": sn.SandboxID, "status": "completed", "kind": kind, "generation": sn.Generation,
		"chainId": nz(sn.ChainID), "createdAt": ts(sn.CreatedAt), "completedAt": ts(sn.CreatedAt), "sizeBytes": sn.SizeBytes,
		"fileCount": nil, "contentSizeBytes": nil, "contentFileCount": nil, "origin": "captured",
		// CrowdVM extensions
		"parentId": nz(sn.ParentID), "imageId": nz(sn.ImageID), "trigger": sn.Kind, "uploaded": sn.UploadedAt != nil, "uploadedAt": up,
	}
}

func (s *Server) listSnaps(c *Ctx, where string, args ...any) error {
	pg, err := ParsePage(c.R)
	if err != nil {
		return err
	}
	w, pargs, tail := pg.SQL("created_at", "id")
	rows, err := s.DB.Query(`SELECT `+snapCols+` FROM snapshots WHERE deleted_at IS NULL AND `+where+w+tail, append(args, pargs...)...)
	if err != nil {
		return err
	}
	var list []*Snapshot
	for rows.Next() {
		sn, err := scanSnap(rows)
		if err != nil {
			rows.Close()
			return err
		}
		list = append(list, sn)
	}
	rows.Close()
	list, info := PageResult(pg, list, func(sn *Snapshot) (int64, string) { return store.T(sn.CreatedAt), sn.ID })
	out := make([]map[string]any, 0, len(list))
	for _, sn := range list {
		out = append(out, s.snapJSON(sn))
	}
	return c.OK(200, "snapshot.list", map[string]any{"snapshots": out, "pageInfo": pageJSON(info)})
}

func (s *Server) handleListSnapshots(c *Ctx) error {
	if c.P.Key != nil && c.P.Key.Restricted() {
		var ids []string
		rows, err := s.DB.Query(`SELECT `+sbCols+` FROM sandboxes WHERE owner_id=? AND deleted_at IS NULL`, c.P.User.ID)
		if err == nil {
			for rows.Next() {
				if sb, err := scanSandbox(rows); err == nil && c.P.Key.AllowsSandbox(sb, "snapshot.read") {
					ids = append(ids, sb.ID)
				}
			}
			rows.Close()
		}
		if len(ids) == 0 {
			return s.listSnaps(c, "0")
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		return s.listSnaps(c, "sandbox_id IN ("+ph+")", args...)
	}
	return s.listSnaps(c, "owner_id=? AND sandbox_id IN (SELECT id FROM sandboxes WHERE deleted_at IS NULL)", c.P.User.ID)
}

func (s *Server) handleListSandboxSnapshots(c *Ctx) error {
	sb, err := c.Sandbox("snapshot.read", false)
	if err != nil {
		return err
	}
	return s.listSnaps(c, "sandbox_id=?", sb.ID)
}

func (s *Server) handleLatestSnapshot(c *Ctx) error {
	sb, err := c.Sandbox("snapshot.read", false)
	if err != nil {
		return err
	}
	sn, err := scanSnap(s.DB.QueryRow(`SELECT `+snapCols+` FROM snapshots WHERE sandbox_id=? AND deleted_at IS NULL ORDER BY created_at DESC, generation DESC, id DESC LIMIT 1`, sb.ID))
	if err != nil {
		return c.OK(200, "snapshot.latest", map[string]any{"snapshot": nil})
	}
	return c.OK(200, "snapshot.latest", map[string]any{"snapshot": s.snapJSON(sn)})
}

func (s *Server) handleDeleteSnapshot(c *Ctx) error {
	sn, err := s.SnapshotByID(c.Param("snapshotId"))
	if err != nil || sn.DeletedAt != nil {
		return errNotFound("Snapshot")
	}
	if sn.OwnerID != c.P.User.ID {
		return errNotFound("Snapshot")
	}
	if c.P.Key != nil {
		if sb, err := s.SandboxByID(sn.SandboxID); err == nil && !c.P.Key.AllowsSandbox(sb, "snapshot.write") {
			return Err(403, "api_key_sandbox_forbidden", "The API key is not allowed on this snapshot's sandbox.")
		}
	}
	if confirmHeader(c) != sn.ID {
		return Err(409, "confirmation_required", "Set X-Ascii-Confirm-Delete to the snapshot id to delete it.")
	}
	op, err := s.deleteSnapshots(c.P.User.ID, []*Snapshot{sn})
	if err != nil {
		return err
	}
	return c.OK(202, "snapshot.deleting", map[string]any{"operation": op[0]})
}

func (s *Server) handleDeleteSandboxSnapshots(c *Ctx) error {
	sb, err := c.Sandbox("snapshot.write", true)
	if err != nil {
		return err
	}
	if confirmHeader(c) != sb.ID {
		return Err(409, "confirmation_required", "Set X-Ascii-Confirm-Delete to the sandbox id to delete its snapshots.")
	}
	rows, err := s.DB.Query(`SELECT `+snapCols+` FROM snapshots WHERE sandbox_id=? AND deleted_at IS NULL`, sb.ID)
	if err != nil {
		return err
	}
	var list []*Snapshot
	for rows.Next() {
		if sn, err := scanSnap(rows); err == nil {
			list = append(list, sn)
		}
	}
	rows.Close()
	ops, err := s.deleteSnapshots(c.P.User.ID, list)
	if err != nil {
		return err
	}
	return c.OK(202, "snapshot.deleting", map[string]any{"operations": ops})
}

// deleteSnapshots hides snapshots at once and purges their data in the
// background once nothing reads it.
func (s *Server) deleteSnapshots(owner string, list []*Snapshot) ([]map[string]any, error) {
	ops := []map[string]any{}
	var opIDs []string
	now := store.Now()
	touched := map[string]bool{}
	for _, sn := range list {
		id, err := s.newDeletionOp(owner, "snapshot", sn.ID, "explicit")
		if err != nil {
			return nil, err
		}
		_, _ = s.DB.Exec(`UPDATE snapshots SET deleted_at=? WHERE id=?`, now, sn.ID)
		opIDs = append(opIDs, id)
		touched[sn.SandboxID] = true
	}
	// Heads move back to the newest snapshot that is left.
	for sbID := range touched {
		var head sql.NullString
		_ = s.DB.QueryRow(`SELECT id FROM snapshots WHERE sandbox_id=? AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1`, sbID).Scan(&head)
		var cur string
		_ = s.DB.QueryRow(`SELECT COALESCE(head_snapshot_id,'') FROM sandboxes WHERE id=?`, sbID).Scan(&cur)
		if sn, err := s.SnapshotByID(cur); err == nil && sn.DeletedAt != nil {
			_, _ = s.DB.Exec(`UPDATE sandboxes SET head_snapshot_id=? WHERE id=?`, nullStr(head), sbID)
			// No snapshot left of a stopped sandbox: its disk is gone, so
			// resume and fork answer 409 snapshot_data_expired (Boat).
			if sb, err := s.SandboxByID(sbID); err == nil && !head.Valid && (sb.State == api.StateArchived || sb.State == api.StateCancelled) {
				_ = s.SetSandboxExtra(sbID, "snapshotDataExpired", true)
				_ = s.SetSandboxExtra(sbID, "snapshotDataExpiredAt", now)
			}
		}
	}
	for _, id := range opIDs {
		op, _ := s.deletionOpJSON(id)
		ops = append(ops, op)
	}
	s.Go(s.processDeletionOps)
	return ops, nil
}

// ---------------------------------------------------------------------------
// Saved snapshots and share codes
// ---------------------------------------------------------------------------

var savedNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var reservedSavedNames = []string{"latest", "tree", "pull", "rm", "save", "current", "self", "new"}

type savedSnap struct {
	// OwnerID is the wallet: the user (personal) or the org (team image,
	// SNAP-022). SavedBy is the account whose disk it holds.
	ID, OwnerID, OrgID, Name, Status, Error, SourceSandboxID, SnapshotID, PendingID, Size string
	SavedBy                                                                               string
	SizeBytes                                                                             int64
	Env                                                                                   map[string]string
	NoEnv                                                                                 bool
	CreatedAt, UpdatedAt                                                                  time.Time
	SavingSince                                                                           *time.Time
	Pending                                                                               *savedMeta // metadata of the save in flight
}

// savedMeta is what a re-save replaces, swapped in only when the new disk
// is stored (SNAP-017).
type savedMeta struct {
	SourceSandboxID string            `json:"sourceSandboxId"`
	Size            string            `json:"size"`
	Env             map[string]string `json:"env"`
	NoEnv           bool              `json:"noEnv"`
	SavedBy         string            `json:"savedBy"`
}

// savingTimeout: a save that has not stored its disk by then failed.
const savingTimeout = 2 * time.Hour

func init() {
	store.Register("snapverify", store.Migration{Version: 1, SQL: `ALTER TABLE snapshots ADD COLUMN verified_at INTEGER`})
	store.Register("saved_snapshots", store.Migration{Version: 1, SQL: `
ALTER TABLE saved_snapshots ADD COLUMN saved_by TEXT;
ALTER TABLE saved_snapshots ADD COLUMN pending_meta TEXT;
ALTER TABLE saved_snapshots ADD COLUMN saving_since INTEGER;
UPDATE saved_snapshots SET saved_by=owner_id;
UPDATE saved_snapshots SET name = substr(name, 1, 50) || '-' || lower(substr(id, 4, 6))
  WHERE org_id IS NOT NULL AND id IN (SELECT a.id FROM saved_snapshots a JOIN saved_snapshots b
    ON a.org_id=b.org_id AND a.name=b.name AND a.id<>b.id AND (a.created_at>b.created_at OR (a.created_at=b.created_at AND a.id>b.id)));
UPDATE saved_snapshots SET name = substr(name, 1, 50) || '-' || lower(substr(id, 4, 6))
  WHERE org_id IS NOT NULL AND EXISTS (SELECT 1 FROM saved_snapshots p WHERE p.owner_id=saved_snapshots.org_id AND p.name=saved_snapshots.name);
UPDATE saved_snapshots SET owner_id=org_id WHERE org_id IS NOT NULL;
CREATE INDEX saved_snapshots_saved_by ON saved_snapshots(saved_by);
`})
}

const savedCols = `id,owner_id,COALESCE(org_id,''),name,status,COALESCE(error,''),source_sandbox_id,COALESCE(snapshot_id,''),COALESCE(pending_snapshot_id,''),size_name,size_bytes,env,no_env,created_at,updated_at,COALESCE(saved_by,owner_id),COALESCE(pending_meta,''),saving_since`

func scanSaved(sc interface{ Scan(...any) error }) (*savedSnap, error) {
	v := &savedSnap{}
	var env, pend string
	var c, u int64
	var since sql.NullInt64
	if err := sc.Scan(&v.ID, &v.OwnerID, &v.OrgID, &v.Name, &v.Status, &v.Error, &v.SourceSandboxID, &v.SnapshotID, &v.PendingID, &v.Size, &v.SizeBytes, &env, &v.NoEnv, &c, &u,
		&v.SavedBy, &pend, &since); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(env), &v.Env)
	if pend != "" {
		v.Pending = &savedMeta{}
		_ = json.Unmarshal([]byte(pend), v.Pending)
	}
	v.CreatedAt, v.UpdatedAt, v.SavingSince = store.Time(c), store.Time(u), store.NullTime(since)
	return v, nil
}

// savedByName finds a saved snapshot by name in wallet (user or org id).
func (s *Server) savedByName(wallet, name string) (*savedSnap, error) {
	return scanSaved(s.DB.QueryRow(`SELECT `+savedCols+` FROM saved_snapshots WHERE owner_id=? AND name=?`, wallet, name))
}

// savedLookup finds name for the caller: in the organization of ?org= /
// the org header / an org key when given, else personal first, then the
// caller's organizations (newest first).
func (s *Server) savedLookup(c *Ctx, name string) (*savedSnap, error) {
	v, err := s.savedLookupAny(c, name)
	if err == nil && !s.keySeesSaved(c.P, v, "snapshot.read") {
		return nil, Err(404, "snapshot_not_found", "No saved snapshot named "+name+".")
	}
	return v, err
}

func (s *Server) savedLookupAny(c *Ctx, name string) (*savedSnap, error) {
	notFound := Err(404, "snapshot_not_found", "No saved snapshot named "+name+".")
	explicit := c.Query("org") != "" || c.Query("teamId") != "" || orgHeader(c.R) != "" || (c.P.Key != nil && c.P.Key.OrgID != "")
	if explicit {
		org, err := c.Scope("")
		if err != nil {
			return nil, err
		}
		wallet := c.P.User.ID // --org personal: your own saved snapshots
		if org != nil {
			wallet = org.ID
		}
		v, err := s.savedByName(wallet, name)
		if err != nil {
			return nil, notFound
		}
		return v, nil
	}
	// The active organization (`org switch`) comes first, like the save and
	// `new --from` (Boat: the CLI uses the active wallet), then personal.
	if org, err := c.Scope(""); err == nil && org != nil {
		if v, err := s.savedByName(org.ID, name); err == nil {
			return v, nil
		}
	}
	if v, err := s.savedByName(c.P.User.ID, name); err == nil {
		return v, nil
	}
	v, err := scanSaved(s.DB.QueryRow(`SELECT `+savedCols+` FROM saved_snapshots WHERE name=? AND owner_id IN (SELECT org_id FROM org_members WHERE user_id=?) ORDER BY updated_at DESC LIMIT 1`, name, c.P.User.ID))
	if err != nil {
		return nil, notFound
	}
	return v, nil
}

// savedManage: who may remove a saved snapshot or manage its share codes:
// the owner of a personal one; for a team image the member who saved it or
// an owner/admin of the organization (SNAP-022).
func (s *Server) savedManage(c *Ctx, v *savedSnap) error {
	if v.OrgID == "" {
		if v.OwnerID != c.P.User.ID {
			return Err(404, "snapshot_not_found", "No saved snapshot named "+v.Name+".")
		}
		return nil
	}
	if v.SavedBy == c.P.User.ID {
		return nil
	}
	if r := s.memberRole(v.OrgID, c.P.User.ID); r == "owner" || r == "admin" {
		return nil
	}
	return Err(403, "saved_snapshot_forbidden", "Only the member who saved "+v.Name+", or an owner or admin of the organization, can remove or share it.")
}

func (s *Server) savedJSON(v *savedSnap) map[string]any {
	m := map[string]any{"name": v.Name, "status": v.Status, "sourceSandboxId": v.SourceSandboxID, "type": v.Size,
		"sizeBytes": v.SizeBytes, "createdAt": ts(v.CreatedAt), "updatedAt": ts(v.UpdatedAt), "id": v.ID, "savedBy": v.SavedBy}
	if v.SnapshotID != "" {
		m["snapshotId"] = v.SnapshotID
	}
	if v.Status == "failed" {
		m["error"] = v.Error
	}
	if v.Status == "saving" && v.SavingSince != nil {
		m["savingSince"] = ts(*v.SavingSince)
	}
	m["available"] = v.SnapshotID != "" // a failed re-save keeps the last good disk
	if v.OrgID != "" {
		m["org"] = v.OrgID
		if o, err := s.OrgByID(v.OrgID); err == nil {
			m["orgName"] = o.Name
		}
	}
	return m
}

// expireSaves fails saves that never stored their disk.
func (s *Server) expireSaves() {
	cut := store.Now() - savingTimeout.Milliseconds()
	_, _ = s.DB.Exec(`UPDATE saved_snapshots SET status='failed', error='save_timeout: the snapshot was not stored in time', pending_snapshot_id=NULL, pending_meta=NULL, saving_since=NULL, updated_at=?
WHERE status='saving' AND COALESCE(saving_since, updated_at)<?`, store.Now(), cut)
}

// savedSnapshotFailed marks saves waiting for snapID failed; the last good
// disk and its metadata stay (SNAP-015/017).
func (s *Server) savedSnapshotFailed(snapID, msg string) {
	_, _ = s.DB.Exec(`UPDATE saved_snapshots SET status='failed', error=?, pending_snapshot_id=NULL, pending_meta=NULL, saving_since=NULL, updated_at=? WHERE pending_snapshot_id=? AND status='saving'`,
		"snapshot_failed: "+msg, store.Now(), snapID)
}

// savedSnapshotStored completes saves waiting for snapID: the new disk and
// the metadata of that save replace the old ones together.
func (s *Server) savedSnapshotStored(snapID string) {
	rows, err := s.DB.Query(`SELECT `+savedCols+` FROM saved_snapshots WHERE pending_snapshot_id=? AND status='saving'`, snapID)
	if err != nil {
		return
	}
	var list []*savedSnap
	for rows.Next() {
		if v, err := scanSaved(rows); err == nil {
			list = append(list, v)
		}
	}
	rows.Close()
	size := s.chainSize(snapID)
	for _, v := range list {
		m := v.Pending
		if m == nil {
			m = &savedMeta{SourceSandboxID: v.SourceSandboxID, Size: v.Size, Env: v.Env, NoEnv: v.NoEnv, SavedBy: v.SavedBy}
		}
		_, _ = s.DB.Exec(`UPDATE saved_snapshots SET status='ready', error=NULL, snapshot_id=?, pending_snapshot_id=NULL, pending_meta=NULL, saving_since=NULL,
size_bytes=?, source_sandbox_id=?, size_name=?, env=?, no_env=?, saved_by=?, updated_at=? WHERE id=? AND pending_snapshot_id=?`,
			snapID, size, m.SourceSandboxID, m.Size, mustJSON(m.Env), boolInt(m.NoEnv), firstNonEmpty(m.SavedBy, v.SavedBy), store.Now(), v.ID, snapID)
	}
}

// keySeesSaved: a key limited to some sandboxes or environments reaches a
// saved snapshot only through its source sandbox (Boat api-keys__scopes).
func (s *Server) keySeesSaved(p *Principal, v *savedSnap, action string) bool {
	if p.Key == nil || !p.Key.Restricted() {
		return true
	}
	sb, err := s.SandboxByID(v.SourceSandboxID)
	return err == nil && p.Key.AllowsSandbox(sb, action)
}

func (s *Server) handleListSaved(c *Ctx) error {
	q := `SELECT ` + savedCols + ` FROM saved_snapshots WHERE owner_id=? OR owner_id IN (SELECT org_id FROM org_members WHERE user_id=?)`
	args := []any{c.P.User.ID, c.P.User.ID}
	if c.P.Key != nil && c.P.Key.OrgID != "" {
		q, args = `SELECT `+savedCols+` FROM saved_snapshots WHERE owner_id=?`, []any{c.P.Key.OrgID}
	} else if c.Query("org") != "" || c.Query("teamId") != "" || orgHeader(c.R) != "" {
		// A named scope (--org, the dashboard's Viewing select) lists only
		// that wallet, the same one the by-name actions look in (Boat
		// organizations__active-wallet).
		org, err := c.Scope("")
		if err != nil {
			return err
		}
		q, args = `SELECT `+savedCols+` FROM saved_snapshots WHERE owner_id=?`, []any{walletOf(c.P.User, org)}
	}
	rows, err := s.DB.Query(q+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return err
	}
	var list []*savedSnap
	for rows.Next() {
		if v, err := scanSaved(rows); err == nil {
			list = append(list, v)
		}
	}
	rows.Close()
	out := []map[string]any{}
	for _, v := range list {
		if s.keySeesSaved(c.P, v, "snapshot.read") {
			out = append(out, s.savedJSON(v))
		}
	}
	org, err := c.Scope("")
	if err != nil {
		return err
	}
	return c.OK(200, "snapshot.saved.list", map[string]any{"snapshots": out, "allowance": s.allowanceJSON(walletOf(c.P.User, org), org)})
}

func (s *Server) handleGetSaved(c *Ctx) error {
	v, err := s.savedLookup(c, c.Param("name"))
	if err != nil {
		return err
	}
	return c.OK(200, "snapshot.saved.info", map[string]any{"snapshot": s.savedJSON(v)})
}

func (s *Server) handleSaveSaved(c *Ctx) error {
	var req struct {
		SandboxID string `json:"sandboxId"`
		Name      string `json:"name"`
		Org       string `json:"org"`
		TeamID    string `json:"teamId"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if !savedNameRe.MatchString(req.Name) || slices.Contains(reservedSavedNames, req.Name) {
		return Err(400, "invalid_name", "name must be 1-63 lowercase letters, digits or dashes, starting with a letter or digit, and not reserved.")
	}
	sb, err := c.SandboxByID(req.SandboxID, "snapshot.write", false)
	if err != nil {
		return err
	}
	if !sb.SnapshotsEnabled {
		return Err(409, "snapshots_disabled", "This sandbox has snapshots off and cannot be saved.")
	}
	// A save goes to the active organization or the one passed (--org).
	org, err := c.Scope(firstNonEmpty(req.Org, req.TeamID))
	if err != nil {
		return err
	}
	wallet := walletOf(c.P.User, org)
	live := sb.Usable() && s.nodes.get(sb.NodeID) != nil
	if !live && sb.HeadSnapshotID == "" {
		return Err(409, "no_snapshot", "The sandbox has no snapshot to save yet.")
	}
	unlock := s.sbLocks.Lock("saved:" + wallet + "/" + req.Name)
	defer unlock()
	s.expireSaves()
	old, oldErr := s.savedByName(wallet, req.Name)
	if oldErr == nil && !s.keySeesSaved(c.P, old, "snapshot.write") {
		return Err(403, "api_key_sandbox_forbidden", "The API key is not allowed on the sandbox this saved snapshot was saved from.")
	}
	if oldErr == nil && old.Status == "saving" {
		return Err(409, "save_in_progress", "A save under this name is still running.")
	}
	// Check and insert under one lock, or parallel saves of new names all
	// pass the allowance (bugs-r13-sonnet 5).
	unlockLimits := func() {}
	if oldErr != nil {
		unlockLimits = s.lockLimits()
		defer unlockLimits()
		if err := s.checkSavedAllowance(wallet); err != nil {
			return err
		}
	}
	now := store.Now()
	meta := savedMeta{SourceSandboxID: sb.ID, Size: sb.Size, Env: sb.Env, NoEnv: sb.NoEnv, SavedBy: c.P.User.ID}
	id := "ss_" + randToken(16)
	if oldErr == nil {
		// Any member may update a team image; the old disk stays deployable
		// until the new one is stored (SNAP-017).
		id = old.ID
		_, err = s.DB.Exec(`UPDATE saved_snapshots SET status='saving', error=NULL, pending_meta=?, saving_since=?, updated_at=? WHERE id=?`, mustJSON(meta), now, now, id)
	} else {
		_, err = s.DB.Exec(`INSERT INTO saved_snapshots(id,owner_id,org_id,name,status,source_sandbox_id,size_name,env,no_env,created_at,updated_at,saved_by,pending_meta,saving_since) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, wallet, nz(orgID(org)), req.Name, "saving", sb.ID, sb.Size, mustJSON(sb.Env), boolInt(sb.NoEnv), now, now, c.P.User.ID, mustJSON(meta), now)
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			return Err(409, "name_taken", "You already have a saved snapshot named "+req.Name+" in another wallet; pick another name.")
		}
	}
	unlockLimits()
	if err != nil {
		return err
	}
	finish := func(snapID string) {
		_, _ = s.DB.Exec(`UPDATE saved_snapshots SET pending_snapshot_id=? WHERE id=? AND status='saving'`, snapID, id)
		if sn, err := s.SnapshotByID(snapID); err == nil && sn.UploadedAt != nil {
			s.savedSnapshotStored(snapID)
		}
	}
	if live {
		s.Go(func(ctx context.Context) {
			snap, err := s.takeLiveSnapshot(ctx, sb)
			if err != nil {
				_, _ = s.DB.Exec(`UPDATE saved_snapshots SET status='failed', error=?, pending_meta=NULL, saving_since=NULL, updated_at=? WHERE id=? AND status='saving'`, "snapshot_failed: "+err.Error(), store.Now(), id)
				return
			}
			finish(snap)
		})
	} else {
		finish(sb.HeadSnapshotID)
	}
	v, _ := s.savedByName(wallet, req.Name)
	return c.OK(202, "snapshot.saved.saving", map[string]any{"status": "saving", "snapshot": s.savedJSON(v)})
}

func (s *Server) handleDeleteSaved(c *Ctx) error {
	v, err := s.savedLookup(c, c.Param("name"))
	if err != nil {
		return err
	}
	if err := s.savedManage(c, v); err != nil {
		return err
	}
	s.expireSaves()
	if v, _ = s.savedByName(v.OwnerID, v.Name); v == nil {
		return Err(404, "snapshot_not_found", "No saved snapshot named "+c.Param("name")+".")
	}
	if v.Status == "saving" {
		return Err(409, "save_in_progress", "A save under this name is still running.")
	}
	if _, err := s.DB.Exec(`DELETE FROM saved_snapshots WHERE id=?`, v.ID); err != nil {
		return err
	}
	s.Go(s.gcSnapshots)
	c.W.Header().Set("Cache-Control", "no-store")
	return c.OK(200, "snapshot.saved.deleted", map[string]any{"name": v.Name, "status": "deleted"})
}

func shareJSON(id, prefix, status string, uses int, last, rev sql.NullInt64, created int64) map[string]any {
	return map[string]any{"id": id, "codePrefix": prefix, "status": status, "useCount": uses, "lastUsedAt": tsNull(last), "revokedAt": tsNull(rev), "createdAt": tsMs(created)}
}

func (s *Server) handleCreateShare(c *Ctx) error {
	v, err := s.savedLookup(c, c.Param("name"))
	if err != nil {
		return err
	}
	if err := s.savedManage(c, v); err != nil {
		return err
	}
	// 256 random bits (SNAP-024): 52 characters of a 32-letter alphabet.
	code := api.PrefixShare + randToken(52)
	id := "shr_" + randToken(16)
	now := store.Now()
	if _, err := s.DB.Exec(`INSERT INTO snapshot_shares(id,saved_id,code_hash,code_prefix,status,created_by,created_at) VALUES(?,?,?,?,?,?,?)`,
		id, v.ID, hashToken(code), code[:12], "active", c.P.User.ID, now); err != nil {
		return err
	}
	return c.OK(201, "snapshot.share.created", map[string]any{"name": v.Name, "shareCode": code,
		"share": shareJSON(id, code[:12], "active", 0, sql.NullInt64{}, sql.NullInt64{}, now)})
}

func (s *Server) handleListShares(c *Ctx) error {
	v, err := s.savedLookup(c, c.Param("name"))
	if err != nil {
		return err
	}
	if err := s.savedManage(c, v); err != nil {
		return err
	}
	rows, err := s.DB.Query(`SELECT id,code_prefix,status,use_count,last_used_at,revoked_at,created_at FROM snapshot_shares WHERE saved_id=? ORDER BY created_at DESC`, v.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, prefix, status string
		var uses int
		var last, rev sql.NullInt64
		var created int64
		if rows.Scan(&id, &prefix, &status, &uses, &last, &rev, &created) == nil {
			out = append(out, shareJSON(id, prefix, status, uses, last, rev, created))
		}
	}
	return c.OK(200, "snapshot.share.list", map[string]any{"name": v.Name, "shares": out})
}

func (s *Server) handleRevokeShare(c *Ctx) error {
	v, err := s.savedLookup(c, c.Param("name"))
	if err != nil {
		return err
	}
	if err := s.savedManage(c, v); err != nil {
		return err
	}
	_, _ = s.DB.Exec(`UPDATE snapshot_shares SET status='revoked', revoked_at=? WHERE id=? AND saved_id=? AND status='active'`, store.Now(), c.Param("shareId"), v.ID)
	var id, prefix, status string
	var uses int
	var last, rev sql.NullInt64
	var created int64
	if s.DB.QueryRow(`SELECT id,code_prefix,status,use_count,last_used_at,revoked_at,created_at FROM snapshot_shares WHERE id=? AND saved_id=?`, c.Param("shareId"), v.ID).
		Scan(&id, &prefix, &status, &uses, &last, &rev, &created) != nil {
		return errNotFound("Share")
	}
	return c.OK(200, "snapshot.share.revoked", map[string]any{"name": v.Name, "share": shareJSON(id, prefix, status, uses, last, rev, created)})
}

// sourceSnap is a resolved "from" of a create.
type sourceSnap struct {
	Kind, ID, Name, SnapshotID, Size, ShareID string
	OwnerID, SourceSandboxID                  string // the saver and the sandbox it was saved from
	Env                                       map[string]string
	NoEnv, Shared                             bool
	// Foreign: saved by another account (share code) or another org member;
	// the deploy scrubs the saver's credentials and gets none of their
	// per-sandbox env (SNAP-023).
	Foreign bool
}

// resolveFrom resolves a saved snapshot name (own, or of an org the user is
// in) or a share code. A name is looked up in the create's org first
// (`--org acme new --from web-stack` deploys acme's team image), then in the
// caller's own, then in their other organizations.
func (s *Server) resolveFrom(u *User, from string, org *Org) (*sourceSnap, error) {
	notFound := Err(404, "snapshot_not_found", "No saved snapshot or share code "+from+".")
	var v *savedSnap
	var err error
	src := &sourceSnap{}
	if strings.HasPrefix(from, api.PrefixShare) || strings.HasPrefix(from, api.PrefixShareLegacy) {
		var savedID string
		if s.DB.QueryRow(`SELECT id,saved_id FROM snapshot_shares WHERE code_hash=? AND status='active'`, hashToken(from)).Scan(&src.ShareID, &savedID) != nil {
			return nil, notFound
		}
		v, err = scanSaved(s.DB.QueryRow(`SELECT `+savedCols+` FROM saved_snapshots WHERE id=?`, savedID))
		src.Kind, src.Shared = "share", true
	} else {
		err = sql.ErrNoRows
		if org != nil {
			v, err = s.savedByName(org.ID, from)
		}
		if err != nil {
			v, err = s.savedByName(u.ID, from)
		}
		if err != nil {
			v, err = scanSaved(s.DB.QueryRow(`SELECT `+savedCols+` FROM saved_snapshots WHERE name=? AND owner_id IN (SELECT org_id FROM org_members WHERE user_id=?) ORDER BY updated_at DESC LIMIT 1`, from, u.ID))
		}
		src.Kind = "saved"
	}
	if err != nil {
		return nil, notFound
	}
	if v.SnapshotID == "" {
		if v.Status == "failed" {
			return nil, Err(409, "snapshot_failed", "The saved snapshot "+v.Name+" failed to save: "+v.Error)
		}
		return nil, Err(409, "snapshot_not_ready", "The saved snapshot "+v.Name+" is still saving.")
	}
	if v.SavedBy != u.ID {
		src.Env, src.Foreign = nil, true
	} else {
		src.Env = v.Env
		src.Shared = false
	}
	src.ID, src.Name, src.SnapshotID, src.Size, src.NoEnv = v.ID, v.Name, v.SnapshotID, v.Size, v.NoEnv
	src.OwnerID, src.SourceSandboxID = v.SavedBy, v.SourceSandboxID
	return src, nil
}

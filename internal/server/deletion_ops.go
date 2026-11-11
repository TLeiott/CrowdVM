package server

// Deletion operations (DATA-*, API-140; Boat DeletionOperation): a target
// leaves every list at once, and its operation reports "completed" only
// once its data is confirmed gone:
//
//   - sandbox: its node confirmed the machine and overlay are removed (or the
//     sandbox never had a machine), and
//   - every snapshot of the target is purged from the server and from every
//     node that held a copy, or is still read by a newer snapshot, fork or
//     saved snapshot (Boat: completed; that data goes with its last user).
//
// A snapshot a node is still uploading is never purged mid-upload: the
// operation shows stage "waiting_for_uploads" until the upload ended and
// what it stored is erased. Periodic layers are recorded when the node takes
// them (recordTaken), with their parent and the node as location; an upload
// in flight that still has no row (its report failed) gets a deleted row
// the same way, so GC erases it like any other copy and keeps what it
// builds on. Uploads that land for deleted data are refused (410) or erased
// (onSnapshotUploaded).
//
// A node that is offline or fails keeps the operation "processing"/
// "retrying" (attempts counted); it is retried on every maintenance pass
// and when a node reconnects, and turns "blocked" after deletionBlockedAfter
// (still retried).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// deletionBlockedAfter: a deletion that keeps failing this long is "blocked".
var deletionBlockedAfter = 24 * time.Hour

func init() {
	store.Register("deletion_ops", store.Migration{Version: 1, SQL: `
ALTER TABLE deletion_ops ADD COLUMN node_id TEXT;
ALTER TABLE deletion_ops ADD COLUMN vm_removed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deletion_ops ADD COLUMN last_error TEXT;
`})
}

// processDeletionOps advances every unfinished deletion operation once.
func (s *Server) processDeletionOps(ctx context.Context) {
	s.deletionMu.Lock()
	defer s.deletionMu.Unlock()
	type op struct {
		id, kind, target, node, problem string
		vmRemoved, blocked              bool
		requested                       int64
	}
	rows, err := s.DB.Query(`SELECT id,kind,target_id,COALESCE(node_id,''),vm_removed,requested_at FROM deletion_ops WHERE status<>'completed'`)
	if err != nil {
		return
	}
	var ops []*op
	for rows.Next() {
		o := &op{}
		if rows.Scan(&o.id, &o.kind, &o.target, &o.node, &o.vmRemoved, &o.requested) == nil {
			ops = append(ops, o)
		}
	}
	rows.Close()
	if len(ops) == 0 {
		return
	}
	// Machines first: once the VM is gone it takes no new snapshots, so the
	// uploads read below are all there will be.
	for _, o := range ops {
		if o.kind != "sandbox" {
			o.vmRemoved = true // snapshot ops have no VM to remove
		}
		if o.vmRemoved {
			continue
		}
		switch c := s.nodes.get(o.node); {
		case o.node == "":
			o.vmRemoved = true
		case c == nil && !s.nodeExists(o.node):
			// m5: its node was removed. A stopped sandbox has no machine
			// left (its disk is a snapshot, tracked by location); a running
			// one's disk stays on that machine and nothing can reach it.
			var state string
			_ = s.DB.QueryRow(`SELECT state FROM sandboxes WHERE id=?`, o.target).Scan(&state)
			if state == "archived" || state == "cancelled" {
				o.vmRemoved = true
			} else {
				o.problem, o.blocked = "node_removed: the node that ran this sandbox was removed; its disk stays there until its owner wipes the node's data directory", true
			}
		case c == nil:
			o.problem = "node_offline: the node that ran this sandbox is offline; its disk is erased when it reconnects"
		default:
			cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			err := c.Do(cctx, "DELETE", "/v1/vms/"+o.target, nil, nil)
			cancel()
			var ne *NodeError
			if err == nil || (errors.As(err, &ne) && ne.Status == 404) {
				o.vmRemoved = true
			} else {
				o.problem = "node_error: " + err.Error()
			}
		}
	}
	up := s.uploadsInFlight(ctx)
	for id, f := range up {
		s.trackDeletedUpload(id, f)
	}
	needed := s.gcSnapshotsNeeded(ctx, up)
	now := store.Now()
	for _, o := range ops {
		q := `SELECT id, purged_at IS NOT NULL FROM snapshots WHERE id=?`
		if o.kind == "sandbox" {
			q = `SELECT id, purged_at IS NOT NULL FROM snapshots WHERE sandbox_id=?`
		}
		waiting, uploading := 0, 0
		if r, err := s.DB.Query(q, o.target); err == nil {
			for r.Next() {
				var id string
				var purged bool
				if r.Scan(&id, &purged) == nil && !purged && !needed[id] {
					waiting++
					if _, ok := up[id]; ok {
						uploading++
					}
				}
			}
			r.Close()
		}
		if o.vmRemoved && waiting == 0 {
			_, _ = s.DB.Exec(`UPDATE deletion_ops SET status='completed', stage='completed', vm_removed=1, last_error=NULL, attempt_count=attempt_count+1, completed_at=? WHERE id=?`, now, o.id)
			continue
		}
		status, stage, problem := "processing", "retrying", o.problem
		switch removed := s.copiesOnRemovedNodes(o.kind, o.target); {
		case problem != "":
		case uploading > 0:
			stage = "waiting_for_uploads"
			problem = fmt.Sprintf("waiting_for_uploads: %d snapshot(s) of it are still uploading from their node; they are erased once the upload ends", uploading)
		case removed > 0:
			// m5: nothing can reach a removed node any more.
			status = "blocked"
			problem = fmt.Sprintf("node_removed: %d snapshot copy(ies) stayed on a node that was removed while offline; its owner must wipe that node's data directory", removed)
		default:
			problem = "snapshot copies are still held by a node that is offline or did not confirm the deletion"
		}
		if o.blocked || (status == "processing" && now-o.requested > deletionBlockedAfter.Milliseconds()) {
			status = "blocked"
		}
		_, _ = s.DB.Exec(`UPDATE deletion_ops SET status=?, stage=?, vm_removed=?, last_error=?, attempt_count=attempt_count+1 WHERE id=?`,
			status, stage, boolInt(o.vmRemoved), problem, o.id)
	}
}

// kickDeletions advances unfinished deletion operations in the background
// (e.g. when an upload they wait for ended).
func (s *Server) kickDeletions() {
	var n int
	if s.DB.QueryRow(`SELECT COUNT(*) FROM deletion_ops WHERE status<>'completed'`).Scan(&n) == nil && n > 0 {
		s.Go(s.processDeletionOps)
	} else {
		s.Go(s.gcSnapshots)
	}
}

// copiesOnRemovedNodes counts unpurged snapshot copies of a deletion target
// that are recorded on nodes no longer registered (nodes remove while the
// node was offline).
func (s *Server) copiesOnRemovedNodes(kind, target string) int {
	col := "s.id"
	if kind == "sandbox" {
		col = "s.sandbox_id"
	}
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM snapshot_locations l JOIN snapshots s ON s.id=l.snapshot_id
		WHERE `+col+`=? AND s.purged_at IS NULL AND NOT EXISTS (SELECT 1 FROM nodes WHERE id=l.node_id)`, target).Scan(&n)
	return n
}

func (s *Server) nodeExists(id string) bool {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id=?`, id).Scan(&n)
	return n > 0
}

// flight is a snapshot upload a connected node has not finished.
type flight struct{ node, sandbox, parent string }

// uploadsInFlight asks every connected node which snapshots it is still
// uploading (snapshot id -> node and sandbox). Nodes that do not answer
// count as uploading nothing.
func (s *Server) uploadsInFlight(ctx context.Context) map[string]flight {
	up := map[string]flight{}
	for id, c := range s.nodes.online() {
		list, ok := s.nodeSnapshots(ctx, c, true)
		if !ok {
			continue
		}
		for _, ns := range list {
			if ns.Uploading {
				up[ns.ID] = flight{node: id, sandbox: ns.SandboxID, parent: ns.ParentID}
			}
		}
	}
	return up
}

// nodeSnapshots reads GET /v1/snapshots of a node (ok=false when it did not
// answer, e.g. a node that predates the call).
func (s *Server) nodeSnapshots(ctx context.Context, c *NodeConn, uploadingOnly bool) ([]proto.NodeSnapshot, bool) {
	var out struct {
		Snapshots []proto.NodeSnapshot `json:"snapshots"`
	}
	path := "/v1/snapshots"
	if uploadingOnly {
		path += "?uploading=true"
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := c.Do(cctx, "GET", path, nil, &out); err != nil {
		return nil, false
	}
	return out.Snapshots, true
}

// trackDeletedUpload records an upload of a deleted sandbox the server has
// no row for (a periodic layer whose report failed) as a deleted snapshot
// held by its node, with its parent: the deletion waits for it, GC erases
// it once the upload ended and keeps what it builds on.
func (s *Server) trackDeletedUpload(id string, f flight) {
	var deleted bool
	if f.sandbox == "" || s.DB.QueryRow(`SELECT deleted_at IS NOT NULL FROM sandboxes WHERE id=?`, f.sandbox).Scan(&deleted) != nil || !deleted {
		return
	}
	s.recordTaken(f.node, proto.SnapshotRef{ID: id, ParentID: f.parent, SandboxID: f.sandbox})
}

// snapshotGone reports whether data uploaded as snapshot id must not be
// kept: its row is purged, or it has no row and its sandbox (from the node's
// ref; "" = unknown, kept) is deleted or does not exist.
func (s *Server) snapshotGone(id, sandboxID string) bool {
	var purged bool
	err := s.DB.QueryRow(`SELECT purged_at IS NOT NULL FROM snapshots WHERE id=?`, id).Scan(&purged)
	if err == nil {
		return purged
	}
	if !errors.Is(err, sql.ErrNoRows) || sandboxID == "" {
		return false
	}
	var del sql.NullInt64
	err = s.DB.QueryRow(`SELECT deleted_at FROM sandboxes WHERE id=?`, sandboxID).Scan(&del)
	return errors.Is(err, sql.ErrNoRows) || (err == nil && del.Valid)
}

// dropNodeSnapshot erases a snapshot the server keeps nothing of: its blob
// (if any) and the node's copy.
func (s *Server) dropNodeSnapshot(nodeID, id string) {
	_ = s.Blobs.Delete(store.BlobSnapshot, id)
	c := s.nodes.get(nodeID)
	if c == nil {
		return
	}
	s.Go(func(ctx context.Context) {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_ = c.Do(cctx, "DELETE", "/v1/snapshots/"+id, nil, nil)
	})
}

// reconcileNodeSnapshots (on node connect) erases the node's copies of
// snapshots that are purged, or that have no row and belong to a deleted or
// unknown sandbox, and records the node as a location of the others so GC
// reaches them. Uploads in flight and snapshots of a live (or unknown)
// sandbox without a row are left alone: they may still become its head.
func (s *Server) reconcileNodeSnapshots(ctx context.Context, c *NodeConn) {
	list, ok := s.nodeSnapshots(ctx, c, false)
	if !ok {
		return
	}
	for _, ns := range list {
		if ns.Uploading {
			continue
		}
		var purged bool
		err := s.DB.QueryRow(`SELECT purged_at IS NOT NULL FROM snapshots WHERE id=?`, ns.ID).Scan(&purged)
		switch {
		case err == nil && !purged:
			s.addLocation(ns.ID, c.ID)
		case err == nil || s.snapshotGone(ns.ID, ns.SandboxID):
			s.Log.Info("erasing a node's copy of deleted data", "node", c.ID, "snapshot", ns.ID, "sandbox", ns.SandboxID)
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_ = c.Do(cctx, "DELETE", "/v1/snapshots/"+ns.ID, nil, nil)
			cancel()
		}
	}
}

// orphanBlobAge: a snapshot blob with no row is erased once this old (a
// blob may land a moment before its row is recorded).
var orphanBlobAge = 15 * time.Minute

// sweepOrphanBlobs erases snapshot blobs whose row is purged or missing.
func (s *Server) sweepOrphanBlobs() {
	// Partial uploads a crash left (an upload in progress keeps writing, so
	// its file is never an hour old).
	s.Blobs.CleanTemp(time.Hour)
	list, err := s.Blobs.List(store.BlobSnapshot)
	if err != nil {
		return
	}
	for _, bi := range list {
		var purged bool
		err := s.DB.QueryRow(`SELECT purged_at IS NOT NULL FROM snapshots WHERE id=?`, bi.ID).Scan(&purged)
		if err == nil && !purged {
			continue
		}
		if errors.Is(err, sql.ErrNoRows) {
			fi, serr := os.Stat(bi.Path)
			if serr != nil || time.Since(fi.ModTime()) < orphanBlobAge {
				continue
			}
		} else if err != nil {
			continue
		}
		if err := s.Blobs.Delete(store.BlobSnapshot, bi.ID); err == nil || errors.Is(err, fs.ErrNotExist) {
			s.Log.Info("erased an orphan snapshot blob", "snapshot", bi.ID, "bytes", bi.Size)
		}
	}
}

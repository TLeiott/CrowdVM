package server

// Node evacuation (node-ui design, "Evacuate"): a node's owner (or an
// admin) takes the machine back. The node is drained and its sandboxes'
// owners get a notice. At the deadline the reaper stops every sandbox still
// running there through the normal stop path (pre-flight save, pre-stop
// hook, disk saved; a failed save refuses the stop and it is retried, never
// forced), and afterStopResume boots each one on another node
// (extra.evacuateFrom). --no-snapshots sandboxes are erased by their stop.
// The evacuation ends when no sandbox is left on the node and no layer
// something needs has its only copy there (a stop's layer uploads in the
// background); the node stays drained. Node earnings for the heartbeat
// live here too.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

func init() {
	store.Register("node_evacuations", store.Migration{Version: 1, SQL: `
CREATE TABLE node_evacuations (
  node_id TEXT PRIMARY KEY,
  started_at INTEGER NOT NULL,
  deadline INTEGER NOT NULL
);`}, store.Migration{Version: 2, SQL: `
CREATE INDEX IF NOT EXISTS credit_ledger_node ON credit_ledger(node_id, kind, created_at);`})
}

// MaxEvacuationGrace caps the grace period of an evacuation.
const MaxEvacuationGrace = 24 * time.Hour

// onNodeStates: a sandbox in one of these states still occupies its node.
const onNodeStates = `state NOT IN ('archived','error','cancelled')`

// offlineOnNode: a sandbox whose node went offline is still there (it comes
// back to ready when the node reconnects).
const offlineOnNode = `(state='error' AND error='node_offline')`

// evacuation returns node id's active evacuation (nil: none).
func (s *Server) evacuation(nodeID string) *proto.Evacuation {
	var started, deadline int64
	if s.DB.QueryRow(`SELECT started_at, deadline FROM node_evacuations WHERE node_id=?`, nodeID).Scan(&started, &deadline) != nil {
		return nil
	}
	return &proto.Evacuation{StartedAt: store.Time(started), Deadline: store.Time(deadline)}
}

// sandboxesOn lists the sandboxes that still occupy node id (cond narrows it).
func (s *Server) sandboxesOn(nodeID, cond string) []*Sandbox {
	rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE node_id=? AND deleted_at IS NULL AND `+onNodeStates+cond, nodeID)
	if err != nil {
		return nil
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	var out []*Sandbox
	for _, id := range ids {
		if sb, err := s.SandboxByID(id); err == nil {
			out = append(out, sb)
		}
	}
	return out
}

func clockUTC(t time.Time) string { return t.UTC().Format("15:04") + " UTC" }

// handleEvacuateNode: POST /nodes/{nodeId}/evacuate {"graceSec": N}.
func (s *Server) handleEvacuateNode(c *Ctx) error {
	n, err := s.nodeByRef(c.Param("nodeId"))
	if err != nil {
		return errNotFound("Node")
	}
	if !s.canManageNode(c.P, n) {
		return Err(403, "forbidden", "Only the node owner or an administrator can evacuate it.")
	}
	var req struct {
		GraceSec *int `json:"graceSec"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	grace := 600
	if req.GraceSec != nil {
		grace = *req.GraceSec
	}
	if grace < 0 || time.Duration(grace)*time.Second > MaxEvacuationGrace {
		return Err(400, "invalid_grace", fmt.Sprintf("graceSec must be between 0 and %d.", int(MaxEvacuationGrace.Seconds())))
	}
	now := store.Now()
	deadline := now + int64(grace)*1000
	prev := s.evacuation(n.ID)
	_, _ = s.DB.Exec(`UPDATE nodes SET disabled=1 WHERE id=?`, n.ID)
	if _, err := s.DB.Exec(`INSERT INTO node_evacuations(node_id,started_at,deadline) VALUES(?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET deadline=excluded.deadline`, n.ID, now, deadline); err != nil {
		return err
	}
	at := clockUTC(store.Time(deadline))
	// Owners are told once, and again only when the deadline moves earlier.
	notify := prev == nil || store.Time(deadline).Before(prev.Deadline)
	for _, sb := range s.sandboxesOn(n.ID, "") {
		if !notify {
			break // a re-POST that only moves the deadline later
		}
		if sb.SnapshotsEnabled {
			s.notifySandbox(sb, "node_evacuation", "warning", 0, "Sandbox will move to another node",
				"The machine hosting "+sb.Name+" is being reclaimed at "+at+". Stop or snapshot it before then to choose the moment; after that it is stopped and resumed on another node.")
		} else {
			s.notifySandbox(sb, "node_evacuation", "warning", 0, "Sandbox will be erased",
				"The machine hosting "+sb.Name+" is being reclaimed at "+at+". This sandbox has snapshots off, so it can't be moved and will be erased at "+at+". Copy out anything you need now.")
		}
	}
	s.Log.Info("node evacuation", "node", n.ID, "graceSec", grace, "by", c.P.User.ID)
	s.evacuateStep(n.ID) // ends at once on an empty node; stops now when grace is 0
	n, _ = s.NodeByID(n.ID)
	return c.OK(200, "node.evacuating", map[string]any{"node": s.nodeJSON(n, true)})
}

// handleCancelEvacuation: DELETE /nodes/{nodeId}/evacuate, allowed until
// the deadline. The node stays drained.
func (s *Server) handleCancelEvacuation(c *Ctx) error {
	n, err := s.nodeByRef(c.Param("nodeId"))
	if err != nil {
		return errNotFound("Node")
	}
	if !s.canManageNode(c.P, n) {
		return Err(403, "forbidden", "Only the node owner or an administrator can cancel its evacuation.")
	}
	ev := s.evacuation(n.ID)
	if ev == nil {
		return Err(404, "not_evacuating", "This node is not being evacuated.")
	}
	res, _ := s.DB.Exec(`DELETE FROM node_evacuations WHERE node_id=? AND deadline>?`, n.ID, store.Now())
	if k, _ := res.RowsAffected(); k == 0 {
		return Err(409, "evacuation_started", "The grace period is over: the sandboxes are already being moved.")
	}
	n, _ = s.NodeByID(n.ID)
	return c.OK(200, "node.evacuation_cancelled", map[string]any{"node": s.nodeJSON(n, true)})
}

// reapEvacuations runs every evacuation one step (reaper pass).
func (s *Server) reapEvacuations() {
	rows, err := s.DB.Query(`SELECT node_id FROM node_evacuations`)
	if err != nil {
		return
	}
	var list []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			list = append(list, id)
		}
	}
	rows.Close()
	for _, id := range list {
		s.evacuateStep(id)
	}
}

// onlyCopiesOn returns the layers something still needs (snapshotsNeeded)
// whose only copy is on node id: not stored on the server yet, no other
// node holds them. Removing the node loses them.
func (s *Server) onlyCopiesOn(nodeID string) []string {
	rows, err := s.DB.Query(`SELECT s.id FROM snapshot_locations l JOIN snapshots s ON s.id=l.snapshot_id
		WHERE l.node_id=? AND s.uploaded_at IS NULL AND s.purged_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM snapshot_locations o WHERE o.snapshot_id=s.id AND o.node_id<>l.node_id)
		ORDER BY s.generation, s.created_at`, nodeID)
	if err != nil {
		return nil
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return nil
	}
	needed, _ := s.snapshotsNeeded(false)
	if needed == nil {
		return ids // database error: keep them all
	}
	out := ids[:0]
	for _, id := range ids {
		if needed[id] {
			out = append(out, id)
		}
	}
	return out
}

// evacuateStep ends the evacuation once no sandbox is on the node (one
// whose node is offline counts) and no needed layer is only there (it asks
// the node to upload those), and from the deadline on stops every sandbox
// still running there. A stop that is refused (its save failed) stays
// pending and is retried by the reaper; a sandbox a resume brought back is
// stopped again on the next pass.
func (s *Server) evacuateStep(nodeID string) {
	ev := s.evacuation(nodeID) // re-read: a cancel just before the deadline wins
	if ev == nil {
		return
	}
	var offline int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE node_id=? AND deleted_at IS NULL AND `+offlineOnNode, nodeID).Scan(&offline)
	left := s.sandboxesOn(nodeID, "")
	only := s.onlyCopiesOn(nodeID)
	if len(only) > 0 && s.evacUploadDue(nodeID) {
		s.Go(func(ctx context.Context) {
			defer s.evacUploadDone(nodeID)
			for _, id := range only {
				s.requestUpload(ctx, id)
			}
		})
	}
	if len(left) == 0 && offline == 0 && len(only) == 0 {
		_, _ = s.DB.Exec(`DELETE FROM node_evacuations WHERE node_id=?`, nodeID)
		return
	}
	if time.Now().Before(ev.Deadline) {
		return
	}
	for _, sb := range left {
		if !sb.Usable() && sb.State != api.StateProvisioned {
			continue // starting (stopped on a later pass) or already stopping
		}
		if sb.StopID != "" {
			if o, err := s.stopByID(sb.StopID); err == nil && o.Status == "failing" {
				// A refused stop still waits: its own (retried by the reaper),
				// or the owner's / a TTL stop, which takes it off the node
				// when it completes. Not turned into a move.
				continue
			}
		}
		// requestStop marks a new stop of ours (extra.evacuateFrom) so
		// afterStopResume boots the sandbox on another node.
		if _, err := s.requestStop(sb, false, "evacuated"); err != nil {
			s.Log.Warn("evacuation stop", "sandbox", sb.ID, "node", nodeID, "err", err)
		}
	}
}

// evacUploadEvery throttles the upload requests an evacuation sends a node
// (the reaper passes every few seconds; a failed upload restarts whole).
var evacUploadEvery = 30 * time.Second

// evacUploadDue: no request round is running for node id and the last one
// started evacUploadEvery ago; it then marks one running.
func (s *Server) evacUploadDue(nodeID string) bool {
	s.earnMu.Lock()
	defer s.earnMu.Unlock()
	if s.evacUploads == nil {
		s.evacUploads = map[string]time.Time{}
	}
	if at, ok := s.evacUploads[nodeID]; ok && (at.IsZero() || time.Since(at) < evacUploadEvery) {
		return false // zero: a round is still running
	}
	s.evacUploads[nodeID] = time.Time{}
	return true
}

func (s *Server) evacUploadDone(nodeID string) {
	s.earnMu.Lock()
	s.evacUploads[nodeID] = time.Now()
	s.earnMu.Unlock()
}

// evacuationResume (from afterStopResume): boot a sandbox an evacuation
// stopped on another node. When no other node could ever host it (or it is
// pinned to the evacuated node), it stays stopped and its owner is told.
// Not counted as a start of the owner's (the move was not theirs).
func (s *Server) evacuationResume(sb *Sandbox, from string) {
	spec, _ := api.SizeByName(sb.Size)
	if !s.couldEverPlace(schedReq{Size: spec, Placement: sb.Placement, Exclude: from, Owners: s.ownersFor(sb.OwnerID, sb.OrgID)}) {
		s.evacuationStranded(sb)
		return
	}
	_ = s.updateSandbox(sb.ID, map[string]any{"error": nil, "stopped_at": nil, "archive_after": ttlDeadline(sb)})
	s.queueBoot(sb.ID, pendingBoot{Kind: proto.BootResume, Parent: sb.HeadSnapshotID, Exclude: from, Reason: "evacuated"})
	s.Bus.Publish(Event{Type: EvSandboxState, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, PreviousState: api.StateArchived, State: api.StateProvisioning})
	s.Log.Info("resuming evacuated sandbox on another node", "sandbox", sb.ID, "from", from)
}

// evacuationStranded: no other node took an evacuated sandbox (none fits,
// or none had room within CapacityWait). It stays stopped with its disk
// saved, and its owner is told.
func (s *Server) evacuationStranded(sb *Sandbox) {
	if sb.starting() {
		s.transition(sb.ID, startingStates, api.StateArchived, map[string]any{"stopped_at": store.Now(), "archive_after": nil})
	}
	s.notifySandbox(sb, "evacuation_stranded", "warning", 0, "Sandbox stopped",
		"The machine hosting "+sb.Name+" was reclaimed. Its disk was saved, but no other node can host it right now, so it stays stopped. Resume it when a node is available.")
}

func ttlDeadline(sb *Sandbox) any {
	if sb.TTLSeconds == nil {
		return nil
	}
	return store.Now() + int64(*sb.TTLSeconds)*1000
}

// ---------------------------------------------------------------------------
// Earnings in the heartbeat
// ---------------------------------------------------------------------------

type nodeEarnings struct {
	at           time.Time
	total, day24 float64
}

// EarningsCacheTTL: how long a node's summed earnings are reused (heartbeats
// come every 10 s).
var EarningsCacheTTL = time.Minute

// nodeEarned returns what node id earned in total and in the last 24 h,
// summed from the ledger at most once per EarningsCacheTTL.
func (s *Server) nodeEarned(nodeID string) (total, day float64) {
	s.earnMu.Lock()
	if s.earned == nil {
		s.earned = map[string]nodeEarnings{}
	}
	e, ok := s.earned[nodeID]
	s.earnMu.Unlock()
	if ok && time.Since(e.at) < EarningsCacheTTL {
		return e.total, e.day24
	}
	var t, d sql.NullFloat64
	_ = s.DB.QueryRow(`SELECT SUM(delta), SUM(CASE WHEN created_at>? THEN delta END) FROM credit_ledger WHERE node_id=? AND kind='earn'`,
		store.Now()-24*3600_000, nodeID).Scan(&t, &d)
	e = nodeEarnings{at: time.Now(), total: round6(t.Float64), day24: round6(d.Float64)}
	s.earnMu.Lock()
	s.earned[nodeID] = e
	s.earnMu.Unlock()
	return e.total, e.day24
}

package server

// Boots waiting for a node:
//
//   - SIZE-011: without failFast, create/resume/fork do not fail when the
//     pool is full right now; the sandbox stays "provisioning" (fork:
//     "cloning") with no node and the reaper retries placement until
//     capacityWait has passed (then error out_of_capacity). Requests that no
//     registered node could ever satisfy still fail at once.
//   - LIFE-014: a resume while the sandbox is stopping boots again on the
//     same node (its disk is there) as soon as the stop has finished.
//   - LIFE-041: a sandbox whose node was lost is rebuilt on another node
//     from its latest uploaded snapshot.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// pendingBoot is a boot waiting for placement (extra.pendingBoot).
type pendingBoot struct {
	Kind      proto.BootKind `json:"kind"`
	FirstBoot bool           `json:"firstBoot,omitempty"`
	Parent    string         `json:"parent,omitempty"`
	LiveFrom  string         `json:"liveFrom,omitempty"`
	Cleanup   string         `json:"cleanup,omitempty"`
	Prefer    string         `json:"prefer,omitempty"`  // try this node first
	Exclude   string         `json:"exclude,omitempty"` // never this node
	Since     int64          `json:"since"`
	Deadline  int64          `json:"deadline"`
	State     string         `json:"state"` // provisioning | cloning
	Reason    string         `json:"reason,omitempty"`
	NotBefore int64          `json:"notBefore,omitempty"` // a node refused it: wait for fresh capacity data
}

func pendingBootOf(sb *Sandbox) *pendingBoot {
	v, ok := sb.Extra["pendingBoot"]
	if !ok || v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	var pb pendingBoot
	if json.Unmarshal(b, &pb) != nil || pb.Kind == "" {
		return nil
	}
	return &pb
}

// couldEverPlace reports whether some registered, enabled node (online or
// not) matches req's placement and is big enough for its size.
func (s *Server) couldEverPlace(req schedReq) bool {
	nodes, err := s.Nodes()
	if err != nil {
		return false
	}
	for _, n := range nodes {
		// A paused node counts: its owner can resume it, so a request waits
		// for it (only the scheduler and failFast skip it).
		if n.Disabled || n.ID == req.Exclude || (req.Only != "" && n.ID != req.Only) {
			continue
		}
		if matchReason(n, req.Placement, req.Owners) != "" {
			continue
		}
		c := n.Info.Capacity
		if c.VCPU >= req.Size.VCPU && c.MemoryMiB >= req.Size.MemoryMiB && (c.DiskGiB == 0 || c.DiskGiB >= req.Size.DiskGiB) {
			return true
		}
	}
	return false
}

// neverFitsErr: no node of the pool (that the request may use) can ever
// host this size, so waiting is pointless: a final error naming the sizes
// that are available instead of a retryable out_of_capacity.
func (s *Server) neverFitsErr(req schedReq, orig error) error {
	small := req
	small.Size = api.Sizes[0]
	if !s.couldEverPlace(small) {
		return orig // not about the size: no usable node at all (its own error says why)
	}
	var ok []string
	for _, sz := range api.Sizes {
		r := req
		r.Size = sz
		if s.couldEverPlace(r) {
			ok = append(ok, sz.Name)
		}
	}
	where := "No node in this pool"
	if !req.Placement.empty() || req.Only != "" {
		where = "No node that matches the placement" // --node, --gpu, ...: not the whole pool
	}
	msg := fmt.Sprintf("%s can host a %s sandbox (%d vCPU, %d GiB memory).", where, req.Size.Name, req.Size.VCPU, req.Size.MemoryMiB/1024)
	if len(ok) > 0 {
		msg += " Sizes that fit: " + strings.Join(ok, ", ") + " (--type " + ok[len(ok)-1] + "). See `crowdvm limits`."
	} else {
		msg += " No node is online that this sandbox may use."
	}
	return ErrD(409, "size_unavailable", msg, map[string]any{"type": req.Size.Name, "availableTypes": ok, "retryable": false})
}

// queueBoot parks sb (no node) until a node has room, then boots it.
func (s *Server) queueBoot(id string, pb pendingBoot) {
	now := store.Now()
	if pb.Since == 0 {
		pb.Since = now
	}
	if pb.Deadline == 0 {
		pb.Deadline = now + s.Cfg.CapacityWait.Milliseconds()
	}
	if pb.State == "" {
		pb.State = api.StateProvisioning
	}
	_ = s.SetSandboxExtra(id, "pendingBoot", pb)
	_, _ = s.DB.Exec(`UPDATE sandboxes SET node_id=NULL, state=?, error=NULL, updated_at=? WHERE id=?`, pb.State, now, id)
	s.Go(func(ctx context.Context) { s.tryPlace(id) })
}

// tryPlace places a waiting sandbox and starts its boot. It reports
// whether the boot started.
func (s *Server) tryPlace(id string) bool {
	unlock := s.sbLocks.Lock("place:" + id)
	defer unlock()
	sb, err := s.SandboxByID(id)
	if err != nil || sb.DeletedAt != nil || !sb.starting() || sb.NodeID != "" {
		return false
	}
	pb := pendingBootOf(sb)
	if pb == nil || pb.NotBefore > store.Now() {
		return false
	}
	spec, _ := api.SizeByName(sb.Size)
	chainFrom := pb.Parent
	if pb.LiveFrom != "" && chainFrom == "" {
		if src, err := s.SandboxByID(pb.LiveFrom); err == nil {
			chainFrom = src.HeadSnapshotID
		}
	}
	exclude := pb.Exclude
	if pb.Reason == "evacuated" && s.evacuation(exclude) == nil {
		exclude = "" // the evacuation is over: its node may take it again (bugs-r13-opus 10)
	}
	req := schedReq{Size: spec, Placement: sb.Placement, Chain: s.snapshotChain(chainFrom), ImageID: sb.ImageID, Exclude: exclude, Owners: s.ownersFor(sb.OwnerID, sb.OrgID)}
	var placeErr error
	if pb.Prefer != "" {
		req.Only = pb.Prefer
		_, placeErr = s.placeSandbox(id, pb.State, req)
		req.Only = ""
	}
	if pb.Prefer == "" || placeErr != nil {
		_, placeErr = s.placeSandbox(id, pb.State, req)
	}
	if placeErr != nil {
		if store.Now() > pb.Deadline {
			_ = s.SetSandboxExtra(id, "pendingBoot", nil)
			if pb.Reason == "evacuated" {
				s.evacuationStranded(sb) // its disk is saved: stopped, not failed
				return false
			}
			s.abortBoot(sb, pb.Kind, pb.Parent, "out_of_capacity: no node had room for this sandbox within "+s.Cfg.CapacityWait.String())
		}
		return false
	}
	_ = s.SetSandboxExtra(id, "pendingBoot", nil)
	sb, err = s.SandboxByID(id)
	if err != nil {
		return false
	}
	s.startBoot(sb, bootOpts{Kind: pb.Kind, FirstBoot: pb.FirstBoot, Parent: pb.Parent, LiveFrom: pb.LiveFrom, Cleanup: pb.Cleanup})
	return true
}

// abortBoot ends a boot that found no node: an aborted create or fork never
// had a disk, so it is cancelled (LIFE-025); a resume fails into error.
func (s *Server) abortBoot(sb *Sandbox, kind proto.BootKind, parent, msg string) {
	if !sb.FirstBootDone && (kind == proto.BootCreate || kind == proto.BootFork) {
		sets := map[string]any{"error": msg, "stopped_at": store.Now()}
		if kind == proto.BootFork && parent != "" {
			sets["head_snapshot_id"] = parent // resume boots the fork from its source's disk
		}
		if _, ok := s.transition(sb.ID, startingStates, api.StateCancelled, sets); ok {
			s.closeRun(sb.ID)
		}
		return
	}
	s.failSandbox(sb.ID, msg)
}

// placeWaiting retries every waiting boot (reaper pass).
func (s *Server) placeWaiting() {
	rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE deleted_at IS NULL AND node_id IS NULL AND state IN ('provisioning','cloning') AND json_extract(extra,'$.pendingBoot.kind') IS NOT NULL ORDER BY created_at`)
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
		s.tryPlace(id)
	}
}

// layerWaits: sandbox id -> the snapshot its boot waits to be uploaded.
var layerWaits sync.Map

// waitingJSON adds the capacity or upload wait to the sandbox object.
func waitingJSON(sb *Sandbox, m map[string]any) {
	if pb := pendingBootOf(sb); pb != nil && sb.starting() && sb.NodeID == "" {
		m["waitingForCapacity"] = true
		m["waitingSince"] = tsMs(pb.Since)
		m["waitDeadline"] = tsMs(pb.Deadline)
		m["statusMessage"] = "Waiting for a node with room for a " + sb.Size + " sandbox (vCPU, memory or disk). It starts as soon as one frees up; pass failFast to get 503 no_ready_machine instead."
	}
	if v, ok := layerWaits.Load(sb.ID); ok && sb.starting() {
		m["waitingForSnapshot"] = v
		m["statusMessage"] = "Waiting for snapshot " + v.(string) + " to finish uploading to the server from the node that holds it."
	}
}

// ---------------------------------------------------------------------------
// LIFE-014: resume while stopping
// ---------------------------------------------------------------------------

// resumeStartChecks runs a start's gates (credits, concurrency, start
// window) for a resume during a stop, which answers before handleResume's
// own: a stop and a resume restarted a sandbox past them, also one stopped
// for its credit cap (bugs-r11-sonnet 4).
func (s *Server) resumeStartChecks(sb *Sandbox) error {
	owner, err := s.UserByID(sb.OwnerID)
	if err != nil {
		return err
	}
	var org *Org
	if sb.OrgID != "" {
		org, _ = s.OrgByID(sb.OrgID)
	}
	unlock := s.lockLimits()
	defer unlock()
	if err := s.checkCredits(owner, org); err != nil {
		return err
	}
	if err := s.checkConcurrent(owner, org, 1); err != nil {
		return err
	}
	return s.checkStarts(sb.Wallet(), 1)
}

// resumeDuringStop cancels a stop whose machine still runs, or boots the
// sandbox again on the same node right after the stop finished.
func (s *Server) resumeDuringStop(c *Ctx, sb *Sandbox, ttl OptInt) error {
	sets := map[string]any{}
	if ttl.Set {
		if ttl.Null {
			sets["ttl_seconds"] = nil
		} else {
			sets["ttl_seconds"] = ttl.Value
		}
	}
	_ = s.updateSandbox(sb.ID, sets)
	sb, _ = s.SandboxByID(sb.ID)
	if op, err := s.stopByID(sb.StopID); err == nil {
		_, inFlight := stopRunning.Load(op.ID)
		if node := s.nodes.get(sb.NodeID); node != nil && !inFlight && op.Status != "completed" {
			cctx, cancel := context.WithTimeout(c.R.Context(), 10*time.Second)
			var st proto.VMStatus
			err := node.Do(cctx, "GET", "/v1/vms/"+sb.ID, nil, &st)
			cancel()
			if err == nil && st.State == proto.VMRunning && st.GuestReady {
				// The machine never went down: the stop is cancelled.
				_, _ = s.DB.Exec(`UPDATE stops SET status='superseded', ended_at=? WHERE id=? AND status IN ('pending','failing')`, store.Now(), op.ID)
				up := map[string]any{"archive_after": nil, "error": nil}
				if sb.TTLSeconds != nil {
					up["archive_after"] = store.Now() + int64(*sb.TTLSeconds)*1000
				}
				if _, ok := s.transition(sb.ID, []string{api.StateArchiving}, readyState(sb), up, Event{Type: EvSandboxReady, Reason: "stop_cancelled"}); ok {
					s.ensureRun(sb.ID)
					sb, _ = s.SandboxByID(sb.ID)
					return c.OK(202, "sandbox.resuming", map[string]any{"id": sb.ID, "status": sb.State, "sandbox": s.SandboxJSON(sb, c.P), "stopCancelled": true})
				}
			}
		}
	}
	_ = s.SetSandboxExtra(sb.ID, "resumeAfterStop", true)
	sb, _ = s.SandboxByID(sb.ID)
	return c.OK(202, "sandbox.resuming", map[string]any{"id": sb.ID, "status": "resuming", "sandbox": s.SandboxJSON(sb, c.P),
		"message": "The sandbox is saving its disk; it starts again on the same machine right after."})
}

// afterStopResume runs from finishStop: a resume requested while stopping.
func (s *Server) afterStopResume(id string) {
	sb, err := s.SandboxByID(id)
	if err != nil || sb.DeletedAt != nil || sb.State != api.StateArchived {
		return
	}
	again, _ := sb.Extra["resumeAfterStop"].(bool)
	from, _ := sb.Extra["evacuateFrom"].(string) // stopped by an evacuation
	if from != "" && sb.NodeID != from {
		from = "" // stale: it moved off that node another way (rebuilt, node removed)
	}
	if sb.Extra["evacuateFrom"] != nil {
		_ = s.SetSandboxExtra(id, "evacuateFrom", nil)
	}
	if !again && from == "" {
		return
	}
	_ = s.SetSandboxExtra(id, "resumeAfterStop", nil)
	if !sb.SnapshotsEnabled {
		return // nothing was kept; the resume cannot happen
	}
	if from != "" {
		s.evacuationResume(sb, from)
		return
	}
	sets := map[string]any{"error": nil, "stopped_at": nil, "archive_after": nil}
	if sb.TTLSeconds != nil {
		sets["archive_after"] = store.Now() + int64(*sb.TTLSeconds)*1000
	}
	_ = s.updateSandbox(id, sets)
	kind, first := proto.BootResume, false
	if sb.HeadSnapshotID == "" && !sb.FirstBootDone {
		kind, first = proto.BootCreate, true
	}
	prev := sb.NodeID
	s.recordStart(sb.Wallet(), sb.OwnerID)
	s.queueBoot(id, pendingBoot{Kind: kind, FirstBoot: first, Parent: sb.HeadSnapshotID, Prefer: prev, Reason: "resume_after_stop"})
	s.Bus.Publish(Event{Type: EvSandboxState, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, PreviousState: api.StateArchived, State: api.StateProvisioning})
}

// ---------------------------------------------------------------------------
// LIFE-041: rebuild after node loss
// ---------------------------------------------------------------------------

// rebuildLost reschedules node_offline sandboxes whose latest snapshot (and
// its whole chain) is on the server.
func (s *Server) rebuildLost() {
	cut := store.Now() - s.Cfg.NodeLostGrace.Milliseconds()
	rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE deleted_at IS NULL AND state='error' AND error='node_offline' AND snapshots_enabled=1
AND head_snapshot_id IS NOT NULL AND COALESCE(degraded_since,updated_at)<?`, cut)
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
		if err != nil || s.nodes.get(sb.NodeID) != nil {
			continue // node is back: afterNodeConnect recovers it
		}
		uploaded := true
		for _, sn := range s.snapshotChain(sb.HeadSnapshotID) {
			if x, err := s.SnapshotByID(sn); err != nil || x.UploadedAt == nil {
				uploaded = false
				break
			}
		}
		if !uploaded {
			continue // the newest data is only on the lost node: hold
		}
		spec, _ := api.SizeByName(sb.Size)
		if !s.couldEverPlace(schedReq{Size: spec, Placement: sb.Placement, Exclude: sb.NodeID, Owners: s.ownersFor(sb.OwnerID, sb.OrgID)}) {
			// Pinned (--node, labels, ...) to the lost node: it waits for it.
			// Say so, or it just sits in node_offline (field test).
			if !sb.Placement.empty() && s.couldEverPlace(schedReq{Size: spec, Exclude: sb.NodeID, Owners: s.ownersFor(sb.OwnerID, sb.OrgID)}) {
				s.notifySandbox(sb, "rebuild_blocked", "warning", 24*time.Hour, "Sandbox waits for its node",
					"The node hosting "+sb.Name+" is offline, and the sandbox's placement allows no other node, so CrowdVM does not rebuild it elsewhere. "+
						"Wait for the node, or move it with a placement another node meets: crowdvm resume "+sb.ID+" --node <node> (changes after its latest snapshot are lost).")
			}
			continue
		}
		lost := sb.NodeID
		if _, ok := s.transition(id, []string{api.StateError}, api.StateProvisioning, map[string]any{"error": nil, "health": "degraded", "health_reason": "rebuilding"}); !ok {
			continue
		}
		s.queueBoot(id, pendingBoot{Kind: proto.BootResume, Parent: sb.HeadSnapshotID, Exclude: lost, Cleanup: lost, Reason: "node_lost"})
		s.notifySandbox(sb, "rebuilt", "info", time.Minute, "Sandbox rebuilt on another node",
			"The node hosting "+sb.Name+" went offline. CrowdVM is rebuilding it on another node from its latest snapshot; changes made after that snapshot are lost.")
		s.Log.Info("rebuilding sandbox after node loss", "sandbox", id, "lost", lost)
	}
}

// hydrationProgress: the last node.transfer download progress per sandbox
// (LIFE-012), shown as "hydration" while the sandbox is not hydrated.
var hydrationProgress sync.Map // sandbox id -> proto.TransferProgress

func hydrationJSON(sb *Sandbox, m map[string]any) {
	if sb.Hydrated || !(sb.starting() || sb.Usable()) { // lazy chains hydrate while usable
		hydrationProgress.Delete(sb.ID)
		return
	}
	v, ok := hydrationProgress.Load(sb.ID)
	if !ok {
		return
	}
	tp := v.(proto.TransferProgress)
	var total, pct any
	if tp.Total > 0 {
		total, pct = tp.Total, float64(tp.Bytes*1000/tp.Total)/10
	}
	phase := "downloading"
	if tp.Error != "" {
		phase = "degraded"
	}
	// Boat counts entries; a node pulls disk layers, so these are bytes.
	m["hydration"] = map[string]any{"phase": phase, "entriesDone": tp.Bytes, "entriesTotal": total, "percent": pct, "kind": tp.Kind, "unit": "bytes"}
}

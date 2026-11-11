package server

// Billing pauses and refused stops (LIFE-034, LIFE-040, SNAP-008; Boat
// snapshots__stop "When a stop is refused", troubleshooting "A sandbox does
// not answer"):
//
//   - A stop saves the disk first. Before the guest runs prepare-stop (pre-stop
//     hook, docker stop, cache cleanup) the server takes a live snapshot as a
//     pre-flight; when that save fails the stop is refused: nothing on the
//     machine changed, the sandbox goes back to ready and stays usable, the
//     stop is "failing" and retried with backoff, and the meter pauses
//     (stop_refused). The pause ends when the stop completes, when a resume
//     cancels the stop, or, for a refused auto-stop, when the sandbox is used
//     again. A save that fails after the pre-flight leaves the sandbox usable
//     too when its machine still answers.
//   - A sandbox whose guest stops answering (degraded, guest_unreachable) is
//     not metered until it answers again (not_answering); its owner gets a
//     notice.

import (
	"context"
	"errors"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

const (
	pauseStopRefused  = "stop_refused"
	pauseNotAnswering = "not_answering"
)

// billingPause is the reason sb's meter is paused ("" = metered normally).
func billingPause(sb *Sandbox) string {
	v, _ := sb.Extra["billingPaused"].(string)
	return v
}

func (s *Server) billingPausedByID(id string) bool {
	var v *string
	_ = s.DB.QueryRow(`SELECT json_extract(extra,'$.billingPaused') FROM sandboxes WHERE id=?`, id).Scan(&v)
	return v != nil && *v != ""
}

// pauseBilling stops metering id until resumeBilling lifts reason.
func (s *Server) pauseBilling(id, reason string) {
	_ = s.SetSandboxExtra(id, "billingPaused", reason)
	s.closeRun(id)
}

// resumeBilling lifts a pause with reason ("" = any) and meters the sandbox
// again when it runs.
func (s *Server) resumeBilling(id, reason string) {
	sb, err := s.SandboxByID(id)
	if err != nil {
		return
	}
	cur := billingPause(sb)
	if cur == "" || (reason != "" && cur != reason) {
		return
	}
	_ = s.SetSandboxExtra(id, "billingPaused", nil)
	if sb.Usable() || sb.State == api.StateProvisioned {
		s.ensureRun(id)
	}
}

// preflightSave takes a live snapshot before a stop touches the guest. An
// error means the save path is broken and the stop must be refused.
func (s *Server) preflightSave(ctx context.Context, node *NodeConn, sb *Sandbox) error {
	id := api.NewID(api.PrefixSnapshot, 20)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var ref proto.SnapshotRef
	if err := node.Do(cctx, "POST", "/v1/vms/"+sb.ID+"/snapshot", proto.SnapshotRequest{SnapshotID: id}, &ref); err != nil {
		var ne *NodeError
		if errors.As(err, &ne) && ne.Status == 404 {
			return nil // no machine: the stop itself handles it
		}
		_, _ = s.DB.Exec(`UPDATE sandboxes SET last_snapshot_status='failed' WHERE id=?`, sb.ID)
		return err
	}
	if ref.ID == "" {
		ref.ID = id
	}
	s.recordSnapshot(sb, ref, node.ID, "live")
	return nil
}

// machineAnswers reports whether sb's guest still answers on its node.
func (s *Server) machineAnswers(ctx context.Context, node *NodeConn, id string) bool {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return node.Do(cctx, "GET", "/v1/vms/"+id+"/guest/health", nil, nil) == nil
}

// refuseStop keeps a stop waiting after a failed save: the sandbox stays
// usable and unmetered while the reaper retries.
func (s *Server) refuseStop(sb *Sandbox, op *stopOp, msg string) {
	_, _ = s.DB.Exec(`UPDATE stops SET status='failing', error=?, last_attempt_at=? WHERE id=?`, msg, store.Now(), op.ID)
	s.pauseBilling(sb.ID, pauseStopRefused)
	if sb.State == api.StateArchiving {
		s.transition(sb.ID, []string{api.StateArchiving}, readyState(sb), map[string]any{"archive_after": nil})
	}
	s.Log.Warn("stop refused: the save failed; the sandbox keeps running", "sandbox", sb.ID, "err", msg)
	if cur, err := s.SandboxByID(sb.ID); err == nil {
		s.notifySandbox(cur, "stop_failing", "warning", errorHold, "Stopping a sandbox is failing",
			"Saving the disk of "+cur.Name+" failed, so the stop was refused: the sandbox keeps running with its work on its disk. CrowdVM retries the stop automatically and does not bill the sandbox meanwhile. To stop now and keep only the last snapshot, run crowdvm stop --force "+cur.ID+". After 3 days the stop is forced.")
	}
}

// cancelRefusedStop: a resume of a sandbox whose stop was refused cancels
// the stop and starts the meter again.
func (s *Server) cancelRefusedStop(sb *Sandbox) {
	if sb.StopID == "" {
		return
	}
	res, _ := s.DB.Exec(`UPDATE stops SET status='superseded', ended_at=? WHERE id=? AND status='failing'`, store.Now(), sb.StopID)
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	if sb.TTLSeconds != nil {
		_, _ = s.DB.Exec(`UPDATE sandboxes SET archive_after=? WHERE id=?`, store.Now()+int64(*sb.TTLSeconds)*1000, sb.ID)
	}
	s.resumeBilling(sb.ID, pauseStopRefused)
}

// onSandboxUsed: a refused auto-stop meters again once the sandbox is used.
func (s *Server) onSandboxUsed(sb *Sandbox) {
	if billingPause(sb) == pauseStopRefused {
		if r, _ := sb.Extra["stopReason"].(string); r == "auto_stop" {
			s.resumeBilling(sb.ID, pauseStopRefused)
		}
	}
}

// notAnswering pauses billing for a sandbox whose guest stopped answering
// and tells its owner.
func (s *Server) notAnswering(sb *Sandbox) {
	s.pauseBilling(sb.ID, pauseNotAnswering)
	s.notifySandbox(sb, "not_answering", "warning", 6*time.Hour, "Sandbox does not answer",
		"The sandbox "+sb.Name+" does not answer. Billing is paused while it does not answer, and its disk stays on the machine. "+
			"Wait a few minutes: it reconnects by itself when it answers again. Otherwise run crowdvm recover "+sb.ID+
			" (restarts the machine, keeps the disk), and only if that does not help crowdvm resume "+sb.ID+" (comes back from the last snapshot).")
}

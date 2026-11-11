package server

// Long error holds and failing stops (LIFE-034, LIFE-041, SNAP-008):
//
//   - A sandbox in error keeps its machine and disk for errorHold (3 days;
//     node-lost sandboxes are rebuilt elsewhere first when their latest
//     snapshot is on the server). The owner is told when it fails and one
//     day before the hold ends; then it is force-stopped to its last
//     snapshot.
//   - A stop whose save keeps failing is retried with backoff and not
//     metered; the sandbox keeps running meanwhile (lifecycle_pause.go). The owner is told at the first
//     failure and one day before the end; after errorHold it is forced
//     (the last snapshot is kept).

import (
	"context"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

const errorHold = 3 * 24 * time.Hour

func init() {
	RegisterFeature("lifecycle_holds", func(s *Server) error {
		s.Bus.Subscribe(func(e Event) {
			if e.SandboxID != "" && (e.Type == EvSandboxDeleted || (e.Type == EvSandboxState && e.State != api.StateError)) {
				s.clearSandboxErrorNotice(e.SandboxID)
			}
			if e.Type != EvSandboxState || e.SandboxID == "" {
				return
			}
			id, prev, state := e.SandboxID, e.PreviousState, e.State
			if state == api.StateError && prev != api.StateError {
				s.Go(func(ctx context.Context) { s.onSandboxError(id) })
			}
		})
		return nil
	})
}

func (s *Server) onSandboxError(id string) {
	_ = s.SetSandboxExtra(id, "errorSince", store.Now())
	sb, err := s.SandboxByID(id)
	if err != nil || sb.DeletedAt != nil || sb.State != api.StateError {
		return
	}
	msg := "The sandbox " + sb.Name + " failed (" + firstNonEmpty(sb.Error, "error") + "). Its disk is kept for 3 days: resume it (crowdvm resume " + sb.ID + ") or recover it (crowdvm recover " + sb.ID + ")."
	if sb.Error == "node_offline" {
		msg = "The node hosting " + sb.Name + " went offline. CrowdVM waits for the node, then rebuilds the sandbox on another node from its latest snapshot. Its disk is kept for 3 days."
	}
	s.notifySandbox(sb, "sandbox_error", "error", time.Hour, "Sandbox failed", msg)
	// Recovery may race the asynchronous error notification.
	s.clearSandboxErrorNotice(id)
}

// clearSandboxErrorNotice drops warnings whose condition ended: the error
// ones once the sandbox left the error state, the failing-stop ones once
// its stop no longer fails (also a refused stop of a running sandbox), all of them once it is deleted. Notices of
// what happened (forced_stop, stop_forced) stay (bugs-r5-sonnet).
func (s *Server) clearSandboxErrorNotice(id string) {
	_, _ = s.DB.Exec(`DELETE FROM notices WHERE sandbox_id=? AND EXISTS (SELECT 1 FROM sandboxes b WHERE b.id=? AND (
		b.deleted_at IS NOT NULL AND notices.kind IN ('sandbox_error','forced_stop_warning','stop_failing','stop_failing_warning')
		OR b.state!='error' AND notices.kind IN ('sandbox_error','forced_stop_warning')
		OR NOT EXISTS (SELECT 1 FROM stops st WHERE st.id=b.stop_id AND st.status='failing') AND notices.kind IN ('stop_failing','stop_failing_warning')))`, id, id)
}

// reapHolds runs from the reaper.
func (s *Server) reapHolds() {
	s.expireSaves()
	now := store.Now()
	warnAt := now - (errorHold - 24*time.Hour).Milliseconds()
	endAt := now - errorHold.Milliseconds()

	// Error holds.
	rows, err := s.DB.Query(`SELECT id, COALESCE(json_extract(extra,'$.errorSince'), updated_at) FROM sandboxes WHERE deleted_at IS NULL AND state='error' AND COALESCE(json_extract(extra,'$.errorSince'), updated_at)<?`, warnAt)
	if err == nil {
		type hold struct {
			id    string
			since int64
		}
		var list []hold
		for rows.Next() {
			var h hold
			if rows.Scan(&h.id, &h.since) == nil {
				list = append(list, h)
			}
		}
		rows.Close()
		for _, h := range list {
			sb, err := s.SandboxByID(h.id)
			if err != nil {
				continue
			}
			if h.since < endAt {
				s.notifySandbox(sb, "forced_stop", "error", errorHold, "Sandbox stopped after 3 days in error",
					"The sandbox "+sb.Name+" was in error for 3 days and was stopped to its last snapshot. Resume it with crowdvm resume "+sb.ID+".")
				_, _ = s.requestStop(sb, true, "error_hold_expired")
				continue
			}
			s.notifySandbox(sb, "forced_stop_warning", "warning", errorHold, "Sandbox will be stopped in 1 day",
				"The sandbox "+sb.Name+" is still in error. In 1 day it will be stopped to its last snapshot. Resume or recover it before then (crowdvm resume "+sb.ID+").")
		}
	}

	// Failing stops.
	rows, err = s.DB.Query(`SELECT s.id, st.id, st.requested_at, st.forced FROM sandboxes s JOIN stops st ON st.id=s.stop_id WHERE s.state IN ('archiving','ready','idle','running') AND s.deleted_at IS NULL AND st.status='failing'`)
	if err != nil {
		return
	}
	type fst struct {
		sb, stop string
		req      int64
		forced   bool
	}
	var list []fst
	for rows.Next() {
		var x fst
		if rows.Scan(&x.sb, &x.stop, &x.req, &x.forced) == nil {
			list = append(list, x)
		}
	}
	rows.Close()
	for _, x := range list {
		sb, err := s.SandboxByID(x.sb)
		if err != nil {
			continue
		}
		switch {
		case x.req < endAt && !x.forced:
			s.notifySandbox(sb, "stop_forced", "error", errorHold, "Stop forced after 3 days of failed saves",
				"Saving the disk of "+sb.Name+" failed for 3 days. The stop was forced; the sandbox keeps its last snapshot.")
			_, _ = s.requestStop(sb, true, "stop_failing_3_days")
		case x.req < warnAt:
			s.notifySandbox(sb, "stop_failing_warning", "warning", errorHold, "Stop will be forced in 1 day",
				"Saving the disk of "+sb.Name+" keeps failing. In 1 day the stop is forced and the sandbox keeps its last snapshot; work since then is lost.")
		default:
			s.notifySandbox(sb, "stop_failing", "warning", errorHold, "Stopping a sandbox is failing",
				"Saving the disk of "+sb.Name+" failed; CrowdVM retries automatically and does not bill it meanwhile. After 3 days the stop is forced to the last snapshot.")
		}
	}
}

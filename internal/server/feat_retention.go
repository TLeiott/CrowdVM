package server

// Data retention and deletion (Boat parity DATA-001..016, API-005..007, API-140).
//
//   - GET/PATCH /account/data-retention: zero data retention (ZDR), account
//     snapshots off, account monitoring off. PATCH needs a session.
//   - ZDR deletes archived sandboxes and saved snapshots on enable and every
//     sandbox when it stops; no saved snapshots while it is on.
//   - Monitoring off: history/process calls answer 409 monitoring_off.
//   - Close account (browser session), reopen within 30 days, purge after;
//     permanent erasure (GDPR) at once.
//   - Retention job: enforced by default (14 d newest stopped snapshot,
//     24 h older ones); report-only or off via /admin/retention.
//
// Deletion operations themselves (bdop_…, stages, refcounted chain GC) are
// in the core (snapshots.go, lifecycle.go).

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

const (
	retZDRPhrase      = "delete archived sandbox data"
	retClosePhrase    = "close my account"
	retErasePhrase    = "erase my account"
	retCloseWindow    = 30 * 24 * time.Hour
	retDefaultStopTTL = 14 // days a stopped sandbox's newest snapshot is kept
)

func init() {
	store.Register("retention", store.Migration{Version: 1, SQL: `
CREATE TABLE account_policy (
  user_id TEXT PRIMARY KEY,
  zdr INTEGER NOT NULL DEFAULT 0,
  zdr_at INTEGER,
  snapshots_off INTEGER NOT NULL DEFAULT 0,
  snapshots_off_at INTEGER,
  monitoring_off INTEGER NOT NULL DEFAULT 0,
  monitoring_off_at INTEGER,
  closed_at INTEGER,
  purge_at INTEGER,
  purged_at INTEGER,
  erased_at INTEGER
);
`})
	RegisterFeature("retention", func(s *Server) error {
		r := &retState{s: s}
		s.API("GET /account/data-retention", "account.read", r.get)
		s.API("PATCH /account/data-retention", ActionSession, r.patch)
		s.API("POST /account/close", ActionSession, r.close)
		s.API("POST /account/reopen", ActionSession, r.reopen)
		s.API("POST /account/erase", ActionSession, r.erase)
		s.API("GET /admin/retention", ActionAdmin, r.adminGet)
		s.API("PUT /admin/retention", ActionAdmin, r.adminPut)
		s.Hooks.NewSandbox = append(s.Hooks.NewSandbox, r.newSandbox)
		s.Hooks.Guard = append(s.Hooks.Guard, r.guard)
		s.Hooks.UserJSON = append(s.Hooks.UserJSON, r.userJSON)
		s.Bus.Subscribe(r.onEvent)
		s.Go(r.loop)
		return nil
	})
}

type retState struct{ s *Server }

type retPolicy struct {
	ZDR, SnapshotsOff, MonitoringOff       bool
	ZDRAt, SnapshotsOffAt, MonitoringOffAt sql.NullInt64
	ClosedAt, PurgeAt, PurgedAt, ErasedAt  sql.NullInt64
}

func (r *retState) policy(uid string) retPolicy {
	var p retPolicy
	_ = r.s.DB.QueryRow(`SELECT zdr,zdr_at,snapshots_off,snapshots_off_at,monitoring_off,monitoring_off_at,closed_at,purge_at,purged_at,erased_at FROM account_policy WHERE user_id=?`, uid).
		Scan(&p.ZDR, &p.ZDRAt, &p.SnapshotsOff, &p.SnapshotsOffAt, &p.MonitoringOff, &p.MonitoringOffAt, &p.ClosedAt, &p.PurgeAt, &p.PurgedAt, &p.ErasedAt)
	return p
}

func (r *retState) set(uid string, sets map[string]any) error {
	if _, err := r.s.DB.Exec(`INSERT OR IGNORE INTO account_policy(user_id) VALUES(?)`, uid); err != nil {
		return err
	}
	parts, args := []string{}, []any{}
	for k, v := range sets {
		parts = append(parts, k+"=?")
		args = append(args, v)
	}
	_, err := r.s.DB.Exec(`UPDATE account_policy SET `+strings.Join(parts, ",")+` WHERE user_id=?`, append(args, uid)...)
	return err
}

func (p retPolicy) json() map[string]any {
	return map[string]any{"enabled": p.ZDR, "enabledAt": tsNull(p.ZDRAt), "snapshotsOff": p.SnapshotsOff, "snapshotsOffAt": tsNull(p.SnapshotsOffAt),
		"monitoringOff": p.MonitoringOff, "monitoringOffAt": tsNull(p.MonitoringOffAt)}
}

func (r *retState) get(c *Ctx) error {
	if c.P.Key != nil && c.P.Key.SandboxID != "" {
		return Err(403, "api_key_sandbox_forbidden", "Per-sandbox API keys cannot read account settings.")
	}
	c.W.Header().Set("Cache-Control", "no-store")
	return c.OK(200, "data_retention.info", r.policy(c.P.User.ID).json())
}

func (r *retState) patch(c *Ctx) error {
	var req struct {
		Enabled       *bool   `json:"enabled"`
		SnapshotsOff  *bool   `json:"snapshotsOff"`
		MonitoringOff *bool   `json:"monitoringOff"`
		Confirmation  *string `json:"confirmation"`
	}
	if err := c.BindStrict(&req); err != nil {
		return err
	}
	if req.Enabled == nil && req.SnapshotsOff == nil && req.MonitoringOff == nil {
		return Err(400, "invalid_request", "Send enabled, snapshotsOff or monitoringOff.")
	}
	uid := c.P.User.ID
	now := store.Now()
	sets := map[string]any{}
	queued := 0
	p := r.policy(uid)
	if req.Enabled != nil {
		// Older crowdvm CLIs sent "enable zero data retention"; it is accepted too.
		if *req.Enabled && (req.Confirmation == nil || (*req.Confirmation != retZDRPhrase && *req.Confirmation != "enable zero data retention")) {
			return ErrD(400, "confirmation_required", "To enable zero data retention, send confirmation: \""+retZDRPhrase+"\".", map[string]any{"confirmation": retZDRPhrase})
		}
		if *req.Enabled != p.ZDR {
			sets["zdr"], sets["zdr_at"] = boolInt(*req.Enabled), nil
			if *req.Enabled {
				sets["zdr_at"] = now
			}
		}
	}
	if req.SnapshotsOff != nil && *req.SnapshotsOff != p.SnapshotsOff {
		sets["snapshots_off"], sets["snapshots_off_at"] = boolInt(*req.SnapshotsOff), nz64(*req.SnapshotsOff, now)
	}
	if req.MonitoringOff != nil && *req.MonitoringOff != p.MonitoringOff {
		sets["monitoring_off"], sets["monitoring_off_at"] = boolInt(*req.MonitoringOff), nz64(*req.MonitoringOff, now)
	}
	if len(sets) > 0 {
		if err := r.set(uid, sets); err != nil {
			return err
		}
	}
	if req.Enabled != nil && *req.Enabled {
		// Every enable re-checks: archived sandboxes and saved snapshots go.
		n, err := r.applyZDR(uid)
		if err != nil {
			return err
		}
		queued = n
	}
	out := r.policy(uid).json()
	out["queuedSandboxes"] = queued
	out["acceptedDeletionOperationsIrreversible"] = true
	c.W.Header().Set("Cache-Control", "no-store")
	return c.OK(200, "data_retention.updated", out)
}

func nz64(on bool, v int64) any {
	if !on {
		return nil
	}
	return v
}

// applyZDR queues deletion of every archived sandbox of uid and removes its
// saved snapshots. It returns the number of sandboxes queued.
func (r *retState) applyZDR(uid string) (int, error) {
	sbs, err := r.sandboxes(`owner_id=? AND deleted_at IS NULL AND state='archived'`, uid)
	if err != nil {
		return 0, err
	}
	for _, sb := range sbs {
		if _, err := r.s.DeleteSandbox(sb, "zdr"); err != nil {
			return 0, err
		}
	}
	// Personal saves and team images holding this account's disk (SNAP-022).
	if _, err := r.s.DB.Exec(`DELETE FROM saved_snapshots WHERE (owner_id=? OR saved_by=?) AND status!='saving'`, uid, uid); err != nil {
		return 0, err
	}
	r.s.Go(r.s.gcSnapshots)
	return len(sbs), nil
}

func (r *retState) sandboxes(where string, args ...any) ([]*Sandbox, error) {
	rows, err := r.s.DB.Query(`SELECT `+sbCols+` FROM sandboxes WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, nil
}

// onEvent: with ZDR every stop deletes the sandbox (also TTL and credit stops).
func (r *retState) onEvent(e Event) {
	if e.Type != EvSandboxArchived || e.SandboxID == "" {
		return
	}
	id := e.SandboxID
	r.s.Go(func(ctx context.Context) {
		sb, err := r.s.SandboxByID(id)
		if err != nil || sb.DeletedAt != nil || sb.State != api.StateArchived || !r.policy(sb.OwnerID).ZDR {
			return
		}
		if _, err := r.s.DeleteSandbox(sb, "zdr"); err != nil {
			r.s.Log.Warn("zdr delete failed", "sandbox", id, "err", err)
		}
	})
}

// newSandbox applies the account's snapshots-off / monitoring-off settings.
func (r *retState) newSandbox(c *Ctx, p *SandboxPolicy) error {
	pol := r.policy(p.Owner.ID)
	if pol.ClosedAt.Valid {
		return Err(403, "account_closed", "This account is closed. Reopen it on the dashboard first.")
	}
	if pol.SnapshotsOff {
		if p.SnapshotsReq.Set && p.SnapshotsReq.Value {
			return Err(409, "snapshots_off_for_account", "Snapshots are off for this account; snapshots: true is refused.")
		}
		p.Snapshots = false
	}
	if pol.MonitoringOff {
		if p.MonitorReq.Set && p.MonitorReq.Value {
			return Err(409, "monitoring_off_for_account", "Monitoring is off for this account; monitoring: true is refused.")
		}
		p.Monitoring = false
	}
	return nil
}

// guard: monitoring-off sandboxes have no history or process list; ZDR
// accounts cannot save snapshots; closed accounts are read-only.
func (r *retState) guard(c *Ctx) error {
	pat := c.R.Pattern
	_, path, _ := strings.Cut(pat, " ")
	if path == "" {
		path = pat
	}
	if strings.HasPrefix(path, "/api/v1/sandboxes/{sandboxId}/command-history") || strings.HasPrefix(path, "/api/v1/sandboxes/{sandboxId}/processes") ||
		path == "/api/v1/sandboxes/{sandboxId}/metrics" {
		if sb, err := r.s.SandboxByID(c.Param("sandboxId")); err == nil && !sb.Monitoring {
			return Err(409, "monitoring_off", "Monitoring is off for this sandbox: no command history, process list or metrics are kept.")
		}
	}
	if c.R.Method == "POST" && (path == "/api/v1/sandboxes/{sandboxId}/resume" || path == "/api/v1/sandboxes/{sandboxId}/fork") {
		if sb, err := r.s.SandboxByID(c.Param("sandboxId")); err == nil && sb.Extra["snapshotDataExpired"] == true {
			return snapshotDataExpiredErr(sb)
		}
	}
	if isSafe(c.R.Method) {
		return nil
	}
	pol := r.policy(c.P.User.ID)
	if pol.ClosedAt.Valid {
		switch path {
		case "/api/v1/account/reopen", "/api/v1/account/erase", "/auth/logout":
		default:
			return Err(403, "account_closed", "This account is closed. Reopen it on the dashboard first.")
		}
	}
	if pol.ZDR && c.R.Method == "POST" && (path == "/api/v1/saved-snapshots" || path == "/api/v1/named-snapshots") {
		return Err(409, "zero_data_retention", "Zero data retention is on: saved snapshots cannot be created.")
	}
	return nil
}

func (r *retState) userJSON(u *User, m map[string]any) {
	p := r.policy(u.ID)
	m["zeroDataRetention"], m["zeroDataRetentionEnabledAt"] = p.ZDR, tsNull(p.ZDRAt)
	m["closed"], m["closedAt"], m["purgeAt"], m["purgedAt"] = p.ClosedAt.Valid, tsNull(p.ClosedAt), tsNull(p.PurgeAt), tsNull(p.PurgedAt)
	m["snapshotsOff"], m["monitoringOff"] = p.SnapshotsOff, p.MonitoringOff
}

// ---------------------------------------------------------------------------
// Close, reopen, erase (DATA-012, DATA-013)
// ---------------------------------------------------------------------------

func browserOnly(c *Ctx) error {
	if c.P.Session == nil || c.P.Session.Kind != sessionBrowser {
		return Err(403, "browser_session_required", "Closing or erasing an account needs a dashboard (browser) session.")
	}
	return nil
}

func (r *retState) close(c *Ctx) error {
	if err := browserOnly(c); err != nil {
		return err
	}
	var req struct {
		Confirmation string `json:"confirmation"`
	}
	if err := c.BindStrict(&req); err != nil {
		return err
	}
	if req.Confirmation != retClosePhrase {
		return ErrD(400, "confirmation_required", "Send confirmation: \""+retClosePhrase+"\".", map[string]any{"confirmation": retClosePhrase})
	}
	u := c.P.User
	if p := r.policy(u.ID); p.ClosedAt.Valid {
		return Err(409, "account_closed", "This account is already closed.")
	}
	now := time.Now()
	purge := now.Add(retCloseWindow)
	if err := r.set(u.ID, map[string]any{"closed_at": store.T(now), "purge_at": store.T(purge), "purged_at": nil}); err != nil {
		return err
	}
	// Stop every active sandbox (each stop saves the disk first).
	sbs, _ := r.sandboxes(`owner_id=? AND deleted_at IS NULL AND state NOT IN ('archived','archiving')`, u.ID)
	for _, sb := range sbs {
		if _, err := r.s.requestStop(sb, false, "account closed"); err != nil {
			r.s.Log.Warn("close account: stop failed", "sandbox", sb.ID, "err", err)
		}
	}
	// Revoke keys and every other session.
	_, _ = r.s.DB.Exec(`UPDATE api_keys SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, store.T(now), u.ID)
	_, _ = r.s.DB.Exec(`DELETE FROM sessions WHERE user_id=? AND id!=?`, u.ID, c.P.Session.ID)
	if u.Email != "" {
		r.s.sendMail(u.Email, "Your CrowdVM account is closed",
			"Your CrowdVM account "+u.Username+" was closed on "+now.UTC().Format(time.RFC1123)+".\n\n"+
				"You can reopen it on the dashboard until "+purge.UTC().Format(time.RFC1123)+". After that date its sandbox data is purged permanently.\n")
	}
	return c.OK(200, "account.closed", map[string]any{"closedAt": ts(now), "purgeAt": ts(purge), "stoppedSandboxes": len(sbs)})
}

func (r *retState) reopen(c *Ctx) error {
	if err := browserOnly(c); err != nil {
		return err
	}
	p := r.policy(c.P.User.ID)
	if !p.ClosedAt.Valid {
		return Err(409, "account_not_closed", "This account is not closed.")
	}
	if p.ErasedAt.Valid {
		return Err(409, "account_erased", "This account was erased and cannot be reopened.")
	}
	if err := r.set(c.P.User.ID, map[string]any{"closed_at": nil, "purge_at": nil}); err != nil {
		return err
	}
	return c.OK(200, "account.reopened", map[string]any{"purged": p.PurgedAt.Valid, "purgedAt": tsNull(p.PurgedAt)})
}

// erase is GDPR erasure: data and identity go now; it cannot be undone.
func (r *retState) erase(c *Ctx) error {
	if err := browserOnly(c); err != nil {
		return err
	}
	var req struct {
		Confirmation string `json:"confirmation"`
	}
	if err := c.BindStrict(&req); err != nil {
		return err
	}
	if req.Confirmation != retErasePhrase {
		return ErrD(400, "confirmation_required", "Send confirmation: \""+retErasePhrase+"\".", map[string]any{"confirmation": retErasePhrase})
	}
	u := c.P.User
	n, err := r.purge(u.ID)
	if err != nil {
		return err
	}
	now := store.Now()
	_ = r.set(u.ID, map[string]any{"closed_at": now, "purge_at": now, "purged_at": now, "erased_at": now})
	// Identity: keep the row (audit, ledger, node history reference it) but
	// drop every personal field.
	anon := "erased-" + strings.TrimPrefix(u.ID, PrefixUser)
	_, _ = r.s.DB.Exec(`UPDATE users SET username=?, email=NULL, display_name='', password_hash='', disabled=1 WHERE id=?`, anon, u.ID)
	_, _ = r.s.DB.Exec(`DELETE FROM sessions WHERE user_id=?`, u.ID)
	_, _ = r.s.DB.Exec(`DELETE FROM org_members WHERE user_id=?`, u.ID)
	return c.OK(200, "account.erased", map[string]any{"erasedAt": tsMs(now), "deletedSandboxes": n})
}

// purge deletes every sandbox (reason "account"), saved snapshot, key,
// webhook and telemetry export of uid. Records kept: ledger, runs, audit.
func (r *retState) purge(uid string) (int, error) {
	sbs, err := r.sandboxes(`owner_id=? AND deleted_at IS NULL`, uid)
	if err != nil {
		return 0, err
	}
	for _, sb := range sbs {
		if _, err := r.s.DeleteSandbox(sb, "account"); err != nil {
			return 0, err
		}
	}
	_, _ = r.s.DB.Exec(`UPDATE api_keys SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, store.Now(), uid)
	for _, q := range []string{
		`DELETE FROM saved_snapshots WHERE owner_id=?1 OR saved_by=?1`,
		`DELETE FROM webhook_deliveries WHERE webhook_id IN (SELECT id FROM webhooks WHERE owner_id=?)`,
		`DELETE FROM webhooks WHERE owner_id=?`,
		`DELETE FROM telemetry WHERE owner_id=?`,
		`DELETE FROM otel_logs WHERE owner_id=?`,
		// bugs-r10-sonnet 5: the account's stored secrets go too: AI provider
		// credentials, CI links (GitHub token) and their jobs, git sources,
		// environments, SSH keys, AI proxy state, settings and notices.
		`DELETE FROM credential_accounts WHERE profile_id IN (SELECT id FROM credential_profiles WHERE owner_id=?)`,
		`DELETE FROM credential_custom_apis WHERE profile_id IN (SELECT id FROM credential_profiles WHERE owner_id=?)`,
		`DELETE FROM ai_proxy_tokens WHERE profile_id IN (SELECT id FROM credential_profiles WHERE owner_id=?1)
			OR sandbox_id IN (SELECT id FROM sandboxes WHERE owner_id=?1)`,
		`DELETE FROM credential_profiles WHERE owner_id=?`,
		`DELETE FROM ai_proxy_settings WHERE account_id=?`,
		`DELETE FROM ci_jobs WHERE owner_id=?1 OR link_id IN (SELECT id FROM ci_links WHERE owner_id=?1)`,
		`DELETE FROM ci_repos WHERE link_id IN (SELECT id FROM ci_links WHERE owner_id=?)`,
		`DELETE FROM ci_links WHERE owner_id=?`,
		`DELETE FROM git_repos WHERE owner_id=?1 OR source_id IN (SELECT id FROM git_sources WHERE owner_id=?1)`,
		`DELETE FROM git_sources WHERE owner_id=?`,
		// Versions a remaining saved snapshot's sandbox ran with stay, as in
		// org deletion (orgs.go).
		`DELETE FROM environment_versions WHERE env_id IN (SELECT id FROM environments WHERE owner_id=?)
			AND env_id NOT IN (SELECT b.environment_id FROM saved_snapshots ss JOIN sandboxes b ON b.id=ss.source_sandbox_id WHERE b.environment_id IS NOT NULL)`,
		`DELETE FROM environments WHERE owner_id=?`,
		`DELETE FROM sandbox_ssh_keys WHERE user_id=?`,
		`DELETE FROM account_settings WHERE user_id=?`,
		`DELETE FROM notices WHERE user_id=?1 OR wallet_id=?1`,
	} {
		_, _ = r.s.DB.Exec(q, uid)
	}
	r.s.Go(r.s.gcSnapshots)
	return len(sbs), nil
}

// ---------------------------------------------------------------------------
// Retention job (DATA-001)
// ---------------------------------------------------------------------------

type retConfig struct {
	Mode        string `json:"mode"`        // "off" | "report" | "enforce"
	StoppedDays int    `json:"stoppedDays"` // newest snapshot of a stopped sandbox (14)
	OlderHours  int    `json:"olderHours"`  // older, unreferenced snapshots (24)
	LastRunAt   any    `json:"lastRunAt"`   // read-only
	LastReport  any    `json:"lastReport"`  // read-only
}

func (r *retState) config() retConfig {
	// Enforced by default (SNAP-004): older automatic snapshots go after a
	// day, a stopped sandbox's newest after 14 days. Admins can switch to
	// "report" (log only) or "off" at PUT /admin/retention.
	cfg := retConfig{Mode: "enforce", StoppedDays: retDefaultStopTTL, OlderHours: 24}
	var raw string
	if r.s.DB.QueryRow(`SELECT value FROM settings WHERE key='retention.config'`).Scan(&raw) == nil {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	return cfg
}

func (r *retState) adminGet(c *Ctx) error {
	return c.OK(200, "retention.config", map[string]any{"retention": r.config()})
}

func (r *retState) adminPut(c *Ctx) error {
	cfg := r.config()
	var req struct {
		Mode        *string `json:"mode"`
		StoppedDays *int    `json:"stoppedDays"`
		OlderHours  *int    `json:"olderHours"`
		RunNow      bool    `json:"runNow"`
	}
	if err := c.BindStrict(&req); err != nil {
		return err
	}
	if req.Mode != nil {
		switch *req.Mode {
		case "off", "report", "enforce":
			cfg.Mode = *req.Mode
		default:
			return Err(400, "invalid_mode", "mode must be off, report or enforce.")
		}
	}
	if req.StoppedDays != nil {
		if *req.StoppedDays < 1 {
			return Err(400, "invalid_days", "stoppedDays must be at least 1.")
		}
		cfg.StoppedDays = *req.StoppedDays
	}
	if req.OlderHours != nil {
		if *req.OlderHours < 1 {
			return Err(400, "invalid_hours", "olderHours must be at least 1.")
		}
		cfg.OlderHours = *req.OlderHours
	}
	if err := r.saveConfig(cfg); err != nil {
		return err
	}
	if req.RunNow {
		r.run()
	}
	return c.OK(200, "retention.config", map[string]any{"retention": r.config()})
}

func (r *retState) saveConfig(cfg retConfig) error {
	_, err := r.s.DB.Exec(`INSERT INTO settings(key,value) VALUES('retention.config',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, mustJSON(cfg))
	return err
}

func (r *retState) loop(ctx context.Context) {
	every(ctx, time.Minute, func() {
		r.purgeClosed()
		if time.Now().Minute()%15 == 0 {
			r.run()
		}
	})
}

// purgeClosed purges accounts closed more than 30 days ago.
func (r *retState) purgeClosed() {
	rows, err := r.s.DB.Query(`SELECT user_id FROM account_policy WHERE closed_at IS NOT NULL AND purged_at IS NULL AND purge_at<=?`, store.Now())
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
		if _, err := r.purge(id); err == nil {
			_ = r.set(id, map[string]any{"purged_at": store.Now()})
		}
	}
}

// run applies the retention table: stopped sandboxes keep their newest
// snapshot StoppedDays; older snapshots that resume/fork never use go after
// OlderHours (only when nothing references them: GC is refcounted).
func (r *retState) run() {
	cfg := r.config()
	if cfg.Mode == "off" {
		return
	}
	now := time.Now()
	stoppedCut := store.T(now.Add(-time.Duration(cfg.StoppedDays) * 24 * time.Hour))
	olderCut := store.T(now.Add(-time.Duration(cfg.OlderHours) * time.Hour))
	expired, _ := r.sandboxes(`deleted_at IS NULL AND state='archived' AND stopped_at IS NOT NULL AND stopped_at<? AND head_snapshot_id IS NOT NULL`, stoppedCut)
	var olderIDs []string
	rows, err := r.s.DB.Query(`SELECT sn.id FROM snapshots sn JOIN sandboxes sb ON sb.id=sn.sandbox_id
WHERE sn.deleted_at IS NULL AND sn.created_at<? AND sb.deleted_at IS NULL AND sn.id!=COALESCE(sb.head_snapshot_id,'')`, olderCut)
	if err == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				olderIDs = append(olderIDs, id)
			}
		}
		rows.Close()
	}
	report := map[string]any{"mode": cfg.Mode, "expiredSandboxes": len(expired), "olderSnapshots": len(olderIDs)}
	ids := make([]string, 0, len(expired))
	for _, sb := range expired {
		ids = append(ids, sb.ID)
	}
	if len(ids) > 100 {
		ids = ids[:100]
	}
	report["expiredSandboxIds"] = ids
	if cfg.Mode == "enforce" {
		t := store.T(now)
		for _, id := range olderIDs {
			_, _ = r.s.DB.Exec(`UPDATE snapshots SET deleted_at=? WHERE id=? AND deleted_at IS NULL`, t, id)
		}
		for _, sb := range expired {
			// Resume and fork then answer 409 snapshot_data_expired.
			_, _ = r.s.DB.Exec(`UPDATE snapshots SET deleted_at=? WHERE sandbox_id=? AND deleted_at IS NULL`, t, sb.ID)
			_, _ = r.s.DB.Exec(`UPDATE sandboxes SET head_snapshot_id=NULL WHERE id=?`, sb.ID)
			_ = r.s.SetSandboxExtra(sb.ID, "snapshotDataExpired", true)
			_ = r.s.SetSandboxExtra(sb.ID, "snapshotDataExpiredAt", t)
		}
		r.s.Go(r.s.gcSnapshots)
	} else if len(expired) > 0 || len(olderIDs) > 0 {
		r.s.Log.Info("retention report (report-only)", "expiredSandboxes", len(expired), "olderSnapshots", len(olderIDs))
	}
	cfg.LastRunAt, cfg.LastReport = ts(now), report
	_ = r.saveConfig(cfg)
}

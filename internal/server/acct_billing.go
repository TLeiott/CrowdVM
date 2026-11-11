package server

// Credits policy (Boat billing → credits):
//
//   - Wallet policies (BILL-002/022/026, ORG-019/020), set by server admins
//     per account or org wallet (GET|PUT /admin/policies/{wallet}): monthly
//     credit quota, concurrency and start limits, max TTL, allowed sizes,
//     saved-snapshot allowance, a monthly credit cap and a low-credit alert.
//     nil fields fall back to the server config.
//   - Running out (BILL-006): when a wallet reaches zero with enforcement
//     on, its sandboxes keep running for a 24 h grace period (notice + mail);
//     then, or earlier when the grace adds more debt than 25 % of the
//     monthly quota (at least $50 worth), they are stopped (snapshot).
//   - Caps (ORG-021, BILL-026): over-cap org members' org sandboxes and
//     over-cap accounts' personal sandboxes are stopped within a billing
//     tick; caps apply whether or not credits are enforced.
//   - Extra starts (BILL-010): with extra starts on, a start beyond the hour
//     or day window costs extraStartCredits (≤ 6× the window, ≤ 3× per
//     minute, optional monthly credit cap).
//   - Notices (BILL-023, DASH-014): GET /notices, mailed to the notification
//     address of the account (org: owner and admins).
//   - GET /billing: wallet, policy, extra starts, grace (CLI `billing`).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

const (
	extraStartCredits = 0.05 // credits per extra start (≈3 min of a default machine)
	graceDuration     = 24 * time.Hour
)

func init() {
	store.Register("acct_billing", store.Migration{Version: 1, SQL: `
CREATE TABLE wallet_policies (
  wallet_id TEXT PRIMARY KEY,
  data TEXT NOT NULL DEFAULT '{}',
  updated_at INTEGER NOT NULL
);
CREATE TABLE wallet_state (
  wallet_id TEXT PRIMARY KEY,
  grace_since INTEGER,
  grace_debt_start REAL,
  low_noticed INTEGER NOT NULL DEFAULT 0,
  quota_month TEXT,
  quota_granted REAL NOT NULL DEFAULT 0,
  extra_starts INTEGER NOT NULL DEFAULT 0,
  extra_cap REAL
);
CREATE TABLE notices (
  id TEXT PRIMARY KEY,
  wallet_id TEXT,
  user_id TEXT,
  kind TEXT NOT NULL,
  level TEXT NOT NULL,
  title TEXT NOT NULL,
  message TEXT NOT NULL,
  sandbox_id TEXT,
  created_at INTEGER NOT NULL,
  dismissed_at INTEGER
);
CREATE INDEX notices_user ON notices(user_id, created_at);
CREATE INDEX notices_wallet ON notices(wallet_id, created_at);
`})
	RegisterFeature("acct_billing", func(s *Server) error {
		s.API("GET /billing", "account.read", s.handleBilling)
		s.API("PUT /billing/extra-starts", "account.admin", s.handleExtraStarts)
		s.API("GET /notices", "account.read", s.handleNotices)
		s.API("POST /notices/{noticeId}/dismiss", "account.read", s.handleDismissNotice)
		s.API("POST /notices/dismiss-all", "account.read", s.handleDismissAllNotices)
		s.API("GET /admin/policies/{wallet}", ActionAdmin, s.handleAdminPolicyGet)
		s.API("PUT /admin/policies/{wallet}", ActionAdmin, s.handleAdminPolicyPut)
		for _, p := range []string{"/api/boat/billing/checkout", "/api/crowdvm/billing/checkout"} {
			s.Route("POST "+p, ActionAny, s.handleCheckout)
		}
		s.Hooks.Guard = append(s.Hooks.Guard, s.policyGuard)
		return nil
	})
}

// walletPolicy is an admin-set policy; nil = server default.
type walletPolicy struct {
	MonthlyCredits         *float64 `json:"monthlyCredits"`
	MaxActive              *int     `json:"maxActive"`
	StartsPerMinute        *int     `json:"startsPerMinute"`
	StartsPerHour          *int     `json:"startsPerHour"`
	StartsPerDay           *int     `json:"startsPerDay"`
	MaxTTLSeconds          *int     `json:"maxTtlSeconds"`
	AllowedSizes           []string `json:"allowedSizes"`
	SavedSnapshotAllowance *int     `json:"savedSnapshotAllowance"`
	CreditCapMonthly       *float64 `json:"creditCapMonthly"`
	LowCreditAlert         *float64 `json:"lowCreditAlert"`
	// Restricted (RUN-008, Boat's trial): auto-stop at most 2 h; a request
	// without one is refused with trial_auto_stop_required.
	Restricted *bool `json:"restricted"`
}

const restrictedMaxTTL = 7200

// ttlLimit is the wallet's auto-stop cap and the error code that refuses
// more (0 = none).
func (p walletPolicy) ttlLimit() (int, string) {
	max, code := 0, "auto_stop_required"
	if p.MaxTTLSeconds != nil && *p.MaxTTLSeconds > 0 {
		max = *p.MaxTTLSeconds
	}
	if p.Restricted != nil && *p.Restricted {
		code = "trial_auto_stop_required"
		if max == 0 || max > restrictedMaxTTL {
			max = restrictedMaxTTL
		}
	}
	return max, code
}

func (s *Server) walletPolicy(wallet string) walletPolicy {
	var raw string
	var p walletPolicy
	if s.DB.QueryRow(`SELECT data FROM wallet_policies WHERE wallet_id=?`, wallet).Scan(&raw) == nil {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	return p
}

func intOr(p *int, d int) int {
	if p != nil {
		return *p
	}
	return d
}

func (s *Server) maxActiveFor(wallet string) int {
	return intOr(s.walletPolicy(wallet).MaxActive, s.Cfg.MaxActive)
}

// startWindowsFor are the start windows of a wallet (policy, else config).
func (s *Server) startWindowsFor(wallet string) []startWindow {
	p := s.walletPolicy(wallet)
	return []startWindow{
		{"minute", time.Minute, intOr(p.StartsPerMinute, s.Cfg.StartsPerMinute)},
		{"hour", time.Hour, intOr(p.StartsPerHour, s.Cfg.StartsPerHour)},
		{"day", 24 * time.Hour, intOr(p.StartsPerDay, s.Cfg.StartsPerDay)},
	}
}

func monthStart() time.Time {
	n := time.Now().UTC()
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// spentThisMonth is a wallet's usage this month; actor narrows it to one
// member's org usage.
func (s *Server) spentThisMonth(wallet, actor string) float64 {
	var used float64
	q, args := `SELECT COALESCE(-SUM(delta),0) FROM credit_ledger WHERE wallet_id=? AND kind IN ('usage','extra_start') AND created_at>=?`, []any{wallet, monthStart().UnixMilli()}
	if actor != "" {
		q += ` AND actor_id=?`
		args = append(args, actor)
	}
	_ = s.DB.QueryRow(q, args...).Scan(&used)
	return used
}

// checkCreditCap refuses starts of a personal wallet over its monthly cap.
func (s *Server) checkCreditCap(wallet string) error {
	p := s.walletPolicy(wallet)
	if p.CreditCapMonthly == nil {
		return nil
	}
	if used := s.spentThisMonth(wallet, ""); used >= *p.CreditCapMonthly {
		return ErrD(402, "credit_cap_reached", "This wallet reached its monthly credit cap. Ask an administrator to raise it, or wait for the next month.",
			map[string]any{"capCredits": *p.CreditCapMonthly, "usedCredits": round6(used), "billingUrl": s.Cfg.PublicURL + "/#/credits"})
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wallet state, extra starts
// ---------------------------------------------------------------------------

type walletState struct {
	GraceSince   sql.NullInt64
	GraceDebt    sql.NullFloat64
	LowNoticed   bool
	QuotaMonth   sql.NullString
	QuotaGranted float64
	ExtraStarts  bool
	ExtraCap     sql.NullFloat64
}

func (s *Server) walletState(wallet string) walletState {
	var w walletState
	_ = s.DB.QueryRow(`SELECT grace_since,grace_debt_start,low_noticed,quota_month,quota_granted,extra_starts,extra_cap FROM wallet_state WHERE wallet_id=?`, wallet).
		Scan(&w.GraceSince, &w.GraceDebt, &w.LowNoticed, &w.QuotaMonth, &w.QuotaGranted, &w.ExtraStarts, &w.ExtraCap)
	return w
}

func (s *Server) setWalletState(wallet string, sets map[string]any) {
	_, _ = s.DB.Exec(`INSERT OR IGNORE INTO wallet_state(wallet_id) VALUES(?)`, wallet)
	parts, args := []string{}, []any{}
	for k, v := range sets {
		parts = append(parts, k+"=?")
		args = append(args, v)
	}
	_, _ = s.DB.Exec(`UPDATE wallet_state SET `+strings.Join(parts, ",")+` WHERE wallet_id=?`, append(args, wallet)...)
}

func (s *Server) extraStartsJSON(wallet string) map[string]any {
	st := s.walletState(wallet)
	var used float64
	_ = s.DB.QueryRow(`SELECT COALESCE(-SUM(delta),0) FROM credit_ledger WHERE wallet_id=? AND kind='extra_start' AND created_at>=?`, wallet, monthStart().UnixMilli()).Scan(&used)
	var capV any
	if st.ExtraCap.Valid {
		capV = st.ExtraCap.Float64
	}
	return map[string]any{"enabled": st.ExtraStarts, "creditsPerStart": extraStartCredits, "monthlyCapCredits": capV, "usedCreditsThisMonth": round6(used)}
}

// extraStartOK reports whether a start over window w may run as an extra
// start: extra starts on, ≤ 6× the window (≤ 3× for the minute window) and
// within the monthly cap.
func (s *Server) extraStartOK(wallet string, w startWindow, used, count int) bool {
	return s.extraStartRefusal(wallet, w, used, count) == ""
}

// extraStartMult: extra starts stop at this multiple of a window's limit.
func extraStartMult(window string) int {
	if window == "minute" {
		return 3
	}
	return 6
}

// extraStartRefusal says why count starts above w's limit cannot be bought
// ("" = they can): Boat's rate_limited causes plan (no extra starts),
// ceiling (past the extra-start ceiling) and cap (the monthly cap on extra
// starts is spent).
func (s *Server) extraStartRefusal(wallet string, w startWindow, used, count int) string {
	st := s.walletState(wallet)
	if !st.ExtraStarts {
		return "plan"
	}
	if used+count > w.limit*extraStartMult(w.name) {
		return "ceiling"
	}
	if st.ExtraCap.Valid {
		var spent float64
		_ = s.DB.QueryRow(`SELECT COALESCE(-SUM(delta),0) FROM credit_ledger WHERE wallet_id=? AND kind='extra_start' AND created_at>=?`, wallet, monthStart().UnixMilli()).Scan(&spent)
		if spent+float64(count)*extraStartCredits > st.ExtraCap.Float64+1e-9 {
			return "cap"
		}
	}
	return ""
}

// chargeExtraStart bills a start that ran beyond a window. actor, the
// sandbox's owner, lets a member's usage cap count it (bugs-r10-sonnet 9).
func (s *Server) chargeExtraStart(wallet, actor string) {
	_ = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		return walletAdd(tx, wallet, -extraStartCredits, "extra_start", "extra start beyond the start limit", "", "", actor)
	})
}

func (s *Server) handleExtraStarts(c *Ctx) error {
	var req struct {
		Enabled           *bool    `json:"enabled"`
		MonthlyCapCredits OptFloat `json:"monthlyCapCredits"`
		Org               string   `json:"org"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	org, err := c.Scope(req.Org)
	if err != nil {
		return err
	}
	if org != nil {
		if r := s.memberRole(org.ID, c.P.User.ID); r != "owner" && r != "admin" {
			return Err(403, "forbidden", "Only owners and admins change the organization's extra starts.")
		}
	}
	w := walletOf(c.P.User, org)
	sets := map[string]any{}
	if req.Enabled != nil {
		sets["extra_starts"] = boolInt(*req.Enabled)
	}
	if req.MonthlyCapCredits.Set {
		if req.MonthlyCapCredits.Null {
			sets["extra_cap"] = nil
		} else if req.MonthlyCapCredits.Value < 0 || math.IsNaN(req.MonthlyCapCredits.Value) {
			return Err(400, "invalid_amount", "monthlyCapCredits must be 0 or more (null = no cap).")
		} else {
			sets["extra_cap"] = req.MonthlyCapCredits.Value
		}
	}
	if len(sets) == 0 {
		return Err(400, "no_changes", "Send enabled and/or monthlyCapCredits.")
	}
	s.setWalletState(w, sets)
	return c.OK(200, "billing.extra_starts", map[string]any{"extraStarts": s.extraStartsJSON(w), "wallet": w})
}

// OptFloat is a JSON number that distinguishes absent, null and a value.
type OptFloat struct {
	Set, Null bool
	Value     float64
}

func (o *OptFloat) UnmarshalJSON(b []byte) error {
	*o = OptFloat{Set: true} // a repeated key replaces the earlier value
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.Value)
}

// ---------------------------------------------------------------------------
// Billing tick: quotas, alerts, grace, caps
// ---------------------------------------------------------------------------

// billingPolicies runs after every billing tick (BillNow).
func (s *Server) billingPolicies() {
	s.chargeSavedExtras()
	s.grantQuotas()
	s.lowCreditAlerts()
	if s.Cfg.EnforceCredits {
		s.enforceGrace()
	}
	s.enforceCaps()
}

// grantQuotas resets each wallet's monthly quota: the unused part of last
// month's quota expires, the new one is granted.
func (s *Server) grantQuotas() {
	rows, err := s.DB.Query(`SELECT wallet_id, data FROM wallet_policies`)
	if err != nil {
		return
	}
	type q struct {
		w string
		n float64
	}
	var list []q
	for rows.Next() {
		var w, raw string
		var p walletPolicy
		if rows.Scan(&w, &raw) == nil && json.Unmarshal([]byte(raw), &p) == nil && p.MonthlyCredits != nil && *p.MonthlyCredits > 0 {
			list = append(list, q{w, *p.MonthlyCredits})
		}
	}
	rows.Close()
	month := monthStart().Format("2006-01")
	for _, x := range list {
		st := s.walletState(x.w)
		if st.QuotaMonth.Valid && st.QuotaMonth.String == month {
			continue
		}
		bal := s.Balance(x.w)
		expire := math.Max(0, math.Min(bal, st.QuotaGranted))
		_ = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
			if expire > 0 {
				if err := walletAdd(tx, x.w, -expire, "quota", "unused monthly credits expired", "", "", ""); err != nil {
					return err
				}
			}
			return walletAdd(tx, x.w, x.n, "quota", "monthly credits "+month, "", "", "")
		})
		s.setWalletState(x.w, map[string]any{"quota_month": month, "quota_granted": x.n})
	}
}

// walletsWithActivity lists wallets with active sandboxes or a policy.
func (s *Server) walletIDs(q string, args ...any) []string {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func (s *Server) lowCreditThreshold(wallet string) (float64, bool) {
	if p := s.walletPolicy(wallet); p.LowCreditAlert != nil {
		return *p.LowCreditAlert, *p.LowCreditAlert > 0
	}
	if s.Cfg.EnforceCredits {
		return 1, true
	}
	return 0, false
}

func (s *Server) lowCreditAlerts() {
	for _, w := range s.walletIDs(`SELECT DISTINCT wallet_id FROM credit_ledger WHERE created_at>?`, store.Now()-24*3600_000) {
		th, ok := s.lowCreditThreshold(w)
		if !ok {
			continue
		}
		bal := s.Balance(w)
		st := s.walletState(w)
		switch {
		case bal < th && !st.LowNoticed:
			s.walletNotify(w, "", "low_credit", "Credits are running low",
				fmt.Sprintf("The wallet has %.2f credits left (alert below %.2f). Ask an administrator for credits or contribute a node to earn them.", bal, th))
			s.setWalletState(w, map[string]any{"low_noticed": 1})
		case bal >= th && st.LowNoticed:
			s.setWalletState(w, map[string]any{"low_noticed": 0})
		}
	}
}

// enforceGrace: 24 h grace once a wallet with running sandboxes is empty,
// then (or when the new debt grows too large) stop them.
func (s *Server) enforceGrace() {
	empty := s.walletIDs(`SELECT DISTINCT COALESCE(s.org_id,s.owner_id) FROM sandboxes s JOIN wallets w ON w.id=COALESCE(s.org_id,s.owner_id)
		WHERE s.deleted_at IS NULL AND w.balance <= 0 AND s.state IN ('provisioning','provisioned','cloning','ready','idle','running')`)
	isEmpty := map[string]bool{}
	now := store.Now()
	for _, w := range empty {
		isEmpty[w] = true
		st := s.walletState(w)
		bal := s.Balance(w)
		if !st.GraceSince.Valid {
			s.setWalletState(w, map[string]any{"grace_since": now, "grace_debt_start": bal})
			s.walletNotify(w, "", "grace_started", "Credits are used up: 24 hours of grace",
				"The wallet has no credits left. Running sandboxes keep running for 24 hours; then they are stopped (with a snapshot). Add credits before that to keep them running.")
			continue
		}
		limit := math.Max(0.25*ptrF(s.walletPolicy(w).MonthlyCredits), 50*float64(s.Cfg.SecondsPerDollar)/3600)
		newDebt := st.GraceDebt.Float64 - bal
		if now-st.GraceSince.Int64 < graceDuration.Milliseconds() && newDebt <= limit {
			continue
		}
		ids := s.walletIDs(`SELECT id FROM sandboxes WHERE deleted_at IS NULL AND COALESCE(org_id,owner_id)=? AND state IN ('provisioning','provisioned','cloning','ready','idle','running')`, w)
		for _, id := range ids {
			if sb, err := s.SandboxByID(id); err == nil {
				s.Log.Info("stopping sandbox: grace period over", "sandbox", id, "wallet", w)
				_, _ = s.requestStop(sb, false, "billing_required")
			}
		}
		if len(ids) > 0 {
			s.walletNotify(w, "", "billing_stopped", "Sandboxes stopped: no credits",
				fmt.Sprintf("The grace period ended, so %d running sandbox(es) were stopped with a snapshot. Add credits, then resume them.", len(ids)))
		}
	}
	// Wallets back above zero leave the grace period.
	for _, w := range s.walletIDs(`SELECT ws.wallet_id FROM wallet_state ws JOIN wallets w ON w.id=ws.wallet_id WHERE ws.grace_since IS NOT NULL AND w.balance > 0`) {
		if !isEmpty[w] {
			s.setWalletState(w, map[string]any{"grace_since": nil, "grace_debt_start": nil})
		}
	}
}

func ptrF(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// enforceCaps stops sandboxes of org members over their caps and of
// wallets over their monthly credit cap.
func (s *Server) enforceCaps() {
	type member struct {
		org, user string
		usage     sql.NullFloat64
		conc      sql.NullInt64
	}
	var ms []member
	rows, err := s.DB.Query(`SELECT org_id,user_id,usage_cap_credits,concurrent_cap FROM org_members WHERE usage_cap_credits IS NOT NULL OR concurrent_cap IS NOT NULL`)
	if err == nil {
		for rows.Next() {
			var m member
			if rows.Scan(&m.org, &m.user, &m.usage, &m.conc) == nil {
				ms = append(ms, m)
			}
		}
		rows.Close()
	}
	active := `deleted_at IS NULL AND state IN ('provisioning','provisioned','cloning','ready','idle','running')`
	for _, m := range ms {
		ids := s.walletIDs(`SELECT id FROM sandboxes WHERE org_id=? AND owner_id=? AND `+active+` ORDER BY created_at DESC`, m.org, m.user)
		if len(ids) == 0 {
			continue
		}
		var stop []string
		if m.usage.Valid && s.spentThisMonth(m.org, m.user) >= m.usage.Float64 {
			stop = ids
		} else if m.conc.Valid && len(ids) > int(m.conc.Int64) {
			stop = ids[:len(ids)-int(max(m.conc.Int64, 0))]
		}
		if len(stop) > 0 {
			s.stopForCap(stop, "team_member_cap_reached")
			s.walletNotify(m.org, m.user, "member_cap_stopped", "Sandboxes stopped: member cap",
				fmt.Sprintf("%d organization sandbox(es) were stopped because you are over the cap the organization set for you. They resume when the cap is raised or the month renews.", len(stop)))
		}
	}
	rows, err = s.DB.Query(`SELECT wallet_id, data FROM wallet_policies`)
	if err != nil {
		return
	}
	var capped []string
	for rows.Next() {
		var w, raw string
		var p walletPolicy
		if rows.Scan(&w, &raw) == nil && json.Unmarshal([]byte(raw), &p) == nil && p.CreditCapMonthly != nil {
			capped = append(capped, w)
		}
	}
	rows.Close()
	for _, w := range capped {
		if s.checkCreditCap(w) == nil {
			continue
		}
		ids := s.walletIDs(`SELECT id FROM sandboxes WHERE COALESCE(org_id,owner_id)=? AND `+active, w)
		if len(ids) > 0 {
			s.stopForCap(ids, "credit_cap_reached")
			s.walletNotify(w, "", "credit_cap_reached", "Sandboxes stopped: monthly credit cap",
				fmt.Sprintf("The wallet reached its monthly credit cap, so %d sandbox(es) were stopped with a snapshot.", len(ids)))
		}
	}
}

func (s *Server) stopForCap(ids []string, reason string) {
	for _, id := range ids {
		if sb, err := s.SandboxByID(id); err == nil && sb.Active() {
			s.Log.Info("stopping sandbox: over cap", "sandbox", id, "reason", reason)
			_, _ = s.requestStop(sb, false, reason)
		}
	}
}

// ---------------------------------------------------------------------------
// Notices (also used for sandbox notices, lifecycle_notify.go)
// ---------------------------------------------------------------------------

// walletNotify records a notice for a wallet (and optionally one member)
// and mails it to the notification address of the people responsible
// (account; org: owner and admins; or the member).
func (s *Server) walletNotify(wallet, user, kind, title, msg string) {
	level := "warning"
	switch kind {
	case "grace_started", "billing_stopped":
		level = "error"
	}
	_, _ = s.DB.Exec(`INSERT INTO notices(id,wallet_id,user_id,kind,level,title,message,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		"ntc_"+randToken(16), nz(wallet), nz(user), kind, level, title, msg, store.Now())
	var to []string
	switch {
	case user != "":
		to = []string{user}
	case strings.HasPrefix(wallet, PrefixOrg):
		to = s.walletIDs(`SELECT user_id FROM org_members WHERE org_id=? AND role IN ('owner','admin')`, wallet)
	default:
		to = []string{wallet}
	}
	for _, uid := range to {
		if addr := s.notifyAddress(uid); addr != "" {
			s.sendMail(addr, "CrowdVM: "+title, msg+"\n\n"+s.Cfg.PublicURL+"/#/credits\n")
		}
	}
}

func noticeJSON(id, wallet, user, kind, level, title, msg, sb string, created int64, dismissed sql.NullInt64) map[string]any {
	return map[string]any{"id": id, "wallet": nz(wallet), "userId": nz(user), "kind": kind, "level": level, "title": title, "message": msg,
		"sandboxId": nz(sb), "createdAt": tsMs(created), "dismissedAt": tsNull(dismissed)}
}

// noticeScope: the caller's own notices, wallet notices of the personal
// wallet, and wallet notices of orgs the caller owns or administers.
const noticeScope = `(user_id=? OR (user_id IS NULL AND wallet_id=?) OR (user_id IS NULL AND wallet_id IN (SELECT org_id FROM org_members WHERE user_id=? AND role IN ('owner','admin'))))`

const noticeCols = `id,COALESCE(wallet_id,''),COALESCE(user_id,''),kind,level,title,message,COALESCE(sandbox_id,''),created_at,dismissed_at`

func scanNotice(sc interface{ Scan(...any) error }) (map[string]any, error) {
	var id, w, us, k, l, t, m, sb string
	var at int64
	var dis sql.NullInt64
	if err := sc.Scan(&id, &w, &us, &k, &l, &t, &m, &sb, &at, &dis); err != nil {
		return nil, err
	}
	return noticeJSON(id, w, us, k, l, t, m, sb, at, dis), nil
}

func (s *Server) handleNotices(c *Ctx) error {
	u := c.P.User
	q := `SELECT ` + noticeCols + ` FROM notices WHERE ` + noticeScope
	if c.Query("all") != "1" && c.Query("all") != "true" {
		q += ` AND dismissed_at IS NULL`
	}
	rows, err := s.DB.Query(q+` ORDER BY created_at DESC LIMIT 200`, u.ID, u.ID, u.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		if n, err := scanNotice(rows); err == nil {
			out = append(out, n)
		}
	}
	return c.OK(200, "notice.list", map[string]any{"notices": out})
}

func (s *Server) handleDismissNotice(c *Ctx) error {
	u := c.P.User
	res, err := s.DB.Exec(`UPDATE notices SET dismissed_at=COALESCE(dismissed_at,?) WHERE id=? AND `+noticeScope, store.Now(), c.Param("noticeId"), u.ID, u.ID, u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound("Notice")
	}
	n, err := scanNotice(s.DB.QueryRow(`SELECT `+noticeCols+` FROM notices WHERE id=?`, c.Param("noticeId")))
	if err != nil {
		return err
	}
	return c.OK(200, "notice.dismissed", map[string]any{"notice": n})
}

func (s *Server) handleDismissAllNotices(c *Ctx) error {
	u := c.P.User
	res, err := s.DB.Exec(`UPDATE notices SET dismissed_at=? WHERE dismissed_at IS NULL AND `+noticeScope, store.Now(), u.ID, u.ID, u.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	return c.OK(200, "notice.dismissed_all", map[string]any{"dismissed": n})
}

// ---------------------------------------------------------------------------
// Billing status, admin policies, checkout stub
// ---------------------------------------------------------------------------

func (s *Server) policyJSON(wallet string) map[string]any {
	p := s.walletPolicy(wallet)
	b, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["effective"] = map[string]any{
		"maxActive": nullPos(s.maxActiveFor(wallet)), "startsPerMinute": nullPos(intOr(p.StartsPerMinute, s.Cfg.StartsPerMinute)),
		"startsPerHour": nullPos(intOr(p.StartsPerHour, s.Cfg.StartsPerHour)), "startsPerDay": nullPos(intOr(p.StartsPerDay, s.Cfg.StartsPerDay)),
	}
	return m
}

func nullPos(v int) any {
	if v <= 0 {
		return nil
	}
	return v
}

func (s *Server) graceJSON(wallet string) map[string]any {
	st := s.walletState(wallet)
	if !st.GraceSince.Valid {
		return map[string]any{"active": false, "since": nil, "until": nil}
	}
	return map[string]any{"active": true, "since": tsMs(st.GraceSince.Int64), "until": tsMs(st.GraceSince.Int64 + graceDuration.Milliseconds())}
}

func (s *Server) handleBilling(c *Ctx) error {
	org, err := c.Scope("")
	if err != nil {
		return err
	}
	w := walletOf(c.P.User, org)
	out := map[string]any{"wallet": s.walletJSON(w), "policy": s.policyJSON(w), "extraStarts": s.extraStartsJSON(w),
		"grace": s.graceJSON(w), "spentThisMonth": round6(s.spentThisMonth(w, "")), "enforced": s.Cfg.EnforceCredits,
		"payments": false, "billingUrl": s.Cfg.PublicURL + "/#/credits"}
	if org != nil {
		out["teamId"] = org.ID
	}
	return c.OK(200, "billing.status", out)
}

// adminWallet resolves a wallet reference: user id/login or org id/name.
func (s *Server) adminWallet(ref string) (string, error) {
	if u, err := s.UserByLogin(ref); err == nil {
		return u.ID, nil
	}
	var id string
	if s.DB.QueryRow(`SELECT id FROM orgs WHERE id=? OR name=? COLLATE NOCASE LIMIT 1`, ref, ref).Scan(&id) == nil {
		return id, nil
	}
	return "", Err(404, "not_found", "No account or organization "+ref+".")
}

func (s *Server) handleAdminPolicyGet(c *Ctx) error {
	w, err := s.adminWallet(c.Param("wallet"))
	if err != nil {
		return err
	}
	return c.OK(200, "admin.policy", map[string]any{"wallet": s.walletJSON(w), "policy": s.policyJSON(w), "extraStarts": s.extraStartsJSON(w), "grace": s.graceJSON(w)})
}

func (s *Server) handleAdminPolicyPut(c *Ctx) error {
	w, err := s.adminWallet(c.Param("wallet"))
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(c.R.Body, 1<<20))
	cur := s.walletPolicy(w)
	merged := map[string]any{}
	b, _ := json.Marshal(cur)
	_ = json.Unmarshal(b, &merged)
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(body, &patch); err != nil {
		return Err(400, "invalid_json", "Request body must be a JSON object.")
	}
	known := map[string]bool{}
	for k := range merged {
		known[k] = true
	}
	delete(patch, "effective") // read-only (GET echoes it)
	for k, v := range patch {
		if !known[k] {
			return Err(400, "unknown_field", "Unknown policy field "+k+".")
		}
		var x any
		_ = json.Unmarshal(v, &x)
		merged[k] = x
	}
	mb, _ := json.Marshal(merged)
	var p walletPolicy
	if err := json.Unmarshal(mb, &p); err != nil {
		return Err(400, "invalid_policy", err.Error())
	}
	for _, sz := range p.AllowedSizes {
		if _, ok := api.SizeByName(sz); !ok {
			return Err(400, "invalid_size", "Unknown size "+sz+".")
		}
	}
	for _, v := range []*int{p.MaxActive, p.StartsPerMinute, p.StartsPerHour, p.StartsPerDay, p.MaxTTLSeconds, p.SavedSnapshotAllowance} {
		if v != nil && *v < 0 {
			return Err(400, "invalid_policy", "Limits must be 0 or more (null = server default).")
		}
	}
	// A negative credit cap refused every start (bugs-r10-sonnet 10).
	for _, v := range []*float64{p.CreditCapMonthly, p.LowCreditAlert, p.MonthlyCredits} {
		if v != nil && (*v < 0 || math.IsNaN(*v) || math.IsInf(*v, 0)) {
			return Err(400, "invalid_policy", "Credit amounts must be 0 or more (null = none).")
		}
	}
	if _, err := s.DB.Exec(`INSERT INTO wallet_policies(wallet_id,data,updated_at) VALUES(?,?,?) ON CONFLICT(wallet_id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`,
		w, string(mb), store.Now()); err != nil {
		return err
	}
	s.audit(c, "policy.updated", w, map[string]any{"policy": json.RawMessage(mb)})
	return s.handleAdminPolicyGet(c)
}

// handleCheckout (AUTH-018): CrowdVM has no payments; a claimed agent uses
// the account's credits.
func (s *Server) handleCheckout(c *Ctx) error {
	org, err := c.Scope("")
	if err != nil {
		return err
	}
	w := walletOf(c.P.User, org)
	return c.OK(200, "billing.checkout", map[string]any{"checkoutRequired": false, "url": nil, "paymentRequired": false,
		"message": "No payment required: this CrowdVM server runs on credits. Ask an administrator for credits or contribute a node to earn them.",
		"wallet":  s.walletJSON(w)})
}

// ---------------------------------------------------------------------------
// Restriction policy on create / fork / resume / update (BILL-022)
// ---------------------------------------------------------------------------

var policyRoutes = map[string]string{
	"POST /api/v1/sandboxes":           "create",
	"POST /api/v1/sandboxes/{}/fork":   "fork",
	"POST /api/v1/sandboxes/{}/resume": "resume",
	"PATCH /api/v1/sandboxes/{}":       "update",
	"POST /api/v1/saved-snapshots":     "save",
	"POST /api/v1/named-snapshots":     "save",
}

// policyFields: the request fields policyGuard reads.
var policyFields = []string{"type", "ttlSeconds", "org", "teamId", "from"}

func (s *Server) policyGuard(c *Ctx) error {
	op, ok := policyRoutes[normPattern(c.R.Pattern)]
	if !ok {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(c.R.Body, 8<<20))
	if err != nil {
		return Err(400, "invalid_json", "Could not read the request body.")
	}
	restore := func(b []byte) {
		c.R.Body = io.NopCloser(strings.NewReader(string(b)))
		c.R.ContentLength = int64(len(b))
	}
	restore(body)
	var req map[string]any
	if len(strings.TrimSpace(string(body))) > 0 && json.Unmarshal(body, &req) != nil {
		return nil // the handler reports the bad JSON
	}
	if req == nil {
		req = map[string]any{}
	}
	// The handlers decode with encoding/json, which matches keys in any
	// case; this guard reads the map, which does not: "Type" or "TtlSeconds"
	// passed the policy unchecked. Fields it reads must be spelled exactly
	// (bugs-r11-opus 3).
	for k := range req {
		for _, f := range policyFields {
			if k != f && strings.EqualFold(k, f) {
				return Err(400, "invalid_field", "Spell the field "+f+" exactly (got "+k+").")
			}
		}
	}
	// Policies that apply: the caller's personal policy and the wallet's.
	wallets := []string{c.P.User.ID}
	// inherited: the size a fork or a create from a saved snapshot takes
	// when no type is sent; the size policy applies to it too
	// (bugs-r10-sonnet 4).
	inherited := ""
	switch op {
	case "create", "save":
		bodyOrg, _ := req["org"].(string)
		if bodyOrg == "" {
			bodyOrg, _ = req["teamId"].(string)
		}
		org, err := c.Scope(bodyOrg)
		if err == nil && org != nil {
			wallets = append(wallets, org.ID)
		}
		if from, _ := req["from"].(string); op == "create" && from != "" && err == nil {
			if src, err := s.resolveFrom(c.P.User, from, org); err == nil {
				inherited = src.Size
			}
		}
	default:
		if sb, err := s.SandboxByID(c.Param("sandboxId")); err == nil {
			if sb.OrgID != "" {
				wallets = append(wallets, sb.OrgID)
			}
			if op == "fork" {
				inherited = sb.Size
			}
		}
	}
	changed := false
	for _, w := range wallets {
		p := s.walletPolicy(w)
		if op == "save" {
			continue // the allowance is per wallet (handleSaveSaved, SNAP-020)
		}
		if len(p.AllowedSizes) > 0 {
			t, _ := req["type"].(string)
			if t == "" {
				t = inherited
			}
			if t != "" {
				sz, _ := api.SizeByName(t)
				if !slices.Contains(p.AllowedSizes, sz.Name) && !slices.Contains(p.AllowedSizes, t) {
					// Boat gates sizes on the plan (403 machine_class_plan_required);
					// CrowdVM gates them on the admin's size policy for the wallet.
					return ErrD(403, "machine_class_plan_required", "The "+sz.Name+" size is not allowed for this account: the administrator's size policy allows only "+strings.Join(p.AllowedSizes, ", ")+". Pick one of those, or ask an administrator to allow "+sz.Name+".",
						map[string]any{"allowedSizes": p.AllowedSizes, "type": t, "requestedType": sz.Name, "policy": "admin"})
				}
			} else if op == "create" {
				if def, _ := api.SizeByName(""); !slices.Contains(p.AllowedSizes, def.Name) {
					req["type"], changed = p.AllowedSizes[0], true
				}
			}
		}
		if max, code := p.ttlLimit(); max > 0 {
			v, has := req["ttlSeconds"]
			refuse := ErrD(400, code, fmt.Sprintf("This account needs auto-stop: ttlSeconds at most %d.", max), map[string]any{"maxTtlSeconds": max})
			switch {
			case has && v == nil:
				return refuse
			case has:
				if f, ok := v.(float64); ok && (f <= 0 || f > float64(max)) {
					return refuse
				}
			case op != "update" && max < 3600:
				req["ttlSeconds"], changed = max, true
			}
		}
	}
	if changed {
		b, _ := json.Marshal(req)
		restore(b)
		c.R.Header.Set("Content-Length", fmt.Sprint(len(b)))
	}
	return nil
}

var _ = http.MethodPost

// limitsPolicy adds the credits-policy fields of GET /limits (BILL-011).
func (s *Server) limitsPolicy(wallet string, out map[string]any) {
	p := s.walletPolicy(wallet)
	ex := s.extraStartsJSON(wallet)
	out["extraStarts"] = ex
	out["grace"] = s.graceJSON(wallet)
	if enabled, _ := ex["enabled"].(bool); enabled {
		out["nextStartDollars"] = round6(extraStartCredits * 3600 / float64(s.Cfg.SecondsPerDollar))
		out["nextStartCredits"] = extraStartCredits
	}
	// Boat: startWindowFull is present only while a limit is full.
	out["startWindowFull"] = nil
	wins := s.startWindowsFor(wallet)
	exOn, _ := ex["enabled"].(bool)
	if exOn && (wins[0].limit > 0 || wins[1].limit > 0 || wins[2].limit > 0) {
		with := map[string]any{}
		for _, w := range wins {
			key := map[string]string{"minute": "perMinute", "hour": "perHour", "day": "perDay"}[w.name]
			if w.limit > 0 {
				with[key] = w.limit * extraStartMult(w.name)
			} else {
				with[key] = nil
			}
		}
		out["startLimitsWithWallet"] = with
	}
	now := store.Now()
	for _, w := range wins {
		win, ok := out["starts"].(map[string]any)[w.name].(map[string]any)
		if !ok || win["remaining"] != 0 {
			continue
		}
		var oldest int64
		_ = s.DB.QueryRow(`SELECT COALESCE(MIN(at),0) FROM starts WHERE wallet_id=? AND at>?`, wallet, now-w.dur.Milliseconds()).Scan(&oldest)
		used, _ := win["used"].(int)
		var paidBy any
		msg := fmt.Sprintf("The start limit of %d per %s is full.", w.limit, w.name)
		help := []string{"Wait until " + time.UnixMilli(oldest+w.dur.Milliseconds()).UTC().Format(time.RFC3339) + ", when the oldest start leaves the window."}
		if s.extraStartRefusal(wallet, w, used, 1) == "" {
			paidBy = "wallet"
			msg = fmt.Sprintf("The start limit of %d per %s is full; the next start is an extra start paid from the wallet (%.2f credits).", w.limit, w.name, extraStartCredits)
		} else {
			help = append(help, "Turn on extra starts (crowdvm billing extra-starts on) to start beyond the limit for credits, or ask an administrator for a higher limit.")
		}
		out["startWindowFull"] = map[string]any{"window": w.name, "limit": w.limit, "freesAt": tsMs(oldest + w.dur.Milliseconds()),
			"paidBy": paidBy, "message": msg, "help": help}
		break
	}
	if p.MonthlyCredits != nil {
		out["planIncluded"] = map[string]any{"credits": *p.MonthlyCredits, "vmHours": *p.MonthlyCredits,
			"vmHoursRemaining": round6(math.Max(0, *p.MonthlyCredits-s.spentThisMonth(wallet, "")))}
	}
	maxTTL, _ := p.ttlLimit()
	out["restrictions"] = map[string]any{"maxTtlSeconds": nullPos(maxTTL), "allowedSizes": p.AllowedSizes, "savedSnapshotAllowance": p.SavedSnapshotAllowance,
		"creditCapMonthly": p.CreditCapMonthly, "restricted": p.Restricted != nil && *p.Restricted}
	if len(p.AllowedSizes) > 0 {
		if types, ok := out["machineTypes"].([]map[string]any); ok {
			for _, t := range types {
				if !slices.Contains(p.AllowedSizes, t["type"].(string)) {
					t["available"], t["reason"] = false, "not_allowed_by_policy"
				}
			}
		}
	}
	out["planNudge"] = s.planNudge(wallet, out)
}

// planNudge tells the caller what to do before starts fail (null = nothing).
func (s *Server) planNudge(wallet string, out map[string]any) any {
	url := s.Cfg.PublicURL + "/#/credits"
	bal := s.Balance(wallet)
	if g := s.graceJSON(wallet); g["active"] == true {
		return map[string]any{"reason": "grace", "message": "Credits are used up; running sandboxes stop when the grace period ends. Ask an administrator for credits.", "billingUrl": url, "until": g["until"]}
	}
	if th, ok := s.lowCreditThreshold(wallet); ok && bal < th {
		return map[string]any{"reason": "low_credit", "message": fmt.Sprintf("%.2f credits left. Ask an administrator for credits or contribute a node to earn them.", bal), "billingUrl": url}
	}
	if full, _ := out["startWindowFull"].(map[string]any); full != nil {
		if ex, _ := out["extraStarts"].(map[string]any); ex["enabled"] != true {
			return map[string]any{"reason": "start_limit", "message": "The start limit is reached. Turn on extra starts (crowdvm billing extra-starts on) to start beyond it for credits.", "billingUrl": url}
		}
	}
	return nil
}

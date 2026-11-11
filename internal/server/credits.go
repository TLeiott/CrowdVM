package server

import (
	"context"
	"database/sql"
	"math"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Credits replace Boat's billing: 1 credit = 1 hour of a "default" machine.
// Running sandboxes spend hours*multiplier from their wallet (the creator's
// account or the org); the owner of the hosting node earns NodeEarnRate of it.

// walletAdd changes a wallet balance and writes a ledger row.
func walletAdd(tx *sql.Tx, wallet string, delta float64, kind, reason, sandboxID, nodeID, actor string) error {
	// bugs-r10-sonnet 11: never store a non-finite balance.
	if math.IsNaN(delta) || math.IsInf(delta, 0) {
		return Err(400, "invalid_amount", "amount must be a finite number.")
	}
	var cur float64
	_ = tx.QueryRow(`SELECT balance FROM wallets WHERE id=?`, wallet).Scan(&cur)
	if next := cur + delta; math.IsNaN(next) || math.IsInf(next, 0) {
		return Err(400, "invalid_amount", "The resulting balance is out of range.")
	}
	now := store.Now()
	if _, err := tx.Exec(`INSERT INTO wallets(id,balance,updated_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET balance=balance+excluded.balance, updated_at=excluded.updated_at`,
		wallet, delta, now); err != nil {
		return err
	}
	if delta == 0 && kind != "grant" {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO credit_ledger(wallet_id,delta,kind,reason,sandbox_id,node_id,actor_id,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		wallet, delta, kind, reason, nz(sandboxID), nz(nodeID), nz(actor), now)
	return err
}

// GrantCredits adds (or with a negative amount removes) credits.
func (s *Server) GrantCredits(wallet string, amount float64, reason, actor string) error {
	return s.DB.Tx(s.ctx, func(tx *sql.Tx) error { return walletAdd(tx, wallet, amount, "grant", reason, "", "", actor) })
}

// Balance returns a wallet's credits.
// walletDebit takes amount from wallet as a transfer, only if the wallet
// holds it: check and debit are one statement, so transfers at once cannot
// overdraw it (bugs-r3-opus 14, bugs-r4-sol 2). It returns insufficient
// otherwise.
func walletDebit(tx *sql.Tx, wallet string, amount float64, reason, actor string, insufficient error) (float64, error) {
	// The API shows balances rounded to 6 decimals, which can round up:
	// asking for the shown balance takes the exact balance (bugs-r5-opus 6).
	var bal float64
	_ = tx.QueryRow(`SELECT balance FROM wallets WHERE id=?`, wallet).Scan(&bal)
	if amount > bal && round6(amount) <= round6(bal) { // compared as shown: no float edge at the 7th decimal (bugs-r6-opus 5)
		amount = bal
	}
	res, err := tx.Exec(`UPDATE wallets SET balance=balance-?, updated_at=? WHERE id=? AND balance>=?`, amount, store.Now(), wallet, amount)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 || amount <= 0 {
		return 0, insufficient
	}
	_, err = tx.Exec(`INSERT INTO credit_ledger(wallet_id,delta,kind,reason,sandbox_id,node_id,actor_id,created_at) VALUES(?,?,?,?,NULL,NULL,?,?)`,
		wallet, -amount, "transfer", reason, nz(actor), store.Now())
	return amount, err
}

func (s *Server) Balance(wallet string) float64 {
	var b float64
	_ = s.DB.QueryRow(`SELECT balance FROM wallets WHERE id=?`, wallet).Scan(&b)
	return b
}

func round6(f float64) float64 { return math.Round(f*1e6) / 1e6 }

func (s *Server) walletJSON(wallet string) map[string]any {
	b := s.Balance(wallet)
	return map[string]any{"id": wallet, "balanceCredits": round6(b), "balanceHours": round6(b),
		"balanceDollars": round6(b * 3600 / float64(s.Cfg.SecondsPerDollar)), "enforced": s.Cfg.EnforceCredits}
}

// checkCredits refuses starts for empty wallets when enforcement is on,
// and over-cap starts (wallet credit cap, org member usage cap) always.
func (s *Server) checkCredits(u *User, org *Org) error {
	if err := s.checkOrgActive(org); err != nil {
		return err
	}
	w := walletOf(u, org)
	if b := s.Balance(w); s.Cfg.EnforceCredits && b <= 0 {
		return ErrD(402, "billing_required", "The wallet has no credits left. Ask an administrator for credits or contribute a node to earn them.",
			map[string]any{"balanceCredits": round6(b), "billingUrl": s.Cfg.PublicURL + "/#/credits"})
	}
	if err := s.checkCreditCap(w); err != nil {
		return err
	}
	if org != nil {
		var capU sql.NullFloat64
		_ = s.DB.QueryRow(`SELECT usage_cap_credits FROM org_members WHERE org_id=? AND user_id=?`, org.ID, u.ID).Scan(&capU)
		if capU.Valid {
			now := time.Now().UTC()
			month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			var used float64
			_ = s.DB.QueryRow(`SELECT COALESCE(-SUM(delta),0) FROM credit_ledger WHERE wallet_id=? AND actor_id=? AND kind IN ('usage','extra_start') AND created_at>=?`, org.ID, u.ID, month.UnixMilli()).Scan(&used)
			if used >= capU.Float64 {
				return ErrD(402, "team_member_cap_reached", "You used your usage cap in this organization. Ask the owner to raise it.",
					map[string]any{"capCredits": capU.Float64, "usedCredits": round6(used)})
			}
		}
	}
	return nil
}

// openRun starts metering sb (called when its VM is launched).
func (s *Server) openRun(sb *Sandbox) {
	spec, _ := api.SizeByName(sb.Size)
	now := store.Now()
	_, _ = s.DB.Exec(`UPDATE sandbox_runs SET ended_at=? WHERE sandbox_id=? AND ended_at IS NULL`, now, sb.ID)
	if s.billingPausedByID(sb.ID) {
		return // refused stop or not answering (lifecycle_pause.go)
	}
	_, _ = s.DB.Exec(`INSERT INTO sandbox_runs(sandbox_id,wallet_id,owner_id,node_id,size,multiplier,started_at,billed_until) VALUES(?,?,?,?,?,?,?,?)`,
		sb.ID, sb.Wallet(), sb.OwnerID, nz(sb.NodeID), spec.Name, spec.Multiplier, now, now)
}

// closeRun stops metering sb.
func (s *Server) closeRun(sbID string) {
	_, _ = s.DB.Exec(`UPDATE sandbox_runs SET ended_at=? WHERE sandbox_id=? AND ended_at IS NULL`, store.Now(), sbID)
}

func (s *Server) billingLoop(ctx context.Context) {
	every(ctx, s.Cfg.BillingInterval, func() { s.BillNow() })
}

// BillNow charges every metered run up to now (also called by tests).
func (s *Server) BillNow() {
	now := store.Now()
	type run struct {
		id                      int64
		sb, wallet, owner, node string
		mult                    float64
		billed                  int64
		ended                   sql.NullInt64
	}
	rows, err := s.DB.Query(`SELECT id,sandbox_id,wallet_id,owner_id,COALESCE(node_id,''),multiplier,billed_until,ended_at FROM sandbox_runs WHERE ended_at IS NULL OR billed_until < ended_at`)
	if err != nil {
		return
	}
	var runs []run
	for rows.Next() {
		var r run
		if rows.Scan(&r.id, &r.sb, &r.wallet, &r.owner, &r.node, &r.mult, &r.billed, &r.ended) == nil {
			runs = append(runs, r)
		}
	}
	rows.Close()
	for _, r := range runs {
		until := now
		if r.ended.Valid && r.ended.Int64 < until {
			until = r.ended.Int64
		}
		if until <= r.billed {
			continue
		}
		credits := float64(until-r.billed) / 3600_000 * r.mult
		var nodeOwner string
		if r.node != "" {
			_ = s.DB.QueryRow(`SELECT COALESCE(owner_id,'') FROM nodes WHERE id=?`, r.node).Scan(&nodeOwner)
		}
		_ = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
			if err := walletAdd(tx, r.wallet, -credits, "usage", "sandbox time", r.sb, r.node, r.owner); err != nil {
				return err
			}
			if nodeOwner != "" && s.Cfg.NodeEarnRate > 0 {
				if err := walletAdd(tx, nodeOwner, credits*s.Cfg.NodeEarnRate, "earn", "hosted sandbox time", r.sb, r.node, ""); err != nil {
					return err
				}
			}
			_, err := tx.Exec(`UPDATE sandbox_runs SET billed_until=? WHERE id=?`, until, r.id)
			return err
		})
	}
	s.billingPolicies() // quotas, low-credit alerts, 24 h grace, caps (acct_billing.go)
}

// usageSeconds returns billable (multiplied) seconds of sb in [since, until].
func (s *Server) usageSeconds(sbID string, since, until int64) (float64, bool) {
	rows, err := s.DB.Query(`SELECT multiplier,started_at,ended_at FROM sandbox_runs WHERE sandbox_id=?`, sbID)
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	var total float64
	running := false
	for rows.Next() {
		var m float64
		var st int64
		var en sql.NullInt64
		if rows.Scan(&m, &st, &en) != nil {
			continue
		}
		end := until
		if en.Valid {
			end = min(en.Int64, until)
		} else {
			running = true
		}
		start := max(st, since)
		if end > start {
			total += float64(end-start) / 1000 * m
		}
	}
	return total, running
}

func (s *Server) creditRoutes() {
	s.API("GET /credits", "account.read", s.handleCredits)
	s.API("POST /credits/transfer", "account.admin", s.handleCreditTransfer)
}

func (s *Server) handleCredits(c *Ctx) error {
	org, err := c.Scope("")
	if err != nil {
		return err
	}
	w := walletOf(c.P.User, org)
	rows, err := s.DB.Query(`SELECT delta,kind,reason,COALESCE(sandbox_id,''),COALESCE(node_id,''),created_at FROM credit_ledger WHERE wallet_id=? ORDER BY id DESC LIMIT 100`, w)
	if err != nil {
		return err
	}
	defer rows.Close()
	ledger := []map[string]any{}
	for rows.Next() {
		var d float64
		var k, r, sb, n string
		var at int64
		if rows.Scan(&d, &k, &r, &sb, &n, &at) == nil {
			ledger = append(ledger, map[string]any{"delta": round6(d), "kind": k, "reason": r, "sandboxId": nz(sb), "nodeId": nz(n), "createdAt": tsMs(at)})
		}
	}
	var earned, spent float64
	_ = s.DB.QueryRow(`SELECT COALESCE(SUM(delta),0) FROM credit_ledger WHERE wallet_id=? AND kind='earn'`, w).Scan(&earned)
	_ = s.DB.QueryRow(`SELECT COALESCE(-SUM(delta),0) FROM credit_ledger WHERE wallet_id=? AND kind='usage'`, w).Scan(&spent)
	out := map[string]any{"wallet": s.walletJSON(w), "ledger": ledger, "earnedCredits": round6(earned), "spentCredits": round6(spent),
		"rates": map[string]any{"creditsPerDefaultHour": 1, "nodeEarnRate": s.Cfg.NodeEarnRate, "multipliers": sizeMultipliers()}}
	if org != nil {
		out["teamId"] = org.ID
	}
	return c.OK(200, "credits.info", out)
}

func sizeMultipliers() map[string]float64 {
	m := map[string]float64{}
	for _, sz := range api.Sizes {
		m[sz.Name] = sz.Multiplier
	}
	return m
}

func (s *Server) handleCreditTransfer(c *Ctx) error {
	var req struct {
		From   string  `json:"from"`
		To     string  `json:"to"`
		Amount float64 `json:"amount"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.Amount <= 0 || math.IsNaN(req.Amount) {
		return Err(400, "invalid_amount", "amount must be positive.")
	}
	from, err := s.resolveOrgRef(c.P.User, req.From)
	if err != nil {
		return err
	}
	// "to" is an organization, or another user's personal wallet (username).
	toWallet := ""
	to, err := s.resolveOrgRef(c.P.User, req.To)
	if err != nil {
		u, uerr := s.UserByLogin(req.To)
		if req.To == "" || uerr != nil {
			return err
		}
		toWallet = u.ID
	}
	if from != nil {
		if r := s.memberRole(from.ID, c.P.User.ID); r != "owner" && r != "admin" {
			return Err(403, "forbidden", "Only owners and admins move credits out of an organization.")
		}
	}
	fw, tw := walletOf(c.P.User, from), walletOf(c.P.User, to)
	if toWallet != "" {
		tw = toWallet
	}
	if fw == tw {
		return Err(400, "same_wallet", "from and to are the same wallet.")
	}
	insufficient := Err(402, "insufficient_credits", "The source wallet does not have that many credits.")
	err = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		// The balance check and the debit are one statement, so transfers
		// running at once cannot overdraw the wallet (bugs-r3-opus 14).
		took, err := walletDebit(tx, fw, req.Amount, "transfer to "+tw, c.P.User.ID, insufficient)
		if err != nil {
			return err
		}
		return walletAdd(tx, tw, took, "transfer", "transfer from "+fw, "", "", c.P.User.ID)
	})
	if err != nil {
		return err
	}
	return c.OK(200, "credits.transferred", map[string]any{"from": s.walletJSON(fw), "to": s.walletJSON(tw)})
}

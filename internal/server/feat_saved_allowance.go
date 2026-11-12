package server

// Saved-snapshot allowance (SNAP-020, ADAPTED): every wallet (account or
// organization) includes Config.SavedSnapshotsIncluded saved snapshots
// (default 10; an admin wallet policy savedSnapshotAllowance overrides
// it). More are allowed: each one above the included count costs
// SavedSnapshotExtraDollarsPerDay (default $0.056) in credits, charged once
// per UTC day. With credit enforcement on, saving a new one above the
// included count from an empty wallet fails with 402 credit_required.

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

const (
	defaultSavedIncluded       = 10
	defaultSavedExtraDollarDay = 0.056
)

func init() {
	store.Register("saved_allowance", store.Migration{Version: 1, SQL: `
CREATE TABLE saved_charges (
  wallet_id TEXT PRIMARY KEY,
  day TEXT NOT NULL
);`})
}

// savedIncluded is the wallet's included saved snapshots.
func (s *Server) savedIncluded(wallet string) int {
	if p := s.walletPolicy(wallet).SavedSnapshotAllowance; p != nil {
		return *p
	}
	if s.Cfg.SavedSnapshotsIncluded > 0 {
		return s.Cfg.SavedSnapshotsIncluded
	}
	return defaultSavedIncluded
}

func (s *Server) savedCount(wallet string) int {
	var n int
	// Saves that hold a disk, or are being saved; a failed first save stores
	// nothing and costs nothing (bugs-r19-opus 2).
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM saved_snapshots WHERE owner_id=? AND (snapshot_id IS NOT NULL OR status='saving')`, wallet).Scan(&n)
	return n
}

// savedExtraDollarsPerDay is the price of one saved snapshot above the allowance.
func (s *Server) savedExtraDollarsPerDay() float64 {
	if s.Cfg.SavedSnapshotExtraDollarsPerDay > 0 {
		return s.Cfg.SavedSnapshotExtraDollarsPerDay
	}
	return defaultSavedExtraDollarDay
}

func (s *Server) savedExtraCreditsPerDay() float64 {
	return s.savedExtraDollarsPerDay() * float64(s.Cfg.SecondsPerDollar) / 3600
}

// checkSavedAllowance runs before a save of a new name into wallet.
func (s *Server) checkSavedAllowance(wallet string) error {
	inc := s.savedIncluded(wallet)
	if s.savedCount(wallet) < inc || !s.Cfg.EnforceCredits || s.Balance(wallet) > 0 {
		return nil
	}
	return ErrD(402, "credit_required", fmt.Sprintf("This wallet's %d included saved snapshots are used and it has no credits for more (%.4f credits a day each). Delete a saved snapshot or ask an administrator for credits.",
		inc, s.savedExtraCreditsPerDay()), map[string]any{"included": inc, "pricePerExtraDailyDollars": s.savedExtraDollarsPerDay()})
}

// allowanceJSON is the "allowance" object of GET /saved-snapshots.
func (s *Server) allowanceJSON(wallet string, org *Org) map[string]any {
	inc, used := s.savedIncluded(wallet), s.savedCount(wallet)
	extra := max(0, used-inc)
	day := s.savedExtraDollarsPerDay()
	var orgRef, orgName any
	if org != nil {
		orgRef, orgName = org.ID, org.Name
	}
	return map[string]any{"org": orgRef, "orgName": orgName, "included": inc, "free": max(0, inc-used), "used": used, "extra": extra,
		"extraDailyDollars": round6(float64(extra) * day), "extraMonthlyDollars": round6(float64(extra) * day * 30),
		"pricePerExtraDailyDollars": day, "pricePerExtraMonthlyDollars": round6(day * 30),
		"extraDailyCredits": round6(float64(extra) * s.savedExtraCreditsPerDay())}
}

// chargeSavedExtras bills each wallet's saved snapshots above its allowance
// once per UTC day (billing tick).
func (s *Server) chargeSavedExtras() {
	today := time.Now().UTC().Format("2006-01-02")
	rows, err := s.DB.Query(`SELECT owner_id, COUNT(*) FROM saved_snapshots WHERE snapshot_id IS NOT NULL GROUP BY owner_id`) // bugs-r19-opus 2
	if err != nil {
		return
	}
	counts := map[string]int{}
	for rows.Next() {
		var w string
		var n int
		if rows.Scan(&w, &n) == nil {
			counts[w] = n
		}
	}
	rows.Close()
	rate := s.savedExtraCreditsPerDay()
	for w, n := range counts {
		extra := n - s.savedIncluded(w)
		if extra <= 0 {
			continue
		}
		_ = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
			var last string
			_ = tx.QueryRow(`SELECT day FROM saved_charges WHERE wallet_id=?`, w).Scan(&last)
			if last == today {
				return nil
			}
			if err := walletAdd(tx, w, -float64(extra)*rate, "usage", fmt.Sprintf("%d saved snapshots above the included %d (%s)", extra, n-extra, today), "", "", ""); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO saved_charges(wallet_id,day) VALUES(?,?) ON CONFLICT(wallet_id) DO UPDATE SET day=excluded.day`, w, today)
			return err
		})
	}
}

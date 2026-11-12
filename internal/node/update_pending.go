package node

// A bad build must not strand a node that updated itself: before the
// update swaps a new crowdvm-node in, it writes <data dir>/update-pending.json
// (from, to, starts). A supervised daemon of that build counts its starts
// there (CheckUpdateStart) and deletes the marker after its first
// heartbeat. A build that starts maxUpdateStarts times without reaching
// the server is rolled back by the next start and listed in
// <data dir>/update-skip.json, which auto-update honors (and a manual
// update without --force).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

const (
	pendingFile = "update-pending.json"
	skipFile    = "update-skip.json"
)

// maxUpdateStarts: starts a new build gets to reach the server.
const maxUpdateStarts = 3

type pendingUpdate struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Starts int    `json:"starts"`
}

func savePending(dataDir string, p pendingUpdate) error {
	if dataDir == "" {
		return nil
	}
	b, _ := json.Marshal(p)
	return os.WriteFile(filepath.Join(dataDir, pendingFile), b, 0o600)
}

// loadPending: ok is false without a marker (or with a broken one).
func loadPending(dataDir string) (p pendingUpdate, ok bool) {
	b, err := os.ReadFile(filepath.Join(dataDir, pendingFile))
	return p, err == nil && json.Unmarshal(b, &p) == nil
}

func removePending(dataDir string) {
	if dataDir != "" {
		_ = os.Remove(filepath.Join(dataDir, pendingFile))
	}
}

// CheckUpdateStart runs as a supervised daemon starts (exe "": this
// executable). rolledBack: this build kept failing after an update and the
// previous one is back; the daemon exits so the service manager starts it.
func CheckUpdateStart(ctx context.Context, cfg *Config, exe string) (rolledBack bool, err error) {
	p, ok := loadPending(cfg.DataDir)
	switch {
	case !ok:
		removePending(cfg.DataDir) // a broken marker
		return false, nil
	case p.To != Version: // rolled back by hand, or another build installed since
		removePending(cfg.DataDir)
		return false, nil
	case p.Starts < maxUpdateStarts:
		p.Starts++
		return false, savePending(cfg.DataDir, p)
	}
	skipVersion(cfg.DataDir, p.To)
	_, err = SelfUpdate(ctx, cfg, UpdateOptions{Rollback: true, Exe: exe})
	removePending(cfg.DataDir) // also when the rollback failed: this build goes on, once
	if err != nil {
		return false, fmt.Errorf("roll back %s, which failed %d starts after an update: %w", p.To, p.Starts, err)
	}
	return true, nil
}

// confirmUpdate: this build reached the server, so its update worked.
func confirmUpdate(dataDir string) {
	if p, ok := loadPending(dataDir); ok && p.To == Version {
		removePending(dataDir)
	}
}

func loadSkipped(dataDir string) []string {
	var vs []string
	if b, err := os.ReadFile(filepath.Join(dataDir, skipFile)); err == nil {
		_ = json.Unmarshal(b, &vs)
	}
	return vs
}

func isSkipped(dataDir, v string) bool {
	return dataDir != "" && v != "" && slices.Contains(loadSkipped(dataDir), v)
}

// skipVersion lists v as a build that failed on this node.
func skipVersion(dataDir, v string) {
	if dataDir == "" || v == "" || isSkipped(dataDir, v) {
		return
	}
	b, _ := json.Marshal(append(loadSkipped(dataDir), v))
	_ = os.WriteFile(filepath.Join(dataDir, skipFile), b, 0o600)
}

func errSkipped(v string) error {
	return fmt.Errorf("crowdvm-node %s was rolled back on this node after it failed, so it is skipped; `crowdvm-node update --force` installs it anyway", v)
}

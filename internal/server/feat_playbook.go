package server

// Startup playbooks (LIFE-018): a sandbox deployed from a saved snapshot
// records which files it read first (~/.crowdvm/playbook.json, guest event
// playbook.recorded). When the deploy belongs to the saved snapshot's own
// wallet, the server keeps that path list with the saved snapshot and
// passes it to later deploys (GuestConfig.Playbook), so their guests read
// those paths first while the chain is still arriving.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

const playbookMaxPaths = 20000

func init() {
	store.Register("playbook", store.Migration{Version: 1, SQL: `ALTER TABLE saved_snapshots ADD COLUMN playbook TEXT`})
	RegisterFeature("playbook", func(s *Server) error {
		s.Bus.Subscribe(func(e Event) {
			if e.Type == EvGuestEvent && e.Reason == "playbook.recorded" && e.SandboxID != "" {
				id := e.SandboxID
				s.Go(func(ctx context.Context) { s.learnPlaybook(ctx, id) })
			}
		})
		s.Hooks.PrepareGuest = append(s.Hooks.PrepareGuest, func(_ context.Context, sb *Sandbox, g *GuestPrep) error {
			if sb.SourceKind != "saved" || g.Boot != proto.BootCreate {
				return nil
			}
			var raw string
			if s.DB.QueryRow(`SELECT COALESCE(playbook,'') FROM saved_snapshots WHERE id=?`, sb.SourceID).Scan(&raw) == nil && raw != "" {
				_ = json.Unmarshal([]byte(raw), &g.Config.Playbook)
			}
			return nil
		})
		return nil
	})
}

// learnPlaybook stores a deploy's recorded playbook with its saved snapshot.
func (s *Server) learnPlaybook(ctx context.Context, sbID string) {
	sb, err := s.SandboxByID(sbID)
	if err != nil || sb.SourceKind != "saved" || sb.SourceID == "" {
		return
	}
	var owner string
	if s.DB.QueryRow(`SELECT owner_id FROM saved_snapshots WHERE id=?`, sb.SourceID).Scan(&owner) != nil || owner != sb.Wallet() {
		return // another account's deploy (share code) teaches nothing
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := s.envGuestRead(cctx, sb, ".crowdvm/playbook.json")
	var pf struct {
		Paths []string `json:"paths"`
	}
	if err != nil || json.Unmarshal([]byte(raw), &pf) != nil || len(pf.Paths) == 0 {
		return
	}
	if len(pf.Paths) > playbookMaxPaths {
		pf.Paths = pf.Paths[:playbookMaxPaths]
	}
	_, _ = s.DB.Exec(`UPDATE saved_snapshots SET playbook=? WHERE id=?`, mustJSON(pf.Paths), sb.SourceID)
}

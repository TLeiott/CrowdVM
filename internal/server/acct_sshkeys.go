package server

// Making an org sandbox private removes its teammates' SSH keys (ORG-012).
// Keys sent through POST /sandboxes/{id}/sshkey are recorded with the user
// who sent them. On unshare, keys of everyone but the creator are deleted
// from ~/.ssh/authorized_keys: at once when the sandbox runs, else when it
// is next ready.

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

func init() {
	store.Register("acct_sshkeys", store.Migration{Version: 1, SQL: `
CREATE TABLE sandbox_ssh_keys (
  sandbox_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  key TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (sandbox_id, key)
);
`})
	RegisterFeature("acct_sshkeys", func(s *Server) error {
		s.Hooks.Guard = append(s.Hooks.Guard, s.sshKeyGuard)
		s.Bus.Subscribe(s.sshKeysOnEvent)
		return nil
	})
}

// sshKeyFields is "type base64" of an authorized_keys line (no options or
// comment), the identity used to match keys.
func sshKeyFields(line string) string {
	f := strings.Fields(line)
	for i := 0; i+1 < len(f); i++ {
		if strings.HasPrefix(f[i], "ssh-") || strings.HasPrefix(f[i], "ecdsa-") || strings.HasPrefix(f[i], "sk-") {
			return f[i] + " " + f[i+1]
		}
	}
	return ""
}

func (s *Server) sshKeyGuard(c *Ctx) error {
	if normPattern(c.R.Pattern) != "POST /api/v1/sandboxes/{}/sshkey" {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(c.R.Body, 1<<20))
	if err != nil {
		return Err(400, "invalid_json", "Could not read the request body.")
	}
	c.R.Body = io.NopCloser(strings.NewReader(string(body)))
	var req struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(body, &req) == nil {
		if k := sshKeyFields(req.Key); k != "" {
			_, _ = s.DB.Exec(`INSERT INTO sandbox_ssh_keys(sandbox_id,user_id,key,created_at) VALUES(?,?,?,?) ON CONFLICT(sandbox_id,key) DO NOTHING`,
				c.Param("sandboxId"), c.P.User.ID, k, store.Now())
		}
	}
	return nil
}

func (s *Server) sshKeysOnEvent(e Event) {
	switch {
	case e.Type == EvSandboxUpdated && e.Reason == "unshared":
		_ = s.SetSandboxExtra(e.SandboxID, "sshKeysRevokePending", true)
		s.Go(func(ctx context.Context) { s.revokeTeammateKeys(ctx, e.SandboxID) })
	case e.Type == EvSandboxReady:
		sb, err := s.SandboxByID(e.SandboxID)
		if err == nil {
			if p, _ := sb.Extra["sshKeysRevokePending"].(bool); p {
				s.Go(func(ctx context.Context) { s.revokeTeammateKeys(ctx, e.SandboxID) })
			}
		}
	case e.Type == EvSandboxDeleted:
		_, _ = s.DB.Exec(`DELETE FROM sandbox_ssh_keys WHERE sandbox_id=?`, e.SandboxID)
	}
}

// revokeTeammateKeys deletes non-creator keys from authorized_keys.
func (s *Server) revokeTeammateKeys(ctx context.Context, id string) {
	sb, err := s.SandboxByID(id)
	if err != nil || sb.DeletedAt != nil || sb.Visibility != "private" || !sb.Usable() {
		return
	}
	keys := map[string]bool{}
	for _, k := range s.walletIDs(`SELECT key FROM sandbox_ssh_keys WHERE sandbox_id=? AND user_id!=?`, id, sb.OwnerID) {
		keys[k] = true
	}
	// A key the creator also sent stays.
	for _, k := range s.walletIDs(`SELECT key FROM sandbox_ssh_keys WHERE sandbox_id=? AND user_id=?`, id, sb.OwnerID) {
		delete(keys, k)
	}
	if len(keys) > 0 {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		cur, err := s.envGuestRead(ctx, sb, ".ssh/authorized_keys")
		if err != nil {
			s.Log.Warn("unshare: reading authorized_keys failed", "sandbox", id, "err", err)
			return
		}
		var keep []string
		removed := 0
		for _, line := range strings.Split(cur, "\n") {
			if keys[sshKeyFields(line)] {
				removed++
				continue
			}
			keep = append(keep, line)
		}
		if removed > 0 {
			if err := s.envGuestWrite(ctx, sb, ".ssh/authorized_keys", []byte(strings.Join(keep, "\n"))); err != nil {
				s.Log.Warn("unshare: writing authorized_keys failed", "sandbox", id, "err", err)
				return
			}
			_ = s.envGuestRun(ctx, sb, "chmod 600 ~/.ssh/authorized_keys")
		}
		_, _ = s.DB.Exec(`DELETE FROM sandbox_ssh_keys WHERE sandbox_id=? AND user_id!=?`, id, sb.OwnerID)
	}
	_ = s.SetSandboxExtra(id, "sshKeysRevokePending", false)
}

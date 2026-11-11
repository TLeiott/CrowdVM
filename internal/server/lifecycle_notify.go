package server

// Sandbox notices for its owner (LIFE-034/040/041/044, SNAP-008): failing
// stops, forced stops, error holds, node loss, disk 85/95 %. They go into
// the shared notices table (acct_billing.go: GET /notices, dismiss) with
// sandbox_id set, and are mailed to the owner's notification address.

import (
	"time"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

// notifySandbox records a notice for sb's owner unless the same kind was
// noticed for sb within window, and mails it.
func (s *Server) notifySandbox(sb *Sandbox, kind, level string, window time.Duration, title, msg string) {
	if sb == nil {
		return
	}
	if window > 0 {
		var n int
		q, args := `SELECT COUNT(*) FROM notices WHERE user_id=? AND kind=? AND sandbox_id=? AND created_at>?`,
			[]any{sb.OwnerID, kind, sb.ID, store.Now() - window.Milliseconds()}
		if kind == "file_skipped" { // one notice per path (the message names it)
			q, args = q+` AND message=?`, append(args, msg)
		}
		_ = s.DB.QueryRow(q, args...).Scan(&n)
		if n > 0 {
			return
		}
	}
	if _, err := s.DB.Exec(`INSERT INTO notices(id,wallet_id,user_id,kind,level,title,message,sandbox_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		"ntc_"+randToken(16), sb.Wallet(), sb.OwnerID, kind, level, title, msg, sb.ID, store.Now()); err != nil {
		s.Log.Warn("notice", "err", err)
		return
	}
	s.Log.Info("sandbox notice", "user", sb.OwnerID, "sandbox", sb.ID, "kind", kind)
	if addr := s.notifyAddress(sb.OwnerID); addr != "" {
		s.sendMail(addr, "CrowdVM: "+title, msg+"\n\nSandbox: "+sb.Name+" ("+sb.ID+")\n"+s.Cfg.PublicURL+"/#/sandboxes/"+sb.ID+"\n")
	}
}

// onDiskUsage records the guest's last root filesystem usage (shrink
// check, SIZE-010) and alerts at 85 % / 95 % at most once per level per day
// (LIFE-044).
func (s *Server) onDiskUsage(sbID string, used, total int64) {
	if total <= 0 {
		return
	}
	_ = s.SetSandboxExtra(sbID, "disk", map[string]any{"usedBytes": used, "totalBytes": total, "at": store.Now()})
	pct := float64(used) / float64(total) * 100
	level := ""
	switch {
	case pct >= 95:
		level = "95"
	case pct >= 85:
		level = "85"
	default:
		return
	}
	sb, err := s.SandboxByID(sbID)
	if err != nil {
		return
	}
	s.notifySandbox(sb, "disk_usage_"+level, "warning", 24*time.Hour, "Sandbox disk "+level+"% full",
		"The disk of "+sb.Name+" is "+level+"% full. Free space (docker system prune, apt clean, old build output) or resume it with a larger --type.")
}

// diskUsedBytes is the last disk usage the guest reported (0 = unknown).
func diskUsedBytes(sb *Sandbox) int64 {
	m, _ := sb.Extra["disk"].(map[string]any)
	v, _ := m["usedBytes"].(float64)
	return int64(v)
}

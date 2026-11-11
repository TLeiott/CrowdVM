package server

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// ipLimiter is a fixed-window request counter per client IP.
type ipLimiter struct {
	limit  int
	window time.Duration
	mu     sync.Mutex
	m      map[string]*ipWin
	last   time.Time
}

type ipWin struct {
	start time.Time
	n     int
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	return &ipLimiter{limit: limit, window: window, m: map[string]*ipWin{}}
}

// allow counts the request, writes the RateLimit headers and, when over the
// limit, the 429 response. It returns false when the request must stop.
func (l *ipLimiter) allow(w http.ResponseWriter, ip string) bool {
	if l.limit <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	if now.Sub(l.last) > l.window {
		for k, v := range l.m {
			if now.Sub(v.start) > l.window {
				delete(l.m, k)
			}
		}
		l.last = now
	}
	e := l.m[ip]
	if e == nil || now.Sub(e.start) >= l.window {
		e = &ipWin{start: now}
		l.m[ip] = e
	}
	e.n++
	n, start := e.n, e.start
	l.mu.Unlock()
	left := l.limit - n
	if left < 0 {
		left = 0
	}
	reset := int((l.window - now.Sub(start)).Seconds() + 0.999)
	if reset < 1 {
		reset = 1
	}
	secs := int(l.window.Seconds())
	w.Header().Set("RateLimit-Policy", fmt.Sprintf(`"ip";q=%d;w=%d`, l.limit, secs))
	w.Header().Set("RateLimit", fmt.Sprintf(`"ip";r=%d;t=%d`, left, reset))
	if n > l.limit {
		w.Header().Set("Retry-After", fmt.Sprint(reset))
		e := api.NewError(429, "IP_RATE_LIMIT_EXCEEDED", fmt.Sprintf("More than %d requests in %d seconds from this IP address.", l.limit, secs),
			map[string]any{"retryAfter": reset})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		body := map[string]any{"ok": false, "type": "sandbox.error", "status": 429, "code": e.Code, "message": e.Message,
			"error": e.Err, "retryAfter": reset, "requestId": w.Header().Get("X-Request-Id")}
		_, _ = w.Write([]byte(mustJSON(body) + "\n"))
		return false
	}
	return true
}

// keyUsage buffers per-key request counts and last-use times.
type keyUsage struct {
	mu   sync.Mutex
	hits map[string]int
	last map[string]int64
}

func newKeyUsage() *keyUsage { return &keyUsage{hits: map[string]int{}, last: map[string]int64{}} }

func (u *keyUsage) hit(keyID string) {
	u.mu.Lock()
	u.hits[keyID]++
	u.last[keyID] = store.Now()
	u.mu.Unlock()
}

func (u *keyUsage) flush(s *Server) {
	u.mu.Lock()
	hits, last := u.hits, u.last
	u.hits, u.last = map[string]int{}, map[string]int64{}
	u.mu.Unlock()
	if len(hits) == 0 {
		return
	}
	day := time.Now().UTC().Format("2006-01-02")
	for id, n := range hits {
		_, _ = s.DB.Exec(`INSERT INTO api_key_usage(key_id,day,requests) VALUES(?,?,?) ON CONFLICT(key_id,day) DO UPDATE SET requests=requests+excluded.requests`, id, day, n)
		_, _ = s.DB.Exec(`UPDATE api_keys SET last_used_at=? WHERE id=?`, last[id], id)
	}
}

// ---------------------------------------------------------------------------
// Start limits (create, fork, resume) per wallet
// ---------------------------------------------------------------------------

type startWindow struct {
	name  string
	dur   time.Duration
	limit int
}

// startUsage returns the starts object of /limits.
func (s *Server) startUsage(wallet string) map[string]any {
	wins := s.startWindowsFor(wallet)
	out := map[string]any{"unlimited": wins[0].limit <= 0 && wins[1].limit <= 0 && wins[2].limit <= 0}
	now := store.Now()
	for _, w := range wins {
		if w.limit <= 0 {
			out[w.name] = nil
			continue
		}
		var n int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM starts WHERE wallet_id=? AND at>?`, wallet, now-w.dur.Milliseconds()).Scan(&n)
		left := w.limit - n
		if left < 0 {
			left = 0
		}
		out[w.name] = map[string]any{"limit": w.limit, "used": n, "remaining": left}
	}
	return out
}

// checkStarts returns 429 rate_limited when count more starts do not fit
// (unless the wallet buys extra starts, acct_billing.go).
func (s *Server) checkStarts(wallet string, count int) error {
	now := store.Now()
	for _, w := range s.startWindowsFor(wallet) {
		if w.limit <= 0 {
			continue
		}
		var n int
		var oldest int64
		_ = s.DB.QueryRow(`SELECT COUNT(*), COALESCE(MIN(at),0) FROM starts WHERE wallet_id=? AND at>?`, wallet, now-w.dur.Milliseconds()).Scan(&n, &oldest)
		if n+count <= w.limit {
			continue
		}
		if cause := s.extraStartRefusal(wallet, w, n, count); cause != "" {
			free := oldest + w.dur.Milliseconds()
			retry := int((free-now)/1000) + 1
			return ErrD(429, "rate_limited", fmt.Sprintf("Start limit reached: %d starts per %s.", w.limit, w.name), map[string]any{
				"window": w.name, "limit": w.limit, "cause": cause, "retryAfterSeconds": retry,
				"nextStartAt": tsMs(free), "starts": s.startUsage(wallet),
			})
		}
	}
	return nil
}

func (s *Server) recordStart(wallet string) {
	now := store.Now()
	_, _ = s.DB.Exec(`INSERT INTO starts(wallet_id,at) VALUES(?,?)`, wallet, now)
	_, _ = s.DB.Exec(`DELETE FROM starts WHERE at < ?`, now-25*3600_000)
	for _, w := range s.startWindowsFor(wallet) {
		var n int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM starts WHERE wallet_id=? AND at>?`, wallet, now-w.dur.Milliseconds()).Scan(&n)
		if w.limit > 0 && n > w.limit {
			s.chargeExtraStart(wallet) // an extra start (BILL-010)
			return
		}
	}
}

// activeStates are the states that count as "sandboxes at once".
var activeStates = []string{api.StateProvisioning, api.StateProvisioned, api.StateCloning, api.StateReady, api.StateIdle, api.StateRunning}

func (s *Server) activeCount(wallet string) int {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE deleted_at IS NULL AND COALESCE(org_id, owner_id)=? AND state IN ('provisioning','provisioned','cloning','ready','idle','running')`, wallet).Scan(&n)
	return n
}

// checkConcurrent returns 429 limit_reached / member_limit_reached.
func (s *Server) checkConcurrent(u *User, org *Org, count int) error {
	wallet := walletOf(u, org)
	if maxA := s.maxActiveFor(wallet); maxA > 0 && s.activeCount(wallet)+count > maxA {
		return ErrD(429, "limit_reached", fmt.Sprintf("At most %d sandboxes can run at once. Stop one first.", maxA),
			map[string]any{"limit": maxA, "activeSandboxes": s.activeCount(wallet)})
	}
	if org != nil {
		var capN *int
		_ = s.DB.QueryRow(`SELECT concurrent_cap FROM org_members WHERE org_id=? AND user_id=?`, org.ID, u.ID).Scan(&capN)
		if capN != nil {
			var n int
			_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE deleted_at IS NULL AND org_id=? AND owner_id=? AND state IN ('provisioning','provisioned','cloning','ready','idle','running')`, org.ID, u.ID).Scan(&n)
			if n+count > *capN {
				return ErrD(429, "member_limit_reached", fmt.Sprintf("The organization owner capped you at %d sandboxes at once.", *capN), map[string]any{"limit": *capN})
			}
		}
	}
	return nil
}

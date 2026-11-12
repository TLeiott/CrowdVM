package node

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// bugs-r12-opus 1: the page session is no cookie (browsers send cookies to
// every port on 127.0.0.1). /login hands "<nonce>.<hmac>" to the page, which
// keeps it in localStorage (per origin, so per port) and sends it in
// UISessionHeader. The nonce is random per login and listed in <data
// dir>/ui-sessions, so one login can end (POST /api/logout) without rotating
// the token; the HMAC binds the session to the token, which is not in it.
const (
	UISessionHeader = "X-Crowdvm-Ui-Session"
	uiSessionsFile  = "ui-sessions"
	uiSessionTTL    = 30 * 24 * time.Hour
	uiSessionMax    = 20
)

// uiSessions is the set of live session nonces -> expiry, saved on change.
type uiSessions struct {
	mu    sync.Mutex
	path  string
	token string
	m     map[string]time.Time
}

func newUISessions(dataDir, token string) *uiSessions {
	s := &uiSessions{path: filepath.Join(dataDir, uiSessionsFile), token: token, m: map[string]time.Time{}}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.m)
	}
	return s
}

func (s *uiSessions) sign(nonce string) string {
	m := hmac.New(sha256.New, []byte(s.token))
	m.Write([]byte("crowdvm-node-ui-session-v2:" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// save drops expired entries and keeps the newest uiSessionMax; s.mu is held.
func (s *uiSessions) save() {
	now := time.Now()
	for n, exp := range s.m {
		if now.After(exp) {
			delete(s.m, n)
		}
	}
	for len(s.m) > uiSessionMax {
		var oldest string
		for n, exp := range s.m {
			if oldest == "" || exp.Before(s.m[oldest]) {
				oldest = n
			}
		}
		delete(s.m, oldest)
	}
	b, _ := json.Marshal(s.m)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// create starts a session and returns its value.
func (s *uiSessions) create() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	nonce := hex.EncodeToString(b)
	s.mu.Lock()
	s.m[nonce] = time.Now().Add(uiSessionTTL)
	s.save()
	s.mu.Unlock()
	return nonce + "." + s.sign(nonce)
}

// valid reports whether v is a live session of this token.
func (s *uiSessions) valid(v string) bool {
	nonce, sig, ok := strings.Cut(v, ".")
	if !ok || s.token == "" || subtle.ConstantTimeCompare([]byte(sig), []byte(s.sign(nonce))) != 1 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[nonce]
	return ok && time.Now().Before(exp)
}

// revoke ends the session v.
func (s *uiSessions) revoke(v string) {
	nonce, _, _ := strings.Cut(v, ".")
	s.mu.Lock()
	if _, ok := s.m[nonce]; ok {
		delete(s.m, nonce)
		s.save()
	}
	s.mu.Unlock()
}

// HelloProof is the daemon's answer to GET /api/hello?nonce=<nonce>: it shows
// the CLI that the listener holds the control token before the CLI sends it.
func HelloProof(token, nonce string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("crowdvm-node-hello:" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

package server

// Subscription sign-in renewal (AGT-031, AIC-009, AIC-016). A stored
// sign-in with a refresh token is renewed by the server when its access
// token expires within aiRefreshMargin, at the moments it is used: every
// sandbox start (aiPlan), every prompt and every AI proxy request. There is
// no background renewal ("sign-ins renew only while in use").
//
// When the server can renew a credential, sandboxes never receive its
// refresh token: refresh tokens rotate, and a harness renewing on its own
// would invalidate the server's copy. Renewed tokens are pushed to running
// sandboxes that hold real keys.
//
// Token endpoints are the public OAuth clients of Claude Code and the Codex
// CLI. Others (Kimi, Grok) renew only when configured:
// CROWDVM_AI_REFRESH_URL_<CRED> and CROWDVM_AI_REFRESH_CLIENT_<CRED>, with
// <CRED> the credential id upper-cased with "_" (e.g. KIMI_SUBSCRIPTION).
// The same variables override the built-in endpoints (LAN gateways, tests).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const aiRefreshMargin = 10 * time.Minute

type aiRefresher struct{ URL, ClientID, Scope string }

var aiRefreshers = map[string]aiRefresher{
	"claude-subscription":  {URL: "https://console.anthropic.com/v1/oauth/token", ClientID: "9d1c250a-e61b-44d9-88ed-5944d1962f5e"},
	"chatgpt-subscription": {URL: "https://auth.openai.com/oauth/token", ClientID: "app_EMoamEEZ73f0CkXaXp7hrann", Scope: "openid profile email"},
}

func aiRefresherFor(cred string) (aiRefresher, bool) {
	env := strings.ToUpper(strings.ReplaceAll(cred, "-", "_"))
	r := aiRefreshers[cred]
	if v := os.Getenv("CROWDVM_AI_REFRESH_URL_" + env); v != "" {
		r.URL = v
	}
	if v := os.Getenv("CROWDVM_AI_REFRESH_CLIENT_" + env); v != "" {
		r.ClientID = v
	}
	return r, r.URL != ""
}

// aiServerRenews reports whether the server renews cred (so the refresh
// token stays on the server).
func aiServerRenews(cred string) bool {
	_, ok := aiRefresherFor(cred)
	return ok
}

var aiRefreshLocks keyedMutex

// aiChatGPTAuth is the shape of ~/.codex/auth.json.
type aiChatGPTAuth struct {
	Tokens struct {
		IDToken      string `json:"id_token,omitempty"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token,omitempty"`
		AccountID    string `json:"account_id,omitempty"`
	} `json:"tokens"`
}

// aiRefreshToken returns the refresh token of a secret (also inside a
// pasted auth.json).
func aiRefreshToken(sec aiSecret) string {
	if sec.RefreshToken != "" {
		return sec.RefreshToken
	}
	if strings.HasPrefix(sec.Value, "{") {
		var a aiChatGPTAuth
		_ = json.Unmarshal([]byte(sec.Value), &a)
		return a.Tokens.RefreshToken
	}
	return ""
}

// aiAccessToken returns the bearer the vendor sees.
func aiAccessToken(sec aiSecret) string {
	if strings.HasPrefix(sec.Value, "{") {
		t, _ := aiChatGPTToken(sec)
		return t
	}
	return sec.Value
}

// jwtExpiry reads the exp claim of a JWT (0 = not a JWT / no exp).
func jwtExpiry(tok string) int64 {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return 0
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &c) != nil || c.Exp == 0 {
		return 0
	}
	return c.Exp * 1000
}

// aiExpiresAt is the access token's expiry in unix ms (0 = unknown).
func aiExpiresAt(sec aiSecret) int64 {
	if sec.ExpiresAt > 0 {
		return sec.ExpiresAt
	}
	return jwtExpiry(aiAccessToken(sec))
}

// aiActiveFresh is aiActive with expiring sign-ins renewed. changed reports
// a renewal (running sandboxes with real keys then need a push).
func (s *Server) aiActiveFresh(profileID string) (map[string]aiSecret, bool) {
	active := s.aiActive(profileID)
	changed := false
	for cred, sec := range active {
		if nsec, ok := s.aiRenew(profileID, cred, sec); ok {
			active[cred], changed = nsec, true
		}
	}
	return active, changed
}

// aiRenew renews one credential when it is about to expire. It returns the
// new secret and true after a renewal.
func (s *Server) aiRenew(profileID, cred string, sec aiSecret) (aiSecret, bool) {
	r, ok := aiRefresherFor(cred)
	if !ok || aiRefreshToken(sec) == "" {
		return sec, false
	}
	exp := aiExpiresAt(sec)
	if exp == 0 || time.Until(time.UnixMilli(exp)) > aiRefreshMargin {
		return sec, false
	}
	unlock := aiRefreshLocks.Lock(profileID + "|" + cred)
	defer unlock()
	// Another request may have renewed it while we waited.
	if cur, ok := s.aiActive(profileID)[cred]; ok && aiExpiresAt(cur) > exp {
		return cur, true
	}
	nsec, err := s.aiRefreshCall(r, cred, sec)
	if err != nil {
		s.Log.Warn("AI sign-in renewal failed", "profile", profileID, "credential", cred, "err", err)
		return sec, false
	}
	accts, _ := s.aiAccounts(profileID, cred)
	for _, a := range accts {
		if a.Active {
			b, _ := json.Marshal(nsec)
			sealed, err := s.envSeal(b)
			if err != nil {
				return sec, false
			}
			_, _ = s.DB.Exec(`UPDATE credential_accounts SET secret=? WHERE id=?`, sealed, a.ID)
			s.aiTouch(profileID)
		}
	}
	return nsec, true
}

var aiRefreshClient = &http.Client{Timeout: 30 * time.Second, Transport: aiTransport}

// aiRefreshCall posts a refresh_token grant and returns the renewed secret.
func (s *Server) aiRefreshCall(r aiRefresher, cred string, sec aiSecret) (aiSecret, error) {
	body := map[string]any{"grant_type": "refresh_token", "refresh_token": aiRefreshToken(sec)}
	if r.ClientID != "" {
		body["client_id"] = r.ClientID
	}
	if r.Scope != "" {
		body["scope"] = r.Scope
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(b))
	if err != nil {
		return sec, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := aiRefreshClient.Do(req)
	if err != nil {
		return sec, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return sec, fmt.Errorf("token endpoint answered %d: %.200s", resp.StatusCode, raw)
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil || tr.AccessToken == "" {
		return sec, fmt.Errorf("token endpoint answered without access_token")
	}
	n := sec
	n.ExpiresAt = 0
	if tr.ExpiresIn > 0 {
		n.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).UnixMilli()
	}
	if strings.HasPrefix(sec.Value, "{") {
		var m map[string]any
		_ = json.Unmarshal([]byte(sec.Value), &m)
		tok, _ := m["tokens"].(map[string]any)
		if tok == nil {
			tok = map[string]any{}
		}
		tok["access_token"] = tr.AccessToken
		if tr.RefreshToken != "" {
			tok["refresh_token"] = tr.RefreshToken
		}
		if tr.IDToken != "" {
			tok["id_token"] = tr.IDToken
		}
		m["tokens"] = tok
		m["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
		nb, _ := json.Marshal(m)
		n.Value = string(nb)
	} else {
		n.Value = tr.AccessToken
		if tr.RefreshToken != "" {
			n.RefreshToken = tr.RefreshToken
		}
	}
	if n.ExpiresAt == 0 {
		n.ExpiresAt = jwtExpiry(tr.AccessToken)
	}
	return n, nil
}

// aiRenewForSandbox renews the sign-ins of the profile that put real keys
// into sb, and pushes renewed tokens into it (synchronously) and into the
// profile's other running sandboxes (in the background).
func (s *Server) aiRenewForSandbox(ctx context.Context, sb *Sandbox) {
	m := envManagedOf(sb)
	if m.ProfileID == "" || m.Proxied {
		return // proxied sandboxes hold placeholders; the proxy renews
	}
	if _, changed := s.aiActiveFresh(m.ProfileID); !changed {
		return
	}
	if err := s.envApplyLive(ctx, sb, false); err != nil {
		s.Log.Warn("pushing renewed AI sign-in failed", "sandbox", sb.ID, "err", err)
	}
	s.envPushProfile(m.ProfileID)
}

// aiSandboxSecret strips what a sandbox must not hold: the refresh token of
// a sign-in the server renews.
func aiSandboxSecret(cred string, sec aiSecret) aiSecret {
	if !aiServerRenews(cred) {
		return sec
	}
	sec.RefreshToken = ""
	if strings.HasPrefix(sec.Value, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(sec.Value), &m) == nil {
			if tok, ok := m["tokens"].(map[string]any); ok {
				delete(tok, "refresh_token")
			}
			b, _ := json.Marshal(m)
			sec.Value = string(b)
		}
	}
	return sec
}

package server

// Request guards of the accounts layer (run on every authenticated API call
// after the scope check):
//
//   - Organization keys are refused on account settings with
//     403 api_key_org_forbidden (Boat: webhooks, AI credentials, billing,
//     keys, members).
//   - Default deny: routes registered with ActionAny name the scope an API
//     key needs here; an unlisted ActionAny route refuses API keys.
//   - Every DELETE answer carries Cache-Control: no-store (deletion data).

import (
	"net/http"
	"strings"
)

func init() {
	RegisterFeature("acct_guard", func(s *Server) error {
		s.Hooks.Guard = append(s.Hooks.Guard, s.acctGuard)
		return nil
	})
}

// orgKeyForbidden lists account-settings routes (path prefixes under
// /api/v1, optionally with a method) that organization keys may not call.
var orgKeyForbidden = []string{
	"/webhooks", "/telemetry", "/credential-profiles", "/ai-proxy", "/ai-usage", "/credits", "/billing",
	"/api-keys", "/account/", "/account-settings", "/secrets", "/org-invites", "/audit-events", "/notices",
	"POST /orgs", "DELETE /orgs/{org}", "/orgs/{org}/members", "/orgs/{org}/invites", "/orgs/{org}/transfer",
	"/orgs/{org}/service-accounts", "/orgs/{org}/move-personal", "/orgs/{org}/policy",
}

// keyActionAny maps ActionAny routes (pattern without method prefix when
// it applies to all methods) to the scope an API key needs. "" = any key.
var keyActionAny = map[string]string{
	"GET /api/v1/server":                 "",
	"GET /api/v1/provider-models":        "",
	"POST /api/v1/api-keys/scoped":       "", // the handler checks the creating key
	"POST /api/v1/api-keys":              "",
	"DELETE /api/v1/api-keys/{}":         "",
	"POST /api/v1/api-keys/{}/rotate":    "",
	"POST /auth/logout":                  "",
	"GET /auth/me":                       "",
	"POST /agent/identity/claim":         "",
	"GET /api/v1/nodes":                  "account.read",
	"GET /api/v1/nodes/{}":               "account.read",
	"PATCH /api/v1/nodes/{}":             "account.admin",
	"DELETE /api/v1/nodes/{}":            "account.admin",
	"POST /api/v1/nodes/{}/evacuate":     "account.admin",
	"DELETE /api/v1/nodes/{}/evacuate":   "account.admin",
	"GET /api/v1/join-tokens":            "account.read",
	"POST /api/v1/join-tokens":           "account.admin",
	"DELETE /api/v1/join-tokens/{}":      "account.admin",
	"POST /api/v1/feedback":              "",
	"GET /api/v1/openapi.yaml":           "",
	"GET /api/v1/openapi.json":           "",
	"POST /api/boat/billing/checkout":    "",
	"POST /api/crowdvm/billing/checkout": "",
}

func (s *Server) acctGuard(c *Ctx) error {
	r := c.R
	if r.Method == http.MethodDelete {
		c.W.Header().Set("Cache-Control", "no-store")
	}
	k := c.P.Key
	if k == nil {
		return nil
	}
	pat := normPattern(r.Pattern)
	if c.Action == ActionAny {
		need, ok := keyActionAny[pat]
		if !ok {
			_, path, _ := strings.Cut(pat, " ")
			if path == "" {
				path = pat
			}
			// Guest pass-through routes check their own action.
			if strings.HasPrefix(path, "/api/v1/sandboxes/{}/") {
				ok = true
			}
		}
		if !ok {
			return ErrD(403, "api_key_action_forbidden", "API keys cannot call this route.", map[string]any{"action": "session"})
		}
		if need != "" && !k.Allows(need) {
			return ErrD(403, "api_key_action_forbidden", "The API key does not allow "+need+".", map[string]any{"action": need})
		}
	}
	if k.OrgID != "" && orgKeyRefused(r.Method, pat) {
		return Err(403, "api_key_org_forbidden", "Organization API keys cannot change or read account settings (webhooks, AI credentials, billing, keys, members). Use an account key or a browser session.")
	}
	return nil
}

// orgKeyRefused matches a normalized route pattern against orgKeyForbidden.
func orgKeyRefused(method, pat string) bool {
	_, path, ok := strings.Cut(pat, " ")
	if !ok {
		path = pat
	}
	rel, ok := strings.CutPrefix(path, "/api/v1")
	if !ok {
		return false
	}
	for _, f := range orgKeyForbidden {
		m, p, hasMethod := strings.Cut(f, " ")
		if !hasMethod {
			m, p = "", f
		}
		if m != "" && m != method {
			continue
		}
		p = normPattern(p)
		if rel == p || strings.HasPrefix(rel, strings.TrimSuffix(p, "/")+"/") || (strings.HasSuffix(p, "/") && strings.HasPrefix(rel, p)) {
			if m != "" && rel != p {
				continue // a method-specific entry matches that exact route only
			}
			return true
		}
	}
	return false
}

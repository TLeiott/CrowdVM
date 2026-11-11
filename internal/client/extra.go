package client

import (
	"context"
	"io"
	"net/url"
	"strconv"
)

// --- hosted ports ---

// HostedPortInfo is one entry of GET /sandboxes/{id}/host.
type HostedPortInfo struct {
	Port        int    `json:"port"`
	URL         string `json:"url"`
	HostURL     string `json:"hostUrl,omitempty"`
	PathURL     string `json:"pathUrl,omitempty"`
	Access      string `json:"access"`
	IsProtected bool   `json:"isProtected"`
	Localhost   bool   `json:"localhost"`
	Title       string `json:"title,omitempty"`
	Hidden      bool   `json:"hidden"`
	Firewall    Object `json:"firewall,omitempty"`
	CreatedAt   string `json:"createdAt,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
}

// ListHostedPorts lists the hosted ports of a sandbox (all includes hidden ones).
func (c *Client) ListHostedPorts(ctx context.Context, id string, all bool) ([]HostedPortInfo, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	r, err := call[struct {
		Ports []HostedPortInfo `json:"ports"`
	}](ctx, c, get(p("sandboxes", id, "host"), q))
	if err != nil {
		return nil, err
	}
	if r.Ports == nil {
		r.Ports = []HostedPortInfo{}
	}
	return r.Ports, nil
}

// GetHostedPort returns one hosted port (404 port_not_hosted when hidden or unknown).
func (c *Client) GetHostedPort(ctx context.Context, id string, port int) (*HostedPortInfo, error) {
	return call[HostedPortInfo](ctx, c, get(p("sandboxes", id, "host", strconv.Itoa(port)), nil))
}

// HideHostedPort turns a hosted port's URL off.
func (c *Client) HideHostedPort(ctx context.Context, id string, port int) (Object, error) {
	return obj(ctx, c, del(p("sandboxes", id, "host", strconv.Itoa(port)), nil))
}

// --- browser view ---

type BrowserResult struct {
	BrowserURL       string `json:"browserUrl"`
	ViewerURL        string `json:"viewerUrl,omitempty"`
	Profile          string `json:"profile,omitempty"`
	ExpiresAt        string `json:"expiresAt,omitempty"`
	ExpiresInSeconds int    `json:"expiresInSeconds,omitempty"`
	Mode             string `json:"mode,omitempty"`
}

// Browser starts (or reuses) the browser-only stream of a running sandbox.
// profile is an absolute Chrome user-data dir on the sandbox ("" = managed).
func (c *Client) Browser(ctx context.Context, id, profile string) (*BrowserResult, error) {
	body := Object{}
	if profile != "" {
		body["profile"] = profile
	}
	r, err := call[BrowserResult](ctx, c, post(p("sandboxes", id, "browser"), body))
	if err == nil {
		if r.BrowserURL != "" {
			r.BrowserURL = c.resolve(r.BrowserURL)
		}
		if r.ViewerURL != "" {
			r.ViewerURL = c.resolve(r.ViewerURL)
		}
	}
	return r, err
}

// --- provider catalog ---

type ProviderModel struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	ReasoningEfforts       []string `json:"reasoningEfforts"`
	DefaultReasoningEffort string   `json:"defaultReasoningEffort,omitempty"`
	FastMode               bool     `json:"fastMode"`
}

type Provider struct {
	Provider     string          `json:"provider"`
	Name         string          `json:"name"`
	Aliases      []string        `json:"aliases,omitempty"`
	DefaultModel string          `json:"defaultModel,omitempty"`
	Models       []ProviderModel `json:"models"`
	Configured   *bool           `json:"configured,omitempty"`
	Credentials  []string        `json:"credentials,omitempty"`
}

// ProviderModels returns the live harness/model catalog (GET /provider-models).
func (c *Client) ProviderModels(ctx context.Context) ([]Provider, error) {
	r, err := call[struct {
		Providers []Provider `json:"providers"`
	}](ctx, c, get("/provider-models", nil))
	if err != nil {
		return nil, err
	}
	return r.Providers, nil
}

// --- email login ---

// StartEmailLogin sends a 6-digit code to email.
func (c *Client) StartEmailLogin(ctx context.Context, email string) (Object, error) {
	return obj(ctx, c, &Request{Method: "POST", Path: "/auth/email/start", RootPath: true, NoRetry: true, Body: Object{"email": email}})
}

// VerifyEmailLogin exchanges the emailed code for a CLI token.
// inviteCode (optional) lets a new address sign up on a closed server.
func (c *Client) VerifyEmailLogin(ctx context.Context, email, code string, inviteCode ...string) (*LoginResult, error) {
	var raw map[string]any
	body := Object{"email": email, "code": code, "client": "cli"}
	if len(inviteCode) > 0 && inviteCode[0] != "" {
		body["inviteCode"] = inviteCode[0]
	}
	r := &Request{Method: "POST", Path: "/auth/email/verify", RootPath: true, NoRetry: true, Body: body}
	if err := c.Do(ctx, r, &raw); err != nil {
		return nil, err
	}
	key := str(raw, "apiKey", "api_key", "token")
	if key == "" {
		return nil, &Error{Code: "login_failed", Message: "the server returned no token"}
	}
	return &LoginResult{APIKey: key, User: userFrom(raw)}, nil
}

// --- admin (server administrators) ---

func (c *Client) AdminUsers(ctx context.Context) (Object, error) {
	return obj(ctx, c, get("/admin/users", nil))
}

func (c *Client) AdminCreateUser(ctx context.Context, body Object) (Object, error) {
	return obj(ctx, c, post("/admin/users", body))
}

// AdminUpdateUser changes admin/disabled/password/displayName of a user (id or login).
func (c *Client) AdminUpdateUser(ctx context.Context, user string, body Object) (Object, error) {
	return obj(ctx, c, patch(p("admin", "users", user), body))
}

// AdminGrant adds (or with a negative amount removes) credits; body has user or org.
func (c *Client) AdminGrant(ctx context.Context, body Object) (Object, error) {
	return obj(ctx, c, post("/admin/credits", body))
}

func (c *Client) AdminInviteCodes(ctx context.Context) (Object, error) {
	return obj(ctx, c, get("/admin/invite-codes", nil))
}

func (c *Client) AdminCreateInviteCode(ctx context.Context, body Object) (Object, error) {
	return obj(ctx, c, post("/admin/invite-codes", body))
}

func (c *Client) AdminRevokeInviteCode(ctx context.Context, id string) (Object, error) {
	return obj(ctx, c, del(p("admin", "invite-codes", id), nil))
}

func (c *Client) AdminSandboxes(ctx context.Context, q url.Values) (Object, error) {
	return obj(ctx, c, get("/admin/sandboxes", q))
}

// --- nodes ---

// UpdateNode changes a node's name, disabled (drain) flag or labels.
func (c *Client) UpdateNode(ctx context.Context, id string, body Object) (Object, error) {
	return obj(ctx, c, patch(p("nodes", id), body))
}

// DeleteNode removes a node; force evicts it while it still runs sandboxes.
func (c *Client) DeleteNode(ctx context.Context, id string, force bool) (Object, error) {
	q := url.Values{}
	if force {
		q.Set("force", "true")
	}
	return obj(ctx, c, del(p("nodes", id), q))
}

// EvacuateNode drains a node and, after graceSec, stops its sandboxes and
// resumes them on other nodes.
func (c *Client) EvacuateNode(ctx context.Context, id string, graceSec int) (Object, error) {
	return obj(ctx, c, post(p("nodes", id, "evacuate"), Object{"graceSec": graceSec}))
}

// CancelEvacuation cancels a node's evacuation during its grace period.
func (c *Client) CancelEvacuation(ctx context.Context, id string) (Object, error) {
	return obj(ctx, c, del(p("nodes", id, "evacuate"), nil))
}

func (c *Client) ListJoinTokens(ctx context.Context) (Object, error) {
	return obj(ctx, c, get("/join-tokens", nil))
}

// --- CI ---

func (c *Client) CIUnlink(ctx context.Context, linkID string) (Object, error) {
	return obj(ctx, c, del(p("ci", "links", linkID), nil))
}

// --- git sources ---

func (c *Client) RepoSources(ctx context.Context) (Object, error) {
	return obj(ctx, c, get("/repos/sources", nil))
}

func (c *Client) AddRepoSource(ctx context.Context, body Object) (Object, error) {
	return obj(ctx, c, post("/repos/sources", body))
}

func (c *Client) DeleteRepoSource(ctx context.Context, id string) (Object, error) {
	return obj(ctx, c, del(p("repos", "sources", id), nil))
}

// --- AI sign-ins and credential catalog ---

// SetCredentialSignIn stores a pasted subscription sign-in token.
func (c *Client) SetCredentialSignIn(ctx context.Context, profile, credential string, body Object) (*CredentialProfile, error) {
	return profResult(ctx, c, put(p("credential-profiles", profile, "sign-ins", credential), body))
}

// CredentialInfo is one entry of the credential catalog of GET /credential-profiles.
type CredentialInfo struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Proxied bool   `json:"proxied"`
	EnvVar  string `json:"envVar,omitempty"`
	GetURL  string `json:"getUrl,omitempty"`
}

// CredentialCatalog lists the credentials a profile can hold, with where to get each.
func (c *Client) CredentialCatalog(ctx context.Context) ([]CredentialInfo, error) {
	r, err := call[struct {
		Credentials []CredentialInfo `json:"credentials"`
	}](ctx, c, get("/credential-profiles", nil))
	if err != nil {
		return nil, err
	}
	return r.Credentials, nil
}

// --- feedback ---

// Feedback sends a note to the server operators (POST /feedback; 5/min, 30/day).
func (c *Client) Feedback(ctx context.Context, text, sandboxID string) (Object, error) {
	body := Object{"text": text}
	if sandboxID != "" {
		body["sandboxId"] = sandboxID
	}
	return obj(ctx, c, &Request{Method: "POST", Path: "/feedback", Body: body, NoRetry: true})
}

// AdminFeedback lists the feedback users sent.
func (c *Client) AdminFeedback(ctx context.Context) (Object, error) {
	return obj(ctx, c, get("/admin/feedback", nil))
}

// LoginLink returns a one-time 15-minute dashboard sign-in URL.
func (c *Client) LoginLink(ctx context.Context) (Object, error) {
	return obj(ctx, c, &Request{Method: "POST", Path: "/auth/login-links", NoRetry: true})
}

// Billing returns the wallet, policy (limits), extra starts and grace state in scope.
func (c *Client) Billing(ctx context.Context) (Object, error) {
	return obj(ctx, c, get("/billing", nil))
}

// SetExtraStarts turns paying for starts past a limit with credits on or off.
func (c *Client) SetExtraStarts(ctx context.Context, body Object) (Object, error) {
	return obj(ctx, c, put("/billing/extra-starts", body))
}

// AdminPolicy reads (body nil) or replaces the restriction policy of a wallet.
func (c *Client) AdminPolicy(ctx context.Context, wallet string, body Object) (Object, error) {
	if body == nil {
		return obj(ctx, c, get(p("admin", "policies", wallet), nil))
	}
	return obj(ctx, c, put(p("admin", "policies", wallet), body))
}

// Notices lists wallet and sandbox notices (low credit, grace, stopped
// sandboxes, disk usage, failing stops...); all includes dismissed ones.
func (c *Client) Notices(ctx context.Context, all bool) (Object, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	return obj(ctx, c, get("/notices", q))
}

// DismissNotice dismisses one notice ("" = all).
func (c *Client) DismissNotice(ctx context.Context, id string) (Object, error) {
	if id == "" {
		return obj(ctx, c, post("/notices/dismiss-all", nil))
	}
	return obj(ctx, c, post(p("notices", id, "dismiss"), nil))
}

// --- binaries ---

// DownloadBinary streams /downloads/{os}/{arch}/{name} from the server root.
// ServerInfo returns GET /server: version, public URL, sizes, signup policy.
func (c *Client) ServerInfo(ctx context.Context) (Object, error) {
	return obj(ctx, c, get("/server", nil))
}

func (c *Client) DownloadBinary(ctx context.Context, goos, arch, name string) (io.ReadCloser, error) {
	resp, err := c.Raw(ctx, &Request{Method: "GET", Path: p("downloads", goos, arch, name), RootPath: true})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// --- account settings, service accounts, merge ---

// AccountSettings returns agent defaults, the onboarding useCase and more.
func (c *Client) AccountSettings(ctx context.Context) (Object, error) {
	return obj(ctx, c, get("/account-settings", nil))
}

func (c *Client) UpdateAccountSettings(ctx context.Context, body Object) (Object, error) {
	return obj(ctx, c, patch("/account-settings", body))
}

// CreateServiceAccount creates a service user of org (org "" with admin=true
// uses POST /admin/service-accounts).
func (c *Client) CreateServiceAccount(ctx context.Context, org string, admin bool, body Object) (Object, error) {
	if admin {
		if org != "" {
			body["org"] = org
		}
		return obj(ctx, c, post("/admin/service-accounts", body))
	}
	return obj(ctx, c, post(p("orgs", org, "service-accounts"), body))
}

func (c *Client) ListServiceAccounts(ctx context.Context, org string) (Object, error) {
	return obj(ctx, c, get(p("orgs", org, "service-accounts"), nil))
}

func (c *Client) DeleteServiceAccount(ctx context.Context, org, user string) (Object, error) {
	return obj(ctx, c, del(p("orgs", org, "service-accounts", user), nil))
}

// AdminMergeUser moves everything of user into another account.
func (c *Client) AdminMergeUser(ctx context.Context, user, into string) (Object, error) {
	return obj(ctx, c, post(p("admin", "users", user, "merge"), Object{"into": into}))
}

// MovePersonalToOrg moves your personal sandboxes (and their billing) into org.
func (c *Client) MovePersonalToOrg(ctx context.Context, org string, body Object) (Object, error) {
	if body == nil {
		body = Object{}
	}
	return obj(ctx, c, post(p("orgs", org, "move-personal"), body))
}

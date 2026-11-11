package client

import (
	"context"
	"fmt"
	"io"
	"net/url"
)

// --- snapshots ---

// ListSnapshots lists snapshots of all sandboxes (id == "") or of one.
func (c *Client) ListSnapshots(ctx context.Context, id string, limit int, cursor string) (*SnapshotList, error) {
	q := url.Values{}
	setInt(q, "limit", limit)
	setStr(q, "cursor", cursor)
	path := "/snapshots"
	if id != "" {
		path = p("sandboxes", id, "snapshots")
	}
	return call[SnapshotList](ctx, c, get(path, q))
}

func (c *Client) LatestSnapshot(ctx context.Context, id string) (*Snapshot, error) {
	r, err := call[struct {
		Snapshot *Snapshot `json:"snapshot"`
	}](ctx, c, get(p("sandboxes", id, "snapshots", "latest"), nil))
	if err != nil {
		return nil, err
	}
	return r.Snapshot, nil
}

// DeleteSnapshot permanently deletes one ordinary snapshot.
func (c *Client) DeleteSnapshot(ctx context.Context, snapshotID string) (*DeletionOperation, error) {
	r := del(p("snapshots", snapshotID), nil)
	r.Header = confirmHeader(snapshotID)
	return opResult(ctx, c, r)
}

// DeleteSandboxSnapshots deletes every ordinary snapshot of a sandbox.
func (c *Client) DeleteSandboxSnapshots(ctx context.Context, id string) (Object, error) {
	r := del(p("sandboxes", id, "snapshots"), nil)
	r.Header = confirmHeader(id)
	return obj(ctx, c, r)
}

func (c *Client) SnapshotTree(ctx context.Context, snapshotID string) (*SnapshotTree, error) {
	return call[SnapshotTree](ctx, c, get(p("snapshots", snapshotID, "tree"), nil))
}

// SnapshotFiles streams one file, or a folder as tar ("" or "/" = everything).
// The returned content type tells which ("application/x-tar" for folders).
func (c *Client) SnapshotFiles(ctx context.Context, snapshotID, path string) (io.ReadCloser, string, error) {
	resp, err := c.send(ctx, get(p("snapshots", snapshotID, "files"), url.Values{"path": {path}}))
	if err != nil {
		return nil, "", err
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

// SnapshotDownload returns the download manifest of a snapshot.
func (c *Client) SnapshotDownload(ctx context.Context, snapshotID string) (Object, error) {
	return obj(ctx, c, get(p("snapshots", snapshotID, "download"), nil))
}

// Saved snapshots ("templates"). The legacy /named-snapshots paths are the
// same resource; use NamedSnapshots to target them.

func (c *Client) ListSavedSnapshots(ctx context.Context) (*SavedSnapshotList, error) {
	return call[SavedSnapshotList](ctx, c, get("/saved-snapshots", nil))
}

func (c *Client) SaveSnapshot(ctx context.Context, sandboxID, name string) (*SavedSnapshot, error) {
	return savedSnap(ctx, c, post("/saved-snapshots", Object{"sandboxId": sandboxID, "name": name}))
}

func (c *Client) GetSavedSnapshot(ctx context.Context, name string) (*SavedSnapshot, error) {
	return savedSnap(ctx, c, get(p("saved-snapshots", name), nil))
}

func (c *Client) DeleteSavedSnapshot(ctx context.Context, name string) error {
	return c.Do(ctx, del(p("saved-snapshots", name), nil), nil)
}

func (c *Client) ListSnapshotShares(ctx context.Context, name string) ([]SnapshotShare, error) {
	r, err := call[struct {
		Shares []SnapshotShare `json:"shares"`
	}](ctx, c, get(p("saved-snapshots", name, "shares"), nil))
	if err != nil {
		return nil, err
	}
	return r.Shares, nil
}

func (c *Client) CreateSnapshotShare(ctx context.Context, name string) (*SnapshotShareCreated, error) {
	return call[SnapshotShareCreated](ctx, c, post(p("saved-snapshots", name, "shares"), nil))
}

func (c *Client) RevokeSnapshotShare(ctx context.Context, name, shareID string) (*SnapshotShare, error) {
	r, err := call[struct {
		Share SnapshotShare `json:"share"`
	}](ctx, c, del(p("saved-snapshots", name, "shares", shareID), nil))
	if err != nil {
		return nil, err
	}
	return &r.Share, nil
}

// NamedSnapshots calls the legacy /named-snapshots alias of saved snapshots.
func (c *Client) NamedSnapshots(ctx context.Context, method, name string, body any) (Object, error) {
	path := "/named-snapshots"
	if name != "" {
		path = p("named-snapshots", name)
	}
	return obj(ctx, c, &Request{Method: method, Path: path, Body: body})
}

func savedSnap(ctx context.Context, c *Client, r *Request) (*SavedSnapshot, error) {
	out, err := call[struct {
		Snapshot SavedSnapshot `json:"snapshot"`
	}](ctx, c, r)
	if err != nil {
		return nil, err
	}
	return &out.Snapshot, nil
}

// --- account ---

func (c *Client) Me(ctx context.Context) (*User, error) {
	r, err := call[struct {
		User User `json:"user"`
	}](ctx, c, get("/me", nil))
	if err != nil {
		return nil, err
	}
	return &r.User, nil
}

func (c *Client) ListOrgs(ctx context.Context) (*OrgList, error) {
	return call[OrgList](ctx, c, get("/orgs", nil))
}

// SetActiveOrg sets the org new sandboxes bill to; "" or "personal" = personal.
func (c *Client) SetActiveOrg(ctx context.Context, org string) (Object, error) {
	var v any
	if org != "" && org != "personal" {
		v = org
	}
	return obj(ctx, c, patch("/orgs/active", Object{"org": v}))
}

// Organization management beyond Boat's v1 API (CrowdVM extensions).

func (c *Client) CreateOrg(ctx context.Context, name string, private bool) (Object, error) {
	return obj(ctx, c, post("/orgs", Object{"name": name, "newSandboxesPrivate": private}))
}

func (c *Client) GetOrg(ctx context.Context, org string) (Object, error) {
	return obj(ctx, c, get(p("orgs", org), nil))
}

func (c *Client) DeleteOrg(ctx context.Context, org string) error {
	return c.Do(ctx, del(p("orgs", org), nil), nil)
}

func (c *Client) OrgMembers(ctx context.Context, org string) (Object, error) {
	return obj(ctx, c, get(p("orgs", org, "members"), nil))
}

func (c *Client) RemoveOrgMember(ctx context.Context, org, userID string) error {
	return c.Do(ctx, del(p("orgs", org, "members", userID), nil), nil)
}

func (c *Client) TransferOrg(ctx context.Context, org, user string) (Object, error) {
	return obj(ctx, c, post(p("orgs", org, "transfer"), Object{"user": user}))
}

func (c *Client) OrgInvites(ctx context.Context, org string) (Object, error) {
	return obj(ctx, c, get(p("orgs", org, "invites"), nil))
}

func (c *Client) CreateOrgInvite(ctx context.Context, org, email, role string) (Object, error) {
	return obj(ctx, c, post(p("orgs", org, "invites"), Object{"email": email, "role": role}))
}

func (c *Client) AcceptOrgInvite(ctx context.Context, code string) (Object, error) {
	return obj(ctx, c, post("/org-invites/accept", Object{"code": code}))
}

// UpdateOrg changes an org's name and/or newSandboxesPrivate.
func (c *Client) UpdateOrg(ctx context.Context, org string, fields Object) (Object, error) {
	return obj(ctx, c, patch(p("orgs", org), fields))
}

// Limits returns the account limits; the shape is large and open.
func (c *Client) Limits(ctx context.Context) (Object, error) {
	return obj(ctx, c, get("/limits", c.orgQuery(nil)))
}

// Credits is the CrowdVM credit balance (replaces Boat billing/wallet).
func (c *Client) Credits(ctx context.Context) (Object, error) {
	return obj(ctx, c, get("/credits", c.orgQuery(nil)))
}

// Nodes lists the contributing nodes with their benchmark categories
// (CrowdVM extension).
func (c *Client) Nodes(ctx context.Context) (Object, error) {
	return obj(ctx, c, get("/nodes", nil))
}

// GetNode returns one node in detail (CrowdVM extension).
func (c *Client) GetNode(ctx context.Context, id string) (Object, error) {
	return obj(ctx, c, get(p("nodes", id), nil))
}

// CreateJoinToken makes a token for `crowdvm-node join` (admin; CrowdVM extension).
func (c *Client) CreateJoinToken(ctx context.Context, body Object) (Object, error) {
	return obj(ctx, c, post("/join-tokens", body))
}

func (c *Client) RevokeJoinToken(ctx context.Context, id string) error {
	return c.Do(ctx, del(p("join-tokens", id), nil), nil)
}

// CreditTransfer moves credits between wallets (CrowdVM extension).
func (c *Client) CreditTransfer(ctx context.Context, from, to string, amount float64) (Object, error) {
	return obj(ctx, c, post("/credits/transfer", Object{"from": from, "to": to, "amount": amount}))
}

func (c *Client) DataRetention(ctx context.Context) (*DataRetention, error) {
	return call[DataRetention](ctx, c, get("/account/data-retention", nil))
}

func (c *Client) UpdateDataRetention(ctx context.Context, req *DataRetentionUpdate) (*DataRetention, error) {
	return call[DataRetention](ctx, c, patch("/account/data-retention", req))
}

func (c *Client) DeletionOperation(ctx context.Context, opID string) (*DeletionOperation, error) {
	return opResult(ctx, c, get(p("deletion-operations", opID), nil))
}

// Repos lists GitHub repositories (Boat compatibility; open shape).
func (c *Client) Repos(ctx context.Context, q url.Values) (Object, error) {
	return obj(ctx, c, get("/repos", q))
}

func (c *Client) SelectRepo(ctx context.Context, repositoryID, baseBranch string) (Object, error) {
	return obj(ctx, c, post("/repos", Object{"repositoryId": repositoryID, "baseBranch": baseBranch}))
}

func (c *Client) Secrets(ctx context.Context) (Object, error) {
	return obj(ctx, c, get("/secrets", nil))
}

func (c *Client) UpdateSecrets(ctx context.Context, envContents string, files []SecretFile) (Object, error) {
	return obj(ctx, c, post("/secrets", Object{"envContents": envContents, "secretFiles": files}))
}

// --- API keys ---

func (c *Client) ListAPIKeys(ctx context.Context, all ...bool) (*APIKeyList, error) {
	q := url.Values{}
	if len(all) > 0 && all[0] {
		q.Set("all", "1") // include the in-sandbox keys
	}
	return call[APIKeyList](ctx, c, get("/api-keys", q))
}

func (c *Client) CreateAPIKey(ctx context.Context, req *CreateAPIKeyRequest) (*APIKeySecret, error) {
	return call[APIKeySecret](ctx, c, post("/api-keys/scoped", req))
}

func (c *Client) RotateAPIKey(ctx context.Context, id string) (*APIKeySecret, error) {
	return call[APIKeySecret](ctx, c, post(p("api-keys", id, "rotate"), nil))
}

func (c *Client) RevokeAPIKey(ctx context.Context, id string) error {
	return c.Do(ctx, del(p("api-keys", id), nil), nil)
}

func (c *Client) APIKeyUsage(ctx context.Context, id string) (*APIKey, error) {
	return call[APIKey](ctx, c, get(p("api-keys", id, "usage"), nil))
}

// --- webhooks ---

func (c *Client) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	r, err := call[struct {
		Webhooks []Webhook `json:"webhooks"`
	}](ctx, c, get("/webhooks", nil))
	if err != nil {
		return nil, err
	}
	return r.Webhooks, nil
}

func (c *Client) CreateWebhook(ctx context.Context, req *WebhookRequest) (*WebhookResult, error) {
	if req.Events == nil {
		req.Events = WebhookEvents
	}
	return call[WebhookResult](ctx, c, post("/webhooks", req))
}

// WebhookEvents is every lifecycle event type.
var WebhookEvents = []string{"sandbox.ready", "sandbox.error", "sandbox.archived", "sandbox.hydrated", "sandbox.degraded", "sandbox.recovered"}

func (c *Client) GetWebhook(ctx context.Context, id string) (*WebhookResult, error) {
	return call[WebhookResult](ctx, c, get(p("webhooks", id), nil))
}

func (c *Client) UpdateWebhook(ctx context.Context, id string, req *WebhookRequest) (*WebhookResult, error) {
	return call[WebhookResult](ctx, c, patch(p("webhooks", id), req))
}

func (c *Client) DeleteWebhook(ctx context.Context, id string) error {
	return c.Do(ctx, del(p("webhooks", id), nil), nil)
}

func (c *Client) RotateWebhook(ctx context.Context, id string) (*WebhookResult, error) {
	return call[WebhookResult](ctx, c, post(p("webhooks", id, "rotate"), nil))
}

// --- telemetry ---

func (c *Client) GetTelemetry(ctx context.Context) (*TelemetryResult, error) {
	return call[TelemetryResult](ctx, c, get("/telemetry", nil))
}

func (c *Client) SetTelemetry(ctx context.Context, endpoint string, headers map[string]string) (*TelemetryResult, error) {
	return call[TelemetryResult](ctx, c, put("/telemetry", Object{"endpoint": endpoint, "headers": headers}))
}

func (c *Client) DeleteTelemetry(ctx context.Context) error {
	return c.Do(ctx, del("/telemetry", nil), nil)
}

// --- CI (Boat compatibility; open shapes) ---

func (c *Client) CIOverview(ctx context.Context, q url.Values) (Object, error) {
	return obj(ctx, c, get("/ci", q))
}
func (c *Client) CIConnect(ctx context.Context, body Object) (Object, error) {
	return obj(ctx, c, post("/ci/connect", body))
}
func (c *Client) CIPublicRepos(ctx context.Context, body Object) (Object, error) {
	return obj(ctx, c, post("/ci/public", body))
}
func (c *Client) CIJob(ctx context.Context, id string) (Object, error) {
	return obj(ctx, c, get(p("ci", "jobs", id), nil))
}
func (c *Client) CIJobLog(ctx context.Context, id string) (Object, error) {
	return obj(ctx, c, get(p("ci", "jobs", id, "logs"), nil))
}

// --- environments ---

func (c *Client) envQ() url.Values { return c.orgQuery(nil) }

func (c *Client) ListEnvironments(ctx context.Context) ([]Environment, error) {
	r, err := call[struct {
		Environments []Environment `json:"environments"`
	}](ctx, c, get("/environments", c.envQ()))
	if err != nil {
		return nil, err
	}
	return r.Environments, nil
}

// CreateEnvironment creates an environment and returns it. The API answers with the whole
// list (SandboxEnvironmentListResponse); the new one is picked from it by name.
func (c *Client) CreateEnvironment(ctx context.Context, name string) (*Environment, error) {
	r := post("/environments", Object{"name": name})
	r.Query = c.envQ()
	out, err := call[struct {
		Environment  *Environment  `json:"environment"`
		Environments []Environment `json:"environments"`
	}](ctx, c, r)
	if err != nil {
		return nil, err
	}
	if out.Environment != nil {
		return out.Environment, nil
	}
	for i := len(out.Environments) - 1; i >= 0; i-- {
		if out.Environments[i].Name == name {
			return &out.Environments[i], nil
		}
	}
	return nil, fmt.Errorf("environment %q missing from the create response", name)
}

func (c *Client) UpdateEnvironment(ctx context.Context, id string, req *UpdateEnvironmentRequest) (*Environment, error) {
	r := put(p("environments", id), req)
	r.Query = c.envQ()
	return envResult(ctx, c, r)
}

func (c *Client) DeleteEnvironment(ctx context.Context, id string) error {
	return c.Do(ctx, del(p("environments", id), c.envQ()), nil)
}

// UpgradeEnvironment moves the caller's sandboxes on older versions to the
// latest one; with sandboxIDs, only those.
func (c *Client) UpgradeEnvironment(ctx context.Context, id string, sandboxIDs ...string) (*EnvironmentUpgrade, error) {
	body := Object{}
	if len(sandboxIDs) > 0 {
		body["sandboxIds"] = sandboxIDs
	}
	r := post(p("environments", id, "upgrade"), body)
	r.Query = c.envQ()
	return call[EnvironmentUpgrade](ctx, c, r)
}

func (c *Client) SetEnvironmentVar(ctx context.Context, id, key, value string) (*EnvironmentChange, error) {
	r := put(p("environments", id, "vars", key), Object{"value": value})
	r.Query = c.envQ()
	return call[EnvironmentChange](ctx, c, r)
}

func (c *Client) DeleteEnvironmentVar(ctx context.Context, id, key string) (*EnvironmentChange, error) {
	return call[EnvironmentChange](ctx, c, del(p("environments", id, "vars", key), c.envQ()))
}

func (c *Client) SetEnvironmentSecretFile(ctx context.Context, id, path, contents string) (*EnvironmentChange, error) {
	r := put(p("environments", id, "secret-files"), SecretFile{Path: path, Contents: contents})
	r.Query = c.envQ()
	return call[EnvironmentChange](ctx, c, r)
}

func (c *Client) DeleteEnvironmentSecretFile(ctx context.Context, id, path string) (*EnvironmentChange, error) {
	q := c.envQ()
	q.Set("path", path)
	return call[EnvironmentChange](ctx, c, del(p("environments", id, "secret-files"), q))
}

func (c *Client) AddEnvironmentRepo(ctx context.Context, id, repositoryID, baseBranch string) (*EnvironmentChange, error) {
	body := Object{"repositoryId": repositoryID}
	if baseBranch != "" {
		body["baseBranch"] = baseBranch
	}
	r := post(p("environments", id, "repos"), body)
	r.Query = c.envQ()
	return call[EnvironmentChange](ctx, c, r)
}

func (c *Client) DeleteEnvironmentRepo(ctx context.Context, id, repositoryID string) (*EnvironmentChange, error) {
	return call[EnvironmentChange](ctx, c, del(p("environments", id, "repos", repositoryID), c.envQ()))
}

func envResult(ctx context.Context, c *Client, r *Request) (*Environment, error) {
	out, err := call[struct {
		Environment *Environment `json:"environment"`
	}](ctx, c, r)
	if err != nil {
		return nil, err
	}
	if out.Environment == nil {
		return &Environment{}, nil
	}
	return out.Environment, nil
}

// --- AI credentials ---

func (c *Client) ListCredentialProfiles(ctx context.Context) ([]CredentialProfile, error) {
	r, err := call[struct {
		Profiles []CredentialProfile `json:"profiles"`
	}](ctx, c, get("/credential-profiles", nil))
	if err != nil {
		return nil, err
	}
	return r.Profiles, nil
}

func (c *Client) CreateCredentialProfile(ctx context.Context, req *CreateCredentialProfileRequest) (*CredentialProfile, error) {
	return profResult(ctx, c, post("/credential-profiles", req))
}

func (c *Client) UpdateCredentialProfile(ctx context.Context, profile string, req *UpdateCredentialProfileRequest) (*CredentialProfile, error) {
	return profResult(ctx, c, put(p("credential-profiles", profile), req))
}

func (c *Client) DeleteCredentialProfile(ctx context.Context, profile string) error {
	return c.Do(ctx, del(p("credential-profiles", profile), nil), nil)
}

func (c *Client) SetCredentialKey(ctx context.Context, profile, credential, value string) (*CredentialProfile, error) {
	return profResult(ctx, c, put(p("credential-profiles", profile, "keys", credential), Object{"value": value}))
}

func (c *Client) DeleteCredentialKey(ctx context.Context, profile, credential string) (*CredentialProfile, error) {
	return profResult(ctx, c, del(p("credential-profiles", profile, "keys", credential), nil))
}

func (c *Client) ListCredentialAccounts(ctx context.Context, profile, credential string) (*CredentialAccounts, error) {
	return call[CredentialAccounts](ctx, c, get(p("credential-profiles", profile, "accounts", credential), nil))
}

func (c *Client) AddCredentialAccount(ctx context.Context, profile, credential, value string) (*CredentialAccounts, error) {
	return call[CredentialAccounts](ctx, c, post(p("credential-profiles", profile, "accounts", credential), Object{"value": value}))
}

func (c *Client) SwitchCredentialAccount(ctx context.Context, profile, credential, account string) (*CredentialAccounts, error) {
	return call[CredentialAccounts](ctx, c, put(p("credential-profiles", profile, "accounts", credential, account, "active"), nil))
}

func (c *Client) DeleteCredentialAccount(ctx context.Context, profile, credential, account string) (*CredentialAccounts, error) {
	return call[CredentialAccounts](ctx, c, del(p("credential-profiles", profile, "accounts", credential, account), nil))
}

func (c *Client) SetCustomAPI(ctx context.Context, profile, name, value, baseURL string) (*CredentialProfile, error) {
	return profResult(ctx, c, put(p("credential-profiles", profile, "custom-apis", name), Object{"value": value, "baseUrl": baseURL}))
}

func (c *Client) DeleteCustomAPI(ctx context.Context, profile, name string) (*CredentialProfile, error) {
	return profResult(ctx, c, del(p("credential-profiles", profile, "custom-apis", name), nil))
}

func (c *Client) GetAIProxy(ctx context.Context) (*AIProxy, error) {
	return call[AIProxy](ctx, c, get("/ai-proxy", c.orgQuery(nil)))
}

func (c *Client) SetAIProxy(ctx context.Context, enabled bool) (*AIProxy, error) {
	r := put("/ai-proxy", Object{"enabled": enabled})
	r.Query = c.orgQuery(nil)
	return call[AIProxy](ctx, c, r)
}

func (c *Client) AIUsage(ctx context.Context, days int, profile string) (Object, error) {
	q := c.orgQuery(nil)
	setInt(q, "days", days)
	setStr(q, "profile", profile)
	return obj(ctx, c, get("/ai-usage", q))
}

func profResult(ctx context.Context, c *Client, r *Request) (*CredentialProfile, error) {
	out, err := call[struct {
		Profile *CredentialProfile `json:"profile"`
	}](ctx, c, r)
	if err != nil {
		return nil, err
	}
	if out.Profile == nil {
		return &CredentialProfile{}, nil
	}
	return out.Profile, nil
}

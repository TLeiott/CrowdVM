# Extending the server

Notes for features built on top of the core (environments, webhooks, OTel,
data retention, AI credentials/proxy, public URLs/desktop, CI).

## Register a feature

Add a file to `package server` (or a subpackage) with:

```go
func init() {
	store.Register("webhooks", store.Migration{Version: 1, SQL: `CREATE TABLE webhooks (...)`})
	server.RegisterFeature("webhooks", func(s *server.Server) error {
		s.API("GET /webhooks", "account.read", s.handleListWebhooks) // path relative to /api/v1
		s.Bus.Subscribe(func(e server.Event) { /* enqueue, never block */ })
		s.Go(func(ctx context.Context) { /* background loop until ctx ends */ })
		return nil
	})
}
```

* `s.API(pattern, action, fn)`: auth (Bearer `cvm_` key, `cvms_` CLI session,
  or cookie + `X-CrowdVM-CSRF: 1`), the API key scope check for `action`, and
  error envelopes. Special actions: `ActionPublic`, `ActionAny`,
  `ActionSession` (no API keys), `ActionAdmin`. `s.Route` is the same for
  absolute paths; `s.Router().Handle` registers raw handlers.
* Handlers: `func(c *server.Ctx) error`. Return `server.Err(status, code, msg)`
  / `server.ErrD(..., details)`; `c.OK(status, type, fields)`; `c.Bind`,
  `c.BindStrict`; `c.Sandbox(action, ownerOnly)` loads `{sandboxId}` with
  org/visibility/key-scope checks; `c.Scope(bodyOrg)` resolves the wallet
  (org) of the request; `server.ParsePage` + `Page.SQL` + `PageResult` for
  cursor pagination.
* Routes this core stubs with 501 (`routes.go`, `notYet`) are dropped
  automatically when a feature registers the same method+path (wildcard
  names may differ).

## Events

`s.Bus` publishes `server.Event` in order on one goroutine. Types in
`events.go`: the six Boat webhook events (`sandbox.ready`, `.error`,
`.archived`, `.hydrated` = stop snapshot stored on the server, `.degraded`,
`.recovered`, all with `PreviousState`/`State`/`Reason`) plus
`sandbox.state` (every transition), `sandbox.created`, `.updated`, `.deleted`,
`snapshot.created`, `snapshot.stored`, `guest.event` (raw guest events
relayed by nodes, e.g. `prompt.event`, `setup.status`, `command.exited`),
`node.online`, `node.offline`. Subscribers must not block.

## Hooks (`s.Hooks`)

* `ResolveEnvironment(ctx, EnvRequest) (*ResolvedEnv, error)`: validate and pin
  `environment` / `noEnv` on create, resume and fork (default: only `base`).
* `PrepareGuest []func(ctx, sb, *GuestPrep) error`: runs before every boot;
  add env vars, secret files, repos, registry logins, `AIProxy`, SSH keys to
  `GuestPrep.Config` (a `proto.GuestConfig`) or tweak `GuestPrep.Spec`.
* `SandboxJSON []func(sb, map)`: set `url`, `desktopUrl`, `hostedPorts`, ...
* `HostPorts`: create-time `hostedPorts`.
* `Mail`: every outgoing mail (codes, invites).
* `EventTx []func(tx, Event) error`: runs inside the state-change transaction
  for each lifecycle event (`e.ID` = `evt_…`, also set on the Bus event); the
  webhook/OTel outbox (`feat_webhooks.go`, `hook_events`).
* `Guard []func(c *Ctx) error`: every API request after auth + scope check
  (`c.R.Pattern` names the route); an error refuses it.
* `NewSandbox []func(c *Ctx, p *SandboxPolicy) error`: create/fork may change
  `Snapshots`/`Monitoring` or refuse (account data-retention settings).
* `UserJSON []func(u, map)`: fields of the user object (`GET /me`).
* `s.SetSandboxExtra(id, key, v)` stores feature data in the sandbox row
  (`sb.Extra[key]`).

## Reaching a sandbox

* `s.NodeForSandbox(sb)` -> `*NodeConn`; `conn.Do(ctx, method, nodePath, in, out)`
  for JSON calls to the node API (`/v1/vms/{id}/...`), `conn.Transport()` for
  reverse proxies, `conn.DialTunnel(sbID, port)` for raw TCP.
* `s.ProxyToGuest(w, r, sb, rest)` proxies to guest `/v1/<rest>` with flushes
  (e.g. `rest = "internal/desktop/start"`).
* `s.DialSandbox(ctx, sb, port)`: raw TCP to a port in the VM (public URLs,
  VNC).
* `s.WaitUsable(ctx, id, d)` waits while a sandbox starts.

## Lifecycle helpers

* `s.addPendingScrub(id, proto.GuestScrub{...})`: credentials to delete
  before the sandbox is usable at its next boot (it stays `provisioned` /
  `securing` until the scrub is confirmed). Call from `NewSandbox` or
  `PrepareGuest` hooks.
* `s.notifySandbox(sb, kind, level, window, title, msg)`: owner notice
  (`GET /notices`, mailed), deduplicated per kind and sandbox within `window`.
* `s.pauseBilling(id, reason)` / `s.resumeBilling(id, reason)`: stop and
  restart metering for a reason (`stop_refused`, `not_answering`); `openRun`
  and `ensureRun` never meter a paused sandbox.
* `s.checkOutboundURL(ctx, url, code)` + `whClient`: user-supplied URLs the
  server calls (webhooks, OTel) may not reach loopback, link-local or
  metadata addresses unless `Config.WebhookAllowLocal`; dials are checked
  too (`withOutboundPolicy(ctx, allow)`).
* `s.checkCredits(u, org)` also refuses suspended organizations
  (`403 org_suspended`, `orgs_suspend.go`).
* Deleting data: create a `bdop_` operation (`newDeletionOp`) and run
  `s.processDeletionOps`; it completes the operation once the data is
  confirmed gone (`deletion_ops.go`).

## Test with the fake node

```go
env := servertest.New(t)                     // server + admin + admin key
node := env.StartNode(servertest.NodeOptions{Guest: myGuestHandler})
id := env.Do(env.AdminKey, "POST", "/api/v1/sandboxes", nil).Str("sandbox.id")
env.WaitState(env.AdminKey, id, "ready")
node.SendGuestEvent(id, "prompt.event", ...) // simulate guest events
```

## Accounts-layer helpers (acct_*.go)

* **Default deny**: a route registered with `ActionAny` refuses API keys
  unless `keyActionAny` in `acct_guard.go` lists it (with the scope it
  needs, or `""` for any key). Prefer a real scope action. Guest
  pass-through routes (`/sandboxes/{id}/<guest prefix>`) are exempt.
* Org keys are refused (`api_key_org_forbidden`) on the account-settings
  prefixes in `orgKeyForbidden`; add new account-level routes there.
* Notices: `s.walletNotify(wallet, user, kind, title, msg)` (credits) and
  `s.notifySandbox(...)` (lifecycle) write the `notices` table served at
  `GET /notices`; both mail the notification address (`s.notifyAddress`).
* Audit log: mutating control-plane API calls are recorded automatically
  (`audit_events`); `s.audit(c, action, target, data)` adds an explicit one.
* Settings: `s.getSetting(key)` / `s.putSetting(key, value)` ("" deletes).
* Wallet policy: `s.walletPolicy(wallet)`, `s.maxActiveFor`,
  `s.startWindowsFor` (limits are per wallet, falling back to `Cfg`).
* The OpenAPI document is generated from `s.API`/`s.Route` registrations;
  Boat-mirroring routes take names from `acct_openapi_ops.go` (generated
  from `.ref/boat-v1.yaml`), others are tagged `CrowdVM`.

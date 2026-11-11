# web: the CrowdVM dashboard

Plain HTML, CSS and ES modules. There is no build step. `web.FS` (`//go:embed`) holds
`index.html`, `favicon.svg`, `css/`, `js/` and `vendor/` (noVNC 1.7.0, MPL-2.0, core files only).

The server mounts `web.FS` at `/`:

- If a request path matches a file, serve the file.
- If it does not, serve `index.html`. The client routes `/sandboxes/...`, `/nodes`, `/device`, `/desktop/{id}` and so on.
- `/api/*` and `/auth/*` must never fall back to `index.html`. Unknown API paths get a JSON 404 envelope.

## Develop

```sh
go run ./web/devserver -dir web            # http://127.0.0.1:8480, demo / demo, edits are live
go run ./web/devserver -autologin          # skip sign-in (screenshots)
```

The devserver keeps a mock of every endpoint below in memory. It also runs a fake RFB
server, so the noVNC desktop viewer works end to end.

## What the server must provide

The dashboard authenticates with a session cookie. Every non-GET request carries
`X-CrowdVM-CSRF: 1`. Responses use the `/api/v1` envelope: `{ok:true,type,...}`, or
`{ok:false,status,code,message,error:{code,message,status,details}}`. A 401 sends the
user to `/login?next=…`. Optional endpoints are marked *(opt)*. When one returns
404, 405 or 501, the UI hides that feature or shows a hint.

### Auth (outside /api/v1)
| Method | Path | Body → response |
|---|---|---|
| POST | `/auth/login` | `{username,password}` → sets cookie, `{user}` (401 on bad credentials) |
| POST | `/auth/signup` | `{username,password}` → sets cookie, `{user}` |
| POST | `/auth/logout` | clears cookie |
| GET | `/auth/me` | `{user:{id,login,displayName,isAdmin?,createdAt?}}`, 401 if no session |
| PATCH | `/auth/me` *(opt)* | `{displayName}` → `{user}` |
| POST | `/auth/password` *(opt)* | `{currentPassword,newPassword}` (401/403 when the current password is wrong) |
| POST | `/auth/device/approve` | `{userCode}` (404 for an unknown or expired code) |

### /api/v1: Boat-compatible shapes (see .ref/boat-v1.yaml)
- Account: `GET /me`, `GET /limits`, `GET /orgs`, `PATCH /orgs/active {org|null}`, `PATCH /orgs/{org} {name?, newSandboxesPrivate?}`
- Sandboxes: `GET /sandboxes?limit=200`, `POST /sandboxes` (sends an `Idempotency-Key` header), `GET|PATCH /sandboxes/{id}`, `DELETE /sandboxes/{id}` with header `X-CrowdVM-Confirm-Delete: <id>`. Also `POST /sandboxes/{id}/stop {force?}`, `/resume {type,ttlSeconds}`, `/fork {name,type,ttlSeconds}`, `/recover`, `/share`, `/unshare`, and `GET /sandboxes/{id}/usage`.
- Guest pass-through:
  - `POST /sandboxes/{id}/commands`. With `{stream:true}` the answer is NDJSON frames (`started`, `stdout`, `stderr`, `exit`, `error`), flushed per frame. A plain JSON answer is accepted too.
  - Same endpoint with `{detached:true}`.
  - `DELETE /sandboxes/{id}/commands/{processId}`
  - `GET /sandboxes/{id}/command-history?limit=`, `GET …/command-history/{commandId}/logs?stream=stdout|stderr`
  - `GET /sandboxes/{id}/processes`
  - Files: `GET /files/list?path=`, `GET /files?path=`, `PUT /files {path,content}`, `DELETE /files?path=`, `POST /files/folder {path}`, `POST /files/move {source,destination}`
  - Signed URLs: `POST /files/download-url {path,ttlSeconds}` and `POST /files/upload-url {path,ttlSeconds}` → `{url,method}`. The browser fetches `url` directly: GET to download, PUT with the raw body to upload.
  - Agent: `GET /conversations`, `GET /events?conversation=&limit=`, `POST /prompt {prompt,provider,new?|conversationId?}` (→ `conversationId`), `POST /steer {message,conversation}`, `POST /interrupt?conversation=`
- Desktop: `POST /sandboxes/{id}/desktop?vnc=1&theme=light|dark` → `{desktopUrl, wsUrl, token?, password?, provisioning?}`. **`wsUrl`** is a websockify-style websocket (binary frames, subprotocol `binary`) that carries raw RFB. The viewer never puts the token in the URL: it removes any `?token=` from `wsUrl` and sends the token as a second subprotocol, `crowdvm-token.<token>` (from `token`, the `wsUrl` query or the `#token=` of `desktopUrl`). `desktopUrl` points at `/desktop/{id}[?theme=][&view=browser]#token=…`: the dashboard's full-screen viewer, which needs no session, reads the token once and strips it from the address bar. `?overlay=0` hides the viewer's toolbar. `POST /sandboxes/{id}/browser` → `{viewerUrl}` opens the browser view the same way.
- Public URLs: `POST /sandboxes/{id}/host {port,public,localhost,title,firewall?:{allow,deny}}` → `{port,url,pathUrl,access,hidden,firewall}` (re-posting changes access/firewall or un-hides). `GET /sandboxes/{id}/host?all=1` → `{ports:[…]}` including hidden ones; `DELETE /sandboxes/{id}/host/{port}` hides a port.
- Snapshots:
  - `GET /sandboxes/{id}/snapshots`, `GET /snapshots/{snapshotId}/tree?path=&depth=`
  - `GET|POST /saved-snapshots`, `DELETE /saved-snapshots/{name}`
  - `GET|POST /saved-snapshots/{name}/shares`, `DELETE /saved-snapshots/{name}/shares/{shareId}`. A share code is accepted as `from` on `POST /sandboxes`.
- Environments:
  - `GET|POST /environments`; `GET /environments/{id}` *(opt; falls back to the list)*; `PUT|DELETE /environments/{id}`; `POST /environments/{id}/upgrade`
  - `PUT|DELETE /environments/{id}/vars/{key}`; `PUT /environments/{id}/secret-files {path,contents}`; `DELETE /environments/{id}/secret-files?path=`
  - Repositories are saved as the full list: `PUT /environments/{id} {repositories:[{repositoryId,baseBranch,setupScript,setupBlocking}]}` (`repositoryId` = a clone URL, `owner/name` or the `databaseId` of a listed repo). `DELETE /environments/{id}/repos/{databaseId}`.
  - Also on `PUT /environments/{id}`: `passSandboxCredentials`, `proxyAiCredentials`, `credentialProfileId`, `access {restricted,members}` (org environments), `registryLogins`. The page shows `effective` and `warning`.
  - Organization environments: every call carries `?org=<orgId>`.
  - Git hosts: `GET|POST /repos/sources {kind,name?,baseUrl?,token}`, `DELETE /repos/sources/{id}`.
- AI credentials:
  - `GET|POST /credential-profiles`, `PUT|DELETE /credential-profiles/{id}`
  - `GET /credential-profiles` → `{profiles, credentials:[{id,kind,getUrl}]}` (the catalog drives the page). `PUT|DELETE /credential-profiles/{id}/keys/{credential}` (DELETE removes any credential, sign-ins too), `PUT /credential-profiles/{id}/sign-ins/{credential} {value,label?,keepCurrent?,region?}`, `PUT /credential-profiles/{id}/accounts/{credential}/{account}/active`, `DELETE /credential-profiles/{id}/accounts/{credential}/{account}`, `PUT|DELETE /credential-profiles/{id}/custom-apis/{NAME}`
  - `GET|PATCH /account-settings {agentDefaults:{provider,model,fast},useCase}`, `GET /provider-models`; per sandbox `GET /sandboxes/{id}/ai-usage`, `PUT /sandboxes/{id}/ai-access {blocked}`
  - `GET|PUT /ai-proxy`, `GET /ai-usage?days=30`
- API keys: `GET /api-keys` (`{apiKeys,catalog}`), `POST /api-keys/scoped {name,ttl,preset|actions,sandboxIds?,environmentIds?}` → `{apiKey,secret}`, `POST /api-keys/{id}/rotate` → `{secret}`, `DELETE /api-keys/{id}`, `GET /api-keys/{id}/usage`
- Webhooks: `GET|POST /webhooks` (POST → `{webhook,secret}`), `PATCH|DELETE /webhooks/{id}`, `POST /webhooks/{id}/rotate` → `{secret}`. The UI documents the signature: `X-CrowdVM-Signature: v1=<hex HMAC-SHA256 of "<X-CrowdVM-Delivery>.<X-CrowdVM-Timestamp>.<raw body>">`.

- The viewer treats desktop tokens as single-use. It keeps the token only in memory, and a reconnect or "Open in a new tab" asks `POST /sandboxes/{id}/desktop` for a fresh one (when signed in), so the UI keeps working if the server makes tokens single-use (DESK-005). Without a session the viewer reuses the link's token and, if the server refuses it, says to get a new link.
- API explorer (`/api-explorer`): reads `GET /api/v1/openapi.json` (the server's generated document: `paths`, `parameters`, `requestBody`, `summary`, `x-crowdvm-action`, `x-crowdvm-extension`) and sends "Try it" requests with the session cookie and `X-CrowdVM-CSRF: 1`. The YAML link is `/openapi/crowdvm-v1.yaml`.
- Also used: `GET /notices` + `POST /notices/{id}/dismiss|dismiss-all` (banner above every page), `GET /snapshots/{id}/tree|files?path=|download`, `DELETE /snapshots/{id}` (confirm header), `GET /sandboxes/{id}/snapshots/latest`, `GET /sandboxes?include=cost,deletedCost`, `GET …/command-history/{id}/logs?offset=` (live follow), `GET|PUT|DELETE /telemetry`, `GET /webhooks/{id}/deliveries`, `POST /webhooks/{id}/test`, `/ci…`, `/account/data-retention|close|reopen|erase`, `PATCH|DELETE /nodes/{id}`, `POST|DELETE /nodes/{id}/evacuate {graceSec}` (node `evacuation:{startedAt,deadline}`, `pauseReason`, `noSnapshotSandboxes` on GET /nodes/{id}), `/admin/users|invite-codes|retention|policies/{wallet}`, `PATCH /orgs/{org}/members/{id} {concurrentCap,usageCapCredits|clearUsageCap}`.

### /api/v1: CrowdVM extensions
| Method | Path | Shape |
|---|---|---|
| GET | `/nodes` | `{nodes:[{id,name,online,lastSeenAt?,owner:{login,displayName}\|string,info:proto.NodeInfo,usage:proto.Usage}]}` |
| POST | `/nodes/join-tokens` *(opt)* | `{token,expiresAt?}`. Without it the UI tells users to ask an admin. |
| GET | `/credits?days=30` *(opt)* | `{balance,enforced,earned30d,spent30d,burnPerHour,earnRate?,daily:[{day:"YYYY-MM-DD",earned,spent}],ledger:[{at,kind:"earn"\|"spend"\|"grant",amount,description,sandboxId?,nodeId?,nodeName?}]}` |
| POST | `/orgs` *(opt)* | `{name}` → `{org}` |
| GET | `/orgs/{org}/members` *(opt)* | `{members:[{id,login,displayName,role,joinedAt}],invites:[{id,login,createdAt}]}` |
| PATCH | `/orgs/{org}/members/{id}` *(opt)* | `{role}` |
| DELETE | `/orgs/{org}/members/{id\|me}` *(opt)* | Removes a member. `me` leaves the org. |
| POST / DELETE | `/orgs/{org}/invites`, `/orgs/{org}/invites/{id}` *(opt)* | `{login,role}` |
| GET | `/org-invites` *(opt)* | Invites for the current user: `{invites:[{id,org:{id,name},invitedBy}]}` |
| POST / DELETE | `/org-invites/{id}/accept`, `/org-invites/{id}` *(opt)* | Accept or decline |
| GET | `/sandboxes/{id}/metrics?from=&to=&step=` | Metrics graphs (DASH-004) from the 30 s monitoring samples (kept 7 days). `from`/`to`: RFC 3339 or unix seconds (default: the last hour). `step`: seconds or a duration like `5m` (default: range/360, at least 30 s; at most 1000 points). → `{id,from,to,stepSeconds,intervalSeconds:30,live,points:[{at,samples,cpuUtilization:0..1,memoryUsedBytes,memoryTotalBytes,diskUsedBytes,diskTotalBytes,ioPressure:0..1}]}`. A point averages the samples in `[at, at+step)` (totals: the maximum); steps without samples (sandbox stopped) are left out. `409 monitoring_off` when monitoring is off; `400 invalid_range\|invalid_step`. |
| GET | `/downloads` *(outside /api/v1)* | `{downloads:[{os,arch,name,size,modifiedAt,url}]}`: binaries served at `/downloads/{os}/{arch}/{name}` (incl. `crowdvm-linux-amd64`, the in-sandbox CLI). |

Extra fields on existing shapes:

- **Sandbox**: `node:{id,name,tier}`, null while stopped.
- **`POST /sandboxes`**: also accepts `placement:{nodeId?,minTier?,gpu?}` and `setupScript`.
- **`GET /sandboxes/{id}/usage`**: also returns `credits`.
- **ApiKey**: `scope.preset`.

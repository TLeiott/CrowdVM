# Bug hunt r10 (sonnet)

2026-11-11. Product code unchanged; I only added `internal/server/bughunt_r10_test.go` and this report. Unit tests only (`servertest`, the fake GitHub and fake guest of `feat_ci_test.go`). No cluster, VM or e2e test. I did the work myself, no sub-agents.

## What I audited

- **Round-9 fixes** (`grep r9`): `pathScopeCookies` and `guardGuestContent` (cookie and origin headers), the `Supersedes` check by node (`snapshots.go:262`), the hydration-progress node check (`nodes.go:634`), `addPendingScrub` history (`lifecycle_secure.go:77`), `idemResponse`, the `node_offline` guard on resume (`lifecycle.go:1432`). I found no hole in the ones I could exercise from the server. See "What held" and "Not verified".
- **Webhooks** (`feat_webhooks.go`): URL checks and the dial-time guard, signature, retries and leases, secret rotation, per-owner fairness, deletion, tenant isolation, outbox fan-out.
- **OpenTelemetry** (`feat_otel.go`): endpoint checks, header handling, log and metric flush, deletion.
- **CI** (`feat_ci.go`): there is no inbound GitHub webhook (the controller polls), so I audited link/token handling, job tracking, label parsing, cross-account links.
- **Billing, credits, limits, rate limiting** (`acct_billing.go`, `credits.go`, `ratelimit.go`, `acct_settings.go`): start and concurrency limits, policies, extra starts, caps, grace, quotas, transfers, grants, account merge.
- **Retention and deletion** (`feat_retention.go`, `deletion_ops.go`): ZDR, close, erase, purge, the retention job.

## How to run

```
go test -count=1 -run TestBughuntR10 ./internal/server/
```

Each test fails today. I also ran `go vet ./internal/server/` and the rest of the package (`-skip TestBughuntR10`): it passes.

## Bugs proven by a failing test

All in `internal/server/bughunt_r10_test.go`.

### 1. A CI job deletes any sandbox named by its runner (security, data-loss, medium)

- Test: `TestBughuntR10CIRunnerNameDeletesForeignSandbox` (`:22`)
- Code: `feat_ci.go:851-869` (`track`), `:790` (`deleteSandbox`)
- Observed: the job's `runner_name` is `crowdvm-<sandbox id of someone else's account>`. When the job completes, `track` takes that id as "the sandbox that ran it" and deletes it. The victim's sandbox returns 404.
- Expected: only a sandbox CI booted for this link (label `crowdvm.ci.job`, same owner) is ever touched.
- Cause: the runner name is chosen by whoever runs the job. A user who links their own GitHub account runs a self-hosted runner named `crowdvm-<id>` in their own repository. The opus-14 B1 fix trusts the name and never checks who owns the sandbox. It needs the victim's sandbox id (visible in shared links, logs, org lists).

### 2. Job ids collide across links: the second account's job is dropped (wrong-result, stuck, medium)

- Test: `TestBughuntR10CIJobIDCollidesAcrossLinks` (`:53`)
- Code: `feat_ci.go:484` (`newJob` dedupes on the id alone), table `ci_jobs.id` is the primary key (`:103`).
- Observed: two links on different API URLs (two Gitea instances, or a Gitea and GitHub) that both have a job `1`: the second link's job never appears, never runs, and stays queued on the provider.
- Expected: jobs are keyed by link and provider job id.
- Cause: provider job ids are only unique per instance. Gitea counts 1, 2, 3 on every instance, so two LAN Gitea servers collide at once. It is also a squat: an attacker's own "Gitea" can pre-claim ids of a victim's GitHub jobs.

### 3. Parallel creates exceed `maxActive` and the start limits (security, limit bypass, medium)

- Test: `TestBughuntR10ConcurrentCreatesBypassMaxActive` (`:84`)
- Code: `sandboxes.go:897-904` (checks), `:983` (insert), `:1005` (`recordStart`). The same pattern is in resume (`lifecycle.go:1472-1478` vs `:1556`) and fork (`:1655-1661` vs `:1750`).
- Observed: `--max-active 1`, 12 creates at once: 2 to 6 are accepted (5 runs, always more than 1).
- Expected: exactly 1.
- Cause: check and insert are separate steps with no lock or transaction. The same window lets a script bypass the minute/hour/day start windows and the credit and member-cap gates.

### 4. A fork keeps its source's size and skips the allowed-sizes policy (security, limit bypass, low-medium)

- Test: `TestBughuntR10ForkIgnoresAllowedSizesPolicy` (`:114`)
- Code: `acct_billing.go:833-846` (`policyGuard` reads only `req["type"]`), `lifecycle.go:~1668` (`size := src.Size`), `sandboxes.go:860-863` (create from a saved snapshot takes `src.Size`).
- Observed: policy `allowedSizes: [small, default]`; `POST /sandboxes {type: large}` gives 403, but forking a `large` sandbox with no `type` gives 202 and a `large` sandbox.
- Expected: 403 `machine_class_plan_required`.
- Cause: the guard checks the requested type. When none is sent, the size is inherited and nothing checks it. Create from a saved snapshot has the same path (not tested separately).

### 5. Account erase keeps provider credentials and the CI token (security, privacy, medium)

- Test: `TestBughuntR10EraseKeepsCredentialsAndCIToken` (`:137`)
- Code: `feat_retention.go:394-412` (`purge`).
- Observed: after `POST /account/erase` the account's `credential_accounts` row (API key or sign-in token) and its `ci_links` row with the sealed GitHub token are still in the database. The CI poller keeps using that token (status stays `active`).
- Expected: "data and identity go now": credentials, CI links and tokens are deleted.
- Cause: `purge` deletes sandboxes, saved snapshots, webhooks, telemetry and OTel logs only. Also left, by code reading (no test): `git_sources` (repo tokens), `environments` (secret env), `sandbox_ssh_keys`, `ai_proxy_*`, `account_settings`, `notices`.

### 6. Admin merge does not move the OpenTelemetry export (wrong-result, low)

- Test: `TestBughuntR10MergeLeavesTelemetryBehind` (`:171`)
- Code: `acct_settings.go:417-424` (`mergeMoves`).
- Observed: after `merge from --into into`, `GET /telemetry` as the target is `null`. The source's sandboxes now belong to the target, so their lifecycle logs and metrics stop reaching the collector.
- Expected: docs say "Move everything of USER ... into TARGET". The table lacks `telemetry`, and also `account_settings`, `wallet_policies`, `wallet_state`, `account_policy`, `ai_proxy_settings`, `starts`.

### 7. Any user can squat an organization's CI link (security, low-medium)

- Test: `TestBughuntR10CILinkSquatsForeignOrg` (`:232`)
- Code: `feat_ci.go:1010-1047` (`connect`).
- Observed: a user whose token is not a member of `acme` links `account: acme` with `confirm: true`. The real administrator then gets 409 `ci_account_linked` and cannot link it until the squatter unlinks.
- Expected: for an organization the token must belong to an admin (membership check), as personal accounts must match the token's login (`account_mismatch`).
- Cause: for organizations only `/users/{login}` is read, which any token can do.

### 8. `ci public` picks the first link by login, across accounts (wrong-result, low)

- Test: `TestBughuntR10CIPublicPicksAnotherAccountsLink` (`:251`)
- Code: `feat_ci.go:1154` (`links("status='active' AND (lower(login)=lower(?) OR id=?)")`, then `ls[0]`).
- Observed: user B's link `alice` (their own Gitea) gets 403 `ci_link_forbidden`, because user A's earlier `alice` link on GitHub matched first.
- Expected: only links the caller can reach are considered (like `visibleLinks`).

### 9. A member's usage cap ignores the extra starts the member bought (wrong-result, low)

- Test: `TestBughuntR10MemberCapIgnoresExtraStarts` (`:272`)
- Code: `acct_billing.go:268-272` (`chargeExtraStart` writes the ledger row with no actor), counted by `spentThisMonth(wallet, actor)` (`:164`, `:166`) and `checkCredits` (`credits.go:96`).
- Observed: org member capped at 0.04 credits, one extra start (0.05) paid by the org: the next create is still accepted (202).
- Expected: 402 `team_member_cap_reached`. `kind IN ('usage','extra_start')` shows the intent, but the row has `actor_id` NULL, so the per-member sum never includes it. `enforceCaps` has the same blind spot.

### 10. A negative `creditCapMonthly` policy is accepted and locks the wallet (wrong-result, low, admin only)

- Test: `TestBughuntR10PolicyAcceptsNegativeCreditCap` (`:313`)
- Code: `acct_billing.go:751-756` (only the integer limits are range-checked).
- Observed: `PUT /admin/policies/{wallet} {"creditCapMonthly": -1}` gives 200. Every start then fails with `credit_cap_reached`, and `enforceCaps` stops the wallet's running sandboxes at the next billing tick.
- Expected: 400 `invalid_policy`. `lowCreditAlert` and `monthlyCredits` also accept negatives (no effect, but stored).

### 11. Two huge admin grants overflow the balance to +Inf and blank every wallet response (crash-ish, wrong-result, low, admin only)

- Test: `TestBughuntR10GrantOverflowBreaksWalletResponses` (`:331`)
- Code: `account.go:255` (`handleAdminGrant` checks only `amount != 0`), `credits.go:19` (`walletAdd`), `credits.go:70` (`walletJSON`).
- Observed: two grants of `1.7e308` give 200 with `{"ok":true,"type":"admin.credits_granted"}` and no wallet. `GET /credits` for that user answers 200 with no `wallet` or `ledger` (the JSON encode of +Inf fails and the fields are dropped).
- Expected: the grant is refused (or capped), the wallet stays valid.

### 12. Azure's WireServer is not on the metadata block list (security, low)

- Test: `TestBughuntR10WireServerNotBlocked` (`:355`)
- Code: `outbound_guard.go:25-27` (`metadataIPs`).
- Observed: a webhook for `http://168.63.129.16/...` is accepted (201) with `--webhook-allow-local` off. The same list guards OTel endpoints and CI `apiUrl`. Webhook `test` returns the first 512 bytes of the answer.
- Expected: refused like `100.100.100.200`. 168.63.129.16 serves a VM's extension settings, including protected ones. Only matters for servers hosted on Azure.

## Code only

- **Create from a saved snapshot skips the size policy** (`sandboxes.go:860-863`): same cause as bug 4; I tested the fork path only.
- **Rotated webhook secrets**: `rotate` stores `prev_secret` and returns `previousSecretExpiresAt`, but no code ever signs with the previous secret or clears it (`feat_webhooks.go:835`, `:400-415`). Pending retries are re-signed with the new secret. The API promises a grace window that the sender does not provide (receivers accepting both is the only help). Cosmetic.
- **OTel flush drops a whole batch on any non-retryable status** (`feat_otel.go:~405`): a wrong header (401) deletes every queued lifecycle log. Design choice, listed for the owner.
- **Closed and erased accounts keep being polled by CI**: `ci_links` stay `active` (see bug 5 for erase; close keeps links for 30 days) and each queued job posts a failed status to the user's commit. Related to r8 sonnet's deleted-org finding.

## What held

- Webhook SSRF: the dial-time `Control` check catches loopback, link-local, unspecified and metadata forms (`127.1`, `2130706433`, `::ffff:127.0.0.1`, `[::1]`, `localhost.`, `0x7f.1`, `0177.0.0.1`) at save time. `127.0.0.1.` and a few IPv6 translation forms (`64:ff9b::`, `2002:7f00:1::`) pass the save-time check but cannot connect; redirects are never followed, proxies are not used.
- Webhooks are tenant-isolated (`load` checks the owner); signature, constant headers, retry schedule and the 8-attempt cap match the docs; per-owner leases keep one account's hanging endpoints from taking all slots; deleting a webhook removes its deliveries.
- OTel: header names and values are validated, queued logs of a deleted export are removed, per-account flushing, SSRF check at save and dial.
- Rate limiting: `clientIP` uses the rightmost forwarded address only with `TrustProxy`; the window counter and headers are consistent.
- Transfers: check and debit are one statement (no overdraw under parallel transfers), NaN and non-positive amounts are refused.
- Round-9 fixes read: `pathScopeCookies` strips `Clear-Site-Data`, `Refresh` and the dashboard's cookie names and scopes the rest to the app prefix; `guardGuestContent` removes the same three headers; the `Supersedes` update now needs `node_id=nodeID`; `history` is merged into the pending scrub.

## Not verified

- Not exercised with a real GitHub or Gitea (the fake in `feat_ci_test.go` only), nor live OTLP collectors.
- Billing ticks (`BillNow`, grace, quotas) were read, not driven: `BillingInterval` is one hour in `servertest`. I saw no double charge by reading; two concurrent `BillNow` calls would double-charge a run, but only the billing loop calls it.
- The round-9 resume guard for a node that just went offline covers `resume` only. I did not check the other paths that boot an existing sandbox from its last snapshot (`lifecycle_place.go:285` auto-rebuild, `nodes.go:891`).
- `deletion_ops.go` was read only (earlier rounds covered it with tests). ZDR and the retention job were read, not stressed with chains of snapshots.
- Bug 3 depends on timing: it failed in every run here (about 8 runs, 2 to 6 accepted).

## Fixes

All 12 fixed; each test in `internal/server/bughunt_r10_test.go` now passes.

1. **CI runner name** (`feat_ci.go`, `runnerSandbox`): a runner's `crowdvm-<id>` counts only when CI booted that sandbox for this link (a `ci_jobs` row of the link names it). Anything else is ignored.
2. **CI job id collisions** (`feat_ci.go`, migration 3): new column `gh_id` holds the provider's job id; dedupe is per link. A second link's job with a taken id gets the key `<id>-<link id>`. Provider calls (track, logs) use `gh_id`.
3. **Parallel creates** (`ratelimit.go` `lockLimits`, `sandboxes.go`, `lifecycle.go`): one lock holds the limit checks (credits, concurrency, start windows) together with the insert/state change and `recordStart`, in create, resume and fork.
4. **Size policy on forks and deploys** (`acct_billing.go` `policyGuard`): with no `type`, the guard checks the inherited size (the fork's source, or the saved snapshot of `from`).
5. **Erase** (`feat_retention.go` `purge`): also deletes credential profiles, accounts, custom APIs and proxy tokens, CI links, repos and jobs, git sources and repos, environments (versions a saved snapshot needs stay, as in org deletion), SSH keys, AI proxy settings, account settings and notices.
6. **Merge** (`acct_settings.go`): moves telemetry, account settings, wallet policies and state, AI proxy settings, OTel logs, starts and notices; one-row-per-account tables move only when the target has none, and the source row is then dropped. `account_policy` is not moved: it holds the source's close and erase dates.
7. **CI org squat** (`feat_ci.go` `connect`, `ciGH.orgAdmin`): an organization links only with a token of one of its admins (GitHub `/user/memberships/orgs/{org}` role admin; Gitea `/users/{me}/orgs/{org}/permissions` owner or admin). Help text and `cli-reference.md` name the extra token scope (fine-grained: Members read).
8. **`ci public`** (`feat_ci.go`): picks the caller's own link among those with the login.
9. **Extra starts and member caps** (`acct_billing.go`, `ratelimit.go`): `recordStart` takes the sandbox owner; the `extra_start` ledger row carries it as actor.
10. **Negative credit policies** (`acct_billing.go`): `creditCapMonthly`, `lowCreditAlert` and `monthlyCredits` must be finite and 0 or more.
11. **Grant overflow** (`account.go`, `credits.go`): a grant is at most 1e9 and the balance stays within 1e12; `walletAdd` refuses a non-finite delta or result.
12. **Azure WireServer** (`outbound_guard.go`): `168.63.129.16` is on the metadata list.

Code-only items left open: create from a saved snapshot is now covered by fix 4; rotated webhook secrets (cosmetic), OTel batch drop on 401 (design), CI polling of closed accounts (erase now removes the links; close keeps them for its 30 days).

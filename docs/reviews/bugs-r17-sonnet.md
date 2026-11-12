# Bug hunt r17 (sonnet)

2026-11-12. Product code unchanged. I only added `internal/cli/bughunt_r17_test.go`, `internal/guest/agents/bughunt_r17_test.go`, `internal/server/bughunt_r17_test.go` and this report. Unit tests only (the CLI's functions, the agent `Manager` with its fake harness, the server's `servertest` environment with a fake node and a fake AI upstream). No cluster, VM, e2e test or real AI call. I did the work myself, no sub-agents. Tree: `main` at `9ffe0fe`, in a detached worktree.

## What I audited

- **Round 16's fixes (`9ffe0fe`)**
  - `preinit_linux.go` `growData` / `alignPartition`: partition numbering (`/dev/vdaN`, `nvme0n1pN`, `mmcblk0pN` all give the right `-N`), a partition that is not the last one (`growpart` grows only into the free space behind it, the align step only shrinks), a disk that did not grow (`growpart` answers `NOCHANGE`, which is skipped; an aligned partition is left alone), a failed `sfdisk` (logged, the partition keeps its size, the open is still tried). I ran `alignPartition` on a file-backed GPT with `sfdisk` and checked that `sfdisk -N n` with only `start,size` keeps the partition's **name and type** (it does; otherwise `dataPartition()` would no longer find the volume). `growpart` is not installed here, so `growData` as a whole was not run.
  - `guest_egress.go`: the session context is created per `guestStreams` call, and `guestStreams` runs once per channel session (`guest_channel.go:566`), so a session replaced by a reconnect closes only its own flows. `context.AfterFunc` registered after the dial fires at once if the session already ended; the deferred stop prevents the callback after `Join` returned (no double close, no goroutine left: the closer goroutine ends with the session). `SetLinger(0)` is set only on the destination socket.
  - `cmd_access.go`: `tunnel --pin` / `authorizeAndPin` / `sshOptionsPin`: what it does on every ssh connection (one `AddSSHKey` call, which the guest de-duplicates in `addAuthorizedKeys`, plus a `known_hosts` rewrite: bug 2), errors (a refused `AddSSHKey` stops the tunnel with the API's message), stdout (nothing printed). Windows: the quoting only matters for spaces; `syscall.SIGPIPE` exists in Go's windows `syscall`, so `catchPipe` compiles there.
  - `cmd_exec.go` `warnStdin`, `catchPipe` scope (only around `runStream` and `attach`; `runCommand`'s sync path, `--detach` and other commands keep the default), the write-error stop path (`readAttach` returns `done` with the error, `runLong` then runs `StopCommand`; a direct `exec --attach PID` does not stop a process it did not start). `helpers.go` `stoppedWaiting`, `update.go` `comparePre`, `cmd_snapshot.go` strip roots (`path.Base(rel)`, `_data` for a volume): no hole found.
  - `web/js/vnc.js`, `web/js/pages/desktop.js`: what an unauthenticated visitor reaches now. The page itself only tries the websocket without a token; the server decides (`feat_desktop.go:403`): a token must verify, else the sandbox must be public (`isPublic`), else normal auth. An anonymous visitor of a public desktop can start the desktop inside the sandbox (`startDesktop`) and gets raw RFB; the browser kind needs a token. A refusal before RFB sends the visitor to `/login`. Nothing else is reachable.
  - `sdk/typescript/src/client.ts` idle body timeout: 204 and 304 have a `null` body in Node's fetch, so the `Response` copy is skipped (I checked in Node 26: `res.body === null` for both, and a copy of a 200 with an empty body works); `_op`/`readJson` read the body at once, so the idle timer never waits for a slow consumer; retries of GET only cover errors raised inside `fetch`. A body that stalls surfaces as `APIConnectionError: reading the response failed: This operation was aborted` (no mention of the timeout; cosmetic, not tested). The test suite itself could not be built here (no `node_modules`).
- **Integrated agents, guest side**: `agents/agents.go`, `http.go` (prompt, steer, interrupt, events, attachments), `run.go` (turn loop, steer fallback, interrupt, process-group signals), `store.go` (persistence, compaction, cursors, zero data retention), `preflight.go`, `harness.go` (argv of every harness, route selection for DeepSeek/Bedrock/ChatGPT), `rpc.go` (Codex driver), the start of `acp.go`; `agentsmgr.go`, `skills.go`, `playbook.go`, `apply.go` (applied config).
- **Integrated agents, server side**: `acct_prompt.go` (defaults, pre-flight), `feat_ai.go` (profiles, accounts, tokens, `aiPlan`, usage), `feat_ai_proxy.go` (token lookup, upstream choice, header rewriting, usage counting, custom APIs), `acct_ai_refresh.go`, `passthrough.go` (scopes of the guest routes), `feat_history.go` (agent events while stopped), `orgs.go` member removal, `lifecycle_secure.go` (sandbox token).

## How to run

```
go test -count=1 -run TestBughuntR17 ./internal/cli/ ./internal/guest/agents/ ./internal/server/
```

The 7 tests fail today. `go vet ./internal/cli/ ./internal/guest/agents/ ./internal/server/` and `gofmt -l` are clean. Without them, the rest of `./internal/cli/` and `./internal/guest/agents/` pass; I ran no more of `./internal/server/` than the new tests (its suite takes long and the new tests use only existing helpers).

## Bugs proven by a failing test

### 1. A member removed from an organization keeps using the organization's AI key through a running sandbox (security, medium)

- Test: `TestBughuntR17RemovedMemberKeepsOrgAIProxyAccess` (`internal/server/bughunt_r17_test.go:21`)
- Code: `internal/server/orgs.go:546` (`handleRemoveMember`), `internal/server/feat_ai_proxy.go:105` (`handleAIProxy`: token → `profile_id`, nothing else is checked), `internal/server/feat_ai.go:1375` (`aiSandboxProfile`, the only place that asks `orgCanUse`).
- Observed: org `R17Org` has a profile with an OpenAI key and the proxy switched on for the org. Member `bob`'s personal environment names that profile, so his personal sandbox boots with a proxy placeholder for it; a proxy call with the placeholder reaches the upstream with the org's real key (`200`). The owner removes `bob` (`DELETE /orgs/r17org/members/{id}` answers 200: it only refuses for bob's *org* sandboxes, and this one is personal). The same placeholder still reaches the upstream with the org's key (`200`).
- Expected: after the removal (or after the owner narrows the profile's `access` list so bob may not use it, `aiUsable`/`envCanUse`) a call with that placeholder is refused, as the next start would fall back to bob's own profile.
- Cause: the placeholder row binds `(sandbox, profile)` at start and is deleted only when that sandbox stops or is deleted. Nothing revokes it when the owner's right to use the profile ends. The sandbox keeps the real key's reach for as long as it runs, and a sandbox can run for months. Fix: check `orgCanUse` (and the profile's access list) in `handleAIProxy` for an org profile against the sandbox owner, or delete the rows of that user's sandboxes whose `profile_id` belongs to the org in `handleRemoveMember` and when a profile's access changes.

### 2. Concurrent `known_hosts` writes lose host key pins: parallel ssh connections fail with "Host key verification failed" (wrong-result, medium)

- Test: `TestBughuntR17KnownHostsConcurrentPinsKeepEveryLine` (`internal/cli/bughunt_r17_test.go:17`)
- Code: `internal/cli/cmd_access.go:168` (`setKnownHost`: `ReadFile`, edit in memory, `WriteFile`; no lock, no temp file). Called from `authorizeAndPin` (`:110`) on every `crowdvm ssh`, and since r16 from every ssh connection through the `ssh-config` ProxyCommand (`tunnel --pin`).
- Observed: 12 goroutines pin 12 different sandboxes in one `known_hosts`. In round 0 of the test only 9 of the 12 lines survive; the others were overwritten by a writer that read the file before them.
- Expected: every pin is in the file.
- Cause: each writer rewrites the whole file from the copy it read. Two `crowdvm ssh a` and `crowdvm ssh b` started together, or VS Code Remote-SSH / `rsync` / `scp` opening several connections to different sandboxes at once, lose each other's line. The ssh with `StrictHostKeyChecking yes` (set because the pin succeeded) then refuses the host ("No ED25519 host key is known", exit 255); the next attempt re-pins and works, so it looks flaky. `os.WriteFile` also truncates the file in place, so a connection to a third sandbox that reads `known_hosts` at that moment sees an empty file. Round 16 made this common (the config entry now pins on *every* connection). Fix: take `lockConfig` around the read-modify-write and write through a temp file with `rename`.

### 3. A prompt that failed in the same millisecond it started is served as "interrupted" after the sandbox stops (wrong-result, low-medium)

- Test: `TestBughuntR17StoredPromptStatusOrderOfSameMillisecond` (`internal/server/bughunt_r17_test.go:91`)
- Code: `internal/server/feat_history.go:644` (`agentEvents`: `ORDER BY ts, event_id`), the overlay right below it (the *last* `prompt.status` wins), used by `storedEvents`, `storedConversations` and `storedPromptRun`.
- Observed: the guest emits `prompt.status running` and then `prompt.status failed` with the same timestamp (they are one millisecond apart at most: a harness that is not installed or a refused credential fails immediately). Their ids are random (`pst_` + random), so when the id of "running" sorts after "failed", the stored copy answers `GET /prompts/{id}` with `status: interrupted`, `error: the sandbox stopped while this prompt was running`; the real error is gone. `GET /events?sort=asc` lists the two events in id order, not emission order.
- Expected: `status: failed` with its error; events of one millisecond in the order the guest emitted them.
- Cause: the table has no insertion order in the sort key. Use `rowid` as the tie breaker (`ORDER BY ts, rowid`; the upsert `ON CONFLICT DO UPDATE` keeps the rowid). It happens about half the time for a prompt that fails at once, in the dashboard's history of every stopped sandbox.

### 4. The account's default provider silently moves a continued conversation to another agent (wrong-result, medium)

- Test: `TestBughuntR17AccountDefaultProviderDoesNotHijackAConversation` (`internal/server/bughunt_r17_test.go:132`)
- Code: `internal/server/acct_prompt.go:196` (`applyPromptDefaults`).
- Observed: account default provider `codex`; `POST /sandboxes/{id}/prompt {"prompt":"go on","conversationId":"<a Claude Code conversation>"}` reaches the guest as `provider: "codex"`.
- Expected: the provider stays unset for a prompt that names an existing conversation, so the guest keeps that conversation's harness. The guest implements exactly that rule (`agents/http.go`: `provider == ""` and `c.LastHarness != ""` → `provider = c.LastHarness`); the server's default fills it first and the rule never runs. The same applies to the CLI's "current conversation" (no `conversationId`, `new` false), which the server cannot see.
- Cause: the account default is meant for new conversations; here it is applied to every prompt without a provider. The guest then runs the other agent: the native Claude session is dropped (`c.Session/SessionHarness` are overwritten), the transcript is replayed as text into Codex, and it needs a Codex credential the user may not have configured for this sandbox. Not tested, but the same holds for `model` and `fast` defaults. Fix: skip the provider default when `conversationId` is set (and let the guest apply it for a new conversation, or send it only with `new: true`).

### 5. `~/` attachments skip the size limits (wrong-result, low)

- Test: `TestBughuntR17AttachmentLimitsApplyToTildePaths` (`internal/guest/agents/bughunt_r17_test.go:16`)
- Code: `internal/guest/agents/preflight.go:100` (`checkAttachments`), against `http.go` `eventAttachments`, which does resolve `~/`.
- Observed: a sparse 25 MB + 1 file: the absolute path is refused (`400 invalid_attachments`), `~/attachments/big.bin` is accepted. The file is attached to the turn and read into the prompt event path as usual.
- Expected: the same limits (12 files, 25 MB each, 64 MB in total).
- Cause: `checkAttachments` joins `~/x` under the home as a directory named `~`; the `Stat` fails and the file is skipped (`continue`). The API documents attachments as "paths in the sandbox (~/attachments/...)" (`promptRequest.Attachments`), and `eventAttachments` expands `~/`. Only the CLI (which always sends `/home/user/attachments/<name>`) is safe. Fix: expand `~/` the same way in `checkAttachments`.

### 6. Zero data retention keeps earlier prompt text in `state.json` when the agent starts with retention on (leak, low)

- Test: `TestBughuntR17ZeroRetentionDropsEarlierPromptTextAtStart` (`internal/guest/agents/bughunt_r17_test.go:40`)
- Code: `internal/guest/agents/store.go:76` (`load`: `if m.zdr() { Truncate(0) }` only touches `events.jsonl`), `saveLocked` is called there only when a prompt had to be marked interrupted.
- Observed: a disk with a finished prompt from before retention was turned on (a fork or resume of an older sandbox, or a retention switch while the sandbox was stopped). The agent starts with `ZeroDataRetention` true and no prompt is pending: `state.json` still holds the prompt text (`"prompt":"earlier-secret-prompt"`) and the conversation's `lastPromptPreview`, until some later prompt makes the agent rewrite the file.
- Expected: the comment says "retention turned on: drop what an earlier boot kept"; the file written without prompt text, or removed.
- Cause: `load` clears the event log but not the other file, and the in-memory state keeps the text until `saveLocked` runs. Fix: call `m.saveLocked()` at the end of `load` when `m.zdr()` (it already strips the text) and drop `Prompt`/`Message` from the in-memory records.
### 7. A FIFO as attachment hangs `POST /v1/prompt` and, with its lock, the whole agent API (stuck, medium)

- Test: `TestBughuntR17FifoAttachmentDoesNotWedgeTheAgent` (`internal/guest/agents/bughunt_r17_test.go:63`)
- Code: `internal/guest/agents/http.go` `eventAttachments` (`os.ReadFile(full)`), called from `newPromptLocked` while `handlePrompt` holds `m.mu`; `preflight.go:100` `checkAttachments` only stats the path.
- Observed: `{"provider":"fake","prompt":"look","attachments":["<fifo>"]}`. The request never answers (3 s in the test, for ever in practice): a FIFO has size 0, so the limits pass, and `ReadFile` blocks on the open or read with `m.mu` held. Every other agent call (`/v1/events`, `/v1/conversations`, `/v1/interrupt`, further prompts, `Busy()` for auto-stop) then waits on the same lock.
- Expected: a prompt that is answered (202, or 400 for a path that is not a regular file); the agent stays usable.
- Cause: the read has no regular-file check and no deadline, and it runs under the manager lock. Anyone with `agent.prompt` on the sandbox (a key without `exec`) can wedge the agent until the sandbox restarts; the user can also do it to themselves with `mkfifo ~/a && crowdvm prompt --attach`-like API calls. `/dev/zero`-style devices read 256 KiB and stop; named pipes and some `/proc` or `/sys` files hang. Fix: `openFileNB` plus a regular-file check as in `prefetchFile`, and build the event data before taking `m.mu`.

## Code only

- **A stalled sign-in renewal can write one account's refreshed tokens over another account's secret** (`acct_ai_refresh.go:152-163`, `aiRenew`; wrong-result, low). The refresh call runs up to 30 s with no lock on the account list; afterwards `aiAccounts(profileID, cred)` is read again and *every* account with `Active` set is updated with the new secret. If the user switches the active account (or replaces the sign-in) while the call is in flight, the other account's secret is replaced by the renewed tokens of the first. The window is the refresh latency; needs a fake token endpoint that blocks and a concurrent `PUT .../active`. Fix: update the row by the account id the old secret came from.
- **Every proxy request inside the 10-minute renewal margin calls the token endpoint again if renewal fails** (`aiRenew`/`aiActiveFresh`; cosmetic). A revoked refresh token (`invalid_grant`) is retried, one call at a time under the per-profile lock, on each request until the access token expires; nothing remembers the failure. Each request waits for that call (up to 30 s).
- **Agent prompt/event text from a private period is visible to teammates once the sandbox is shared** (`passthrough.go` `guestAction`, `feat_history.go` `historyTeammate`; wrong-result, unclear). ORG-012 says "history recorded while private hidden from teammates" and the code hides private-period *command* history only; `events`, `conversations` and `prompts` (prompt text, answers, base64 attachments up to 256 KiB) of the same period are served in full. I read "history" as command history (the PARITY row sits next to `command_history`); if it covers the agent log, this is a gap. Not tested for that reason.
- **Attachments are read by the guest agent as root** (`agents/http.go` `eventAttachments`: `os.ReadFile(full)` for any path, up to 256 KiB, inlined as base64 in the `prompt` event, readable with `sandbox.read`). The harness runs as `user`, so a caller with `agent.prompt` gets root-only files (for example `/etc/shadow`, the guest's own data under `/var/lib/crowdvm`) into the event log. The sandbox user has passwordless sudo (`systemNote`), so this crosses no boundary today; it would on an image without sudo. No test: the guest runs as non-root in unit tests.

## What held

- **Round 16 fixes**: see "What I audited". In particular: `alignPartition` keeps the GPT name and type of the partition (so `dataPartition()` still finds it), a replaced channel session only closes its own flows, `catchPipe` is only active around the two streaming loops, `comparePre` orders `nightly.<date>.<run>` and `rc.N` correctly, the anonymous desktop viewer reaches only what the server's `handleWS` admits.
- **AI proxy**: the token is looked up by hash per sandbox (no cross-sandbox reach), the real secret goes only in the auth headers (custom APIs also in the query, never the path), `..`/`\` in the path is refused after unescape, `Cookie` is removed, usage is counted once per request (success, error, client abort) and SSE usage takes the maximum of cumulative counts, `aiBlocked` stops a sandbox at once, placeholders are deleted at stop/delete and rotated on every start, a live apply swaps the profile of the token with the environment's.
- **Agent endpoints**: one turn at a time per conversation, interrupt of a queued/starting/steer-restarting turn (the `c.stop` check at the top of the loop and after `startRun` closes the window), steer `late`/`failed`/`fallback` paths, the long-poll and ETag of `/events`, cursor fallback after compaction, request env is dropped from the prompt record when it finishes, interrupted marking of queued/running prompts at agent start.
- **Scopes**: guest routes map to `exec`, `file.*`, `agent.prompt`, `sandbox.read`, `ssh`; the per-sandbox token is bound to its sandbox id and holds only `sandbox.read` and `host`.
- **Skills and playbook**: `installSkills` keeps a user's edited or deleted file, removes files a newer version dropped only when unchanged; the playbook recorder ignores its own opens and the usual noise paths.

## Not verified

- Bug 1 on a real cluster (removal, then a real harness call). The test uses the fake node, the real proxy handler and a fake upstream. Narrowing a profile's `access` list is the same code path and was not run separately.
- Bug 2's effect in real ssh (that `StrictHostKeyChecking yes` refuses an unknown host is from `ssh_config(5)`); the lost update itself is what the test proves.
- Bug 4's consequence in a real Claude/Codex pair (the guest's rule is read from code; the server's forwarded body is what the test proves).
- `growData` end to end (`growpart` is not installed here), LUKS opening of the aligned partition, and the real `sfdisk --relocate` on a disk that did not grow.
- The TypeScript SDK suite (no `node_modules` in the worktree), Python SDK.
- Real browser behavior of `vnc.js` for an anonymous visitor (read only).
- `computer.go`, the second half of `acp.go` and `pi.go` (read only in part), and the real harness parsers against real harness output.
- The Windows build of `catchPipe`.

## Fixes

All 7 fixed; every `TestBughuntR17` test passes.

1. **Removed member keeps the org's AI key** (`feat_ai_proxy.go` `handleAIProxy`): each proxy call checks that the sandbox's owner may still use an organization's profile (`orgCanUse` with its access list); otherwise 403 `ai_profile_not_usable`.
2. **Concurrent known_hosts writes** (`cmd_access.go` `setKnownHost`, `config.go` `lockFile`): the read-modify-write runs under `known_hosts.lock` and the file is replaced by a rename.
3. **Same-millisecond prompt status** (`feat_history.go` `agentEvents`): stored events are ordered by `ts, rowid` (arrival order).
4. **Account default provider moves a conversation** (`acct_prompt.go` `applyPromptDefaults`): the account's defaults are not applied to a prompt that names a conversation. Not fixed: a prompt that continues the guest's current conversation without naming it still gets the account default (the server cannot see it; the guest would have to apply the default, which older guests do not).
5. **`~/` attachments skip the limits** (`agents/preflight.go` `attachPath`): `~/` is resolved before the limits are checked.
6. **Zero retention keeps earlier prompt text** (`agents/store.go` `load`): with retention on, `state.json` is rewritten without prompt text at start.
7. **FIFO attachment wedges the agent** (`agents/preflight.go`, `agents/http.go` `readInline`): a non-regular attachment is refused before the lock (400); the inline read opens non-blocking, checks for a regular file and skips large files without reading them.

Code only, also fixed: a sign-in renewal updates only the account whose refresh token it renewed (`acct_ai_refresh.go`).

Not fixed: renewal retries after a failed refresh (cosmetic), private-period agent history visible to teammates (needs a decision on what ORG-012 covers), attachments read as root (no boundary today).

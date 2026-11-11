# Node UI: offering a machine to the pool

Status: implemented on branch `feat/node-ui` (2026-11-11): Phases 1–4. Differences
from the text below: evacuation needed no billing change (a stopped sandbox isn't
metered), notices give times in UTC, and the dashboard has no per-node page, so the
`crowdvm-node open` hint sits on the node card. After review: `open` uses a single-use
login code (not the token in the URL), the cookie is derived from the token and only
allows the page's own settings, "Restart node" exits the daemon and lets systemd or
the Windows service's recovery actions start it again (not a service-manager call),
and evacuation ends only once no snapshot is held only by the node. Not done: the
"Later" items.

## Summary

Keep two surfaces and give every setting exactly one owner:

- **The local page** (`http://127.0.0.1:8421`, served by `crowdvm-node`) is the control
  panel for *this machine*: pause and resume, a weekly schedule, resource limits, what
  runs here, what it earned, and a checklist for leaving. It grows out of today's status
  page. It does not add a process, an app or a tray icon.
- **The server dashboard's Nodes page** is the control panel for *the account*: add a
  machine (it already shows the join command), drain, evacuate, remove, rename,
  labels and earnings history. It already has Drain, Resume scheduling and Remove.
- **The CLI** (`crowdvm-node`, `crowdvm nodes`) can do everything either page does,
  and both pages call the same APIs.

The design adds one server feature, **evacuate**: stop the sandboxes on a node, which
saves their disks as snapshots, and resume them on other nodes. This makes "take my
machine back now" possible without losing anyone's data. The rule for sandbox users
becomes: **a node owner can always get their machine back; your sandbox keeps its
disk, but not its running processes.**

Joining stays in the CLI and installer. "Offer only when idle", a tray icon and a
desktop app are left for later.

## What exists today

| Piece | Where | Behaviour |
|---|---|---|
| Owner pause | node config `paused`; `NodeInfo.Paused` | No new sandboxes are placed here; running ones continue. Requests that only fit this node wait for it (`couldEverPlace` counts paused nodes). `config set paused` applies at once through `POST /api/pause`. |
| Drain | server DB `nodes.disabled` | No new sandboxes; requests never wait for this node. The node owner (`canManageNode`) or an admin sets it from `crowdvm nodes drain` or the dashboard. The node sees it as the heartbeat's `Drained`. |
| Capacity | node config `cpus`, `memory`, `disk`, `max-sandboxes` | Takes effect after a daemon restart. |
| Service | systemd user unit (`Restart=always`, `KillMode=process`); Windows SCM service (restarts on failure) or a logon task | VMs survive daemon restarts, and `adoptVMs` takes them back. |
| Node loss | server `NodeLostGrace` = 3 min | Sandboxes are rebuilt elsewhere from their latest snapshot, so up to about a minute of data is lost. |
| Local page | `internal/node/ui.go`, `ui.html` | Shows status and has a pause toggle. Writes need the `X-Crowdvm-Ui` header and a localhost or IP `Host`. There is no other authentication. |
| Dashboard | `web/js/pages/nodes.js` | Add a machine (join command), drain/resume, remove, labels. |

Two problems in today's code matter for this design:

1. **Lost config updates.** `Config.Save` writes the daemon's whole in-memory config.
   Suppose the daemon is running and you run `crowdvm-node config set cpus 8`, then
   pause from the page or the CLI. The daemon saves its old in-memory config and
   silently undoes `cpus 8`.
2. **Wrong labels and docs.** The page and `crowdvm-node status` say "Drained by the
   server admin", but owners can drain too. `security.md` says to restart the node
   after `config set paused`, but the change already applies at once.

## Goals and non-goals

Goals:

- Pause, resume, set a schedule and set limits without a terminal.
- See at a glance whether the machine is offering, what runs on it and what it earned.
- Leave cleanly, and never lose a user's sandbox disk.
- Stay consistent with the CLI, the config file, the service manager and drain.
- Linux and Windows, no new runtime, no paid signing, and the server stays internal.

Non-goals for v1:

- Joining from a GUI.
- Detecting when the owner is idle.
- A tray icon or a desktop app.
- Live migration, where a VM moves with its memory to another node.

## Recommended design

### Architecture

```
 owner's browser                                  owner's browser / any machine
       │ 127.0.0.1:8421 (cookie)                         │ server URL (user session)
       ▼                                                 ▼
 ┌───────────────────────┐   heartbeat (node token)  ┌─────────────────────────┐
 │ crowdvm-node daemon   │ ───────────────────────▶  │ crowdvm-server          │
 │  local page + API     │   NodeInfo: paused,       │  dashboard Nodes page   │
 │  config.json (owner)  │   pauseReason, capacity   │  nodes.disabled (drain) │
 │  VMs (QEMU)           │ ◀───────────────────────  │  evacuate, remove       │
 └───────────────────────┘   Drained, Evacuating,    │  credit ledger          │
       ▲                     EarnedCredits           └─────────────────────────┘
       │ Bearer token file                                ▲
 crowdvm-node CLI                                    crowdvm CLI (user login)
```

The local page and the dashboard link to each other. The local page links to the
server's Nodes page, and the dashboard can show `crowdvm-node open` as the way to
reach a machine's local page. The daemon never gets user credentials. Anything that
needs the owner's identity, such as drain, evacuate or remove, happens on the server
under the owner's login. The local page shows that state and links to it.

### Settings and their owners

| Setting | Written by | Stored in | Effect on the scheduler |
|---|---|---|---|
| `paused` (master switch) | owner: local page, `crowdvm-node config set paused` | node config | Part of the effective pause (below). |
| `schedule` | owner: local page, `config set schedule` | node config | Part of the effective pause. |
| `schedule-hold` | owner: local page ("Offer until 08:00") | node config | Overrides the schedule until its next boundary. |
| `cpus`, `memory`, `disk`, `max-sandboxes` | owner: local page, `config set` | node config | Applied live (new). |
| `disabled` (drain) | owner or admin: dashboard, `crowdvm nodes drain` | server DB | No new sandboxes, and requests never wait for this node. |
| evacuation | owner or admin: dashboard, `crowdvm nodes evacuate` (new) | server DB | Sets drain, then stops and moves the sandboxes. |
| daemon running | service manager | systemd / SCM | Down for more than 3 min means sandboxes are rebuilt elsewhere. |

**Effective pause**, which the daemon computes and sends as `NodeInfo.Paused` plus a
new `PauseReason`:

1. If `paused` is set, the node is paused. Reason: "paused by you".
2. Otherwise, if a `schedule-hold` is active, the hold decides. Reason: "held until 08:00".
3. Otherwise, if a `schedule` is set, the node offers inside its windows only. Reason:
   "outside schedule (offers again 19:00)".
4. Otherwise, the node offers.

The schedule never writes `paused`, so it never fights the CLI. The scheduler on the
server does not change; it already reads `NodeInfo.Paused`. Old servers ignore
`PauseReason`.

### States shown to the owner

The states are computed for display and never stored. The local page header shows
one of them, and the dashboard row shows the same one.

| State | Condition | Main action on the local page |
|---|---|---|
| **Offering** | connected, not paused, not drained | Pause |
| **Paused**, *N still running* | effective pause, VMs > 0 | Resume; "Take it back now…" |
| **Paused** | effective pause, no VMs | Resume |
| **Outside schedule** | paused by the schedule only | "Offer until <next boundary>" |
| **Drained** | `Drained` from the heartbeat | Open dashboard to undrain |
| **Evacuating**, *N left, done by 14:32* | `Evacuating` from the heartbeat | Open dashboard (cancel is possible during the grace period) |
| **Ready to remove** | drained, no VMs | Leave checklist, step 3 |
| **Disconnected** | daemon up, server unreachable | (shows the last error) |
| **Daemon stopped** | the page doesn't load; `crowdvm-node status` says so | Start the service |

### Start and stop: what happens to running sandboxes

| Owner wants to… | Action | New sandboxes | Running sandboxes | Data |
|---|---|---|---|---|
| Stop offering for a while | **Pause** (or the schedule) | None | Keep running until they stop on their own. Most stop within 1 h (default auto-stop). | Kept |
| Share less | Lower the limits | Only what fits | Keep running even above the new limit, because the limit only blocks new placements. | Kept |
| Stop offering for good | **Drain** | None, and no request waits for this node | Keep running | Kept |
| Get the machine back now | **Evacuate** (grace: now / 10 min / 1 h) | None (drained) | Stopped after the grace period, then resumed on other nodes | Disk kept; running processes lost |
| Leave the pool | **Leave checklist**: drain, evacuate, remove, uninstall | None | Moved off first | Kept |
| (never offered as a button) | Stop the daemon | None | Go offline. After 3 min they are rebuilt elsewhere. | Up to ~1 min lost |

The UI never offers "stop the service" as a way to stop offering. Stopping the
service is the only path above that can lose data.

### Evacuate (new, on the server)

`POST /api/v1/nodes/{id}/evacuate {graceSec}`, allowed for the node's owner or an
admin (`canManageNode`). The CLI command is `crowdvm nodes evacuate <node> [--grace 10m]`,
and the dashboard has an "Evacuate…" button.

1. Set `disabled` on the node (drain) and record the evacuation and its deadline.
2. Send a notice to every user with a sandbox on the node: "Node *X* is being reclaimed
   at 14:32. Stop or snapshot your sandbox before then to choose the moment; after that,
   it is stopped and resumed on another node." Use the existing notices table.
3. At the deadline, run a normal `stop` on each remaining sandbox. The stop runs the
   pre-stop hook, saves the disk as a snapshot and powers the VM off. A failed save
   refuses the stop, the same as today, so no sandbox is forced before its disk is saved.
4. Queue a resume elsewhere with `pendingBoot{Kind: BootResume, Exclude: node,
   Reason: "evacuated"}`. This is the same mechanism `rebuildLost` uses. If no other
   node fits, the sandbox stays stopped and its owner gets a notice. Sandboxes pinned
   with `--node` end up stopped for the same reason.
5. While a sandbox is being moved, its user is not charged (a new `billingPaused`
   reason, `evacuated`). The node owner earns for the time the sandbox actually ran
   on the node, nothing more. Credits aren't enforced yet, so this only matters once
   the server runs with `--enforce-credits`.

Special cases:

- **`--no-auto-stop`** does not block evacuation. That flag means the sandbox does not
  stop when it is inactive. It does not promise that the machine stays available.
  Document this next to `--no-auto-stop` in `sandboxes.md`.
- **`--no-snapshots`**: the user asked for the disk never to leave the node, and
  stopping such a sandbox erases it. Evacuate erases these sandboxes at the deadline.
  It never snapshots them against the user's choice, and it never lets them block the
  owner. This is kinder than today: when a node is lost, `rebuildLost` only rebuilds
  sandboxes with snapshots enabled, so these are already gone without warning. Their
  notice says so: "The machine hosting *X* leaves at 14:32. This sandbox has snapshots
  off, so it can't be moved and will be erased then. Copy out anything you need now."
  The evacuate dialog tells the owner how many sandboxes this affects.
- **The node goes offline during evacuation**: the existing node-loss path takes over.

### The local page

```
┌ CrowdVM Node · lab-desktop ─────────────────── ● Offering   [ Pause ] ┐
│ Connected to http://100.101.102.103:8420 · v1.4.0 · tier gold (612)   │
├ Availability ──────────────────────┬ Earned ─────────────────────────┤
│ ( ) Always  (•) On a schedule       │ 41.5 credits in total          │
│ Mon–Fri 19:00–08:00, Sat–Sun all day│ 3.2 in the last 24 h           │
│ Now: offering · pauses at 08:00     │ History → dashboard            │
│ [ Pause until 19:00 ]               │                                │
├ Shared resources ───────────────────┴────────────────────────────────┤
│ CPUs [ 8 ▾] of 16   Memory [ 24 GiB ] of 64   Disk [ 300 GiB ]        │
│ Max sandboxes [ 4 ]           [ Save ]  applies at once               │
│ Network/GPU settings: change them, then [ Restart node ]              │
│   (sandboxes keep running)                                            │
├ Running here (2) ────────────────────────────────────────────────────┤
│ bx_7f2…  default  2 vCPU / 4 GiB   running 34 min                     │
│ bx_a91…  large    4 vCPU / 8 GiB   running 2 h 10 min                 │
│ Usage 6/8 vCPU · 12/24 GiB · 2/4 sandboxes                            │
├ Leave the pool ──────────────────────────────────────────────────────┤
│ 1 Drain            ✓ (via dashboard or `crowdvm nodes drain lab-desktop`)
│ 2 Move sandboxes   2 left [ Evacuate in dashboard ]                   │
│ 3 Remove the node  [ Open dashboard ]                                 │
│ 4 Uninstall        `crowdvm-node uninstall-service`                   │
└──────────────────────────────────────────────────────────────────────┘
```

- Without a login cookie the page is read-only. In place of the buttons it says:
  "Run `crowdvm-node open` to control this node."
- Earnings come from a new heartbeat field, `EarnedCredits` (total and last 24 h). The
  server sums the ledger for the node at most once a minute and caches the result.
  This uses the node token and needs no user login.
- "Running here" uses what the daemon already knows: sandbox ID, size, state and
  uptime. Owner names and history stay on the dashboard.

### How it coexists with the rest

**The config file has one writer at a time.** This fixes the lost-update bug and
covers every UI:

- While the daemon is running, every change goes through it: the page, and the CLI's
  `config set`, which calls `POST /api/config` with the key and value. The daemon
  validates the value, does a load-modify-save of the file for that key only, and
  applies it live if it can. It answers `{applied: true}` or `{restartNeeded: true}`.
- When the daemon is not running, or `ui-addr` is `off`, the CLI writes the file
  itself, as today. The daemon also switches to load-modify-save for the keys it
  writes itself (`paused`, `schedule-hold`, the node ID at join). That way, someone
  editing the file by hand loses nothing either.

**The CLI stays complete.** New keys: `schedule` and `schedule-hold`. New command:
`crowdvm-node open`, which opens the page logged in. `crowdvm nodes evacuate` is
added on the user CLI. `crowdvm-node status` shows the same state names as the page.

**Service manager.** The page shows "Restart node" only when the service manager will
bring the daemon back: the systemd user unit, or an SCM service with its recovery
actions. A logon task or a foreground `run` gets "restart it from a terminal". The
button calls the service manager (`systemctl --user restart crowdvm-node`, or SCM
stop/start), so there is no new self-exit path. VMs survive the restart thanks to
`KillMode=process` and `adoptVMs`.

**Drain.** Drain stays a server setting. The node only shows it. The labels change
from "Drained by the server admin" to "Drained (by you or an admin)".

**Server stays internal.** Nothing changes. The page links to the server URL the node
already uses. Earnings and evacuation state reach the node in the heartbeat it
already sends. No new inbound port and no cloud relay.

### Security of the local API

- **Token**: `ui-token` in the data dir, 32 random bytes, file mode 0600. It is created
  on first start and stays the same across restarts, so restarting the node from the
  page doesn't log the owner out. Delete the file to rotate it. On Windows the service
  writes it into the installing user's data dir, which `install-service` already names.
  Check that the file inherits that directory's ACL, so the user can read it and other
  accounts can't.
- **Browser login**: `crowdvm-node open` asks the daemon for a single-use code
  (60 s, `POST /api/login-code` with the token) and opens
  `http://127.0.0.1:8421/login?c=…`. The daemon answers with a `303` to `/` and sets
  a cookie (`HttpOnly`, `SameSite=Strict`) derived from the token, never the token
  itself: browsers send localhost cookies to every port. The cookie only allows the
  page's own settings (pause, schedule, limits, restart, benchmark). Every response sends
  `Referrer-Policy: no-referrer`. The page has no outbound links that carry
  credentials, and dashboard links open in a new tab with `rel=noreferrer`.
- **Writes** (`POST /api/*`) need the cookie or `Authorization: Bearer <token>`, and
  also the existing `X-Crowdvm-Ui` header and `Host` check. The CLI sends the Bearer
  token and the header.
- **`GET /api/status` stays open on loopback.** It contains no secrets, and keeping it
  open lets the page render read-only. A tray icon added later can use it without
  credentials.
- The page listens on `127.0.0.1` only, as today. Setting `ui-addr` to a LAN address
  stays possible, but the docs should warn that the page then needs the token over
  plain HTTP.
- The local API can't drain, evacuate or remove anything, because it has no
  privileges for that. In the worst case, a local process with the token can pause
  the node or lower its limits. That loses no data.

## Alternatives considered

| Option | Why not (for now) |
|---|---|
| **Tray / menu-bar app** talking to the local API | One more binary, autostart entry and build per OS. Linux tray support is uneven (GNOME needs an extension; Wayland has quirks). All it adds is an "is it on?" glance and a launcher. The open `GET /api/status` keeps it cheap to add later. |
| **Desktop app (Tauri / Wails)** | Needs a second toolchain, WebView2 or WebKitGTK at run time, and installer and signing work. It gives nothing over a browser tab for an internal tool. |
| **Server dashboard controls everything** (pause, limits, schedule pushed in the heartbeat) | `paused` would have two writers. The server would become the source of truth and the config file a cache. You also couldn't pause your laptop while the server is unreachable. Drain already covers remote control. |
| **Join from the local page** | `run` returns `ErrNotJoined`, and under `Restart=always` an unjoined service restarts every 5 s. A join form would send the join token through a browser for no benefit. The dashboard's "Add a machine" already gives a command to copy. |
| **Node token can drain its own node** | It would give `disabled` a second writer and the node token a new privilege. The owner already can drain with their user login. |
| **Node-owned "leaving" flag that the scheduler treats like drain** | It is a second way to exclude a node next to `disabled`, so drain is used instead. |
| **Leave by pausing and waiting** | Requests that only fit a paused node wait for it, which can block other users. Leaving has to drain. |
| **Rebuild elsewhere by replaying the sandbox's inputs** | A VM isn't deterministic: clocks, random numbers, network replies and LLM answers differ on the second run. Replaying would also repeat side effects such as `git push`, deploys, emails and API calls. Snapshots plus resume give the same result without either problem. |
| **Token embedded in the served HTML** | `GET /` is open, so any local process could read the token. |
| **Offer only when idle** in v1 | Idle detection differs per OS. A Windows service runs in session 0 and can't see user input. Wayland idle hints are unreliable. When the owner comes back, a sandbox can't be killed. The choices are to wait for it to stop (the machine stays loaded), evacuate on every coffee break, or throttle it with cgroups / job objects, which is real work. Schedules cover the common case ("evenings and weekends"). |

## Open questions

Decided (2026-11-11):

- **Grace period:** 10 minutes by default (choices: now, 10 min, 1 h). Sandbox users
  see the notice through the existing notices.
- **Billing:** evacuation doesn't charge the user for the move, and leaving costs the
  owner nothing. Credits aren't enforced yet.
- **`--no-snapshots`:** erased at the deadline after the notice (see Evacuate).

Still open:

1. **Should admins be able to set a minimum grace period**, so that owners can't
   always choose "now"?
2. **Should `uninstall-service --purge` wipe the data dir** after a successful
   remove? Today the data dir has to be deleted by hand.
3. **Schedule syntax and time zone:** use the node's local time with a small
   syntax (`mon-fri 19:00-08:00; sat,sun`)? Could the page's editor be the main
   way to set it?

## Implementation plan

Each phase ships separately and is useful on its own.

**Phase 0: fixes the rest builds on** (small)
- Load-modify-save for config writes in the daemon (`SetPaused` and every other write).
- Change the "Drained by the server admin" labels in `ui.html` and `printStatus`.
- Fix `security.md`: `paused` applies at once.

**Phase 1: local control**
- `ui-token` file, `/login` with a cookie, Bearer token for the CLI, `crowdvm-node open`.
- `POST /api/config` with a single writer. `config set` uses it while the daemon runs.
- Live reload of `cpus`, `memory`, `disk` and `max-sandboxes` (new `NodeInfo` capacity,
  admission only; running VMs untouched).
- A "Restart node" button where the service manager brings the daemon back.
- Page: limits form, read-only mode without login.

**Phase 2: visibility**
- `HeartbeatResponse.EarnedCredits` (total, last 24 h; cached on the server).
- Page: an "Earned" card, a "Running here" table with uptime, and a link to the
  dashboard. The dashboard shows `crowdvm-node open` for each node.
- `PauseReason` in `NodeInfo`, shown on the dashboard's node row.

**Phase 3: evacuate and leave**
- Server: an evacuate endpoint, a deadline in the reaper, notices, stop then
  `BootResume` with `Exclude`, a `billingPaused` reason `evacuated`, and
  `Evacuating` in the heartbeat response.
- `crowdvm nodes evacuate`, a dashboard button with a grace picker that shows how many
  `--no-snapshots` sandboxes will be erased, and an "Evacuate first" offer on Remove
  for a busy node.
- Page: the Leave checklist and the Evacuating state with a countdown.
- Docs: rules for owners in `node-owners.md`, and the host guarantee in `sandboxes.md`.
- Tests: evacuate with the process driver in unit tests, plus one VM e2e case.

**Phase 4: schedules**
- `schedule` and `schedule-hold` keys, effective-pause computation, a boundary timer
  in the daemon, and the page's availability card.

**Later, if people ask**
- Tray launcher (Go systray, status from `GET /api/status`).
- Start Menu / `.desktop` entry that runs `crowdvm-node open`.
- Live migration for evacuate, so running processes survive the move. QEMU can do it
  (`-incoming`), but sandboxes use `-cpu host`, so the source and target CPUs would
  have to match, or every VM would need a common, slower CPU model. Memory would also
  have to travel node to node, or through the server. Linux/KVM only (unclear with
  WHPX). Worth it only if people complain about evacuations killing running jobs.
- Offer only when idle: logon-task mode only, input idle plus AC power. Pause on
  return, with optional CPU throttling of QEMU (cgroup `cpu.weight` / Windows job
  object).

# Sandboxes, snapshots and forks

A sandbox is a full Ubuntu 24.04 VM with its own kernel and disk. User `user`, home
`/home/user`, passwordless `sudo`, Docker, a desktop, Chrome, VS Code, Node.js, Bun,
Python, Rust, `gh`, Claude Code, Codex and OpenCode. Ids look like `bx_qzdsd908dm`.

## Lifecycle

```
new ──▶ provisioning ──▶ ready ⇄ running ──stop──▶ archiving ──▶ archived ──resume──▶ provisioning …
                                    │
                                    └──fork──▶ (a new sandbox)
```

| Command | What happens |
|---|---|
| `crowdvm new` | The scheduler picks a node with room and boots the base image (or `--from` a saved snapshot). Waits for capacity unless `--fail-fast`. |
| `crowdvm stop ID` | The guest runs `~/.crowdvm/pre-stop` (or `~/.boat/pre-stop`) if it exists, stops Docker containers, syncs, and the VM is frozen and powered off. The OS does not shut down and other programs get no signal. The disk becomes a snapshot on the server and the node is free again. |
| `crowdvm resume ID` | Boots the latest snapshot on any node with room. The chain is copied to that node if it does not have it. |
| `crowdvm fork ID` | Copies a sandbox from its newest state. Forking a running sandbox takes a live snapshot (the filesystem freezes for about 80 ms) and starts the copy on the same node. |
| `crowdvm recover ID` | Power-cycles a sandbox that does not answer. |
| `crowdvm delete ID` | Erases the sandbox and its snapshots. |

`stop --force` skips the pre-stop hook and Docker and keeps only the last snapshot
that succeeded.

**When a stop is refused.** A stop saves the disk before it changes anything in the
guest. If that save fails, the stop is refused: the sandbox stays `ready` and usable with
its work on its disk, the stop shows `failing` (`crowdvm info ID` → `stop.status`), and the
server retries it by itself. You are not billed meanwhile (`billingPaused` in
`crowdvm info`). A resume cancels the stop; `stop --force` stops at once with the last
good snapshot; after 3 days of failed saves the stop is forced (you get a notice one day
before). A sandbox that stops answering is not billed either until it answers again.

A sandbox whose snapshots you deleted cannot be resumed or forked
(`409 snapshot_data_expired`); CrowdVM never boots an empty disk in its place.

A resume during `archiving` cancels the stop when the VM still runs. If a node goes
offline for more than 3 minutes, the server rebuilds its sandboxes on another node from
their latest uploaded snapshot.

## Auto-stop

A sandbox stops 1 hour after it was created or resumed (`--ttl SECONDS`, up to 30
days). A running agent prompt moves the deadline forward. Commands, SSH and services do
not. Change it while it runs with `crowdvm extend ID --hours 4`; turn it off with
`--no-auto-stop` (on `new`, `resume` or `extend`). Forgotten always-on sandboxes keep
holding a node, so use this sparingly.

`--no-auto-stop` doesn't keep the machine for you. A node's owner can always take their
machine back (`crowdvm nodes evacuate`). You get a notice with the time, by default
10 minutes ahead. Stop the sandbox before then to choose the moment yourself. At that
time CrowdVM stops it the normal way: the disk is saved, running processes are lost.
It then resumes the sandbox on another node. If no other node can host it, it stays
stopped and you get another notice. A `--no-snapshots` sandbox can't be moved, so it
is erased at that time; copy out what you need after the first notice.

## Snapshots

While a sandbox runs, the node takes a live snapshot every 60 seconds when something
changed, and one more at stop. Each snapshot stores only the blocks that changed, and
the node uploads it to the server. Resume and fork always use the newest snapshot;
there is no point-in-time restore.

Not saved: `/tmp`, `~/.cache`, the system journal, the Docker build cache, and paths
matched by a `.crowdvmignore` file (gitignore syntax; `.boatignore` and `.boxignore`
work too). Everything else on the disk is saved, including Docker images, volumes and
changes under `/etc` and `/usr`.

`--no-snapshots` turns snapshots off: stopping such a sandbox erases it.

Read files from a stopped sandbox without resuming it:

```sh
crowdvm snapshot latest ID
crowdvm snapshot tree snap_...                          # files and sizes
crowdvm snapshot pull snap_... -o ./restore --path /home/user/project
```

## Saved snapshots

A saved snapshot is a named copy of a sandbox's disk that you can start new sandboxes
from. Use it for a prepared stack: dependencies installed, databases seeded.

```sh
crowdvm snapshot ID web-stack                 # save
crowdvm new --from web-stack                  # start from it
crowdvm new --from web-stack --count 5        # five at once
crowdvm snapshot list
crowdvm snapshot rm web-stack
```

To give a saved snapshot to another account, make a share code. The code is shown once:

```sh
crowdvm snapshot share web-stack              # prints the code
crowdvm new --from <code>                     # the other account
crowdvm snapshot unshare web-stack <shareId>  # revoke
```

When another account (or another member of your organization) starts from your saved
snapshot, the server removes your secrets before the sandbox becomes ready: Claude,
Codex and GitHub logins, SSH keys, the CLI token, environment secret files and repo
clones. Everything else on the disk, including files you wrote yourself, is copied as
is. Check a snapshot for keys and tokens in your own files before you share it.

## Environments

An environment is what a new sandbox starts with: variables, secret files, git
repositories with setup scripts, registry logins and AI credentials. Manage them with
`crowdvm env` or in the dashboard under Environments. `crowdvm new --environment NAME`
picks one; `--no-env` passes nothing in.

## Sizes

| Size | vCPU | RAM | Credits per hour |
|---|---|---|---|
| small | 2 | 4 GB | 0.5 |
| default | 4 | 8 GB | 1 |
| large | 8 | 16 GB | 2 |
| xlarge | 16 | 32 GB | about 5.6 |

Each size also has a disk size; `GET /api/v1/server` lists the sizes this server
offers. Change the size of a stopped sandbox with `crowdvm resume ID --type large`. A
larger size grows the disk; a smaller one keeps the larger disk.

## Watching a sandbox

- `crowdvm info ID`, `crowdvm ps ID` (processes, busiest first), `crowdvm history ID`
  (commands and their stored output, also while stopped).
- `crowdvm history ID --mode process` lists the output of the sandbox's other
  processes, one record per process, within about 30 s: ssh sessions (with or without
  a terminal) and what they leave running, processes an agent starts, systemd services
  and Docker containers. It keeps what they write to a terminal, a pipe or a socket
  (up to 64 KiB/s per process, 1 MiB bursts), not files or binary output. Commands
  run with `crowdvm exec` keep their own record and are not repeated here.
- The dashboard's sandbox page has a Metrics tab: CPU, memory, disk and I/O pressure
  for the last hour up to 7 days, from the 30-second samples the guest reports.
  `--no-monitoring` keeps none of this.
- `crowdvm desktop ID` opens the Linux desktop in your browser.

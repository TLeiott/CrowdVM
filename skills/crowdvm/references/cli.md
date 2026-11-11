# crowdvm CLI cheat sheet

Taken from `crowdvm --help` and `crowdvm <command> --help`. Inside a sandbox the same
binary answers to `crowdvm`, `boat` and `box`; `host ARGS` is `crowdvm host ARGS`.

`ID` is a sandbox id (`bx_...`), `current` (last sandbox created or forked in this
shell), or `self` (the sandbox the CLI runs in). Commands shown with `[ID]` use the
current sandbox when the id is left out (inside a sandbox: the sandbox itself).

## Global flags

| Flag | Env | Meaning |
|---|---|---|
| `--api-url URL` | `CROWDVM_API_BASE` (also `CROWDVM_API_URL`) | Server URL |
| `--json` | | One JSON object per line. On by default when piped |
| `--org ORG` | `CROWDVM_ORG` | Run against this organization |
| `--profile NAME` | `CROWDVM_PROFILE` | Config profile |
| `--no-update` | `CROWDVM_NO_UPDATE=1` | Skip the newer-CLI check |

The API key comes from `CROWDVM_API_KEY`, else the profile, else `CROWDVM_TOKEN` /
`ASCII_TOKEN`.

## Lifecycle

```sh
crowdvm new [flags]                 # create and wait until ready
crowdvm list [--all] [--filter rsp] # alias ls; groups: r up, s stopped, p pending, t stopping, e error
crowdvm info [ID]                   # state, size, IP, desktop, TTL
crowdvm stop ID [--wait] [--force] [--status OP_ID] [-y]
crowdvm resume ID [flags]           # --type/--environment/--no-env change a stopped sandbox
crowdvm delete ID [-y] [--no-wait]  # alias rm; scripts must pass --yes
crowdvm deletion status OPERATION_ID
crowdvm fork ID [flags]             # copy from the latest snapshot
crowdvm extend ID (--hours H | --ttl SECONDS | --no-auto-stop)
crowdvm recover ID                  # power-cycle a sandbox that does not respond
crowdvm rename ID NAME
crowdvm subdomain ID LABEL          # DNS label of hosted URLs (<label>-<port>.<domain>)
crowdvm share ID / crowdvm unshare ID   # org sandboxes: shared with members / private
crowdvm usage ID [--since T] [--until T]  # machine time and credits used
```

`new` flags:

| Flag | Meaning |
|---|---|
| `--type small\|default\|large\|xlarge` | Machine size |
| `--ttl SECONDS` | Auto-stop after this many seconds from start (default 3600) |
| `--no-auto-stop` | Never stop automatically |
| `--name NAME` | Display name |
| `--from NAME\|CODE` | Start from a saved snapshot or share code |
| `--count N` | With `--from`: create N sandboxes at once |
| `-e, --env KEY=VALUE` | Per-sandbox variable (repeatable) |
| `--environment NAME` | Environment to start from (`crowdvm env list`) |
| `--no-env` | Give the sandbox none of your secrets |
| `--file SANDBOX_PATH=LOCAL_FILE` | Write a file before ready (repeatable) |
| `--setup-file SCRIPT` | Local shell script run once after ready (64 KB) |
| `--host PORT` / `--host PORT:public` | Host a port (repeatable) |
| `--no-snapshots` | Never snapshot; stopping erases the sandbox |
| `--no-monitoring` | Keep no command output, process list or metrics |
| `--no-wait` | Return once the create is accepted |
| `--personal` | Bill your personal account even when an org is active |
| `--visibility private\|shared` | Org sandboxes |
| placement flags | See [Placement](#nodes-and-placement) |

`fork` takes `--name`, `--wait`, `-e/--env`, `--environment`, `--no-env`, `--ttl`,
`--no-auto-stop`, `--no-snapshots`, `--no-monitoring`, `--type` and the placement flags.
`resume` takes `--no-wait`, `-e/--env`, `--environment`, `--no-env`, `--ttl`,
`--no-auto-stop`, `--type` and the placement flags.

## Run work

```sh
crowdvm exec [ID] [flags] [--] COMMAND...
crowdvm run [flags] [--] COMMAND...
crowdvm history ID [COMMAND_ID] [--limit N] [--mode M] [--stderr] [-f]
crowdvm ps ID                       # processes, busiest first
crowdvm ssh [-t] ID [--] [COMMAND...]
```

`exec` flags: one argument is a shell command line; more arguments run as argv.

| Flag | Meaning |
|---|---|
| `--timeout N` | Stop after N seconds and exit 124. Default 30. `0` = no limit. Above 600 or 0 the command runs in the background and its output streams live, surviving network cuts |
| `--cwd DIR` | Working directory, relative to `/home/user` |
| `--push DIR` | Push this local directory first and run in it |
| `--stream` | Print output as it is written |
| `--detach` | Run in the background, print the process id (`.processId` in JSON) |
| `--status PID` | Status and log tails of a detached process |
| `--attach PID` | Follow a detached process's output until it ends |
| `--kill PID` | Stop a detached process and its children |

`run` creates a sandbox, optionally pushes, runs with live output, prints time and
credits, deletes the sandbox, and exits with the command's exit code. Ctrl-C deletes the
sandbox. Flags: `--push DIR`, `--cwd DIR`, `--timeout N`, `--keep` (keep it; it stops 15
minutes later), `--from`, `--type`, `-e/--env`, `--environment`, `--no-env`,
`--personal`, placement flags.

`history --mode` takes `commands`, `process`, `sync`, `stream` or `detached`.
`--limit` max 100 (default 20).

## Files

```sh
crowdvm push ID DIR [--to PATH]     # git-aware archive copy; default /home/user/<dir name>
crowdvm scp [-r] [--api] SRC... DST # ID:/path is the remote side; relative = /home/user
crowdvm ssh-config ID [--print]     # writes ~/.ssh/crowdvm/<id>.conf
crowdvm tunnel ID PORT              # stdin/stdout to 127.0.0.1:PORT (ssh ProxyCommand)
```

After `ssh-config`, add `Include crowdvm/*.conf` at the top of `~/.ssh/config` once;
then `ssh <id>`, `scp file <id>:`, `rsync` and VS Code Remote-SSH work.

## URLs and ports

```sh
crowdvm host [ID] PORT [--public|--private] [--title T] [--localhost]
crowdvm host list [ID] [--all]      # alias ls; --all includes hidden ports
crowdvm host url [ID] PORT [--public] [--localhost]   # full URL, hosting first if needed
crowdvm host hide [ID] PORT         # take the URL down; the token is kept
crowdvm forward ID --remote PORT [--local PORT] [--bind ADDR]   # alias port-forward
crowdvm forward ID --reverse --local PORT   # this machine's port at 127.0.0.1:PORT in the sandbox
crowdvm subdomain ID LABEL
```

- URLs: `http://<label>-<port>.<domain>/` (label = subdomain, or the id with `_` → `-`)
  and `http://<server>/p/<id>/<port>/`.
- Private URLs carry `?_token=`. `--public` drops the token. Running `host` again
  returns the same URL.
- Ports 22, 5900-5902, 6080 and 7070 cannot be hosted. 50 hosted ports per sandbox.
- `--localhost` is accepted for Boat compatibility; the proxy reaches `localhost` and
  `0.0.0.0` services either way.

## Snapshots

```sh
crowdvm snapshot ID NAME [--no-wait]   # save under NAME (replaces an older NAME)
crowdvm snapshot list                  # alias ls
crowdvm snapshot info NAME
crowdvm snapshot rm NAME               # alias remove
crowdvm snapshot share NAME            # secret share code, shown once
crowdvm snapshot shares NAME
crowdvm snapshot unshare NAME SHARE_ID
crowdvm snapshot latest ID             # newest automatic snapshot of a sandbox
crowdvm snapshot tree SNAPSHOT_ID      # files and sizes
crowdvm snapshot pull SNAPSHOT_ID -o DIR [--path P]...   # /home/user → DIR/home_user/, volumes → DIR/docker/
crowdvm snapshot delete SNAPSHOT_ID... [-y] | crowdvm snapshot delete --all SANDBOX_ID [-y]
crowdvm snapshots [ID] [--all] [--limit N]   # snapshots of all sandboxes, or of one
crowdvm new --from NAME|CODE [--count N]
```

## Environments

Environments hold what a new sandbox starts with: repositories, variables, secret files,
credentials. Each change is a new version. Not the same as `new --env KEY=VALUE`.

```sh
crowdvm env list | info NAME | new NAME | rename NAME NEW | rm NAME | default NAME
crowdvm env set-var NAME KEY[=VALUE]        # value from stdin when omitted
crowdvm env rm-var NAME KEY
crowdvm env set-file NAME PATH [--from LOCAL_FILE]   # else stdin
crowdvm env rm-file NAME PATH
crowdvm env add-repo NAME REPO [--branch B] | crowdvm env rm-repo NAME REPO
crowdvm env set NAME [--secrets B] [--github B] [--agents-credentials B] \
  [--crowdvm-credentials B] [--safe-for-third-parties B] [--proxy-ai-credentials B] \
  [--credential-profile P]                  # B = true|false
crowdvm env upgrade NAME [--sandbox ID]...  # move running sandboxes to the latest version
crowdvm env access NAME [--everyone | --only EMAILS | --add M | --remove M]
crowdvm repos [--query Q]
crowdvm repos sources [add --kind github|gitlab|gitea|git --token T [--base-url U] [--api-url U] [--name N] | rm SOURCE_ID]
```

`--crowdvm-credentials true` gives sandboxes `CROWDVM_API_KEY`, so the in-sandbox CLI
can manage its own sandbox.

## Integrated agents

```sh
crowdvm prompt [ID] MESSAGE [flags]
crowdvm events [ID] [-f] [--convo C]...
crowdvm steer [ID] MESSAGE [--convo C]
crowdvm interrupt [ID] [--convo C]
crowdvm conversations [ID]          # alias convos
```

`prompt` flags: `--provider claude|codex|opencode|pi|...`, `--model M`,
`--reasoning-effort none|low|medium|high|xhigh|max`, `--fast`, `--new`,
`--resume CONVO_ID|current`, `--attach FILE` (repeatable, saved under
`~/attachments`; alias `--image`), `--no-wait`, `--local-login[=auto|subscription|api-key]`
(this machine's own Claude/ChatGPT login or API key for this turn; needs `--provider`;
env `CROWDVM_LOCAL_LOGIN`). Without `--new`/`--resume` it continues
this profile's current conversation on that sandbox. `--conversation` is an alias of
`--convo`.

AI credentials:

```sh
crowdvm ai profiles [--org-only]
crowdvm ai profile new NAME [--copy-from P] | rename P NEW | rm P | default P
crowdvm ai profile set-key P CREDENTIAL [VALUE]       # e.g. anthropic-api-key; stdin when omitted
crowdvm ai profile sign-in P CREDENTIAL [TOKEN] [--label L] [--account-id A] [--region R]
crowdvm ai profile set-api P NAME --base-url URL [--value V]
crowdvm ai profile rm-key P CREDENTIAL | rm-api P NAME
crowdvm ai profile access P [--everyone | --only EMAILS | --add M | --remove M]
crowdvm ai accounts CREDENTIAL | add-account CREDENTIAL [VALUE] | switch CREDENTIAL ACCOUNT | forget-account CREDENTIAL ACCOUNT   # --profile P
crowdvm ai proxy [on|off]
crowdvm ai block ID | crowdvm ai unblock ID
crowdvm ai usage [ID] [--days N]
```

## Desktop

```sh
crowdvm desktop ID [--no-open] [--public]    # noVNC URL with a token
crowdvm browser ID [--no-open] [--profile /abs/chrome-dir]   # only Chrome, no desktop
```

Inside the sandbox: `DISPLAY=:0`, `ascii-record-desktop start FILE.mp4 | stop | status`.

## Nodes and placement

```sh
crowdvm nodes                       # machines in the pool, with benchmark categories
crowdvm nodes info NODE_ID
```

Placement flags (on `new`, `fork`, `resume`, `run`):

| Flag | Meaning |
|---|---|
| `--gpu` | A node with a GPU (only nodes that pass one through) |
| `--nested` | Nested virtualization (KVM inside the sandbox) |
| `--min-tier bronze\|silver\|gold\|platinum` | Minimum node tier |
| `--category DIM=LEVEL` | Minimum benchmark level, e.g. `cpu=high` (`low\|medium\|high\|extreme`; repeatable) |
| `--node ID\|NAME` | This node |
| `--node-label KEY=VALUE` | Node label that must match (repeatable) |
| `--fail-fast` | Fail with `no_ready_machine` instead of waiting up to 15 minutes |

Node administration (`nodes drain|undrain|remove|update|join-token|join-tokens`) is for
admins; `crowdvm admin ...` holds the server administration commands.

## Account, keys, settings

```sh
crowdvm status [--check]            # server, account, config paths; --check exits 1 if signed out
crowdvm login [--server URL] [--api-key K | --key-stdin | --email E [--code C] | --username U --password-stdin]
crowdvm login --json                # no terminal: prints URL and code, then: crowdvm login --wait
crowdvm logout
crowdvm onboard [--for own|others] [--email E] [--server URL]
crowdvm limits [--all]              # start limits, active sandboxes, credits
crowdvm credits                     # alias wallet
crowdvm credits transfer --to WALLET --amount N [--from WALLET]
crowdvm billing [--all] | billing plan | billing extra-starts on|off [--monthly-cap N]
crowdvm notices [--all] | notices dismiss [ID] [--all]
crowdvm dashboard [--no-open]       # one-time 15-minute signed-in link
crowdvm api-key create NAME [--preset read-only|full-sandbox|ci|admin | --actions A,B] [--ttl 90d] [--sandbox ID]... [--env ENV_ID]...
crowdvm api-key list [--all] | revoke ID | rotate ID | usage ID [--verbose]
crowdvm org [list|status|switch ORG|create NAME|members|invite [EMAIL] [--role R]|join CODE|...]
crowdvm config list | get KEY | set KEY VALUE | unset KEY | path   # keys: server, profile, current, user
crowdvm webhook create URL [--event E]... [--name N] | list | get ID | update ID | rotate ID | remove ID
crowdvm data-retention [status | enable -y]
crowdvm telemetry [set ENDPOINT [-H k=v]... | unset]
crowdvm ci [connect|repos|job|logs|public|unlink]   # GitHub Actions on CrowdVM
crowdvm feedback [MESSAGE]
crowdvm agent-instructions          # alias agents-md; AGENTS.md section for agents
crowdvm shell-init bash|zsh|fish|powershell
crowdvm completions [bash|zsh|fish|powershell]
crowdvm self-update [--check] [--force]
crowdvm version
```

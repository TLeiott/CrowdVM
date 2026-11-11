# CLI reference

Every `crowdvm` command with the text `crowdvm <command> --help` prints. Generated from the CLI's command tree by `go generate ./docs`; do not edit by hand.

Global flags work on every command: `--api-url`, `--org`, `--json` (on by default when the output is piped), `--profile`, `--no-update`.

## crowdvm

```text
crowdvm creates and drives persistent Ubuntu VMs ("sandboxes") that run on
machines your team lends to a CrowdVM server. Use it to give yourself or a
coding agent more compute than this machine has.

  crowdvm login --server http://10.0.0.5:8420
  id=$(crowdvm new --json | jq -r 'select(.event=="ready") | .id')
  crowdvm push "$id" .
  crowdvm exec "$id" --cwd myrepo --timeout 0 "make test"
  crowdvm stop "$id"

Run 'crowdvm agent-instructions' for a guide aimed at coding agents.

Usage:
  crowdvm [command]

Account and sign-in:
  billing            Credits, limits and extra starts in scope (CrowdVM's billing: no plans, no cards)
  credits            Show your credit balance (CrowdVM's replacement for billing)
  dashboard          Open the server's web dashboard, signed in (one-time 15-minute link)
  feedback           Send feedback to the operators of this CrowdVM server
  limits             Show start limits, active sandboxes and credits
  login              Sign in (browser device login, API key, or username/password)
  logout             Sign out and clear the local API key
  notices            Notices: low credits, grace period, stopped or failing sandboxes, full disks
  onboard            Set up this computer: sign in, say what the sandboxes are for, show credits
  status             Show the server, the signed-in account and the config paths
  usage              Show the machine time and credits one sandbox used

Sandbox lifecycle:
  delete             Permanently delete a sandbox and its snapshots
  deletion           Read deletion operations
  extend             Set the auto-stop timer of a running sandbox (from now)
  fork               Copy a sandbox from its latest snapshot into a new one
  info               Show one sandbox: state, size, IP, desktop, TTL
  list               List your sandboxes (default: up or running)
  new                Create a sandbox and wait until it is ready
  recover            Power-cycle the machine of a sandbox that does not respond
  rename             Rename a sandbox
  resume             Resume a stopped sandbox from its last snapshot
  share              Let every org member use this sandbox
  stop               Stop a sandbox: snapshot its disk, then free the machine
  subdomain          Set the DNS label of a sandbox's hosted URLs (<label>-<port>.<domain>)
  unshare            Make an org sandbox private to you

Run and access:
  browser            Open a stream of only the sandbox's Chrome (no desktop)
  desktop            Open the sandbox desktop (noVNC) in your browser
  direct-ports       Show or set the raw TCP/UDP ports published on the node's direct host
  exec               Run a command in a sandbox and exit with its exit code
  forward            Forward a sandbox TCP port to this computer (or --reverse)
  history            List commands that ran in a sandbox, or print one's stored output
  host               Expose a sandbox port on a stable URL (also: host list|url|hide)
  ps                 List the processes in a sandbox, busiest first
  push               Copy a local directory into a sandbox (git-aware)
  run                Run one command in a new sandbox, then delete it
  scp                Copy files to or from a sandbox (ID:/path is the remote side)
  ssh                Open an SSH session in a sandbox, or run one command
  ssh-config         Write an OpenSSH config entry for a sandbox (for ssh, scp, VS Code, rsync)
  tunnel             Bridge stdin/stdout to a TCP port in the sandbox (ssh ProxyCommand)

Snapshots:
  snapshot           Save a sandbox as a named snapshot, and manage saved snapshots
  snapshots          List your saved snapshots, then the automatic snapshots of all your sandboxes (or of one)

Integrated AI agents:
  ai                 Manage AI credentials: profiles, keys, accounts, proxy, usage, blocking
  conversations      List the agent conversations of a sandbox, newest first
  events             Read the agent's prompts and responses on a sandbox
  interrupt          Stop running agent work (all conversations, or --convo)
  prompt             Send a prompt to the coding agent inside a sandbox and stream its work
  steer              Add a message to the agent's running turn without stopping it

Settings and administration:
  admin              Server administration: users, credits, invite codes, sandboxes, nodes
  agent-instructions Print an AGENTS.md section that teaches a coding agent to use crowdvm
  api-key            Create and manage API keys (cvm_...)
  ci                 GitHub Actions on CrowdVM: setup, problems, stats and last jobs
  completions        Print a shell completion script
  config             Show or change local settings (server URL, default profile)
  data-retention     Show or enable zero data retention
  env                Manage environments (repos, secrets, credentials new sandboxes start from)
  help               Help about any command
  nodes              List the machines lending compute, with their benchmark categories
  org                Show or switch the organization new sandboxes belong to
  repos              Repositories you can add to environments, and the git hosts behind them
  self-update        Install the newer CLI build the server offers
  shell-init         Print the shell function that keeps `current` per shell
  skill              Install the crowdvm skill for Claude Code, Codex and OpenCode on this machine
  telemetry          Send metrics and lifecycle events to an OTLP/HTTP endpoint
  version            Print the CLI version
  webhook            Register sandbox lifecycle webhooks

Flags:
      --api-url string   CrowdVM server URL (env CROWDVM_API_BASE)
  -h, --help             help for crowdvm
      --json             print JSON (one object per line) instead of text; on by default when piped
      --no-update        skip the check for a newer CLI on the server (env CROWDVM_NO_UPDATE=1)
      --org string       run against this organization (env CROWDVM_ORG)
      --profile string   config profile to use (env CROWDVM_PROFILE)
  -v, --version          version for crowdvm
```

## crowdvm admin

```text
Commands for administrators of the CrowdVM server (other accounts get 403 admin_required).

Usage:
  crowdvm admin [command]

Available Commands:
  credits         Grant or remove credits
  feedback        Read the feedback users sent with `crowdvm feedback`
  invite-codes    List signup invite codes
  merge           Move everything of USER (sandboxes, keys, credits...) into TARGET
  nodes           List the machines lending compute, with their benchmark categories
  policy          Show or change the limits of a wallet (user or org): credits, starts, sizes, TTL
  reset-password  Set a new password for an account
  sandboxes       List every account's sandboxes (newest 500)
  service-account Create a service account (optionally in --org) with an API key
  set-admin       Grant or remove administrator rights
  suspend         Suspend an account (signs it out everywhere)
  unsuspend       Reactivate a suspended account
  users           List accounts

Flags:
  -h, --help   help for admin
```

### crowdvm admin credits

```text
Grant or remove credits

Usage:
  crowdvm admin credits [command]

Available Commands:
  grant       Add credits to a wallet (a negative amount removes)

Flags:
  -h, --help   help for credits
```

### crowdvm admin credits grant

```text
Add credits to a wallet (a negative amount removes)

Usage:
  crowdvm admin credits grant AMOUNT (--user USER | --org ORG) [flags]

Examples:
  crowdvm admin credits grant 100 --user alice --reason "welcome"
  crowdvm admin credits grant 500 --org acme

Flags:
  -h, --help            help for grant
      --org string      organization id or name
      --reason string   ledger note
      --user string     user id or login
```

### crowdvm admin feedback

```text
Read the feedback users sent with `crowdvm feedback`

Usage:
  crowdvm admin feedback [flags]

Flags:
  -h, --help   help for feedback
```

### crowdvm admin invite-codes

```text
List signup invite codes

Usage:
  crowdvm admin invite-codes [flags]
  crowdvm admin invite-codes [command]

Aliases:
  invite-codes, invites

Available Commands:
  create      Create a signup invite code (shown once)
  revoke      Revoke an invite code

Flags:
  -h, --help   help for invite-codes
```

### crowdvm admin invite-codes create

```text
Create a signup invite code (shown once)

Usage:
  crowdvm admin invite-codes create [flags]

Flags:
  -h, --help           help for create
      --max-uses int   how many accounts may sign up with it (default 1)
      --note string    who it is for
      --ttl duration   lifetime, e.g. 168h (default: no expiry)
```

### crowdvm admin invite-codes revoke

```text
Revoke an invite code

Usage:
  crowdvm admin invite-codes revoke ID [flags]

Flags:
  -h, --help   help for revoke
```

### crowdvm admin merge

```text
Move everything of USER (sandboxes, keys, credits...) into TARGET

Usage:
  crowdvm admin merge USER --into TARGET [flags]

Flags:
  -h, --help          help for merge
      --into string   the account that keeps everything
  -y, --yes           do not ask for confirmation
```

### crowdvm admin nodes

```text
List the machines lending compute, with their benchmark categories

Usage:
  crowdvm admin nodes [flags]
  crowdvm admin nodes [command]

Available Commands:
  drain       Stop placing new sandboxes on a node (running ones keep running)
  info        Show one node: capacity, benchmark, sandboxes, earned credits
  join-token  Create a token for `crowdvm-node join` (admins)
  join-tokens List node join tokens (admins)
  remove      Remove a node (--force evicts it while it runs sandboxes)
  undrain     Place sandboxes on a drained node again
  update      Rename a node or replace its labels

Flags:
  -h, --help   help for nodes
```

### crowdvm admin nodes drain

```text
Stop placing new sandboxes on a node (running ones keep running)

Usage:
  crowdvm admin nodes drain NODE [flags]

Flags:
  -h, --help   help for drain
```

### crowdvm admin nodes info

```text
Show one node: capacity, benchmark, sandboxes, earned credits

Usage:
  crowdvm admin nodes info NODE [flags]

Flags:
  -h, --help   help for info
```

### crowdvm admin nodes join-token

```text
Create a token for `crowdvm-node join` (admins)

Usage:
  crowdvm admin nodes join-token [flags]

Flags:
  -h, --help           help for join-token
      --max-uses int   how many nodes may join with it
      --name string    label for the token
      --ttl int        seconds until the token expires
```

### crowdvm admin nodes join-tokens

```text
List node join tokens (admins)

Usage:
  crowdvm admin nodes join-tokens [flags]
  crowdvm admin nodes join-tokens [command]

Available Commands:
  revoke      Revoke a join token

Flags:
  -h, --help   help for join-tokens
```

### crowdvm admin nodes join-tokens revoke

```text
Revoke a join token

Usage:
  crowdvm admin nodes join-tokens revoke TOKEN_ID [flags]

Flags:
  -h, --help   help for revoke
```

### crowdvm admin nodes remove

```text
Remove a node (--force evicts it while it runs sandboxes)

Usage:
  crowdvm admin nodes remove NODE [flags]

Aliases:
  remove, evict, rm

Flags:
      --force   remove even while it runs sandboxes
  -h, --help    help for remove
  -y, --yes     do not ask for confirmation
```

### crowdvm admin nodes undrain

```text
Place sandboxes on a drained node again

Usage:
  crowdvm admin nodes undrain NODE [flags]

Aliases:
  undrain, enable

Flags:
  -h, --help   help for undrain
```

### crowdvm admin nodes update

```text
Rename a node or replace its labels

Usage:
  crowdvm admin nodes update NODE [flags]

Examples:
  crowdvm nodes update n_123 --name lab-gpu --label gpu=a100 --label room=2

Flags:
  -h, --help                help for update
      --label stringArray   label KEY=VALUE (repeatable; replaces all labels; --label "" removes them all)
      --name string         new name
```

### crowdvm admin policy

```text
Show the restriction policy of a wallet (a user id/login or an org id/name), or
change fields with --set KEY=VALUE (VALUE null = server default). Keys:
monthlyCredits, maxActive, startsPerMinute, startsPerHour, startsPerDay,
maxTtlSeconds, allowedSizes (comma-separated), savedSnapshotAllowance,
creditCapMonthly, lowCreditAlert.

Usage:
  crowdvm admin policy WALLET [flags]

Examples:
  crowdvm admin policy alice --set maxActive=3 --set allowedSizes=small,default

Flags:
  -h, --help              help for policy
      --set stringArray   KEY=VALUE to change (repeatable; null = default)
```

### crowdvm admin reset-password

```text
Set a new password for an account

Usage:
  crowdvm admin reset-password USER [flags]

Flags:
  -h, --help             help for reset-password
      --password-stdin   read the password from stdin
```

### crowdvm admin sandboxes

```text
List every account's sandboxes (newest 500)

Usage:
  crowdvm admin sandboxes [flags]

Flags:
  -h, --help           help for sandboxes
      --node string    only sandboxes on this node
      --state string   only this API state (e.g. running, archived, error)
```

### crowdvm admin service-account

```text
Create a service account (optionally in --org) with an API key

Usage:
  crowdvm admin service-account NAME [flags]

Flags:
  -h, --help            help for service-account
      --in-org string   organization the account belongs to
      --preset string   key preset: read-only, full-sandbox, ci or admin
      --ttl string      key lifetime, e.g. 90d or never
```

### crowdvm admin set-admin

```text
Grant or remove administrator rights

Usage:
  crowdvm admin set-admin USER on|off [flags]

Flags:
  -h, --help   help for set-admin
```

### crowdvm admin suspend

```text
Suspend an account (signs it out everywhere)

Usage:
  crowdvm admin suspend USER [flags]

Flags:
  -h, --help   help for suspend
```

### crowdvm admin unsuspend

```text
Reactivate a suspended account

Usage:
  crowdvm admin unsuspend USER [flags]

Flags:
  -h, --help   help for unsuspend
```

### crowdvm admin users

```text
List accounts

Usage:
  crowdvm admin users [flags]
  crowdvm admin users [command]

Available Commands:
  create      Create an account (password from stdin with --password-stdin)

Flags:
  -h, --help   help for users
```

### crowdvm admin users create

```text
Create an account (password from stdin with --password-stdin)

Usage:
  crowdvm admin users create USERNAME [flags]

Flags:
      --admin                 make the account an administrator
      --credits float         starting credits
      --display-name string   display name
      --email string          email address
  -h, --help                  help for create
      --password-stdin        read the password from stdin
```

## crowdvm agent-instructions

```text
Print a short guide for coding agents (Claude Code, Codex, Cursor, ...).
Append it to your project's AGENTS.md or CLAUDE.md:

  crowdvm agent-instructions >> AGENTS.md

Usage:
  crowdvm agent-instructions [flags]

Aliases:
  agent-instructions, agents-md

Flags:
  -h, --help   help for agent-instructions
```

## crowdvm ai

```text
Manage AI credentials: profiles, keys, accounts, proxy, usage, blocking

Usage:
  crowdvm ai [command]

Available Commands:
  accounts       List the accounts of a credential (the one in use is marked)
  add-account    Add another API key to switch to (stdin when VALUE is omitted or -)
  block          Refuse the sandbox's AI placeholders
  forget-account Delete an account of a credential
  profile        Create and change credential profiles
  profiles       List the credential profiles you can use
  proxy          Show or switch the AI credentials proxy
  switch         Switch a credential to another account (number, id or label)
  unblock        Accept the sandbox's AI placeholders again
  usage          Proxied AI usage: per sandbox, or per day and model for one sandbox

Flags:
  -h, --help   help for ai
```

### crowdvm ai accounts

```text
List the accounts of a credential (the one in use is marked)

Usage:
  crowdvm ai accounts CREDENTIAL [flags]

Flags:
  -h, --help             help for accounts
      --profile string   credential profile (default: default)
```

### crowdvm ai add-account

```text
Add another API key to switch to (stdin when VALUE is omitted or -)

Usage:
  crowdvm ai add-account CREDENTIAL [VALUE] [flags]

Flags:
  -h, --help             help for add-account
      --profile string   credential profile (default: default)
```

### crowdvm ai block

```text
Refuse the sandbox's AI placeholders

Usage:
  crowdvm ai block ID [flags]

Flags:
  -h, --help   help for block
```

### crowdvm ai forget-account

```text
Delete an account of a credential

Usage:
  crowdvm ai forget-account CREDENTIAL ACCOUNT [flags]

Flags:
  -h, --help             help for forget-account
      --profile string   credential profile (default: default)
```

### crowdvm ai profile

```text
Create and change credential profiles

Usage:
  crowdvm ai profile [command]

Available Commands:
  access      Show or set which org members can use a profile
  default     Make a profile its owner's default
  new         Create a profile (owned by --org when given)
  rename      Rename a profile
  rm          Delete a profile
  rm-api      Remove a custom API
  rm-key      Remove an API key or sign-in
  set-api     Set a custom API (sandboxes get NAME and NAME_BASE_URL)
  set-key     Set an API key, e.g. anthropic-api-key (stdin when VALUE is omitted)
  sign-in     Paste a subscription sign-in token, e.g. claude-subscription (stdin when TOKEN is omitted)

Flags:
  -h, --help   help for profile
```

### crowdvm ai profile access

```text
Show or set which org members can use a profile

Usage:
  crowdvm ai profile access PROFILE [flags]

Flags:
      --add string      add one member
      --everyone        every member can use it
  -h, --help            help for access
      --only string     only owners, admins and these comma-separated emails
      --remove string   remove one member
```

### crowdvm ai profile default

```text
Make a profile its owner's default

Usage:
  crowdvm ai profile default PROFILE [flags]

Flags:
  -h, --help   help for default
```

### crowdvm ai profile new

```text
Create a profile (owned by --org when given)

Usage:
  crowdvm ai profile new NAME [flags]

Flags:
      --copy-from string   copy credentials from this profile
  -h, --help               help for new
```

### crowdvm ai profile rename

```text
Rename a profile

Usage:
  crowdvm ai profile rename PROFILE NEW_NAME [flags]

Flags:
  -h, --help   help for rename
```

### crowdvm ai profile rm

```text
Delete a profile

Usage:
  crowdvm ai profile rm PROFILE [flags]

Flags:
  -h, --help   help for rm
```

### crowdvm ai profile rm-api

```text
Remove a custom API

Usage:
  crowdvm ai profile rm-api PROFILE NAME [flags]

Flags:
  -h, --help   help for rm-api
```

### crowdvm ai profile rm-key

```text
Remove an API key or sign-in

Usage:
  crowdvm ai profile rm-key PROFILE CREDENTIAL [flags]

Flags:
  -h, --help   help for rm-key
```

### crowdvm ai profile set-api

```text
Set a custom API (sandboxes get NAME and NAME_BASE_URL)

Usage:
  crowdvm ai profile set-api PROFILE NAME --base-url URL [--value V] [flags]

Flags:
      --base-url string   API base URL
  -h, --help              help for set-api
      --value string      secret value (default: stdin)
```

### crowdvm ai profile set-key

```text
Set an API key, e.g. anthropic-api-key (stdin when VALUE is omitted)

Usage:
  crowdvm ai profile set-key PROFILE CREDENTIAL [VALUE] [flags]

Flags:
  -h, --help   help for set-key
```

### crowdvm ai profile sign-in

```text
Store a subscription sign-in (Claude, ChatGPT, ...) in a credential profile.
Get the token on a computer where you are signed in (for Claude Code:
'claude setup-token'), then paste it here.

Usage:
  crowdvm ai profile sign-in PROFILE CREDENTIAL [TOKEN] [flags]

Flags:
      --access-key-id string   Bedrock IAM access key id; TOKEN is then the secret access key
      --account-id string      provider account id, when the provider needs one
  -h, --help                   help for sign-in
      --label string           label of this account
      --region string          region, when the provider needs one
      --session-token string   Bedrock IAM session token (optional, with --access-key-id)
```

### crowdvm ai profiles

```text
List the credential profiles you can use

Usage:
  crowdvm ai profiles [flags]

Flags:
  -h, --help       help for profiles
      --org-only   only organization profiles
```

### crowdvm ai proxy

```text
Show or switch the AI credentials proxy

Usage:
  crowdvm ai proxy [on|off] [flags]

Flags:
  -h, --help   help for proxy
```

### crowdvm ai switch

```text
Switch a credential to another account (number, id or label)

Usage:
  crowdvm ai switch CREDENTIAL ACCOUNT [flags]

Flags:
  -h, --help             help for switch
      --profile string   credential profile (default: default)
```

### crowdvm ai unblock

```text
Accept the sandbox's AI placeholders again

Usage:
  crowdvm ai unblock ID [flags]

Flags:
  -h, --help   help for unblock
```

### crowdvm ai usage

```text
Proxied AI usage: per sandbox, or per day and model for one sandbox

Usage:
  crowdvm ai usage [ID] [flags]

Flags:
      --days int   days to cover (default 30)
  -h, --help       help for usage
```

## crowdvm api-key

```text
Create and manage API keys (cvm_...)

Usage:
  crowdvm api-key [command]

Aliases:
  api-key, api-keys

Available Commands:
  create      Create a scoped API key (the secret is shown once)
  list        List keys: scope, expiry, last use
  revoke      Revoke a key
  rotate      Replace a key's secret (the old one stops working)
  usage       30-day requests and live resources of a key

Flags:
  -h, --help   help for api-key
```

### crowdvm api-key create

```text
Create a scoped API key (the secret is shown once)

Usage:
  crowdvm api-key create NAME [flags]

Examples:
  crowdvm api-key create ci --ttl 90d --preset ci
  crowdvm api-key create agent --ttl 7d --sandbox bx_123 --actions sandbox.read,exec

Flags:
      --actions string        comma-separated actions (instead of --preset)
      --env stringArray       limit to this environment id (repeatable)
  -h, --help                  help for create
      --preset string         read-only, full-sandbox, ci or admin
      --sandbox stringArray   limit to this sandbox (repeatable)
      --ttl string            lifetime: 90d, 24h, seconds, up to 365d, or never (default "90d")
```

### crowdvm api-key list

```text
List keys: scope, expiry, last use

Usage:
  crowdvm api-key list [flags]

Aliases:
  list, ls

Flags:
      --all    also per-sandbox machine keys
  -h, --help   help for list
```

### crowdvm api-key revoke

```text
Revoke a key

Usage:
  crowdvm api-key revoke ID [flags]

Flags:
  -h, --help   help for revoke
```

### crowdvm api-key rotate

```text
Replace a key's secret (the old one stops working)

Usage:
  crowdvm api-key rotate ID [flags]

Flags:
  -h, --help   help for rotate
```

### crowdvm api-key usage

```text
30-day requests and live resources of a key

Usage:
  crowdvm api-key usage ID [flags]

Flags:
  -h, --help      help for usage
      --verbose   list the resources it created
```

## crowdvm billing

```text
Show the credit wallet in scope (personal, or --org). CrowdVM replaces
Boat's plans and payments with credits: sandboxes spend them at their size
multiplier, nodes earn them, administrators grant them and set the limits.

  crowdvm billing                   balance, month spend, grace, limits
  crowdvm billing --all             also the ledger and every limit
  crowdvm billing plan              the limits (policy) that replace plans
  crowdvm billing extra-starts on   pay credits for starts past a start limit

Usage:
  crowdvm billing [flags]
  crowdvm billing [command]

Available Commands:
  extra-starts Pay credits for starts past a start limit (instead of being refused)
  plan         Show the limits that replace plans (set by administrators)
  status       Same as crowdvm billing

Flags:
      --all       also show the ledger and every limit
      --details   same as --all
  -h, --help      help for billing
```

### crowdvm billing extra-starts

```text
Pay credits for starts past a start limit (instead of being refused)

Usage:
  crowdvm billing extra-starts on|off [flags]

Flags:
  -h, --help                 help for extra-starts
      --monthly-cap string   credits extra starts may use per month (or none)
      --yes                  accepted for Boat compatibility (no confirmation is asked)
```

### crowdvm billing plan

```text
Show the limits that replace plans (set by administrators)

Usage:
  crowdvm billing plan [flags]

Flags:
  -h, --help            help for plan
      --move-personal   move your personal sandboxes into the organization in scope (as org migrate)
```

### crowdvm billing status

```text
Same as crowdvm billing

Usage:
  crowdvm billing status [flags]

Flags:
      --all    also show the ledger and every limit
  -h, --help   help for status
```

## crowdvm browser

```text
Print and open a URL that streams only Chrome from the running sandbox
(tabs and address bar, no desktop). Running it again keeps the live stream.
The URL carries a token: keep it secret.

--profile picks the Chrome user data dir: an absolute path on the sandbox,
outside /home/user and system directories; it is created for you and kept in
snapshots and forks. (Here --profile is the Chrome profile; choose a config
profile with CROWDVM_PROFILE.)

Usage:
  crowdvm browser ID [flags]

Examples:
  crowdvm browser bx_f7k2q9hd
  crowdvm browser bx_f7k2q9hd --profile /data/chrome-profile

Flags:
  -h, --help             help for browser
      --no-open          print the URL only
      --profile string   Chrome user data dir on the sandbox (absolute path)
```

## crowdvm ci

```text
Show the CI setup steps, problems, 7-day stats and the last jobs. Jobs run
in sandboxes when a workflow uses runs-on: crowdvm (or crowdvm-small, -large,
-xlarge). Connect GitHub first with 'crowdvm ci connect --token <token>'.

Usage:
  crowdvm ci [flags]
  crowdvm ci [command]

Examples:
  crowdvm ci
  crowdvm ci --repo acme/api --state failed
  crowdvm ci repos
  crowdvm ci job <job id>
  crowdvm ci logs <job id> --follow

Available Commands:
  connect     Link a GitHub account or organization with a token (jobs bill the wallet in scope)
  job         One job: wait, run time, cost, sandbox, error
  logs        Print the GitHub log of a job (--follow while it runs)
  public      Show or set whether public repositories of a linked account run on CrowdVM
  repos       Repositories the linked GitHub accounts can see, and their status
  unlink      Remove a linked GitHub account

Flags:
      --account string   only this linked GitHub account
      --all              show every job, not just the last 10
  -h, --help             help for ci
      --limit int        show at most this many jobs (1-500)
      --repo string      only this repository (owner/name)
      --state string     all, active or failed
```

### crowdvm ci connect

```text
Link a GitHub user or organization so its workflows can run on CrowdVM.
CrowdVM has no GitHub App: give a token (fine-grained: Actions read,
Administration read/write, Metadata read, plus Members read for an
organization; classic: repo, plus admin:org for organizations). An
organization can only be linked with a token of one of its admins. Jobs bill the wallet in scope (--org or 'crowdvm org switch').
Without --yes the link stays pending until you confirm it.

Usage:
  crowdvm ci connect [flags]

Examples:
  crowdvm ci connect --token "$GH_TOKEN" --yes
  crowdvm --org acme ci connect --account acme-github --token "$GH_TOKEN" --yes
  crowdvm ci connect --link cil_123 --yes      # confirm a pending link

Flags:
      --account string          GitHub account or organization to link (default: the token's user)
      --api-url-github string   GitHub-compatible API URL (GitHub Enterprise)
  -h, --help                    help for connect
      --link string             confirm this pending link id
      --token string            GitHub token (- or empty: read stdin)
  -y, --yes                     link now, without a pending confirmation step
```

### crowdvm ci job

```text
One job: wait, run time, cost, sandbox, error

Usage:
  crowdvm ci job JOB_ID [flags]

Flags:
  -h, --help   help for job
```

### crowdvm ci logs

```text
Print the GitHub log of a job (--follow while it runs)

Usage:
  crowdvm ci logs JOB_ID [flags]

Flags:
  -f, --follow   keep printing while the job runs
  -h, --help     help for logs
```

### crowdvm ci public

```text
Show or set whether public repositories of a linked account run on CrowdVM

Usage:
  crowdvm ci public ACCOUNT [on|off] [flags]

Examples:
  crowdvm ci public my-account on --users "me, @a-teammate"
  crowdvm ci public my-account
  crowdvm ci public my-account off

Flags:
      --anyone         clear the user list: every run uses CrowdVM
      --dry-run        validate without saving
  -h, --help           help for public
      --users string   GitHub users whose runs may use CrowdVM (comma-separated)
```

### crowdvm ci repos

```text
Repositories the linked GitHub accounts can see, and their status

Usage:
  crowdvm ci repos [flags]

Flags:
  -h, --help   help for repos
```

### crowdvm ci unlink

```text
Remove a linked GitHub account

Usage:
  crowdvm ci unlink LINK_ID [flags]

Flags:
  -h, --help   help for unlink
```

## crowdvm completions

```text
Print a shell completion script

Usage:
  crowdvm completions [bash|zsh|fish|powershell] [flags]

Flags:
  -h, --help   help for completions
```

## crowdvm config

```text
Show or change the settings in the config file. Keys:
  server    the CrowdVM server URL of the active profile
  profile   the profile used when --profile and CROWDVM_PROFILE are not set
  current   the "current" sandbox (of this shell, with the shell function)
  user      the signed-in login (read-only)

Usage:
  crowdvm config [flags]
  crowdvm config [command]

Examples:
  crowdvm config set server http://10.0.0.5:8420
  crowdvm config get server
  crowdvm --profile work config set server http://work-box:8420

Available Commands:
  get         Print one setting
  list        Show all settings
  path        Print the config file path
  set         Change one setting
  unset       Clear one setting

Flags:
  -h, --help   help for config
```

### crowdvm config get

```text
Print one setting

Usage:
  crowdvm config get KEY [flags]

Flags:
  -h, --help   help for get
```

### crowdvm config list

```text
Show all settings

Usage:
  crowdvm config list [flags]

Aliases:
  list, ls

Flags:
  -h, --help   help for list
```

### crowdvm config path

```text
Print the config file path

Usage:
  crowdvm config path [flags]

Flags:
  -h, --help   help for path
```

### crowdvm config set

```text
Change one setting

Usage:
  crowdvm config set KEY VALUE [flags]

Flags:
  -h, --help   help for set
```

### crowdvm config unset

```text
Clear one setting

Usage:
  crowdvm config unset KEY [flags]

Flags:
  -h, --help   help for unset
```

## crowdvm conversations

```text
List the agent conversations of a sandbox, newest first

Usage:
  crowdvm conversations [ID] [flags]

Aliases:
  conversations, convos

Flags:
  -h, --help   help for conversations
```

## crowdvm credits

```text
Show the credit balance in scope (personal, or --org). Running sandboxes
spend credits at their size multiplier; nodes earn credits for the VM time
they host. Admins grant credits on the server.

Usage:
  crowdvm credits [flags]
  crowdvm credits [command]

Aliases:
  credits, wallet

Available Commands:
  transfer    Move credits to another wallet (user or org)

Flags:
  -h, --help   help for credits
```

### crowdvm credits transfer

```text
Move credits to another wallet (user or org)

Usage:
  crowdvm credits transfer --to WALLET --amount N [flags]

Flags:
      --amount float   credits to move
      --from string    source wallet (default: the one in scope)
  -h, --help           help for transfer
      --to string      target wallet: user or org id/name
```

## crowdvm dashboard

```text
Open the server's web dashboard, signed in (one-time 15-minute link)

Usage:
  crowdvm dashboard [flags]

Flags:
  -h, --help      help for dashboard
      --no-open   print the URL only
```

## crowdvm data-retention

```text
Show or enable zero data retention

Usage:
  crowdvm data-retention [flags]
  crowdvm data-retention [command]

Available Commands:
  enable      Delete every sandbox permanently when it stops (irreversible)
  status      Show the policy

Flags:
  -h, --help   help for data-retention
```

### crowdvm data-retention enable

```text
Delete every sandbox permanently when it stops (irreversible)

Usage:
  crowdvm data-retention enable [flags]

Flags:
  -h, --help   help for enable
  -y, --yes    do not ask for confirmation
```

### crowdvm data-retention status

```text
Show the policy

Usage:
  crowdvm data-retention status [flags]

Flags:
  -h, --help   help for status
```

## crowdvm delete

```text
Permanently delete a sandbox and the snapshots only it uses. This cannot be
undone; to keep the data, use 'crowdvm stop'. Scripts must pass --yes.

Usage:
  crowdvm delete ID [flags]

Aliases:
  delete, rm

Flags:
  -h, --help      help for delete
      --no-wait   do not follow the deletion operation
  -y, --yes       do not ask for confirmation
```

## crowdvm deletion

```text
Read deletion operations

Usage:
  crowdvm deletion [command]

Available Commands:
  status      Show a deletion operation: pending, processing, blocked or completed

Flags:
  -h, --help   help for deletion
```

### crowdvm deletion status

```text
Show a deletion operation: pending, processing, blocked or completed

Usage:
  crowdvm deletion status OPERATION_ID [flags]

Flags:
  -h, --help   help for status
```

## crowdvm desktop

```text
Print and open the desktop URL. It carries a token: treat it as a secret.

Usage:
  crowdvm desktop ID [flags]

Flags:
  -h, --help      help for desktop
      --no-open   print the URL only
      --public    allow access without a signed-in session
```

## crowdvm direct-ports

```text
Publish guest ports raw (TCP or UDP) on the LAN/Tailscale address of a node
started with --direct-host, like Boat's "raw TCP or UDP ports" on the sandbox IP.
Without ports it shows the published set (also the ports of host --public).
Ports given replace the earlier list; --clear removes them. The guest firewall
is opened for each port unless --no-firewall.

Usage:
  crowdvm direct-ports ID [PORT[/udp]...] [flags]

Examples:
  crowdvm direct-ports bx_f7k2q9hd
  crowdvm direct-ports bx_f7k2q9hd 27015/udp 25565
  crowdvm direct-ports bx_f7k2q9hd --clear

Flags:
      --clear         remove the ports asked for
  -h, --help          help for direct-ports
      --no-firewall   do not open the guest firewall for the ports
```

## crowdvm env

```text
Environments hold what a sandbox gets at start: repositories, variables,
secret files and credentials. Each change makes a new immutable version; running
sandboxes stay on theirs until 'crowdvm env upgrade'. Add --org to work on an
organization's environments. Not the same as 'crowdvm new --env KEY=VALUE'.

Usage:
  crowdvm env [command]

Available Commands:
  access      Show or limit which org members can use an environment
  add-repo    Clone a repository into new sandboxes
  default     Make it the environment new sandboxes use
  info        Show one environment in full
  list        List environments
  new         Create an environment
  rename      Rename an environment
  rm          Delete an environment (pinned sandboxes keep running)
  rm-file     Remove a secret file
  rm-repo     Stop cloning a repository
  rm-var      Remove a variable
  set         Change what the environment injects
  set-file    Add a secret file (contents from --from or stdin)
  set-var     Set a variable (value from stdin when omitted)
  upgrade     Move this environment's sandboxes (or --sandbox ones) to its latest version

Flags:
  -h, --help   help for env
```

### crowdvm env access

```text
Show or limit which org members can use an environment

Usage:
  crowdvm env access NAME [flags]

Flags:
      --add string      add one member
      --everyone        every member can use it
  -h, --help            help for access
      --only string     only owners, admins and these comma-separated emails
      --remove string   remove one member
```

### crowdvm env add-repo

```text
Clone a repository into new sandboxes

Usage:
  crowdvm env add-repo NAME REPO [flags]

Flags:
      --branch string   base branch (default main)
  -h, --help            help for add-repo
```

### crowdvm env default

```text
Make it the environment new sandboxes use

Usage:
  crowdvm env default NAME [flags]

Flags:
  -h, --help   help for default
```

### crowdvm env info

```text
Show one environment in full

Usage:
  crowdvm env info NAME [flags]

Flags:
  -h, --help   help for info
```

### crowdvm env list

```text
List environments

Usage:
  crowdvm env list [flags]

Aliases:
  list, ls

Flags:
  -h, --help   help for list
```

### crowdvm env new

```text
Create an environment

Usage:
  crowdvm env new NAME [flags]

Flags:
  -h, --help   help for new
```

### crowdvm env rename

```text
Rename an environment

Usage:
  crowdvm env rename NAME NEW_NAME [flags]

Flags:
  -h, --help   help for rename
```

### crowdvm env rm

```text
Delete an environment (pinned sandboxes keep running)

Usage:
  crowdvm env rm NAME [flags]

Flags:
  -h, --help   help for rm
```

### crowdvm env rm-file

```text
Remove a secret file

Usage:
  crowdvm env rm-file NAME PATH [flags]

Flags:
  -h, --help   help for rm-file
```

### crowdvm env rm-repo

```text
Stop cloning a repository

Usage:
  crowdvm env rm-repo NAME REPO [flags]

Flags:
  -h, --help   help for rm-repo
```

### crowdvm env rm-var

```text
Remove a variable

Usage:
  crowdvm env rm-var NAME KEY [flags]

Flags:
  -h, --help   help for rm-var
```

### crowdvm env set

```text
Change what the environment injects

Usage:
  crowdvm env set NAME [flags]

Examples:
  crowdvm env set base --safe-for-third-parties true
  crowdvm env set base --github true --secrets false

Flags:
      --agents-credentials string       pass agent logins (Claude, Codex, ...) (true/false)
      --credential-profile string       credential profile name or id (default = owner's default)
      --crowdvm-credentials string      give sandboxes CLI credentials to manage themselves (true/false)
      --github string                   pass GitHub access (true/false)
  -h, --help                            help for set
      --proxy-ai-credentials string     with safe-for-third-parties: pass AI credentials as proxy placeholders
      --safe-for-third-parties string   true withholds every secret (master switch)
      --secrets string                  pass variables and secret files (true/false)
```

### crowdvm env set-file

```text
Add a secret file to an environment. PATH is relative to /home/user (for example backend/.env, or a repository folder followed by a path in it). Contents come from --from or stdin.

Usage:
  crowdvm env set-file NAME PATH [flags]

Flags:
      --from string   local file to read
  -h, --help          help for set-file
```

### crowdvm env set-var

```text
Set a variable (value from stdin when omitted)

Usage:
  crowdvm env set-var NAME KEY[=VALUE] [flags]

Flags:
  -h, --help   help for set-var
```

### crowdvm env upgrade

```text
Move this environment's sandboxes (or --sandbox ones) to its latest version

Usage:
  crowdvm env upgrade NAME [flags]

Flags:
  -h, --help                  help for upgrade
      --sandbox stringArray   only this sandbox (repeatable)
```

## crowdvm events

```text
Read the agent's prompts and responses on a sandbox

Usage:
  crowdvm events [ID] [flags]

Flags:
      --conversation stringArray   alias of --convo
      --convo stringArray          only this conversation id (or current); repeatable
  -f, --follow                     keep printing new events until interrupted
  -h, --help                       help for events
```

## crowdvm exec

```text
Run a command through the API (no SSH). One argument is a shell command
line; more arguments run as argv. The default timeout is 30 seconds; above 600
or with 0 (no limit) the command runs in the background and its output streams
live, surviving network cuts. Exit code 124 means the timeout hit. ID may also
be the sandbox's name; without one the current sandbox runs the command.

Usage:
  crowdvm exec [ID] [flags] [--] COMMAND...

Examples:
  crowdvm exec bx_f7k2q9hd "npm test"
  crowdvm exec bx_f7k2q9hd --cwd my-repo --timeout 0 -- make -j16 test
  pid=$(crowdvm exec bx_f7k2q9hd --detach "npm run dev" | jq -r .processId)
  crowdvm exec bx_f7k2q9hd --status "$pid"
  crowdvm exec bx_f7k2q9hd --attach "$pid"

Flags:
      --attach int    follow a detached process's output until it ends
      --cwd string    working directory, relative to /home/user
      --detach        run in the background and print the process id
  -h, --help          help for exec
      --kill int      stop a detached process and its children
      --push string   push this local directory first and run in it
      --status int    show status and log tails of a detached process
      --stream        print output as it is written
      --timeout int   stop after this many seconds and exit 124; 0 = no limit (default 30)
```

## crowdvm extend

```text
Set the auto-stop timer of a running sandbox (from now)

Usage:
  crowdvm extend ID [flags]

Examples:
  crowdvm extend bx_f7k2q9hd --hours 12
  crowdvm extend bx_f7k2q9hd --no-auto-stop

Flags:
  -h, --help           help for extend
      --hours float    hours from now
      --no-auto-stop   keep running until stopped
      --ttl int        seconds from now
```

## crowdvm feedback

```text
Send feedback to the people who run this CrowdVM server (administrators
read it with 'crowdvm admin feedback'). Without MESSAGE the text is read from
stdin. You can send 5 per minute and 30 per day.

Usage:
  crowdvm feedback [MESSAGE] [flags]

Examples:
  crowdvm feedback "fork is slow on large disks"
  cat notes.txt | crowdvm feedback

Flags:
  -h, --help   help for feedback
```

## crowdvm fork

```text
Copy a sandbox from its latest snapshot into a new one

Usage:
  crowdvm fork ID [flags]

Flags:
      --category stringArray     placement: minimum benchmark level DIM=LEVEL, e.g. cpu=high (low|medium|high|extreme; repeatable)
  -e, --env stringArray          per-sandbox variable KEY=VALUE (repeatable)
      --environment string       environment to start from (see: crowdvm env list)
      --fail-fast                fail with no_ready_machine instead of waiting for capacity
      --gpu                      placement: run on a node that passes a GPU through (VFIO); the sandbox gets that GPU to itself
  -h, --help                     help for fork
      --min-tier string          placement: minimum node tier: bronze, silver, gold or platinum
      --name string              display name of the fork
      --nested                   placement: run on a node with nested virtualization (KVM inside the sandbox)
      --no-auto-stop             never stop automatically
      --no-env                   give the sandbox none of your secrets
      --no-monitoring            keep no command output, process list or metrics
      --no-snapshots             never snapshot; stopping erases the sandbox
      --node string              placement: run on this node (id or name, see: crowdvm nodes)
      --node-label stringArray   placement: node label KEY=VALUE that must match (repeatable)
      --ttl int                  seconds until auto-stop, counted from start (default 3600)
      --type string              machine size: small, default, large or xlarge
      --wait                     wait until the fork is ready
```

## crowdvm forward

```text
Forward a sandbox TCP port to this computer (or --reverse)

Usage:
  crowdvm forward ID [flags]

Aliases:
  forward, port-forward

Examples:
  crowdvm forward bx_f7k2q9hd --remote 5432
  crowdvm forward bx_f7k2q9hd --remote 3000 --local 8080 --bind 0.0.0.0
  crowdvm forward bx_f7k2q9hd --reverse --local 7777   # your :7777 at 127.0.0.1:7777 in the sandbox

Flags:
      --bind string   bind address (in the sandbox with --reverse) (default "127.0.0.1")
  -h, --help          help for forward
      --local int     port on this computer (default: same as --remote)
      --remote int    port in the sandbox
      --reverse       make a port on this computer reachable inside the sandbox
```

## crowdvm history

```text
List commands that ran in a sandbox, or print one's stored output

Usage:
  crowdvm history ID [COMMAND_ID] [flags]

Flags:
  -f, --follow        keep printing new output until the command ends
  -h, --help          help for history
      --limit int     how many commands to list (max 100) (default 20)
      --mode string   only these modes: commands, process, sync, stream, detached
      --stderr        print stderr instead of stdout
```

## crowdvm host

```text
Expose a service in the sandbox on a URL served by the CrowdVM server
(http://<id>-<port>.<domain> or http://<server>/p/<id>/<port>/). Private URLs
carry ?_token=; --public drops the token. Start the service first, e.g.
crowdvm exec ID --detach "npm run dev". Running it again returns the same URL.

Without ID the current sandbox is used (inside a sandbox: the sandbox itself).

Usage:
  crowdvm host [ID] PORT [flags]
  crowdvm host [command]

Examples:
  crowdvm host bx_f7k2q9hd 3000
  crowdvm host bx_f7k2q9hd 3000 --public --title "Login preview"
  crowdvm host list bx_f7k2q9hd
  BACKEND_URL=$(crowdvm host url bx_f7k2q9hd 3001)
  crowdvm host hide bx_f7k2q9hd 3000

Available Commands:
  hide        Take a port's URL down (the token is kept)
  list        List the hosted ports (tokens are not printed; see host url)
  url         Print the full URL of a port (hosting it first if needed)

Flags:
  -h, --help           help for host
      --localhost      the service listens on localhost only
      --private        require the URL token (default)
      --public         no token needed to open the URL
      --title string   display title
```

### crowdvm host hide

```text
Take a port's URL down (the token is kept)

Usage:
  crowdvm host hide [ID] PORT [flags]

Flags:
  -h, --help   help for hide
```

### crowdvm host list

```text
List the hosted ports (tokens are not printed; see host url)

Usage:
  crowdvm host list [ID] [flags]

Aliases:
  list, ls

Flags:
      --all    include hidden ports
  -h, --help   help for list
```

### crowdvm host url

```text
Print the full URL of a port (hosting it first if needed)

Usage:
  crowdvm host url [ID] PORT [flags]

Flags:
  -h, --help        help for url
      --localhost   the service listens on localhost only
      --public      host the port without a token first
```

### crowdvm host current

```text
Expose a service in the sandbox on a URL served by the CrowdVM server
(http://<id>-<port>.<domain> or http://<server>/p/<id>/<port>/). Private URLs
carry ?_token=; --public drops the token. Start the service first, e.g.
crowdvm exec ID --detach "npm run dev". Running it again returns the same URL.

Without ID the current sandbox is used (inside a sandbox: the sandbox itself).

Usage:
  crowdvm host [ID] PORT [flags]
  crowdvm host [command]

Examples:
  crowdvm host bx_f7k2q9hd 3000
  crowdvm host bx_f7k2q9hd 3000 --public --title "Login preview"
  crowdvm host list bx_f7k2q9hd
  BACKEND_URL=$(crowdvm host url bx_f7k2q9hd 3001)
  crowdvm host hide bx_f7k2q9hd 3000

Available Commands:
  hide        Take a port's URL down (the token is kept)
  list        List the hosted ports (tokens are not printed; see host url)
  url         Print the full URL of a port (hosting it first if needed)

Flags:
  -h, --help           help for host
      --localhost      the service listens on localhost only
      --private        require the URL token (default)
      --public         no token needed to open the URL
      --title string   display title
```

### crowdvm host self

```text
Expose a service in the sandbox on a URL served by the CrowdVM server
(http://<id>-<port>.<domain> or http://<server>/p/<id>/<port>/). Private URLs
carry ?_token=; --public drops the token. Start the service first, e.g.
crowdvm exec ID --detach "npm run dev". Running it again returns the same URL.

Without ID the current sandbox is used (inside a sandbox: the sandbox itself).

Usage:
  crowdvm host [ID] PORT [flags]
  crowdvm host [command]

Examples:
  crowdvm host bx_f7k2q9hd 3000
  crowdvm host bx_f7k2q9hd 3000 --public --title "Login preview"
  crowdvm host list bx_f7k2q9hd
  BACKEND_URL=$(crowdvm host url bx_f7k2q9hd 3001)
  crowdvm host hide bx_f7k2q9hd 3000

Available Commands:
  hide        Take a port's URL down (the token is kept)
  list        List the hosted ports (tokens are not printed; see host url)
  url         Print the full URL of a port (hosting it first if needed)

Flags:
  -h, --help           help for host
      --localhost      the service listens on localhost only
      --private        require the URL token (default)
      --public         no token needed to open the URL
      --title string   display title
```

## crowdvm info

```text
Show one sandbox: state, size, IP, desktop, TTL

Usage:
  crowdvm info [ID] [flags]

Flags:
  -h, --help   help for info
```

## crowdvm interrupt

```text
Stop running agent work (all conversations, or --convo)

Usage:
  crowdvm interrupt [ID] [flags]

Flags:
      --conversation string   alias of --convo
      --convo string          only this conversation id (or current)
  -h, --help                  help for interrupt
```

## crowdvm limits

```text
Show start limits, active sandboxes and credits

Usage:
  crowdvm limits [flags]

Flags:
      --all       show every limit
      --details   same as --all
  -h, --help      help for limits
```

## crowdvm list

```text
List sandboxes. --filter takes state group letters:
  r up/running (ready, cloning, idle, running)   s stopped   p pending
  t stopping   e error.  Combine them (--filter sr), or use --all.

Usage:
  crowdvm list [flags]

Aliases:
  list, ls

Flags:
      --all             include every state
      --filter string   state groups: r, s, p, t, e (combine letters)
  -h, --help            help for list
```

## crowdvm login

```text
Sign in to a CrowdVM server and save the credentials in the config file.

Without a key, crowdvm prints a URL and a code and opens the browser; approve
the login there. Without a terminal (agents, scripts), 'crowdvm login --json'
prints the URL and code and exits; then 'crowdvm login --wait' (or
'crowdvm onboard --wait') finishes the login.

Usage:
  crowdvm login [key] [flags]

Examples:
  crowdvm login --server http://10.0.0.5:8420
  crowdvm login --email you@example.com          # a 6-digit code by email
  crowdvm login --email you@example.com --code 123456
  crowdvm login --key-stdin <<< "$CROWDVM_API_KEY"
  crowdvm login --api-key cvm_abc123...
  crowdvm login --username alice --password-stdin <<< "$PASSWORD"

Flags:
      --api-key string       sign in with this API key (cvm_...)
      --code string          with --email: the code from the email (for scripts and agents)
      --email string[=" "]   sign in with a 6-digit code sent to this address (asks when empty)
  -h, --help                 help for login
      --invite string        with --email: invite code for a new account on a closed server
      --key-stdin            read the API key from stdin (keeps it out of process arguments)
      --new                  start a new browser login even if one is pending
      --no-browser           print the URL but do not open a browser
      --password-stdin       read the password from stdin
      --server string        CrowdVM server URL, e.g. http://10.0.0.5:8420
      --username string      sign in with a username and password
      --wait                 wait for the browser approval (default when a terminal is attached)
```

## crowdvm logout

```text
Sign out and clear the local API key

Usage:
  crowdvm logout [flags]

Flags:
  -h, --help   help for logout
```

## crowdvm new

```text
Create a sandbox. With --json, prints one JSON object per line:
{"event":"created",...}, {"event":"state",...} and finally {"event":"ready","id":...}.

Usage:
  crowdvm new [flags]

Examples:
  crowdvm new --type large --ttl 7200
  crowdvm new --from web-stack --host 3000 --file workspace/.env=./session.env
  id=$(crowdvm new --json | jq -r 'select(.event=="ready") | .id')

Flags:
      --category stringArray     placement: minimum benchmark level DIM=LEVEL, e.g. cpu=high (low|medium|high|extreme; repeatable)
      --count int                with --from: create this many sandboxes at once (default 1)
  -e, --env stringArray          per-sandbox variable KEY=VALUE (repeatable)
      --environment string       environment to start from (see: crowdvm env list)
      --fail-fast                fail with no_ready_machine instead of waiting for capacity
      --file stringArray         write SANDBOX_PATH=LOCAL_FILE before ready (repeatable)
      --from string              start from a saved snapshot name or share code
      --gpu                      placement: run on a node that passes a GPU through (VFIO); the sandbox gets that GPU to itself
  -h, --help                     help for new
      --host stringArray         host PORT (private URL) or PORT:public (repeatable)
      --min-tier string          placement: minimum node tier: bronze, silver, gold or platinum
      --name string              display name
      --nested                   placement: run on a node with nested virtualization (KVM inside the sandbox)
      --no-auto-stop             never stop automatically
      --no-env                   give the sandbox none of your secrets
      --no-monitoring            keep no command output, process list or metrics
      --no-snapshots             never snapshot; stopping erases the sandbox
      --no-wait                  return after the create is accepted
      --node string              placement: run on this node (id or name, see: crowdvm nodes)
      --node-label stringArray   placement: node label KEY=VALUE that must match (repeatable)
      --personal                 bill your personal account even when an org is active
      --setup-file string        local shell script to run once after the sandbox is ready (64KB)
      --ttl int                  seconds until auto-stop, counted from start (default 3600)
      --type string              machine size: small, default, large or xlarge
      --visibility string        org sandboxes: private or shared
```

## crowdvm nodes

```text
List the machines lending compute, with their benchmark categories

Usage:
  crowdvm nodes [flags]
  crowdvm nodes [command]

Available Commands:
  drain       Stop placing new sandboxes on a node (running ones keep running)
  info        Show one node: capacity, benchmark, sandboxes, earned credits
  join-token  Create a token for `crowdvm-node join` (admins)
  join-tokens List node join tokens (admins)
  remove      Remove a node (--force evicts it while it runs sandboxes)
  undrain     Place sandboxes on a drained node again
  update      Rename a node or replace its labels

Flags:
  -h, --help   help for nodes
```

### crowdvm nodes drain

```text
Stop placing new sandboxes on a node (running ones keep running)

Usage:
  crowdvm nodes drain NODE [flags]

Flags:
  -h, --help   help for drain
```

### crowdvm nodes info

```text
Show one node: capacity, benchmark, sandboxes, earned credits

Usage:
  crowdvm nodes info NODE [flags]

Flags:
  -h, --help   help for info
```

### crowdvm nodes join-token

```text
Create a token for `crowdvm-node join` (admins)

Usage:
  crowdvm nodes join-token [flags]

Flags:
  -h, --help           help for join-token
      --max-uses int   how many nodes may join with it
      --name string    label for the token
      --ttl int        seconds until the token expires
```

### crowdvm nodes join-tokens

```text
List node join tokens (admins)

Usage:
  crowdvm nodes join-tokens [flags]
  crowdvm nodes join-tokens [command]

Available Commands:
  revoke      Revoke a join token

Flags:
  -h, --help   help for join-tokens
```

### crowdvm nodes join-tokens revoke

```text
Revoke a join token

Usage:
  crowdvm nodes join-tokens revoke TOKEN_ID [flags]

Flags:
  -h, --help   help for revoke
```

### crowdvm nodes remove

```text
Remove a node (--force evicts it while it runs sandboxes)

Usage:
  crowdvm nodes remove NODE [flags]

Aliases:
  remove, evict, rm

Flags:
      --force   remove even while it runs sandboxes
  -h, --help    help for remove
  -y, --yes     do not ask for confirmation
```

### crowdvm nodes undrain

```text
Place sandboxes on a drained node again

Usage:
  crowdvm nodes undrain NODE [flags]

Aliases:
  undrain, enable

Flags:
  -h, --help   help for undrain
```

### crowdvm nodes update

```text
Rename a node or replace its labels

Usage:
  crowdvm nodes update NODE [flags]

Examples:
  crowdvm nodes update n_123 --name lab-gpu --label gpu=a100 --label room=2

Flags:
  -h, --help                help for update
      --label stringArray   label KEY=VALUE (repeatable; replaces all labels; --label "" removes them all)
      --name string         new name
```

## crowdvm notices

```text
Notices: low credits, grace period, stopped or failing sandboxes, full disks

Usage:
  crowdvm notices [flags]
  crowdvm notices [command]

Aliases:
  notices, notifications

Available Commands:
  dismiss     Dismiss a notice (--all: every one)

Flags:
      --all    include dismissed notices
  -h, --help   help for notices
```

### crowdvm notices dismiss

```text
Dismiss a notice (--all: every one)

Usage:
  crowdvm notices dismiss [ID] [flags]

Flags:
      --all    dismiss every notice
  -h, --help   help for dismiss
```

## crowdvm onboard

```text
Set up this computer. It signs you in through the browser (or --email),
saves the token, asks once whether your sandboxes are for your own work or for
other people (a platform for third parties; that answer makes your default
environment safe for third parties), and shows your credit balance.

Running it again keeps a working sign-in; --email signs in again.

Usage:
  crowdvm onboard [flags]

Examples:
  crowdvm onboard
  crowdvm onboard --email you@example.com
  crowdvm onboard --for others --json

Flags:
      --email string[=" "]   sign in with a code sent to this address
      --for string           what the sandboxes are for: own (your work) or others (a platform for third parties)
  -h, --help                 help for onboard
      --invite string        with --email: invite code for a new account on a closed server
      --no-browser           do not open a browser
      --server string        CrowdVM server URL
      --wait                 wait for the browser approval (default true)
```

## crowdvm org

```text
Show or switch the organization new sandboxes belong to

Usage:
  crowdvm org [flags]
  crowdvm org [command]

Available Commands:
  create             Create an organization
  default-visibility Show or set whether new org sandboxes start private or shared
  delete             Delete the organization in scope (owner only)
  invite             Invite a member (prints a join code and link)
  join               Join an organization with an invite code
  list               Organizations you belong to
  members            List the members and pending invites of the organization in scope
  migrate            Move your personal sandboxes (and their credit spend) into the organization in scope
  service-accounts   List the organization's service accounts (users that act only through org API keys)
  status             Show the active organization
  switch             Make ORG (id or name, or personal) the active organization
  transfer           Make USER the owner of the organization in scope

Flags:
  -h, --help   help for org
```

### crowdvm org create

```text
Create an organization

Usage:
  crowdvm org create NAME [flags]

Flags:
  -h, --help   help for create
```

### crowdvm org default-visibility

```text
Show or set whether new org sandboxes start private or shared

Usage:
  crowdvm org default-visibility [private|shared] [flags]

Flags:
  -h, --help   help for default-visibility
```

### crowdvm org delete

```text
Delete the organization in scope (owner only)

Usage:
  crowdvm org delete [flags]

Flags:
  -h, --help   help for delete
  -y, --yes    do not ask for confirmation
```

### crowdvm org invite

```text
Invite a member (prints a join code and link)

Usage:
  crowdvm org invite [EMAIL] [flags]

Flags:
  -h, --help          help for invite
      --role string   member or admin (default "member")
```

### crowdvm org join

```text
Join an organization with an invite code

Usage:
  crowdvm org join CODE [flags]

Flags:
  -h, --help   help for join
```

### crowdvm org list

```text
Organizations you belong to

Usage:
  crowdvm org list [flags]

Aliases:
  list, ls

Flags:
  -h, --help   help for list
```

### crowdvm org members

```text
List the members and pending invites of the organization in scope

Usage:
  crowdvm org members [flags]

Flags:
  -h, --help   help for members
```

### crowdvm org migrate

```text
Move your personal sandboxes into the organization in scope (--org, or
'crowdvm org switch'): from now on they spend the organization's credits.
Your personal credit balance stays on your account unless you pass
--transfer-credits N (or all). Owners only.

Usage:
  crowdvm org migrate [flags]

Flags:
  -h, --help                      help for migrate
      --transfer-credits string   also move this many personal credits (or all) to the organization
  -y, --yes                       do not ask for confirmation
```

### crowdvm org service-accounts

```text
List the organization's service accounts (users that act only through org API keys)

Usage:
  crowdvm org service-accounts [flags]
  crowdvm org service-accounts [command]

Available Commands:
  create      Create a service account and its API key (the secret is shown once)
  remove      Delete a service account

Flags:
  -h, --help   help for service-accounts
```

### crowdvm org service-accounts create

```text
Create a service account and its API key (the secret is shown once)

Usage:
  crowdvm org service-accounts create NAME [flags]

Flags:
  -h, --help            help for create
      --preset string   key preset: read-only, full-sandbox, ci or admin
      --ttl string      key lifetime, e.g. 90d or never
```

### crowdvm org service-accounts remove

```text
Delete a service account

Usage:
  crowdvm org service-accounts remove USER [flags]

Aliases:
  remove, rm

Flags:
  -h, --help   help for remove
```

### crowdvm org status

```text
Show the active organization

Usage:
  crowdvm org status [flags]

Flags:
  -h, --help   help for status
```

### crowdvm org switch

```text
Make ORG (id or name, or personal) the active organization

Usage:
  crowdvm org switch ORG [flags]

Flags:
  -h, --help   help for switch
```

### crowdvm org transfer

```text
Make USER the owner of the organization in scope

Usage:
  crowdvm org transfer USER [flags]

Flags:
  -h, --help   help for transfer
```

## crowdvm prompt

```text
Send a natural-language prompt to an integrated agent (Claude Code, Codex,
OpenCode, ...) running inside the sandbox. Without --new or --resume the prompt
continues this profile's current conversation on that sandbox. Prints the
agent's events until the turn ends (JSON Lines with --json).

--local-login sends this machine's own login with the prompt, for that turn
only: a Claude or ChatGPT subscription (access token only, never the refresh
token) or a vendor API key, read fresh at every call. It needs --provider.
CROWDVM_LOCAL_LOGIN=1 (or subscription, api-key) turns it on for a session.

Usage:
  crowdvm prompt [ID] MESSAGE [flags]

Examples:
  crowdvm prompt bx_f7k2q9hd "Fix the failing tests"
  crowdvm prompt bx_f7k2q9hd --provider codex --model gpt-6.1-sol --new "Refactor auth"
  crowdvm prompt bx_f7k2q9hd --provider codex --model gpt-6-luna --local-login "Fix the build"
  crowdvm prompt --attach ./trace.log "Why does this crash?"

Flags:
      --attach stringArray            attach a file (saved under ~/attachments; repeatable)
      --fast                          fast mode (only fast-capable models)
  -h, --help                          help for prompt
      --image stringArray             alias of --attach
      --local-login string[="auto"]   use this machine's own login for this turn: auto (subscription, then API key), subscription or api-key; needs --provider (env CROWDVM_LOCAL_LOGIN)
      --model string                  provider-specific model
      --new                           start a new conversation
      --no-wait                       return once the prompt is queued
      --provider string               harness: claude, codex, opencode, pi, ... (default: server default)
      --reasoning-effort string       thinking level: none, low, medium, high, xhigh, max
      --resume string                 continue this conversation id (or current)

Sign in (crowdvm login) to list the providers and models of your server here.
```

## crowdvm ps

```text
List the processes in a sandbox, busiest first

Usage:
  crowdvm ps ID [flags]

Flags:
  -h, --help   help for ps
```

## crowdvm push

```text
Copy a local directory into the sandbox as one archive. In a git repository
only tracked files and new files that .gitignore does not exclude are sent
(node_modules, build output and .git stay local). Default target:
/home/user/<dir name>. Existing files are replaced; nothing is deleted.

Usage:
  crowdvm push ID DIR [flags]

Flags:
  -h, --help        help for push
      --to string   directory on the sandbox (default /home/user/<dir name>)
```

## crowdvm recover

```text
Power-cycle the machine of a sandbox that does not respond

Usage:
  crowdvm recover ID [flags]

Flags:
  -h, --help   help for recover
```

## crowdvm rename

```text
Rename a sandbox

Usage:
  crowdvm rename ID NAME [flags]

Flags:
  -h, --help   help for rename
```

## crowdvm repos

```text
Repositories you can add to environments, and the git hosts behind them

Usage:
  crowdvm repos [flags]
  crowdvm repos [command]

Available Commands:
  sources     List your git hosts (GitHub, GitLab, Gitea/Forgejo, plain git)

Flags:
  -h, --help           help for repos
      --query string   filter by name
```

### crowdvm repos sources

```text
List your git hosts (GitHub, GitLab, Gitea/Forgejo, plain git)

Usage:
  crowdvm repos sources [flags]
  crowdvm repos sources [command]

Available Commands:
  add         Connect a git host with a personal access token (token from stdin when --token is omitted)
  rm          Disconnect a git host

Flags:
  -h, --help   help for sources
```

### crowdvm repos sources add

```text
Connect a git host with a personal access token (token from stdin when --token is omitted)

Usage:
  crowdvm repos sources add [flags]

Examples:
  crowdvm repos sources add --kind gitlab --token "$GITLAB_TOKEN"
  crowdvm repos sources add --kind gitea --base-url https://git.lan < token.txt

Flags:
      --api-url string    API URL, when it is not the usual one
      --base-url string   web URL of the host (default for github/gitlab: the public site)
  -h, --help              help for add
      --kind string       github, gitlab, gitea (also Forgejo) or git (default "github")
      --name string       display name
      --token string      personal access token (default: read stdin)
```

### crowdvm repos sources rm

```text
Disconnect a git host

Usage:
  crowdvm repos sources rm SOURCE_ID [flags]

Aliases:
  rm, remove

Flags:
  -h, --help   help for rm
```

## crowdvm resume

```text
Resume a stopped sandbox (on any node with room) and wait until it runs
commands. --type, --environment and --no-env change a stopped sandbox.

Usage:
  crowdvm resume ID [flags]

Flags:
      --category stringArray     placement: minimum benchmark level DIM=LEVEL, e.g. cpu=high (low|medium|high|extreme; repeatable)
  -e, --env stringArray          per-sandbox variable KEY=VALUE (repeatable)
      --environment string       environment to start from (see: crowdvm env list)
      --fail-fast                fail with no_ready_machine instead of waiting for capacity
      --gpu                      placement: run on a node that passes a GPU through (VFIO); the sandbox gets that GPU to itself
  -h, --help                     help for resume
      --min-tier string          placement: minimum node tier: bronze, silver, gold or platinum
      --nested                   placement: run on a node with nested virtualization (KVM inside the sandbox)
      --no-auto-stop             never stop automatically
      --no-env                   give the sandbox none of your secrets
      --no-wait                  return when the resume is accepted
      --node string              placement: run on this node (id or name, see: crowdvm nodes)
      --node-label stringArray   placement: node label KEY=VALUE that must match (repeatable)
      --ttl int                  seconds until auto-stop, counted from start (default 3600)
      --type string              machine size: small, default, large or xlarge
```

## crowdvm run

```text
Create a sandbox, optionally push a directory, run the command with live
output, print time and credits, and delete the sandbox. Exits with the exit
code of the command. Ctrl-C deletes the sandbox.

Usage:
  crowdvm run [flags] [--] COMMAND...

Examples:
  crowdvm run -- npm test
  crowdvm run --from ci-base --type xlarge --push . -- make -j16 test

Flags:
      --category stringArray     placement: minimum benchmark level DIM=LEVEL, e.g. cpu=high (low|medium|high|extreme; repeatable)
      --cwd string               working directory, relative to /home/user
  -e, --env stringArray          per-sandbox variable KEY=VALUE (repeatable)
      --environment string       environment to start from (see: crowdvm env list)
      --fail-fast                fail with no_ready_machine instead of waiting for capacity
      --from string              start from a saved snapshot or share code
      --gpu                      placement: run on a node that passes a GPU through (VFIO); the sandbox gets that GPU to itself
  -h, --help                     help for run
      --keep                     keep the sandbox afterwards (it stops 15 minutes later)
      --min-tier string          placement: minimum node tier: bronze, silver, gold or platinum
      --nested                   placement: run on a node with nested virtualization (KVM inside the sandbox)
      --no-env                   give the sandbox none of your secrets
      --node string              placement: run on this node (id or name, see: crowdvm nodes)
      --node-label stringArray   placement: node label KEY=VALUE that must match (repeatable)
      --personal                 bill your personal account
      --push string              copy this local directory in first and run there
      --timeout int              stop after this many seconds and exit 124 (0 = no limit)
      --type string              machine size: small, default, large or xlarge
```

## crowdvm scp

```text
Copy files with the system scp through a crowdvm tunnel. Without scp (for
example on Windows without OpenSSH), or with --api, files go through the
API instead. Relative remote paths start in /home/user.

Usage:
  crowdvm scp [-r] SRC... DST [flags]

Examples:
  crowdvm scp bx_f7k2q9hd:/home/user/output.zip ./output.zip
  crowdvm scp ./local-file.txt bx_f7k2q9hd:
  crowdvm scp -r ./my-project bx_f7k2q9hd:/home/user/

Flags:
      --api         copy through the API instead of scp
  -h, --help        help for scp
  -r, --recursive   copy directories recursively
```

## crowdvm self-update

```text
Download the crowdvm build for this platform from the server
(/downloads/<os>/<arch>/crowdvm), read its version, and replace this binary
when the download is newer. An older or equal build is not installed unless
you pass --force. When either version is not a release (a "dev" build), the
CLI compares file contents instead and installs any build that differs.

--check only reports whether an update is available (exit 1 if so).

After other commands the CLI checks the server's version at most every
6 hours and prints a one-line notice on the terminal when a newer release is
available. It never installs anything on its own. Turn the check off with
--no-update or CROWDVM_NO_UPDATE=1.

Usage:
  crowdvm self-update [flags]

Examples:
  crowdvm self-update
  crowdvm self-update --check && echo up to date

Flags:
      --check   only check; exit 1 when an update is available
      --force   install the server's build even when it is not newer
  -h, --help    help for self-update
```

## crowdvm share

```text
Let every org member use this sandbox

Usage:
  crowdvm share ID [flags]

Flags:
  -h, --help   help for share
```

## crowdvm shell-init

```text
Print a shell function named crowdvm. It passes CROWDVM_SHELL_ID (the
shell's process id) to every call, so "current" and the current conversation
belong to this shell: two terminals can work on two sandboxes. The installer
adds it to your shell startup file; to add it yourself:

  bash/zsh:    eval "$(crowdvm shell-init bash)"      (zsh: shell-init zsh)
  fish:        crowdvm shell-init fish | source
  PowerShell:  crowdvm shell-init powershell | Out-String | Invoke-Expression

Scripts and CI jobs do not read startup files: pass sandbox ids there.

Usage:
  crowdvm shell-init bash|zsh|fish|powershell [flags]

Flags:
  -h, --help   help for shell-init
```

## crowdvm skill

```text
The crowdvm skill teaches a coding agent on your computer when and how to use
sandboxes (create, exec, push, host, snapshots, stop). It is the same skill the
integrated agents inside a sandbox already have.

  crowdvm skill install                  # every agent found in your home folder
  crowdvm skill install --agent claude   # one agent (claude, codex, opencode, cursor, agents)
  crowdvm skill install --project .      # into this project (.claude/skills, ...)
  crowdvm skill path                     # where it is (or would be) installed

Also give the agent the docs (<server>/llms.txt, <server>/mcp) and the AGENTS.md
rule from crowdvm agent-instructions.

Usage:
  crowdvm skill [flags]
  crowdvm skill [command]

Available Commands:
  install     Write the crowdvm skill into the agents' skills folders (replaces an older copy)
  path        Print the skill folder of each agent and whether the crowdvm skill is installed there

Flags:
  -h, --help   help for skill
```

### crowdvm skill install

```text
Write the crowdvm skill into the agents' skills folders (replaces an older copy)

Usage:
  crowdvm skill install [flags]

Flags:
      --agent stringArray   claude, codex, opencode, cursor or agents (repeatable; default: the agents found)
  -h, --help                help for install
      --project string      install into this project folder instead of your home folder
```

### crowdvm skill path

```text
Print the skill folder of each agent and whether the crowdvm skill is installed there

Usage:
  crowdvm skill path [flags]

Flags:
      --agent stringArray   claude, codex, opencode, cursor or agents (repeatable; default: the agents found)
  -h, --help                help for path
      --project string      install into this project folder instead of your home folder
```

## crowdvm snapshot

```text
'crowdvm snapshot ID NAME' saves the disk of a sandbox under NAME (replacing
an older one with that name); 'crowdvm new --from NAME' deploys it.

Usage:
  crowdvm snapshot ID NAME | snapshot <subcommand> [flags]
  crowdvm snapshot [command]

Examples:
  crowdvm snapshot bx_f7k2q9hd web-stack
  crowdvm snapshot list
  crowdvm snapshot share web-stack
  crowdvm snapshot pull snap_123 -o ./restore --path /home/user/project

Available Commands:
  delete      Permanently delete ordinary snapshots
  info        Show one saved snapshot
  latest      Show the newest snapshot of a sandbox
  list        List saved snapshots
  pull        Download a snapshot (or --path parts of it) into a local folder
  rm          Remove a saved snapshot and release its storage
  share       Make a secret code another account can deploy (shown once)
  shares      List share codes of a saved snapshot
  tree        List the files and sizes in a snapshot
  unshare     Revoke a share code

Flags:
  -h, --help      help for snapshot
      --no-wait   return while the snapshot is saving
```

### crowdvm snapshot delete

```text
Permanently delete ordinary snapshots

Usage:
  crowdvm snapshot delete SNAPSHOT_ID... | delete --all SANDBOX_ID [flags]

Flags:
      --all string   delete every ordinary snapshot of this sandbox
  -h, --help         help for delete
  -y, --yes          do not ask for confirmation
```

### crowdvm snapshot info

```text
Show one saved snapshot

Usage:
  crowdvm snapshot info NAME [flags]

Flags:
  -h, --help   help for info
```

### crowdvm snapshot latest

```text
Show the newest snapshot of a sandbox

Usage:
  crowdvm snapshot latest ID [flags]

Flags:
  -h, --help   help for latest
```

### crowdvm snapshot list

```text
List saved snapshots

Usage:
  crowdvm snapshot list [flags]

Aliases:
  list, ls

Flags:
  -h, --help   help for list
```

### crowdvm snapshot pull

```text
Download a snapshot. The sandbox can be stopped. A sandbox id pulls its newest
snapshot. /home/user lands in DIR/home_user/ and Docker volumes in DIR/docker/.
--path extracts only that file or folder (repeatable).

Usage:
  crowdvm snapshot pull SNAPSHOT_ID|SANDBOX_ID -o DIR [flags]

Flags:
  -h, --help               help for pull
  -o, --out string         local directory
      --path stringArray   only this path in the sandbox (repeatable)
```

### crowdvm snapshot rm

```text
Remove a saved snapshot and release its storage

Usage:
  crowdvm snapshot rm NAME [flags]

Aliases:
  rm, remove

Flags:
  -h, --help   help for rm
      --yes    accepted for Boat compatibility (no confirmation is asked)
```

### crowdvm snapshot share

```text
Make a secret code another account can deploy (shown once)

Usage:
  crowdvm snapshot share NAME [flags]

Flags:
  -h, --help   help for share
```

### crowdvm snapshot shares

```text
List share codes of a saved snapshot

Usage:
  crowdvm snapshot shares NAME [flags]

Flags:
  -h, --help   help for shares
```

### crowdvm snapshot tree

```text
List the files and sizes in a snapshot

Usage:
  crowdvm snapshot tree SNAPSHOT_ID [flags]

Flags:
  -h, --help   help for tree
```

### crowdvm snapshot unshare

```text
Revoke a share code

Usage:
  crowdvm snapshot unshare NAME SHARE_ID [flags]

Flags:
  -h, --help   help for unshare
```

### crowdvm snapshot current

```text
'crowdvm snapshot ID NAME' saves the disk of a sandbox under NAME (replacing
an older one with that name); 'crowdvm new --from NAME' deploys it.

Usage:
  crowdvm snapshot ID NAME | snapshot <subcommand> [flags]
  crowdvm snapshot [command]

Examples:
  crowdvm snapshot bx_f7k2q9hd web-stack
  crowdvm snapshot list
  crowdvm snapshot share web-stack
  crowdvm snapshot pull snap_123 -o ./restore --path /home/user/project

Available Commands:
  delete      Permanently delete ordinary snapshots
  info        Show one saved snapshot
  latest      Show the newest snapshot of a sandbox
  list        List saved snapshots
  pull        Download a snapshot (or --path parts of it) into a local folder
  rm          Remove a saved snapshot and release its storage
  share       Make a secret code another account can deploy (shown once)
  shares      List share codes of a saved snapshot
  tree        List the files and sizes in a snapshot
  unshare     Revoke a share code

Flags:
  -h, --help      help for snapshot
      --no-wait   return while the snapshot is saving
```

### crowdvm snapshot self

```text
'crowdvm snapshot ID NAME' saves the disk of a sandbox under NAME (replacing
an older one with that name); 'crowdvm new --from NAME' deploys it.

Usage:
  crowdvm snapshot ID NAME | snapshot <subcommand> [flags]
  crowdvm snapshot [command]

Examples:
  crowdvm snapshot bx_f7k2q9hd web-stack
  crowdvm snapshot list
  crowdvm snapshot share web-stack
  crowdvm snapshot pull snap_123 -o ./restore --path /home/user/project

Available Commands:
  delete      Permanently delete ordinary snapshots
  info        Show one saved snapshot
  latest      Show the newest snapshot of a sandbox
  list        List saved snapshots
  pull        Download a snapshot (or --path parts of it) into a local folder
  rm          Remove a saved snapshot and release its storage
  share       Make a secret code another account can deploy (shown once)
  shares      List share codes of a saved snapshot
  tree        List the files and sizes in a snapshot
  unshare     Revoke a share code

Flags:
  -h, --help      help for snapshot
      --no-wait   return while the snapshot is saving
```

## crowdvm snapshots

```text
List your saved snapshots, then the automatic snapshots of all your sandboxes (or of one)

Usage:
  crowdvm snapshots [ID] [flags]

Flags:
      --all         read every page
  -h, --help        help for snapshots
      --limit int   snapshots per page
```

## crowdvm ssh

```text
Open an SSH session (system ssh through a crowdvm tunnel; the key lives in
the crowdvm config directory and is authorized automatically). For one command,
'crowdvm exec' is faster. Pass -t for full-screen tools. ID may also be the
sandbox's name.

Usage:
  crowdvm ssh [-t] ID [--] [COMMAND...] [flags]

Examples:
  crowdvm ssh bx_f7k2q9hd
  crowdvm ssh bx_f7k2q9hd "cd my-repo && npm test"
  crowdvm ssh bx_f7k2q9hd -- bash -s < ./setup.sh
  crowdvm ssh -t bx_f7k2q9hd -- tmux attach

Flags:
  -h, --help   help for ssh
  -t, --tty    give the remote command a terminal
```

## crowdvm ssh-config

```text
Authorize this computer's key on the sandbox and write
~/.ssh/crowdvm/<id>.conf (Host <id>, ProxyCommand crowdvm tunnel, pinned host
key). Add 'Include crowdvm/*.conf' at the top of ~/.ssh/config once; then
'ssh <id>', 'scp file <id>:' and VS Code Remote-SSH work. --print only prints it.

Usage:
  crowdvm ssh-config ID [flags]

Flags:
  -h, --help    help for ssh-config
      --print   print the entry instead of writing it
```

## crowdvm status

```text
Show the server, the signed-in account and the config paths

Usage:
  crowdvm status [flags]

Flags:
      --check   exit 1 when this computer is not signed in
  -h, --help    help for status
```

## crowdvm steer

```text
Add a message to the agent's running turn without stopping it

Usage:
  crowdvm steer [ID] MESSAGE [flags]

Flags:
      --conversation string   alias of --convo
      --convo string          conversation id (or current; default: this profile's)
  -h, --help                  help for steer
```

## crowdvm stop

```text
Stop a sandbox: snapshot its disk, then free the machine

Usage:
  crowdvm stop ID [flags]

Flags:
      --force           stop now and keep only the last successful snapshot (loses newer data)
  -h, --help            help for stop
      --status string   show the status of a stop operation id
      --wait            wait until the sandbox is stopped
  -y, --yes             accepted for symmetry with delete; stop never asks
```

## crowdvm subdomain

```text
Set the DNS label of a sandbox's hosted URLs (<label>-<port>.<domain>)

Usage:
  crowdvm subdomain ID LABEL [flags]

Flags:
  -h, --help   help for subdomain
```

## crowdvm telemetry

```text
Send metrics and lifecycle events to an OTLP/HTTP endpoint

Usage:
  crowdvm telemetry [flags]
  crowdvm telemetry [command]

Available Commands:
  set         Set the export (replaces it) and send a test export
  unset       Remove the export

Flags:
  -h, --help   help for telemetry
```

### crowdvm telemetry set

```text
Set the export (replaces it) and send a test export

Usage:
  crowdvm telemetry set ENDPOINT [flags]

Examples:
  crowdvm telemetry set https://otlp.example.com -H api-key=$KEY

Flags:
  -H, --header stringArray   header key=value (repeatable)
  -h, --help                 help for set
```

### crowdvm telemetry unset

```text
Remove the export

Usage:
  crowdvm telemetry unset [flags]

Flags:
  -h, --help   help for unset
```

## crowdvm tunnel

```text
Connect stdin/stdout to 127.0.0.1:PORT inside the sandbox. This is the
ProxyCommand that 'crowdvm ssh' uses; you can use it in ~/.ssh/config:

  Host bx_*
    User user
    IdentityFile ~/.ssh/crowdvm_ed25519
    ProxyCommand crowdvm tunnel %h 22

('crowdvm ssh-config ID' writes such an entry for you.) The tunnel ends when
the sandbox side closes; end of input on stdin is not passed on yet.

Usage:
  crowdvm tunnel ID PORT [flags]

Flags:
  -h, --help   help for tunnel
```

## crowdvm unshare

```text
Make an org sandbox private to you

Usage:
  crowdvm unshare ID [flags]

Flags:
  -h, --help   help for unshare
```

## crowdvm usage

```text
Show the machine time and credits one sandbox used

Usage:
  crowdvm usage ID [flags]

Flags:
  -h, --help           help for usage
      --since string   count from this time (ISO 8601 or Unix seconds)
      --until string   count up to this time (ISO 8601 or Unix seconds)
```

## crowdvm version

```text
Print the CLI version

Usage:
  crowdvm version [flags]

Flags:
  -h, --help   help for version
```

## crowdvm webhook

```text
Register sandbox lifecycle webhooks

Usage:
  crowdvm webhook [command]

Aliases:
  webhook, webhooks

Available Commands:
  create      Create a webhook (events: ready, error, archived, hydrated, degraded, recovered)
  get         Show a webhook
  list        List webhooks
  remove      Delete a webhook
  rotate      Replace the signing secret
  update      Change a webhook's name, URL or events

Flags:
  -h, --help   help for webhook
```

### crowdvm webhook create

```text
Create a webhook (events: ready, error, archived, hydrated, degraded, recovered)

Usage:
  crowdvm webhook create URL [flags]

Flags:
      --event stringArray   event to send (repeatable or comma-separated; default all)
  -h, --help                help for create
      --name string         display name
```

### crowdvm webhook get

```text
Show a webhook

Usage:
  crowdvm webhook get ID [flags]

Flags:
  -h, --help   help for get
```

### crowdvm webhook list

```text
List webhooks

Usage:
  crowdvm webhook list [flags]

Aliases:
  list, ls

Flags:
  -h, --help   help for list
```

### crowdvm webhook remove

```text
Delete a webhook

Usage:
  crowdvm webhook remove ID [flags]

Aliases:
  remove, rm, delete

Flags:
  -h, --help   help for remove
```

### crowdvm webhook rotate

```text
Replace the signing secret

Usage:
  crowdvm webhook rotate ID [flags]

Flags:
  -h, --help   help for rotate
```

### crowdvm webhook update

```text
Change a webhook's name, URL or events

Usage:
  crowdvm webhook update ID [flags]

Flags:
      --event stringArray   events (replaces the list)
  -h, --help                help for update
      --name string         display name
      --url string          new URL
```


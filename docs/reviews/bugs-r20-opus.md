# Bug hunt r20 (opus): round-19 rechecks, environments and git, Docker across copies, desktop and computer use, 2026-11-12

Setup: commit `443337b`, built with `make build VERSION=0.9.0-r20.1` in my own worktree (`/home/tl/dev/CrowdVM-r20opus`, detached at `443337b`). Binaries and all state lived in `~/.local/share/crowdvm-r20opus`. One `crowdvm-server` listened on :9960 with `--public-url http://192.168.178.31:9963`, `--public-domain r20.test`, `--log-level debug`, no start limits, `--enforce-credits`, `--default-image ubuntu-24.04-crowdvm-v4`, `--egress-allow 192.168.178.31:9967,192.168.178.31:9968` (my git servers) and `--node-update-grace 6m`. Two qemu nodes, `r20n1` and `r20n2` (UI on :9961 and :9962), each with 6 vCPU, 12 GiB, 35 GiB disk ×4 overcommit and at most 5 sandboxes, each with its own binary directory, both running as transient systemd user units (`r20opus-n1`, `r20opus-n2`) with the real unit's properties (`KillMode=process`, `NotifyAccess=main`, `FileDescriptorStoreMax=1024`, `FileDescriptorStorePreserve=yes`, `CROWDVM_FDSTORE=1`, `Restart=always`, `RestartSec=5`). Image `ubuntu-24.04-crowdvm-v4` from `~/.local/share/crowdvm-dev/images/`, imported on `r20n1`, pushed and pulled to `r20n2`. Node↔server and guest↔server traffic went through a Python token-bucket proxy on :9963 at 40 MB/s (2 MB/s for the failed-save test). Git: a small Python HTTP server around `git http-backend` on :9967 (plain http) and :9968 (https, self-signed), with bare repos `pub` (public), `priv` and `priv2` (basic auth, token `tok_PRIVATE_r20_abc123`), logging every request with the credentials it carried. Nothing went to GitHub or Docker Hub: the Docker image was built in the sandbox with `docker import` from the VM's static busybox. Users: `admin` (owns the nodes), `bob`, `carol` and `dave`, each with their own `CROWDVM_CONFIG_DIR` and scratch `HOME`.

To check the time-based paths without waiting, I moved timestamps in my own server database: `wallet_state.quota_month` (monthly quota rollover) and `saved_snapshots.saving_since` (save timeout). Product code was not changed.

The host disk had 66–93 GB free and is shared with other sessions, so every sandbox was `small` and I did not run a `--new-key` copy (its re-encryption writes the whole 51 GB disk; see "Not verified").

Cleanup: see the last section.

## Bugs

### 1. A private repository on an `http://` git host is never cloned: the guest drops its token (wrong-result)

Severity: wrong-result. `crowdvm repos sources add` accepts an `http://` base URL (`baseUrl must be an http(s) URL`), and the server hands the source's token to the guest for any clone URL starting with `http` (`strings.HasPrefix(r.CloneURL, "http")`). The guest only writes credential lines for `https` URLs (`u.Scheme != "https"` → skipped), so git gets no credentials and the clone fails with `could not read Username … terminal prompts disabled`. It retries six times (about a minute, delaying the other repos and setup scripts), then gives up. Nothing in `crowdvm info` says so; only the guest journal and the `repo.setup` event do. A LAN Gitea/Forgejo or plain git server on http (the usual home-lab setup) cannot be used for private repositories.

Where: `internal/guest/setup.go:256` (only `https` gets a credential line), against `internal/server/feat_env_guest.go:93` (token sent for `http` too).

Repro:
1. bob: `echo tok_PRIVATE_r20_abc123 | crowdvm repos sources add --kind git --base-url http://192.168.178.31:9967 --name lan`.
2. `crowdvm env new dev`, `env add-repo dev http://192.168.178.31:9967/pub.git`, `… /priv.git`, `… https://192.168.178.31:9968/priv2.git`, `env set-var dev GIT_SSL_NO_VERIFY=true`.
3. `crowdvm new --name e1 --type small --environment dev`, then `ls ~`.

Observed: `pub` and `priv2` are cloned, `priv` is missing. Git server log: every `GET /priv.git/info/refs` arrives with `user= pw=` (no credentials), while `priv2` (https) gets `user=x-access-token pw=tok_PRIVATE_r20_abc123`. Guest journal: `repo /home/user/priv: clone failed (fatal: could not read Username for 'http://192.168.178.31:9967': terminal prompts disabled), retrying in 2s` … `32s`. `~/.config/crowdvm/git-credentials` holds only the https host.

Expected: the guest writes `http://x-access-token:<token>@host:port` for http clone URLs too (the server already decided to send the token), or the server refuses http git sources with private repos.

### 2. Withholding repository access (`--github false`, `--safe-for-third-parties true`) and `env upgrade` leaves the repository token in the running sandbox, and it still works (security)

Severity: security. `envApplyLive` sends `ApplyConfig{Repos: p.Repos}`; when the new version withholds repositories, `p.Repos` is nil and goes out as `"repos": null`. The guest treats a nil `Repos` as "unchanged" and keeps the old list, tokens included. The server does put `~/.config/crowdvm/git-credentials` into the scrub, and the guest deletes it, but right after that `writeEnvFiles` calls `writeGitCredentials` from the kept repo list and writes the file again. The server then records `gitCreds: false`, so it believes the token is gone. Each repo clone also keeps `credential.helper = store --file=…/git-credentials`, so plain `git fetch`/`git push` in the clone keeps authenticating with the owner's token. Variables, secret files and the CLI key are withdrawn correctly; only the git token survives. `safe-for-third-parties` is the switch for "this sandbox now runs untrusted code or third-party agents", so the token reaches exactly the code it was meant to be kept from. It goes away only at the next boot (I checked after an evacuation: the file was gone).

Where: `internal/server/feat_env_guest.go:396` (`Repos: p.Repos`, nil when withheld) together with `internal/guest/apply.go:324` (`if req.Repos != nil`: a nil list keeps the old one) and `internal/guest/creds.go:53` (rewrites the credential file from that list after the scrub).

Repro (continuing bug 1's e1, which has `priv2` cloned with the token):
1. `crowdvm env set dev --safe-for-third-parties true`, `crowdvm env upgrade dev` → `"upgradedLive":1`.
2. In e1: `echo GIT_SSL=$GIT_SSL_NO_VERIFY; ls -la ~/.config/crowdvm; cat ~/.config/crowdvm/git-credentials; cd ~/priv2 && GIT_SSL_NO_VERIFY=1 git fetch -v`.

Observed: `GIT_SSL=` (variables gone), `config.json` rewritten without the key (92 bytes), but `git-credentials` is present (rewritten at the upgrade, `https://x-access-token:tok_PRIVATE_r20_abc123@192.168.178.31:9968`), and `git fetch` answers `= [up to date] main -> origin/main`. Git server log: `GET /priv2.git/info/refs … user=x-access-token pw=tok_PRIVATE_r20_abc123`. Server: `extra.envManaged = {"keys":[],"files":null,"gitCreds":false,…}`. The guest's `applied-config.json` still lists all three repos with `token`. The same happened with only `env set dev --github false` + upgrade.

Expected: the server sends an empty list (`[]`) when no repositories are passed, so the guest drops the old repos and their tokens, and the credential file stays deleted.

### 3. A monthly quota no longer expires if it is moved to another wallet and back (wrong-result)

Severity: wrong-result. Round 19's fix (bugs-r19-sonnet 7) expires only `quota granted − every debit since the grant`. A transfer out is a debit, and the transfer back in is a credit that never expires. So moving the quota to an organization you own and straight back (two `credits transfer` calls, no usage) makes the whole month's quota count as "spent", and none of it expires. Repeated monthly, the quota accumulates without limit, which is what the expiry is there to prevent.

Where: `internal/server/acct_billing.go:374–376` (the `spent` query counts every debit except `quota`, including `transfer`, and ignores transfers back in).

Repro: admin: `PUT /admin/policies/<dave> {"monthlyCredits":10}` and the same for carol, then one billing tick (both get `+10 quota monthly credits 2026-11`). dave: `crowdvm org create dlaunder`, `credits transfer --from personal --to dlaunder --amount 10`, `credits transfer --from dlaunder --to dave --amount 10`. Then I set `wallet_state.quota_month='2026-10'` for both (a month rollover) and waited one tick.

Observed: carol (control, no transfer): `-9.9958 quota unused monthly credits expired`, then `+10`. dave: no expiry row, only `+10 quota monthly credits 2026-11`. dave's balance is 520 (500 signup + both quotas), carol's 509.9.

Expected: dave's unused 10 expires. For example, net transfers out against transfers in since the grant, or count only `usage`/charges as spending the quota.

### 4. A live snapshot taken right after a boot records no containers, so a copy of it starts none of them (wrong-result)

Severity: wrong-result. Every live snapshot (`POST /internal/freeze`: save, fork, periodic) first runs `recordDocker`, which overwrites `docker-running.json` with the containers running at that moment. After a resume, the guest restarts the recorded containers only once dockerd answers (`restartDocker` in `dockerAfterBoot`). A snapshot taken in between (the first 15–20 s after a resume, longer while the chain is lazy) records an empty or partial list, so a fork or deploy from it starts nothing. A container with `--restart unless-stopped` doesn't help either: the stop's `docker stop` marked it stopped by hand, so Docker does not restart it on its own. The source sandbox itself is fine: it read the list before the snapshot overwrote it.

Where: `internal/guest/internal.go:299` (`handleFreeze` records unconditionally) with `internal/guest/docker.go:81` (`recordDocker` replaces the list). Nothing waits for, or merges with, the boot's pending `restartDocker`.

Repro (d1: containers `web` with `--restart unless-stopped -p 8080:80 -v vol1:/data`, `norestart` with no policy, `stopped` stopped by hand):
1. `crowdvm stop d1`, then `crowdvm resume d1`, then at once `crowdvm snapshot d1 racesave`.
2. In d1 after 20 s: `cat /var/lib/crowdvm/docker-running.json`, `docker ps`.
3. `crowdvm new --from racesave --name rs1 --node r20n2`, then after 25 s `docker ps -a`.

Observed: step 2: `{"savedAt":"2026-11-12T18:54:28.96Z","containers":[]}`, while d1 runs `web` and `norestart` (guest: `18:54:29 docker: started 2 recorded containers again`, 40 ms after the record). Step 3: `norestart Exited (137)`, `web Exited (137)`, `stopped Exited (137)`. Nothing runs in rs1 and the guest logged no restart. Earlier, by chance: a save (`lazysave`) taken 20 s after a lazy resume recorded only `web` (dockerd started it 250 ms before the freeze, the guest started `norestart` 100 ms after it), so the deploy `lz1` ran `web` but not `norestart`.

Expected: until this boot's `restartDocker` has run, a freeze keeps the recorded list or records the union of it and what runs. The same applies to a stop issued in that window (`stopDocker` records the same way; not tried).

### 5. Erasing the sole member of an organization leaves the organization behind, and a pending invite still lets people join it, as members with no owner (stuck)

Severity: stuck. Round 19's fix refuses `POST /account/erase` only while the owner's organization has *other members*. An owner who is the only member can erase: their `org_members` row is deleted, but the organization, its wallet and its pending invites stay. Anyone holding an invite code can still join. They become a plain `member` of an organization whose `ownerId` is the erased account. Nobody can invite, delete the organization or move its credits. Every such call answers 403 (`Your role in dlaunder cannot do this`, `Only owners and admins move credits out of an organization`), and no API puts an owner back.

Where: `internal/server/feat_retention.go:376–383` (only counts current members) and `:395` (removes the membership but leaves `orgs` and `org_invites`), together with the invite redemption in `internal/server/orgs.go:751` (no check that the organization still has an owner).

Repro: dave (owner and only member of `dlaunder`): `crowdvm org invite carol@r20.test --org dlaunder`. dave: `POST /account/erase {"confirmation":"erase my account"}` (browser session from `crowdvm dashboard`) → `account.erased`. carol: `crowdvm org join <code>`, then `org members --org dlaunder`, `org invite bob@r20.test --org dlaunder`, `org delete --org dlaunder`, `credits transfer --from dlaunder --to carol --amount 0.01`.

Observed: join → `"ownerId":"usr_zrs387qk4y0b2rn2"` (erased dave), `"role":"member"`. Members list: only carol, `role: member`. Invite, delete and transfer all answer 403. The org row `team_4kfag6vp8wtt71fgtmp5|dlaunder` is still in `orgs`. Erasing an owner of an org with members is correctly refused (409 `owner_of_organization`, checked live with carol's `acme`).

Expected: erasing the last member deletes the organization (as `org delete` would) and revokes its invites, or refuses like the other case. A join into an organization without an owner is refused.

### 6. The browser view's `profile` accepts `/home` and `/opt`, and the guest takes ownership of that folder (mode 0700): commands stop working, or Chrome and computer use break for good (stuck)

Severity: stuck. `POST /sandboxes/{id}/browser {"profile": …}` refuses `/home/user` and a list of system folders, but not `/home` or `/opt` (also `/mnt`, `/srv`, `/media`). The guest then runs `MkdirAll(profile)`, `Chown(profile, crowdvm-browser)` and `Chmod(profile, 0700)` on the real folder:
- `profile: "/home"`: `/home` becomes `crowdvm-browser 700`, so the sandbox user cannot reach `/home/user`. Every command fails (`crowdvm exec …` → `could not start command: fork/exec /bin/bash: permission denied`, HTTP 500, and `--cwd` must be under `/home/user` or `/tmp`). The integrated agents and SSH sessions use the same home. This lasts until the next stop and resume, which mounts the data partition fresh. Inside the browser's own namespace, binding the profile back over `/home` also undoes the tmpfs that hides `/home/user` (DESK-017): `nsenter -t <chrome> -m ls /home/user` lists the real home. Its mode 750 still blocked reads in my test.
- `profile: "/opt"`: `/opt` becomes `crowdvm-browser 700` and stays that way across a restart. For the sandbox user, `google-chrome` is "command not found" and `/opt/crowdvm/cua-driver` is "Permission denied", so Chrome and the computer-use MCP server that every integrated agent is registered with stop working, until someone runs `sudo chown root /opt; sudo chmod 755 /opt` by hand.

Anyone with the `desktop` action on the sandbox can make this call, including an org member on a shared sandbox or an API key scoped to the desktop.

Where: `internal/server/feat_desktop.go:305` (`systemDirs` misses `/home`, `/opt`, `/mnt`, `/srv`, `/media`), `internal/guest/browser.go:42` (same list) and `:180–184` (chown and chmod of an existing folder).

Repro: on d1, `POST /api/v1/sandboxes/bx_dp3xqxaz/browser {"profile":"/home"}` → 200 `"isolated":true`, then `crowdvm exec d1 -- ls ~`. Stop and resume (works again). Then `{"profile":"/opt"}`, `crowdvm exec d1 -- bash -c 'stat -c "%U %a" /opt; ls /opt/crowdvm/cua-driver; google-chrome --version'`, then stop and resume and run it again.

Observed: `/home`: `root 755` before, then `fork/exec /bin/bash: permission denied` for every command until the restart. `/opt`: `crowdvm-browser 700 /opt`, `ls: cannot access '/opt/crowdvm/cua-driver/': Permission denied`, `google-chrome: command not found`, the same after the restart.

Expected: the profile has to be a new folder (or one already owned by `crowdvm-browser`) under an allowed root such as `/data` or `/var/lib/crowdvm/…`. A folder that exists and belongs to someone else is refused, never chowned.

## Round-19 fixes rechecked live

| r19 item | Result |
|---|---|
| Sparse snapshot upload (r18-opus 2), ordinary sandbox | Holds. In d2: `dd` 3000 MB of random data, `rm`, `sudo fstrim -av` (37 GiB trimmed on `/home`) in one command. The layer that held it was 3 149 856 768 bytes on `r20n1` (holes) and went up as **8 912 896** bytes (`transfer started … bytes=8912896`). d2 resumed on `r20n2`: marker intact, the deleted file gone, Docker volume and containers intact. |
| Snapshot while the chain is still lazy | Data holds. d2 with a kept 3000 MB random file (`sha256 0455130d…`), stopped, resumed on `r20n1` (`lazy boot, 2 of 5 chain layers remote`, `hydrated=0`), marker written and `crowdvm snapshot d2 lazysave` at once. A deploy of `lazysave` on `r20n2` and a live fork of d2 to `r20n2` both had the marker and the same sha256. That deploy is where bug 4 first showed. |
| `--new-key` copy's first snapshot | Not rechecked (disk space, see below). |
| `bx-` subdomain reservation (r19-opus 1) | Holds. carol: `subdomain c2 bx-fw2ned3w` (bob's id label) and `bx-zzzzzzzz` → 400 `Subdomains starting with bx- are reserved for sandbox ids.`; her own `bx-snzhw82z` and `carolapp` are accepted; bob's `bx-fw2ned3w-8080` URL kept serving his app. |
| Failed save and the allowance (r19-opus 2) | Holds. While `failsave` was saving (2 MB/s), `used` was 4. After the save timed out (`save_timeout: the snapshot was not stored in time`, I moved `saving_since` back 2 h), `used: 3, extra: 3` with allowance 0, and the daily charge was `3 saved snapshots above the included 0`. |
| Resume with `allowedSizes` (r19-sonnet 8) | Not rechecked live (time). |
| Quota expiry (r19-sonnet 7) | The plain case holds (carol's unused 9.996 expired, her usage was not taken). The round-trip transfer case is bug 3. |
| Box alias raw uploads (r19-sonnet 2, 3) | Holds. `PUT /api/box/v1/boxes/<id>/files/raw` with `Content-Type: application/json`: a JSON file with `"sandbox"`, `"boxes"` keys and a 2^53+1 number arrived byte-identical (same sha256), and a 20 MB random body too (no 8 MiB cap). A raw GET through the alias comes back as `application/octet-stream`, unchanged. |
| Erase as an org owner (r19-sonnet 5) | Holds for an org with members (carol, owner of `acme` with bob: 409 `owner_of_organization`). The sole-member case is bug 5. |

## New ground: what held

- **Environment contents at create:** variables in `/etc/profile.d/crowdvm-env.sh` and the `~/.bashrc` block, secret files with 0600 (`.config/app/creds`, and `pub/.env` written into the repo folder before the clone and merged with it), public http and private https repos cloned (once `GIT_SSL_NO_VERIFY` was set for my self-signed cert), the token in `~/.config/crowdvm/git-credentials` and the clone's `credential.helper`.
- **Live upgrades:** `env rm-file` + `rm-var` + `upgrade` removed the file and the variable from a running sandbox. Variables, secret files and the in-sandbox CLI key were withdrawn by `safe-for-third-parties`; the git token was not (bug 2).
- **Share to another account:** bob saved e1 and shared it, carol deployed it on the other node. Gone: both repo clones, `pub/.env`, `.config/app/creds`, `git-credentials`, bob's variables, bob's command history (only carol's 3 commands listed). `~/.config/crowdvm/config.json` held carol's own sandbox key. bob's plain file `~/bobfile` stayed, as designed. A grep for the token, the variable value and the file secret over `/home /etc /var/lib/crowdvm /root /tmp` found only carol's own grep command.
- **Deleting an environment** with a stopped sandbox pinned to it: `env rm dev` worked, `resume e1` booted on its version (v13), and a fork of it got the same version.
- **Docker across stop/resume:** a container with `unless-stopped`, one without a policy and one stopped by hand. After stop + resume both running ones ran again, the stopped one stayed stopped, the named volume kept its data, and the published port 8080 answered through the public hosted URL. That held after a live fork to the other node (image built in the sandbox kept), after a node evacuation (d2 moved `r20n2` → `r20n1` with the volume, both containers and the hosted URL working), and after restores and deploys of snapshots taken more than a few seconds after boot. Bug 4 covers the snapshots taken right after a boot.
- **Evacuation of a pinned sandbox:** d1, created with `--node r20n1`, was stopped and got an `evacuation_stranded` notice (`r20n2: not the pinned node`). That is consistent with pinning. A second sandbox stranded for lack of disk on the target (my 35 GiB nodes).
- **Desktop driving:** `desktop/status`, `POST /desktop` (token URL), and `desktop/screenshot` (615 KB PNG).
- **Computer use:** inside a sandbox, both MCP servers answer `initialize` and `tools/list` on stdio: the Cua Driver (66 tools, through `/run/ascii-cua/driver.sock`) and `crowdvm-guest mcp-computer` (20 tools). `launch_app` runs `bash -lc` and `browser_navigate` passes the URL to Chrome as an argument. The agent has a shell anyway, so neither crosses a boundary.

## Not verified

- **`--new-key` copy's first snapshot.** Its re-encryption writes all 51 GB of the disk on the node before `fstrim` gives it back, and the host's only filesystem had 66 GB free and is shared with other sessions. The ordinary-sandbox case above exercises the same compaction code.
- **Resume with `allowedSizes`** was not run live (time).
- **A stop right after a resume** (bug 4's `stopDocker` path) was not tried. By the code it records the same way.
- **Org environments** (`env access`, member forks of org sandboxes with an org environment), registry logins and setup scripts were not run.
- The browser view could only be checked through the API and `nsenter`. I did not drive the `:2` Chrome over VNC, for example with `file:///home/user` after bug 6's `/home` profile.

## Cleanup

Deleted every sandbox (`e1`, `e1f`, `d2`; earlier `b1`, `c1`, `c2`, `d1`, `d1f`, `d2f`, `lz1`, `rs1`) and every saved snapshot (`e1save`, `lazysave`, `racesave`, `failsave`), bob's environment `dev` and git source. dave's account was erased in the test. All 11 deletion operations ended `completed`, all 155 snapshots were purged, and both nodes' `vm/vms` and `vm/disks` held no sandbox files (only the base image). Then I stopped both node units (`systemctl --user kill --kill-whom=all`, `stop`, `reset-failed`; no QEMU process of mine was left), then the server, the throttle proxy and both git servers. Nothing of mine listens on 9960–9979, no process matches `crowdvm-r20opus`, and no `r20opus-*` user unit is loaded. `~/.local/share/crowdvm-r20opus` (binaries, node and server dirs, git repos and certificate, logs, scratch homes) is deleted. The worktree `/home/tl/dev/CrowdVM-r20opus` was removed after this report was copied to `docs/reviews/bugs-r20-opus.md` in the shared checkout (untracked). I touched nothing else there, and did not touch `crowdvm-field`, the e2e clusters, other worktrees, the user's systemd units or `~/.npmrc`.

## Fixes

All 6 fixed. Tests: `internal/guest/fix_r20_test.go` (1), `internal/server/fix_r20_test.go` (2, 3, 6), `internal/server/bughunt_r20_test.go` `TestBughuntR20EraseLeavesSoleOwnedOrganizationSecrets` (5). Bug 4 has no unit test (it needs a Docker daemon); the e2e `docker-resume` covers the area.

1. **http git host gets no token** (`guest/setup.go`): `http` clone URLs get a credential line too.
2. **Withheld repos keep their token** (`feat_env_guest.go` `envApplyLive`): a live apply without repositories sends `"repos": []`, so the guest drops the old list and the scrubbed credential file stays deleted.
3. **Quota moved away and back** (`acct_billing.go`): transfers count toward the spent quota only as net transfers out since the grant.
4. **Snapshot right after a boot records no containers** (`guest/boot.go`, `docker.go`): until this boot has restarted the recorded containers, a freeze or stop keeps the recorded list.
5. **Erased last member leaves a joinable org** (`orgs.go` `deleteOrg`, `feat_retention.go`): erase deletes every organization the account owns alone (bugs-r20-sonnet 3), and deleting an organization removes its pending invites.
6. **Browser profile `/home` or `/opt`** (`feat_desktop.go`, `guest/browser.go`): a profile must be below a top-level directory, and the guest refuses an existing folder that belongs to another user instead of taking it over.

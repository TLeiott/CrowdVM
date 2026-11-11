# Migrating from Boat

CrowdVM follows Boat's v1 API and CLI closely, so most code and habits carry over.
The differences come from running on your own machines instead of Boat's cloud, and
from CrowdVM's own names.

## What changes for you

| Boat | CrowdVM |
|---|---|
| Boat's cloud | Your server (`crowdvm-server`) and your colleagues' machines (`crowdvm-node`) |
| `https://…` API host | `http://<server>:8420/api/v1` (HTTPS only with `--tls-cert`) |
| Plans, cards, invoices | Credits: 1 credit = 1 hour of a `default` sandbox; node owners earn what sandboxes spend. See [Node owner guide](node-owners.md#credits) |
| Regions | Node labels and placement (`--node-label`, `--min-tier`, `--gpu`) |
| Boat's public URL domain | `<server IP>.nip.io` or your `--public-domain`. See [Public URLs](public-urls.md) |
| Operator-run isolation | Node owners can read the sandboxes they host. See [Security model](security.md) |

## Names

| What | Boat | CrowdVM | Boat name still accepted? |
|---|---|---|---|
| CLI | `boat` | `crowdvm` | Inside sandboxes, `boat` and `host` point to `crowdvm` |
| API keys | `boat_…` (legacy `box_…`) | `cvm_…` | Yes, for keys from `crowdvm api-key create` or the dashboard: `boat_<rest>` and `box_<rest>` work for the key `cvm_<rest>`. Not for the `cvms_…` token that `crowdvm login` saves; it works only as written |
| Sandbox ids | `bx_…` | `bx_…` | No, ids are new. Use the ids CrowdVM returns |
| Snapshot ids | | `snap_…` | |
| Share codes | `bsh_…` | `bsh_…` | Same prefix; codes minted before CrowdVM switched to `bsh_` (`csh_…`) still work |
| API base env | `BOAT_API_BASE` (SDKs), `BOAT_API_URL` (CLI) | `CROWDVM_API_BASE` (also `CROWDVM_API_URL`) | Yes: the CLI also reads `BOAT_API_URL` and `ASCII_API_URL` |
| API key env | `BOAT_API_KEY` | `CROWDVM_API_KEY` | Yes (CLI) |
| Sandbox token env | `ASCII_TOKEN` | `CROWDVM_TOKEN` | Yes, both are set in every sandbox |
| Org env | `BOAT_ORG` | `CROWDVM_ORG` | Yes (CLI) |
| No auto-update | `BOAT_NO_UPDATE=1`, `--no-update` | `CROWDVM_NO_UPDATE=1`, `--no-update` | Yes |
| CLI config | `~/.config/ascii/boat/config.json` | `~/.config/crowdvm/config.json` | Inside sandboxes the old path links to the new one |
| Org header | `X-Boat-Org` | `X-CrowdVM-Org` | Yes |
| Delete confirmation header | `X-Ascii-Confirm-Delete` | `X-CrowdVM-Confirm-Delete` | Yes |
| Webhook headers | `X-Ascii-Signature`, `-Delivery`, `-Timestamp`, `-Event`, `-Attempt` | `X-CrowdVM-*` | Every delivery carries both sets |
| Ignore file | `.boatignore` | `.crowdvmignore` | Yes, also `.boxignore` |
| Pre-stop hook | `~/.boat/pre-stop` | `~/.crowdvm/pre-stop` | Yes |
| CI runner labels | `boat`, `boat-small`, `boat-large`, `boat-xlarge` | `crowdvm`, `crowdvm-small`, `crowdvm-large`, `crowdvm-xlarge` | No; options like `/from=<saved>` and `/debug` stay |

## Moving code over

**Boat SDKs and tools.** Point them at the CrowdVM server and give them a CrowdVM key.
Request and response bodies, error codes and the `ok`/`type` envelope are the same:

```sh
export BOAT_API_BASE=http://192.168.1.10:8420/api/v1  # or whatever variable your tool reads
export BOAT_API_KEY=boat_<the part of your cvm_ key after "cvm_">
```

Make that key with `crowdvm api-key create NAME` or in the dashboard. Do not copy the key
from `~/.config/crowdvm/config.json`: `crowdvm login` saves a `cvms_…` token there, and
no `boat_` or `box_` form of it is accepted (401). Send a `cvms_…` token as it is, as
`Authorization: Bearer cvms_…`.

Or switch to the CrowdVM SDKs in `sdk/python` and `sdk/typescript`, which keep Boat's
method names. See [API and SDKs](api-and-sdks.md).

**Scripts that call the `boat` CLI.** Replace `boat` with `crowdvm`. Commands, flags and
JSON events match; the CLI adds node and credit commands (`crowdvm nodes`,
`crowdvm credits`). Sandbox ids keep the `bx_` prefix, but the ids themselves are new,
so replace stored ids with the ones CrowdVM returns.

**The legacy Box API.** Tools built for Boat's predecessor, such as the `harbor-box`
adapter and `@asciidev/eve-box`, work against `http://<server>:8420/api/box/v1`
(their `base_url`/`baseUrl` option). CrowdVM serves it as a rename layer over
`/api/v1`, with `Deprecation` and `Sunset` headers like Boat's.

**Saved snapshots and data.** Nothing moves automatically. Recreate environments with
`crowdvm env`, and rebuild prepared sandboxes, then save them with
`crowdvm snapshot ID NAME`. To carry files over, copy them with `crowdvm push` or
`crowdvm scp`.

## What CrowdVM does not have

- An SLA, a status page or compliance certifications: the pool is as reliable as the
  machines in it.
- Payment. Credits are an accounting tool; admins grant them.
- IPv6 inside sandboxes.

[PARITY.md](../PARITY.md) lists every Boat feature with its CrowdVM status.

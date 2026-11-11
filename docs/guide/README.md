# CrowdVM user guide

CrowdVM runs persistent Ubuntu VMs ("sandboxes") for people and coding agents on
machines your team already owns. One machine runs `crowdvm-server`. Every machine
that lends compute runs `crowdvm-node`. Everyone else uses the `crowdvm` CLI, the
dashboard, the HTTP API or the SDKs.

| Page | Read it when |
|---|---|
| [Getting started](getting-started.md) | You set up a server, add nodes on Linux or Windows, or connect over Tailscale |
| [The CLI for agents](cli-for-agents.md) | A coding agent (or a script) should create and use sandboxes |
| [Sandboxes, snapshots and forks](sandboxes.md) | You want to know what stop, resume, fork and saved snapshots do |
| [Public URLs](public-urls.md) | You want to open a web app that runs in a sandbox, incl. Vite |
| [Security model](security.md) | You lend a machine, or you put other people's work into sandboxes |
| [Node owner guide](node-owners.md) | You run a node: benchmark, tiers, capacity, network policy, credits |
| [API and SDKs](api-and-sdks.md) | You call the API from code |
| [Troubleshooting](troubleshooting.md) | Something does not start, connect or load |
| [Migrating from Boat](migrating-from-boat.md) | You have code or habits from Boat (boat.dev) |

For coding agents, the server serves these pages as Markdown: `/llms.txt` lists every
page, `/llms-full.txt` has all of them, `/docs/<page>.md` has one (for example
`/docs/cli-reference.md`, generated from the CLI, and `/docs/api-reference.md`, generated
from the live API). A docs MCP server with search runs at `/mcp`:
`claude mcp add --transport http crowdvm-docs http://<server>:8420/mcp`.

Two more documents are for contributors: [ARCHITECTURE.md](../ARCHITECTURE.md)
describes the components and wire contracts, and [PARITY.md](../PARITY.md) maps
every Boat feature to its CrowdVM status.

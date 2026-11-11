# AgentEnv on CrowdVM

[AgentEnv](https://www.agentenvframework.com/) already ships a `boat` sandbox provider with a configurable `api_url`, so there is
nothing to install: point it at your CrowdVM server.

```toml
# .agentenv/config.toml (see config.toml.example)
[sandbox.providers.boat.config]
api_key = "env:CROWDVM_API_KEY"                 # a cvm_... key, e.g. crowdvm api-key create agentenv --preset ci
api_url = "http://192.168.1.10:8420/api/v1"

[sandbox]
default = "boat"
agent_default = "boat"
```

```bash
pip install 'agentenv-framework[boat]'
agent-env run hello --sandbox boat
```

What the provider asks of the API (and CrowdVM serves): create with the smallest fitting size, `noEnv: true`,
`snapshots: false`, a TTL, labels `agentenv.managed=true` / `agentenv.attr.*` and `hostedPorts`; find-by-label for crash
cleanup (`GET /sandboxes?label=key=value`); delete with `X-Ascii-Confirm-Delete`; up to 6 retries on 429/503.
`check_provider.py` replays exactly those calls against a server so you can verify a deployment (or a key) before a run:

```bash
CROWDVM_API_KEY=cvm_... CROWDVM_API_BASE=http://<server>:8420/api/v1 python3 check_provider.py
```

Refused by the provider itself, as on Boat: an `image` override and network policies that restrict egress (CrowdVM has no
egress allow-list API either: node owners set egress policy per node).

If the provider insists on a `boat_` key prefix, replace your key's `cvm_` prefix with `boat_` (`cvm_abc…` becomes
`boat_abc…`): the server accepts `boat_` and `box_` in place of `cvm_` on any key, so no patch is needed.

Tests: `python3 -m unittest discover -s tests -t .`

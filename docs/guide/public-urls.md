# Public URLs

`crowdvm host` gives a port in a sandbox a stable URL that anyone who can reach the
server can open. The server proxies the request through the node into the sandbox, so
the sandbox needs no open ports and the node needs no port forwarding.

```sh
crowdvm exec "$id" --detach "npm run dev -- --port 3000"
crowdvm host "$id" 3000                 # private URL (carries ?_token=)
crowdvm host "$id" 3000 --public        # no token
crowdvm host list "$id"
crowdvm host hide "$id" 3000            # take it down
```

The app can listen on `localhost` or `0.0.0.0`: the server reaches it from inside the
sandbox. Ports 22, 5900 to 5902, 6080 and 7070 are used by CrowdVM and cannot be hosted.

## The two URL forms

| Form | Example | Notes |
|---|---|---|
| Host routing | `http://sb-qzdsd908dm-3000.192.168.1.10.nip.io:8420/` | The app runs at `/`, like on a real domain. Needs wildcard DNS. |
| Path routing | `http://192.168.1.10:8420/p/bx_qzdsd908dm/3000/` | Works with any address, including Tailscale MagicDNS names. The app runs under a path prefix. |

In host routing the first label is the sandbox's subdomain (`crowdvm subdomain ID
my-app` sets one) or its id with `_` replaced by `-`, followed by `-<port>`.

## The domain

The server builds host-routing URLs from `--public-domain`:

- **Default:** `<server IP>.nip.io`, taken from `--public-url` when it is an IP address.
  [nip.io](https://nip.io) is a public DNS service that answers
  `anything.192.168.1.10.nip.io` with `192.168.1.10`. It works for LAN and Tailscale
  IPs, but each client needs public DNS. Some routers block answers that point to
  private addresses ("DNS rebinding protection"); then use your own domain or path URLs.
- **Your own domain:** create a wildcard record `*.crowdvm.example.lan → <server IP>` in
  your DNS and start the server with `--public-domain crowdvm.example.lan`.
- **`localhost`:** `http://<label>-<port>.localhost:8420/` always works on the server
  machine itself, because browsers resolve `*.localhost` to the local machine.

## Private and public URLs

A private URL carries `?_token=...`. The first visit sets a cookie for that host and
redirects to the clean URL, so links inside the app keep working. Anyone with the
token URL can open the app: share it like a password. `crowdvm host url "$id" 3000`
prints the full URL again; hiding a port keeps its token, so the same URL works after
you host the port again.

A `--public` URL needs no token. Anyone who can reach the server can open it.

## Dev servers that check the Host header

The proxy forwards the original `Host` header, for example
`sb-qzdsd908dm-5173.192.168.1.10.nip.io:8420`. Dev servers that only accept known hosts
reject it.

**Vite** answers "Blocked request. This host is not allowed." Allow the CrowdVM domain
in `vite.config.ts`:

```ts
export default defineConfig({
  server: {
    allowedHosts: ['.nip.io'],          // or your --public-domain, e.g. '.crowdvm.example.lan'
  },
})
```

A leading dot allows every subdomain. `allowedHosts: true` allows all hosts; only use
that for throwaway sandboxes. Path URLs (`/p/<id>/<port>/`) use the server's own
address as the host, which Vite allows by default when it is an IP, but the app then
runs under a path prefix. Set Vite's `base` to `/p/<id>/<port>/` for that, or use host
routing.

Other tools have the same switch: webpack-dev-server `allowedHosts`, Angular
`ng serve --allowed-hosts`. Django needs the domain in `ALLOWED_HOSTS`, Rails in
`config.hosts`.

Hot reload uses a websocket on the same URL. The proxy passes websocket upgrades
through, so it needs no extra port.

## Path-routed apps

Responses under `/p/...` carry a `Content-Security-Policy: sandbox` header so a hosted
app cannot act as the dashboard, which shares that origin. Cookies and `localStorage`
of the app are therefore limited. Use host routing for apps that sign users in.

## Limits

- 50 hosted ports per sandbox.
- A URL answers 502 "Nothing answered on port N" until the app listens, and stops
  working while the sandbox is stopped.
- Optional per-port IP rules: `POST /api/v1/sandboxes/{id}/host` takes
  `firewall: {allow: [CIDR…], deny: [CIDR…]}`. Rules are checked before the token.

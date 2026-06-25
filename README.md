# proxy-mcp

An aggregating [MCP](https://modelcontextprotocol.io) proxy: it fronts one or
more upstream MCP servers (stdio / SSE / streamable-http) and exposes them over
a single HTTP endpoint, each mounted at `/<name>/`.

It adds a real **readiness gate** on top of the proxy core, closing the
"fails on first load" race where a client (or a socket-activated dependent
service) reaches the proxy after the port binds but before upstream routes are
registered.

## Build

```sh
just build          # -> ./bin/proxy-mcp
just install        # -> $GOBIN/proxy-mcp
nix build .#proxy-mcp
```

## Run

```sh
proxy-mcp --config config.json   # or: -c config.json
```

GNU-style flags (each has a `--long` form; most a short alias):

| Flag | Short | Default | Meaning |
| --- | --- | --- | --- |
| `--config` | `-c` | `config.json` | config file path or http(s) URL |
| `--insecure` | `-k` | `false` | skip TLS verification for the config URL |
| `--expand-env` | `-e` | `true` | expand `$VARS` in the config |
| `--http-headers` | `-H` | — | headers for the config URL (`'K1:V1;K2:V2'`) |
| `--http-timeout` | `-t` | `10` | config-URL fetch timeout (seconds) |
| `--validate` | `-V` | `false` | validate config and exit (no server) |
| `--idle-timeout` | `-i` | `0` | exit after this much idle time with no proxied requests (e.g. `5m`); `0` disables |
| `--version` | `-v` | | print version and exit |
| `--help` | `-h` | | print usage and exit |

## Configuration

See `config.json` for a worked example. Minimal shape:

```json
{
  "mcpProxy": {
    "baseURL": "http://localhost:9090",
    "addr": ":9090",
    "name": "proxy-mcp",
    "version": "1.0.0",
    "type": "streamable-http"
  },
  "mcpServers": {
    "fetch": { "command": "uvx", "args": ["mcp-server-fetch"] }
  }
}
```

Each `mcpServers` entry is either stdio (`command` + `args` + `env`) or
HTTP (`url` + `headers`, optionally `transportType: "streamable-http"`).
Per-server `options` cover `authTokens`, `logEnabled`, `panicIfInvalid`,
`disabled`, a `toolFilter` (`allow`/`block` list), and `callTimeout` (a Go
duration like `"30s"` bounding each forwarded request — tool call, prompt get,
resource read, completion — so a hung upstream fails fast; empty/`"0"` disables).

## Transparency

The proxy aims to be invisible to both sides:

- **Header passthrough.** Every header the caller sends is forwarded verbatim to
  an HTTP/SSE upstream — `Authorization`, `Cookie`, custom `X-*`, all of it. Only
  the hop-by-hop framing headers (`Connection`, `Host`, `Content-Length`,
  `Transfer-Encoding`, …) are regenerated for the new hop, exactly as
  `net/http`/`httputil.ReverseProxy` do. (stdio upstreams have no HTTP hop, so
  there are no headers to carry.)
- **Notification relay.** Upstream notifications — progress, logging, resource
  updates — are forwarded to the connected clients. A streamable upstream is
  consumed with a continuous listening stream so even unsolicited notifications
  are seen.
- **Live `list_changed`.** When an upstream signals tools/prompts/resources
  `list_changed`, the proxy re-lists and re-registers that capability, then
  emits one `list_changed` to its clients — so a dynamic upstream tool set stays
  in sync instead of being frozen at connect time.
- **Full request forwarding.** Beyond tool calls, prompt gets, and resource
  reads, the proxy forwards `completion/complete` (argument autocomplete),
  `resources/subscribe` + `unsubscribe` (so the upstream actually emits
  `resources/updated`), and `logging/setLevel` to the upstream.
- **Mixed capability sets.** An upstream may expose any subset — tools-only,
  prompts-only, completion-only, etc. Missing capabilities are tolerated, not
  fatal, so the proxy fronts any MCP server.
- **Auto-reconnect.** If an upstream drops (stdio child crashes, HTTP upstream
  restarts), the proxy reconnects with exponential backoff and re-syncs its
  capabilities onto the same route — clients keep their connection and see a
  `list_changed` when it returns, rather than a permanently dead upstream.

## Connection modes

Each upstream's `options.mode` controls how connections are shared:

- **`perSession`** (default) — every downstream client gets its **own** upstream
  connection. This makes the proxy fully transparent, including **server→client
  requests**: `sampling`, `roots`, and `elicitation` are relayed to the exact
  client that triggered the call (1:1, no ambiguity). Cost: N clients ⇒ N
  upstream connections (N backend processes for a stdio upstream).
- **`shared`** — one upstream connection multiplexed across all clients (a single
  backend process). Use this for a singleton backend you want exactly one of —
  e.g. a browser. server→client requests are **not** bridged in this mode: an
  upstream-initiated request can't be attributed to one of N clients. (Tool
  calls, notifications, completion, etc. all still work.)

Built on the official [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk),
so server→client requests bridge over **both stdio and streamable-HTTP**
upstreams.

Check a config without starting the server:

```sh
proxy-mcp --validate --config config.json   # exits 0 if valid, 1 otherwise
```

## Readiness

The proxy binds its listener and registers each `/<name>/` route
asynchronously as upstreams connect. To avoid serving before routes exist,
the readiness gate flips **once**, only after every enabled upstream is
resolved and its route registered:

| Signal | Behaviour |
| --- | --- |
| `GET /readyz` | `503` until ready, then `200`. JSON body: `{ready, degraded, upstreams}` with per-upstream `connected`/`failed`/`disabled` state. |
| `GET /healthz` | `200 {"status":"ok"}` once listening (liveness). |
| systemd `sd_notify` | `READY=1` to `$NOTIFY_SOCKET` (no-op when unset). |
| systemd watchdog | pings `WATCHDOG=1` at half `$WATCHDOG_USEC` when the unit sets `WatchdogSec`. |
| log line | `proxy ready: N upstreams connected, degraded=…` (stable, greppable). |

`degraded` is `true` when some upstream failed to connect but the proxy is
still serving the ones that did (only happens when `panicIfInvalid` is false).

Under systemd, run it as a `Type=notify` unit — the unit reaches
`active (running)` only after readiness, so any `After=`/`Requires=` dependent
never races a not-yet-registered route. For orchestrators without systemd,
gate on `GET /readyz`.

## Idle auto-shutdown

With `--idle-timeout` set, the proxy exits cleanly once it has gone that long
without a proxied request (counted from readiness, so a slow upstream cold-start
isn't held against the window). Probe traffic to `/healthz` and `/readyz` does
**not** count as activity, so a readiness poller can't keep it alive.

This makes the proxy a natural fit for pure socket activation: a systemd
`.socket` (or any inetd-style activator) starts it on the first connection, and
it shuts itself down when traffic stops — no external idle-watcher process, no
`socket-proxyd` front. Pair `--idle-timeout=5m` with an `Accept=no` socket unit
and a `StopWhenUnneeded`/`Restart=` service.

## Socket activation

When started with systemd socket activation (`$LISTEN_FDS`/`$LISTEN_PID` set,
the conventional first fd at 3), the proxy adopts the passed listening socket
instead of binding `mcpProxy.addr` itself — so the `.socket` unit owns the port
and survives across proxy restarts. Crucially, an activated proxy holds off
`Accept` until readiness: the connection that triggered activation waits in the
socket backlog through the upstream cold-start, then is served once routes are
registered, so it never races registration and 404s. Without activation the
proxy binds `addr` and serves immediately (external callers still gate on
`/readyz`).

Minimal pair (user units):

```ini
# proxy-mcp.socket
[Socket]
ListenStream=127.0.0.1:9090

[Install]
WantedBy=sockets.target
```

```ini
# proxy-mcp.service  (no [Install]; started by the socket)
[Service]
ExecStart=/path/to/proxy-mcp --config %h/.config/proxy-mcp/config.json --idle-timeout=5m
```

## Nix

The flake exposes a package, an overlay, and NixOS + home-manager modules. The
module runs proxy-mcp as a `Type=notify` service wired to the readiness gate:

```nix
# flake.nix inputs: proxy-mcp.url = "github:stubbedev/proxy-mcp";

# NixOS
imports = [ proxy-mcp.nixosModules.default ];
services.proxy-mcp = {
  enable = true;
  watchdogSec = "30s";                       # optional; restarts if wedged
  settings = {
    mcpProxy.addr = ":9090";
    mcpProxy.baseURL = "http://localhost:9090";
    mcpProxy.name = "proxy-mcp";
    mcpProxy.type = "streamable-http";
    mcpServers.fetch = { command = "uvx"; args = [ "mcp-server-fetch" ]; };
  };
  # environmentFile = "/run/secrets/proxy-mcp.env";  # for $TOKEN-style secrets
};
```

`homeModules.default` exposes the same `services.proxy-mcp` options as a
`systemd --user` service. Prebuilt closures are pushed to the
`nix.stubbe.dev` (`default`) attic cache on every master push + release tag.

## Docker

```sh
docker run --rm -v $PWD/config.json:/config.json:ro -p 9090:9090 \
  ghcr.io/stubbedev/proxy-mcp:latest
```

Multi-arch (`linux/amd64`, `linux/arm64`) images are published to GHCR on each
release tag. The base is alpine — derive from it to add stdio upstream
runtimes (`npx`, `uvx`, …).

## Development

```sh
just install-hooks  # enable the pre-commit format gate
just lint           # gofumpt + goimports + golines + golangci-lint
just lint-check     # read-only gate (what CI runs)
just test
just check          # lint + test + sync-flake
```

## Releasing

```sh
just release-preview         # show next major/minor/patch tags
just release-patch           # bump, sync flake, tag, push -> CI builds + publishes
```

Tagging `v*.*.*` triggers CI to cross-build linux/darwin × amd64/arm64,
publish a GitHub release, push the nix closure to `nix.stubbe.dev`, and bump
the Homebrew tap.

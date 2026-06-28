# proxy-mcp

An aggregating [MCP](https://modelcontextprotocol.io) proxy: it fronts one or
more upstream MCP servers (stdio / SSE / streamable-http) and exposes them over
a single HTTP endpoint, each mounted at `/<name>/`.

It adds a real **readiness gate** on top of the proxy core, closing the
"fails on first load" race where a client (or a socket-activated dependent
service) reaches the proxy after the port binds but before upstream routes are
registered.

## Install

### Homebrew (macOS / Linux)

```sh
brew install stubbedev/proxy-mcp/proxy-mcp
```

This taps `stubbedev/homebrew-proxy-mcp` and installs the prebuilt binary for
your platform (Apple Silicon + Intel macOS, arm64 + amd64 Linux). Upgrade with
`brew upgrade proxy-mcp`. Each release tag bumps the tap automatically.

The formula ships a `brew services` definition, so you can run one always-on
instance shared by every MCP client on the machine — see
[One shared instance (macOS)](#one-shared-instance-macos).

### Prebuilt binary

Grab a tarball for your OS/arch from the
[latest release](https://github.com/stubbedev/proxy-mcp/releases/latest)
(`darwin`/`linux` × `arm64`/`amd64`), verify its `.sha256`, and drop the
`proxy-mcp` binary on your `PATH`.

### Go

```sh
go install github.com/stubbedev/proxy-mcp@latest
```

### Docker / Nix

See [Docker](#docker) and [Nix](#nix) below.

## Build from source

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

## One shared instance (macOS)

Run a single always-on proxy that every Claude client on the machine shares —
one set of upstream backends across all your repos, instead of each client
spawning its own.

1. **Write a central config** at `$(brew --prefix)/etc/proxy-mcp/config.json`
   (e.g. `/opt/homebrew/etc/proxy-mcp/config.json`) listing every upstream you
   want, with `mcpProxy.addr` bound to a loopback port:

   ```json
   {
     "mcpProxy": { "addr": "127.0.0.1:9090", "baseURL": "http://127.0.0.1:9090", "type": "streamable-http" },
     "mcpServers": {
       "fetch":  { "command": "uvx", "args": ["mcp-server-fetch"] },
       "github": { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"] }
     }
   }
   ```

2. **Start the service** (launchd under the hood; `keep_alive` restarts it on
   crash and relaunches at login):

   ```sh
   brew services start proxy-mcp
   ```

3. **Point every client at the per-upstream URLs.** The proxy is not one merged
   endpoint — each `mcpServers` entry mounts at its own path `/<name>/`. For
   Claude Code, register them once with `--scope user` so they apply in *every*
   repo, not per-project:

   ```sh
   claude mcp add -t http -s user fetch  http://127.0.0.1:9090/fetch/
   claude mcp add -t http -s user github http://127.0.0.1:9090/github/
   ```

   Claude Desktop and other clients take the same `http://127.0.0.1:9090/<name>/`
   URLs over the streamable-HTTP transport.

Leave each upstream's `options.mode` at its `perSession` default so
server→client requests (`sampling`, `roots`, `elicitation`) bridge cleanly to
the right client; use `shared` only for a singleton backend (see
[Connection modes](#connection-modes)). Don't set `--idle-timeout` on an
always-on `brew services` instance — it should just stay up. To instead have the
proxy idle out and relaunch on demand (zero footprint while unused), run it
under launchd socket activation rather than `brew services` — see
[Socket activation › macOS](#macos-launchd).

Logs land at `$(brew --prefix)/var/log/proxy-mcp.log`. Stop/restart with
`brew services stop|restart proxy-mcp`.

## Configuration

Minimal shape:

```json
{
  "$schema": "https://raw.githubusercontent.com/stubbedev/proxy-mcp/master/config.schema.json",
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

`config.example.json` is an exhaustive worked example exercising every field
below across stdio, SSE, and streamable-HTTP upstreams.

The optional `$schema` line points editors at
[`config.schema.json`](https://raw.githubusercontent.com/stubbedev/proxy-mcp/master/config.schema.json),
which gives autocomplete and inline validation in VS Code and other JSON-Schema
aware editors. It is generated from the Go config structs (`go run
./cmd/schemagen` / `just schema`) and regenerated on every CI push, so it never
drifts from what the proxy actually accepts — do not edit it by hand.

### `mcpProxy` — the proxy's own listener

| Field | Type | Meaning |
| --- | --- | --- |
| `addr` | string | Listen address, e.g. `":9090"` or `"127.0.0.1:9090"`. Ignored under socket activation. |
| `baseURL` | string | Public base URL. Its path becomes the mount prefix; each upstream serves at `<baseURL path>/<name>/`. |
| `name` | string | Server name advertised to clients. |
| `version` | string | Server version advertised to clients. |
| `type` | enum | Downstream transport the proxy exposes: `streamable-http` (default) or `sse`. |
| `options` | object | Defaults inherited by every upstream that doesn't set its own — only `authTokens`, `logEnabled`, `panicIfInvalid` are inherited. |

### `mcpServers.<name>` — one entry per upstream

`<name>` is the route segment (`/<name>/`). Each entry is **stdio** (set
`command`) or **HTTP** (set `url`); `transportType` is optional and inferred.

| Field | Applies to | Type | Meaning |
| --- | --- | --- | --- |
| `transportType` | both | enum | `stdio`, `sse`, or `streamable-http`. Optional — inferred from `command` vs `url` (HTTP defaults to `sse` unless set to `streamable-http`). |
| `command` | stdio | string | Executable to spawn. |
| `args` | stdio | string[] | Arguments. |
| `env` | stdio | object | Extra environment on top of the proxy's own. |
| `url` | HTTP | string | Upstream endpoint. |
| `headers` | HTTP | object | Static headers added to every upstream request (on top of forwarded caller headers). |
| `timeout` | HTTP | duration-ns | Parsed but **currently inert** — no per-request HTTP timeout is applied. Use `options.callTimeout`. |
| `options` | both | object | Per-upstream options, below. |

### `options` (proxy-level defaults or per-upstream)

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `authTokens` | string[] | — | Bearer tokens required on the upstream's `/<name>/` route. Any one matches; empty = no auth. Per-upstream value overrides the inherited proxy default. |
| `logEnabled` | bool | `false` | Log requests on this route. Inherited from proxy if unset. |
| `panicIfInvalid` | bool | `false` | Fail the whole proxy if this upstream can't connect (instead of serving degraded). Inherited from proxy if unset. |
| `disabled` | bool | `false` | Skip this upstream entirely — no route registered. |
| `toolFilter` | object | — | `{ "mode": "allow" \| "block", "list": [...] }`. `allow` exposes only listed tools; `block` hides them. |
| `callTimeout` | duration | `0` | Bounds each forwarded request (tool call, prompt get, resource read, completion) so a hung upstream fails fast. Go duration like `"30s"`; empty/`"0"` disables. |
| `mode` | enum | `perSession` | `perSession` = one upstream connection per client (full server→client bridging); `shared` = one connection multiplexed across all clients (no server→client bridging). See [Connection modes](#connection-modes). |
| `idleTimeout` | duration | `0` | Per-upstream lazy mode: backend isn't started at boot but on first request to its route, then torn down after this idle span and revived on the next request. An in-flight call is never torn down under it; a held-open server→client stream doesn't count as activity, so a connected-but-idle client is still reclaimed. Go duration like `"5m"`; empty/`"0"` keeps it eager. Independent of the process-level `--idle-timeout`. |

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

## Live config reload

Adding or removing an `mcpServers` entry takes effect **without a restart**. The
proxy watches the config and reconciles the running upstreams against it:

- a new entry is connected and its route mounted;
- a removed entry is unmounted and its backend torn down;
- a changed entry is re-registered (remove + add). Changes to shared
  `mcpProxy.options` (e.g. `authTokens`) propagate to every upstream that
  inherits them.

Listener-level `mcpProxy` fields (`addr`, `type`, `baseURL`) can't change at
runtime; those still need a restart and are ignored on reload with a warning.

A **local-file** config is reloaded automatically when it changes on disk
(mtime poll). For an `http(s)` config — or to force a reload — send `SIGHUP`:

```sh
systemctl reload proxy-mcp          # systemd (or: systemctl --user reload …)
kill -HUP "$(pgrep -f proxy-mcp)"   # anywhere else
```

A reload that fails to load (bad JSON, missing `mcpProxy`) is logged and the
running config is kept untouched.

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

Under socket activation the proxy adopts a listening socket the init system
already opened instead of binding `mcpProxy.addr` itself — so the init system
owns the port and survives across proxy restarts. Crucially, an activated proxy
holds off `Accept` until readiness: the connection that triggered activation
waits in the socket backlog through the upstream cold-start, then is served once
routes are registered, so it never races registration and 404s. Without
activation the proxy binds `addr` and serves immediately (external callers still
gate on `/readyz`). Both activators are detected automatically — systemd on
Linux, launchd on macOS — so the same binary gives zero-idle on-demand start on
both platforms.

### Linux (systemd)

Detected via `$LISTEN_FDS`/`$LISTEN_PID` (conventional first fd at 3). Minimal
pair (user units):

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
ExecReload=/bin/kill -HUP $MAINPID
```

`ExecReload` lets `systemctl reload proxy-mcp` (or `--user`) trigger a live
config reload; the Nix module wires this automatically.

### macOS (launchd)

The macOS binary is built with cgo so it can adopt a launchd socket via
`launch_activate_socket`. Give the agent a `Sockets` entry named `Listeners`
(override with `$PROXY_MCP_LAUNCHD_SOCKET`) and **omit** `KeepAlive`, so launchd
starts the proxy on the first connection and `--idle-timeout` lets it exit when
quiet — full parity with the systemd `.socket` path, zero resident footprint
while idle. Drop this at `~/Library/LaunchAgents/dev.stubbe.proxy-mcp.plist` and
`launchctl load` it:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>            <string>dev.stubbe.proxy-mcp</string>
  <key>ProgramArguments</key>
  <array>
    <string>/opt/homebrew/bin/proxy-mcp</string>
    <string>--config</string>
    <string>/opt/homebrew/etc/proxy-mcp/config.json</string>
    <string>--idle-timeout=5m</string>
  </array>
  <key>Sockets</key>
  <dict>
    <key>Listeners</key>
    <dict>
      <key>SockNodeName</key>    <string>127.0.0.1</string>
      <key>SockServiceName</key> <string>9090</string>
    </dict>
  </dict>
</dict>
</plist>
```

This is the on-demand alternative to the always-on `brew services` instance
above; use one or the other, not both. (`brew services` keeps the proxy resident
with `KeepAlive`; this lets it idle out.)

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

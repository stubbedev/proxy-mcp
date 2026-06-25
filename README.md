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
proxy-mcp -config config.json
```

Flags: `-config` (file path or http(s) URL), `-insecure`, `-expand-env`,
`-http-headers`, `-http-timeout`, `-version`, `-help`.

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
`disabled`, and a `toolFilter` (`allow`/`block` list).

Check a config without starting the server:

```sh
proxy-mcp -validate -config config.json   # exits 0 if valid, 1 otherwise
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

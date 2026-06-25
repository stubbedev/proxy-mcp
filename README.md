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

## Readiness

The proxy binds its listener and registers each `/<name>/` route
asynchronously as upstreams connect. To avoid serving before routes exist,
three readiness signals fire **once**, only after every enabled upstream is
connected and its route is registered:

| Signal | Behaviour |
| --- | --- |
| `GET /readyz` | `503 starting` until ready, then `200 ok`. |
| `GET /healthz` | `200 ok` once the server is listening (liveness). |
| systemd `sd_notify` | `READY=1` sent to `$NOTIFY_SOCKET` (no-op when unset). |
| log line | `proxy ready: N routes registered` (stable, greppable). |

Under systemd, run it as a `Type=notify` unit — the unit reaches
`active (running)` only after readiness, so any `After=`/`Requires=` dependent
never races a not-yet-registered route. For orchestrators without systemd,
gate on `GET /readyz`.

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

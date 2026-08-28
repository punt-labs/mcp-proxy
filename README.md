# mcp-proxy

> Go binary that bridges MCP stdio to a shared daemon over WebSocket.

[![License](https://img.shields.io/github/license/punt-labs/mcp-proxy)](LICENSE)
[![CI](https://img.shields.io/github/actions/workflow/status/punt-labs/mcp-proxy/test.yml?label=CI)](https://github.com/punt-labs/mcp-proxy/actions/workflows/test.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/punt-labs/mcp-proxy.svg)](https://pkg.go.dev/github.com/punt-labs/mcp-proxy)

`mcp-proxy` is a Go binary that Claude Code spawns instead of an MCP server process. It forwards MCP JSON-RPC over WebSocket to a single shared daemon and never inspects message content — any MCP server that exposes a WebSocket endpoint speaking MCP works with it, unchanged.

**Platforms:** macOS, Linux

```text
                    stdio                      WebSocket
Claude Code ◄──────────────► mcp-proxy ◄──────────────────────► daemon
             MCP JSON-RPC                                       (one process)
```

## Why

Ranked by how often it drives a project to reach for the proxy:

1. **Cardinality.** Many Claude Code sessions, one MCP server process. Shared state — ML models, connection pools, singleton devices, audio queues — loads once for the daemon instead of once per tab. On systems with several open Claude Code sessions this is the difference between an MCP server that fits in memory and one that doesn't.
2. **Reconnect logic.** When the daemon disconnects (crash, restart, network blip, transient hang), the proxy reconnects with exponential backoff and preserves in-flight stdin messages across the outage. A ping/pong keepalive detects silent hangs — connections whose TCP stays open but where the daemon has stopped processing — and tears them down so reconnect can proceed. Daemon failures do not cascade into Claude Code session failures.
3. **Upgrade without restart** — mostly a plugin-developer story. When the daemon is upgraded (redeploy, `brew upgrade`, `systemctl restart`, or a `go install` during a dev inner loop), the proxy reconnects transparently and replays the MCP `initialize` handshake so the fresh daemon inherits the client's negotiated session state. You do not restart Claude Code to pick up a new MCP server version. End users benefit from this on daemon upgrades and daemon crashes, but it matters most while you're actively iterating on an MCP server.

## Quick Start

```bash
curl -fsSL https://raw.githubusercontent.com/punt-labs/mcp-proxy/main/install.sh | sh
```

<details>
<summary>Manual install</summary>

```bash
mkdir -p ~/.local/bin
curl -fsSL https://github.com/punt-labs/mcp-proxy/releases/latest/download/mcp-proxy-darwin-arm64 -o ~/.local/bin/mcp-proxy
chmod +x ~/.local/bin/mcp-proxy
```

Substitute `darwin-arm64` for your platform: `darwin-amd64`, `linux-arm64`, `linux-amd64`. Ensure `~/.local/bin` is on your `PATH`.

</details>

<details>
<summary>Inspect before running</summary>

```bash
curl -fsSL https://raw.githubusercontent.com/punt-labs/mcp-proxy/main/install.sh -o install.sh
cat install.sh
sh install.sh
```

</details>

Then point Claude Code at the proxy instead of the direct MCP server:

```json
{
  "mcpServers": {
    "quarry": {
      "type": "stdio",
      "command": "mcp-proxy",
      "args": ["ws://localhost:8420/mcp"]
    }
  }
}
```

## Commands

The binary has three modes, selected by flags. The daemon URL is supplied either as a positional argument or via `--config <profile>` (see [Configuration](#configuration)); a positional URL wins over the config's URL. In hook mode the URL and the `--hook <event>` flag can appear in either order.

| Invocation | What it does |
|------------|--------------|
| `mcp-proxy <url>` | Proxy mode. Reads JSON-RPC from stdin, forwards each line as a WebSocket text frame to the daemon, writes daemon responses (and server-initiated messages) to stdout. Reconnects on daemon disconnect. |
| `mcp-proxy --health [<url>]` | Health check. Dials the daemon, closes immediately. Exits `0` on success, `1` on failure. Prints `mcp-proxy: ok` or a diagnostic to stderr. |
| `mcp-proxy [<url>] --hook <event>` | Hook relay. Reads stdin, wraps it as `params` in a JSON-RPC request with method `hook/<event>`, and sends it to the daemon's `/hook` endpoint. Waits for the response and writes it to stdout. |
| `mcp-proxy [<url>] --hook --async <event>` | Async hook relay. Same as above but sent as a notification (no `id`), and the proxy performs a graceful WebSocket close to guarantee delivery. |
| `mcp-proxy --config <profile> [<url>]` | Read connection details (URL and headers) from `~/.punt-labs/mcp-proxy/<profile>.toml`. Combines with any of the modes above. |

Messages are opaque bytes end-to-end — the proxy never parses JSON.

## Configuration

### Environment Variables

| Variable | Default | Effect |
|----------|---------|--------|
| `MCP_PROXY_TOKEN` | *(unset)* | Bearer token sent as `Authorization: Bearer <token>` on the WebSocket upgrade. |
| `MCP_PROXY_PING_INTERVAL` | `5s` | How often the proxy sends WebSocket ping frames. Set to `0` to disable keepalive. |
| `MCP_PROXY_PONG_TIMEOUT` | `2s` | How long to wait for a pong before treating the daemon as unresponsive and reconnecting. |
| `MCP_PROXY_DEBUG` | *(unset)* | `1` logs to a temp file; a path logs to that path. Log file is created with `0600` permissions. |

### Config File

Instead of passing a URL directly, store connection details in a profile:

```bash
mcp-proxy --config quarry
```

Reads `~/.punt-labs/mcp-proxy/quarry.toml`:

```toml
[quarry]
url = "ws://okinos.user.home.lab:8420/mcp"

[quarry.headers]
Authorization = "Bearer <token>"
```

The proxy enforces `0600` permissions on this file and exits with an error if permissions are wider. If the section is absent, the proxy falls back to `ws://localhost:8420/mcp`. An explicit URL on the command line takes precedence over the config's `url`; headers still apply.

### Exit Codes

| Code | Meaning |
|------|---------|
| `0` | Clean shutdown (stdin EOF), health check success, or hook success |
| `1` | Runtime error, health check failure, or daemon error response |
| `2` | Usage error (missing or malformed arguments) |

### Signal Handling

The first `SIGINT` or `SIGTERM` triggers a graceful shutdown: close the WebSocket, drain, exit `0`. A second signal force-exits immediately.

## Daemon Requirements

An MCP daemon that mcp-proxy connects to must:

1. Accept WebSocket connections with the `mcp` subprotocol (`Sec-WebSocket-Protocol: mcp`).
2. Speak MCP JSON-RPC 2.0 — one JSON object per WebSocket text frame.
3. Be running before the proxy connects. The proxy retries with exponential backoff if the daemon is unreachable, but does not start it.

Optionally, the daemon can read `?session_key=<pid>` from the WebSocket upgrade URL to maintain per-session state, and push server-initiated messages (e.g. `notifications/tools/list_changed`) which the proxy forwards to stdout as they arrive.

For a full daemon-side integration guide including WebSocket ping/pong library notes, session identity, authentication, and the hook endpoint, see [docs/daemon-guide.md](docs/daemon-guide.md).

## How It Works

**Session identity.** The proxy walks the process tree upward to find the topmost `claude` ancestor PID (`ps -eo pid=,ppid=,comm=`), then passes it as `?session_key=<pid>` on the WebSocket upgrade. A daemon can key per-session state off this value.

**Bidirectional forwarding.** Two goroutines share one WebSocket connection: a scanner reads stdin and writes each line as a text frame; a reader reads frames from the daemon and writes them to stdout. The daemon can push unsolicited messages (e.g. `tools/list_changed`) at any time and they surface on stdout immediately.

**Reconnect.** On disconnect (TCP loss, WebSocket close, or pong timeout) the proxy reconnects with exponential backoff, capped at five seconds. Stdin messages queued during the outage are preserved and delivered on the next connection. Status is printed to stderr.

**Keepalive.** The proxy sends WebSocket pings at `MCP_PROXY_PING_INTERVAL`. If a pong does not arrive within `MCP_PROXY_PONG_TIMEOUT`, the connection is torn down and reconnect begins. This detects silent hangs — connections where TCP stays open but the daemon has stopped processing.

**Formal verification.** The bridge protocol has a [Z specification](docs/mcp-proxy.tex) verified by ProB model checking; the invariants and test partitions used in the Go tests are derived from it.

## Documentation

- [DESIGN.md](DESIGN.md) — decisions on transport selection, session identity, concurrency, and message format.
- [docs/daemon-guide.md](docs/daemon-guide.md) — daemon-side integration guide.
- [docs/distribution.md](docs/distribution.md) — release channels and platform binary layout.
- [docs/mcp-proxy.tex](docs/mcp-proxy.tex) — Z specification of the bridge protocol.
- [CHANGELOG.md](CHANGELOG.md) — release history.

## Development

```bash
make check        # Full quality gate: vet, staticcheck, golangci-lint, govulncheck, markdownlint, race tests
make lint         # go vet + staticcheck
make lint-strict  # golangci-lint
make vulncheck    # govulncheck
make test         # go test -race -count=1 ./...
make cover        # Coverage report
make format       # gofmt -w .
make build        # Local binary
make dist         # Cross-compile for darwin/linux × arm64/amd64
```

## License

MIT

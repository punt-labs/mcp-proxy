# mcp-proxy

> Lightweight Go proxy bridging MCP stdio transport to a shared daemon process.

[![License](https://img.shields.io/github/license/punt-labs/mcp-proxy)](LICENSE)
[![CI](https://img.shields.io/github/actions/workflow/status/punt-labs/mcp-proxy/test.yml?label=CI)](https://github.com/punt-labs/mcp-proxy/actions/workflows/test.yml)

Claude Code spawns a fresh MCP server process for every session. If you open three terminal tabs, you get three copies of your server — three copies of its models, connections, and state. When the server is heavy (an ML embedding model, a database connection pool, a NATS relay), this wastes hundreds of megabytes of memory and adds seconds of startup time to each session.

mcp-proxy fixes this. Instead of spawning the real server, Claude Code spawns a tiny Go binary (~5MB, <10ms startup) that forwards MCP messages over WebSocket to a single shared daemon:

```text
                    stdio                      WebSocket
Claude Code ◄──────────────► mcp-proxy ◄──────────────────────► daemon
             MCP JSON-RPC                                       (one process)
```

The proxy works with **any MCP server** that exposes a WebSocket endpoint speaking MCP JSON-RPC — it never inspects message content. Your server doesn't need to be modified; it just needs a WebSocket transport in addition to (or instead of) stdio.

**Platforms:** macOS, Linux

## Daemon Requirements

Your MCP server must:

1. **Accept WebSocket connections** with the `mcp` subprotocol (`Sec-WebSocket-Protocol: mcp`)
2. **Speak MCP JSON-RPC 2.0** — one JSON object per WebSocket text frame
3. **Be running before the proxy connects** — the proxy retries with backoff if the daemon is unreachable, but does not auto-start it

Optionally, the daemon can:

- **Read `?session_key=<pid>`** from the WebSocket upgrade URL to maintain per-session state (e.g., separate database selections per Claude Code tab)
- **Push server-initiated messages** (e.g., `notifications/tools/list_changed`) — the proxy forwards them to stdout immediately

### Authentication

For remote daemons or daemons that require API keys, set `MCP_PROXY_TOKEN`:

```bash
MCP_PROXY_TOKEN=your-api-key mcp-proxy wss://remote-host/mcp
```

The proxy sends this as `Authorization: Bearer <token>` on the WebSocket upgrade request.

For local daemons, auth is typically unnecessary — binding to `127.0.0.1` (the default) is sufficient. The `?session_key=<pid>` query parameter can serve as lightweight per-session identity without requiring a shared secret.

## Install

### Binary

Download from [GitHub Releases](https://github.com/punt-labs/mcp-proxy/releases):

```bash
curl -fsSL https://github.com/punt-labs/mcp-proxy/releases/latest/download/mcp-proxy-darwin-arm64 -o mcp-proxy
chmod +x mcp-proxy
mv mcp-proxy ~/.local/bin/
```

Replace `darwin-arm64` with your platform: `darwin-amd64`, `linux-arm64`, `linux-amd64`.

### Go

```bash
go install github.com/punt-labs/mcp-proxy@latest
```

### Via quarry

If you use [quarry](https://github.com/punt-labs/quarry), `quarry install` downloads mcp-proxy automatically (SHA256-verified, correct platform).

## Usage

```bash
mcp-proxy ws://localhost:8420/mcp
```

The proxy reads JSON-RPC from stdin, forwards each line as a WebSocket text message to the daemon, and writes daemon responses to stdout. Messages are opaque — no parsing, no transformation.

### Reconnect

If the daemon disconnects (restart, crash), the proxy reconnects automatically with exponential backoff (250ms → 5s cap). Messages queued during disconnect are preserved and delivered on the next connection. Status is printed to stderr:

```text
mcp-proxy: connected
mcp-proxy: daemon disconnected, reconnecting...
mcp-proxy: daemon unreachable, retrying in 250ms...
mcp-proxy: connected
```

### Health Check

```bash
mcp-proxy --health ws://localhost:8420/mcp
```

Dials the daemon, closes immediately, exits 0 on success or 1 on failure. Prints `mcp-proxy: ok` or `mcp-proxy: health check failed: <error>` to stderr. Useful for `quarry doctor`, launchd `KeepAlive`, and CI.

### MCP Server Configuration

Replace the direct MCP server command with the proxy:

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

### Debug Logging

```bash
MCP_PROXY_DEBUG=1 mcp-proxy ws://localhost:8420/mcp              # Log to temp file
MCP_PROXY_DEBUG=.tmp/proxy.log mcp-proxy ws://localhost:8420/mcp # Log to specific file
```

Logs include message sizes, connection events, and error details. Stdout is never polluted — all diagnostics go to the debug log file.

### Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Clean shutdown (stdin EOF), or health check success |
| 1 | Runtime error, or health check failure |
| 2 | Usage error (wrong arguments) |

### Signal Handling

First SIGINT/SIGTERM triggers graceful shutdown (close WebSocket, drain). Second signal force-exits immediately.

## How It Works

### Session Identity

The proxy resolves which Claude Code session spawned it by walking the process tree (`ps -eo pid=,ppid=,comm=`) upward to find the topmost `claude` ancestor PID. This session key is passed as `?session_key=<pid>` on the WebSocket upgrade, so the daemon can maintain per-session state.

### Bidirectional Forwarding

Two goroutines share one WebSocket connection:

1. **Scanner**: `bufio.Scanner` on stdin → `conn.Write()` to daemon
2. **Reader**: `conn.Read()` from daemon → `fmt.Fprintf()` to stdout

The daemon can push unsolicited messages (e.g., `tools/list_changed`) at any time — they appear on stdout immediately.

### Message Format

MCP over stdio uses newline-delimited JSON-RPC 2.0 (one JSON object per line). Over WebSocket, each line becomes one text frame. The proxy never parses JSON — messages pass through as opaque bytes.

## Build

```bash
go build -o mcp-proxy .
```

Cross-compile for all platforms:

```bash
make dist         # Builds dist/mcp-proxy-{darwin,linux}-{arm64,amd64}
```

## Development

```bash
make vet          # go vet
make test         # go test -race -count=1 ./...
make check        # vet + test + staticcheck
make cover        # Coverage report
```

### Test Pyramid

| Layer | Tag | What |
|-------|-----|------|
| Unit | (none) | Bridge forwarding, session resolution, transport errors |
| E2E | `e2e` | Compiled binary, black-box stdin/stdout piping |
| Integration | `integration` | Real daemon roundtrips (quarry, biff) |

### Formal Verification

The bridge protocol has a [Z specification](docs/mcp-proxy.tex) verified by ProB model checking (6 states, 43 transitions, all invariants hold). Test partitions are derived from the spec using TTF tactics.

## Design

See [DESIGN.md](DESIGN.md) for the decision log covering transport selection, session identity algorithm, concurrency model, and message format.

<details>
<summary>When does an MCP server need a proxy?</summary>

A proxy makes sense when your MCP server has **expensive startup**, **heavy shared state**, or **needs server push**:

| Symptom | Without Proxy | With Proxy |
|---------|--------------|------------|
| ML model loading (embeddings, classifiers) | Every session loads the model (~200MB, ~2s) | Model loaded once, shared across sessions |
| Database connection pools | N sessions = N pools | One pool, N lightweight proxies |
| Singleton resources (audio device, display) | File lock contention between sessions | Single owner, proxy multiplexes access |
| Server-initiated notifications | Not possible with stdio (client must poll) | Daemon pushes via WebSocket, proxy writes to stdout |

If your MCP server is stateless and starts in <100ms, you don't need a proxy — direct stdio is simpler.

</details>

<details>
<summary>Projects using mcp-proxy</summary>

| Project | Shared State | Why Daemon |
|---------|-------------|-----------|
| [Quarry](https://github.com/punt-labs/quarry) | LanceDB index + ONNX embedding model | ~200MB memory, ~2s cold start |
| [Biff](https://github.com/punt-labs/biff) | NATS relay connection | Persistent TCP, server push (`tools/list_changed`) |
| [Vox](https://github.com/punt-labs/vox) | Audio output device | File lock, singleton resource |
| [Lux](https://github.com/punt-labs/lux) | ImGui display server | Already centralized, interaction events |

</details>

<details>
<summary>Prior art</summary>

- **[SageOx](https://github.com/sageox/ox)** — Go CLI with per-workspace daemon over NDJSON Unix socket. Closest match. Request-response only (no push).
- **[Beads](https://github.com/steveyegge/beads)** — Had a daemon, deleted 24K lines of it in v0.51.0. Replaced with Dolt for native multi-writer. Lesson: keep the proxy small.
- **[Entire.io](https://github.com/entireio/cli)** — Stateless Go CLI. No daemon needed (filesystem-only state).

</details>

## License

MIT

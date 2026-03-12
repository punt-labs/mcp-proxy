# mcp-proxy

> Lightweight Go proxy bridging MCP stdio transport to a shared daemon process.

[![License](https://img.shields.io/github/license/punt-labs/mcp-proxy)](LICENSE)
[![CI](https://img.shields.io/github/actions/workflow/status/punt-labs/mcp-proxy/test.yml?label=CI)](https://github.com/punt-labs/mcp-proxy/actions/workflows/test.yml)

Claude Code spawns one MCP server per session via stdio. When servers hold expensive shared state (ML models, NATS connections, audio queues), every session duplicates it. mcp-proxy eliminates the duplication: a single static binary reads MCP JSON-RPC from stdin and forwards it over WebSocket to a shared daemon.

```text
                    stdio                      WebSocket
Claude Code ◄──────────────► mcp-proxy ◄──────────────────────► daemon
             MCP JSON-RPC                                       (one process)
```

**Platforms:** macOS, Linux

## Usage

```bash
mcp-proxy ws://localhost:8080/mcp
```

The proxy reads JSON-RPC from stdin, forwards each line as a WebSocket text message to the daemon, and writes daemon responses to stdout. Messages are opaque — no parsing, no transformation.

### MCP Server Configuration

Replace the direct MCP server command with the proxy:

```json
{
  "mcpServers": {
    "quarry": {
      "type": "stdio",
      "command": "mcp-proxy",
      "args": ["ws://localhost:8080/mcp"]
    }
  }
}
```

### Debug Logging

```bash
MCP_PROXY_DEBUG=1 mcp-proxy ws://localhost:8080/mcp       # Log to temp file
MCP_PROXY_DEBUG=/tmp/proxy.log mcp-proxy ws://...          # Log to specific file
```

Logs include message sizes, connection events, and error details. Stdout is never polluted — all diagnostics go to the debug log file.

### Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Clean shutdown (stdin EOF) |
| 1 | Connection failed or runtime error |
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
make build-all    # Builds dist/mcp-proxy-{darwin,linux}-{arm64,amd64}
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
<summary>Why these daemons need a proxy</summary>

| Project | Shared State | Cost Per Session | Push Required |
|---------|-------------|-----------------|---------------|
| Quarry | LanceDB index + ONNX model | ~200MB memory | No |
| Biff | NATS relay connection | TCP + KV watches | Yes (`tools/list_changed`) |
| Vox | Audio output device | File lock contention | No |
| Lux | ImGui display server | Already centralized | Yes (interaction events) |

A daemon is necessary when per-invocation state loading is prohibitive, or the system requires server-initiated push. These projects have both constraints.

</details>

<details>
<summary>Prior art</summary>

- **[SageOx](https://github.com/sageox/ox)** — Go CLI with per-workspace daemon over NDJSON Unix socket. Closest match. Request-response only (no push).
- **[Beads](https://github.com/steveyegge/beads)** — Had a daemon, deleted 24K lines of it in v0.51.0. Replaced with Dolt for native multi-writer. Lesson: keep the proxy small.
- **[Entire.io](https://github.com/entireio/cli)** — Stateless Go CLI. No daemon needed (filesystem-only state).

</details>

## License

MIT

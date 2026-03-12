# mcp-proxy Design Decision Log

This file is the authoritative record of design decisions, prior approaches, and their outcomes. **Every design change must be logged here before implementation.**

## Rules

1. Before proposing ANY design change, consult this log for prior decisions on the same topic.
2. Do not revisit a settled decision without new evidence.
3. Log the decision, alternatives considered, and outcome.

---

## System Architecture

```text
                    stdio                      WebSocket
Claude Code ◄──────────────► mcp-proxy ◄──────────────────────► daemon
             MCP JSON-RPC    (static Go       ws://host/mcp     (one process)
             (NDJSON)         binary)
```

The proxy is transparent — it doesn't parse, validate, or transform JSON-RPC messages. Messages are opaque byte sequences forwarded without modification.

---

## DES-001: Transport — WebSocket

**Date:** 2026-03-11
**Status:** SETTLED
**Topic:** Proxy-to-daemon transport protocol

### Design

Use **WebSocket** (`nhooyr.io/websocket`) for the proxy-to-daemon connection. The daemon adds a WebSocket upgrade endpoint at `/mcp` on its existing HTTP server.

### Why

Three constraints drove the decision:

1. **Bidirectional push is required.** Biff needs `tools/list_changed` push notifications. Lux needs interaction event push. HTTP cannot deliver unsolicited messages.
2. **Built-in framing and keepalive.** WebSocket provides RFC 6455 message framing and ping/pong keepalive. Raw Unix sockets require DIY framing (~20 lines) and DIY keepalive (~20 lines) — small but must be correct.
3. **Existing HTTP servers.** Quarry and biff already have `serve` commands with HTTP servers. WebSocket adds as an upgrade endpoint on the same server — one port serves both HTTP clients (quarry-menubar) and WebSocket proxy connections.

### Rejected: HTTP

Fails constraint 1. No server push. Workarounds (SSE sidecar, polling, file watching) add complexity that erodes HTTP's simplicity advantage.

### Rejected: Raw Unix Domain Socket (NDJSON)

Solves push. SageOx ships this in production and chose NDJSON for debuggability (`echo '{"type":"ping"}' | socat - UNIX:/path/sock`). But reimplements what WebSocket provides (framing, keepalive), and is local-only (no cross-machine future).

### Trade-off Accepted

WebSocket requires a third-party Go library (`nhooyr.io/websocket` — not in stdlib). This is the only external dependency beyond testify. Acceptable given what it provides.

---

## DES-002: Session Identity — Process Tree Walking

**Date:** 2026-03-11
**Status:** SETTLED
**Topic:** How the proxy identifies which Claude Code session spawned it

### Design

Port biff's `find_session_key()` algorithm: parse `ps -eo pid=,ppid=,comm=`, walk upward from the proxy's PID, return the PID of the **topmost** `claude` ancestor. Pass the session key as `?session_key=<pid>` on the WebSocket upgrade request.

### Why

The proxy is always a direct descendant of Claude Code (spawned as an MCP stdio subprocess). The topmost `claude` ancestor is the stable session identity — not the nearest, because Claude spawns child `claude` processes. This is a direct port of biff's DES-011/011a algorithm, proven in production.

### Why Topmost

Claude Code's process tree: `claude (main) → claude (child) → mcp-proxy`. The child claude PID changes on reconnect. The main claude PID is stable for the session lifetime.

### Fallback

If no `claude` ancestor is found (e.g., running the proxy manually for testing), falls back to `os.Getppid()`. This preserves pre-DES-011a behavior.

### Platform Differences

macOS `ps` reports full paths (`/Applications/Claude.app/.../claude`). Linux reports basenames (`claude`). The algorithm uses `path.Base()` to normalize.

---

## DES-003: Concurrency Model — Two Goroutines + WaitGroup

**Date:** 2026-03-11
**Status:** SETTLED
**Topic:** How the bridge handles concurrent stdin reads and daemon writes

### Design

Two goroutines sharing one WebSocket connection, coordinated by `sync.WaitGroup` with context cancellation:

1. **Scanner goroutine**: `bufio.Scanner` on stdin → `conn.Write()` to daemon
2. **Reader goroutine**: `conn.Read()` from daemon → `fmt.Fprintf()` to stdout

Either goroutine cancels the shared context on completion or error. `sync.Mutex` serializes stdout writes.

### Why Not errgroup

`errgroup` would also work, but adds an import for two goroutines. The explicit WaitGroup + context pattern is more transparent about shutdown semantics: stdin EOF triggers clean WebSocket close, which triggers reader exit, which completes the WaitGroup.

### Shutdown Sequence

1. stdin reaches EOF → scanner goroutine calls `conn.Close(StatusNormalClosure)` and cancels context
2. Reader goroutine sees close frame or context cancellation → exits
3. WaitGroup completes → `Run()` returns nil

### Race Safety

`-race` is mandatory for all test runs. The proxy's two goroutines share `conn` (safe — nhooyr/websocket is concurrent-safe for one reader and one writer) and `stdout` (serialized by mutex).

---

## DES-004: Message Format — Opaque NDJSON

**Date:** 2026-03-11
**Status:** SETTLED
**Topic:** Whether the proxy should parse JSON-RPC messages

### Design

The proxy treats messages as **opaque byte sequences**. No JSON parsing, no schema validation, no message routing. `bufio.Scanner` reads lines from stdin, each line becomes one WebSocket text message. Each WebSocket message received becomes one stdout line.

### Why

1. **Transparency.** The proxy doesn't need to understand MCP to forward it. The daemon is the real MCP server.
2. **Zero-copy efficiency.** No allocation for JSON parse/serialize per message.
3. **Forward compatibility.** New MCP methods, new JSON-RPC fields, protocol extensions — all pass through unchanged.
4. **Simplicity.** No JSON-RPC types, no error mapping, no message ID tracking.

### MCP stdio Format

MCP over stdio uses **newline-delimited JSON-RPC 2.0** (one JSON object per line), not the LSP `Content-Length` header format. This was confirmed by reading the MCP specification. The proxy's `bufio.Scanner` line-oriented approach matches this exactly.

---

## DES-005: Daemon Lifecycle — Not The Proxy's Job

**Date:** 2026-03-11
**Status:** SETTLED
**Topic:** Should the proxy start the daemon if it's not running?

### Design

**No.** The proxy assumes the daemon is running. If it can't connect, it exits with code 1 and a clear error message to stderr. Daemon lifecycle is managed externally (launchd/systemd, shell script, user invocation).

### Why

1. **Single responsibility.** The proxy does one thing: bridge stdio to WebSocket. Adding daemon management creates coupling and complexity.
2. **Platform differences.** launchd (macOS) vs systemd (Linux) vs manual. Each daemon project may have different requirements.
3. **SageOx lesson.** SageOx's daemon management includes restart loop detection, exponential backoff, and inactivity timeout. This is daemon-specific logic that doesn't belong in a generic proxy.
4. **Beads lesson.** Beads deleted 24K lines of daemon code. Keep the proxy small.

### Future

Individual daemon projects may add auto-start logic to their own installers or launch agents. The proxy stays agnostic.

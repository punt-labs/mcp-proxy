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

---

## Target Daemon Migration Notes

### Quarry (`quarry serve`) — easiest migration

`quarry serve` (stdlib HTTP) already exists. Each `quarry mcp` session currently loads its own LanceDB index and ONNX embedding model (~200MB). The daemon shares one index and one model across all sessions.

**Complications:**

- **Database switching.** The `use` tool switches the active database — currently per-process state. Must become per-session (keyed by proxy session identity).
- **Fire-and-forget.** Side-effect tools (ingest, delete, sync) return optimistic responses and process in a background `ThreadPoolExecutor`. Daemon preserves this; with bidirectional transport, can optionally push completion notifications.
- **MCP `instructions` field.** Proxy must forward the daemon's `initialize` response (which includes formatting guidance) without modification.

### Biff (`biff serve`) — hardest migration

`biff serve` exists with both stdio and HTTP transports. The entire architecture revolves around per-session state: `{user}:{tty}` session key (DES-002), unread counts, talk partner, mesg mode, plan text.

**Complications:**

- **`tools/list_changed` push.** Background poller fires notifications when unread counts change. With HTTP, this is unsolvable without workarounds. **With WebSocket, solved** — daemon pushes down the session's persistent connection.
- **PPID-keyed unread files.** Status line reads `~/.biff/unread/{ppid}.json`. Daemon writes these keyed by the Claude PID received from the proxy. Actually *fixes* DES-011b.
- **Dynamic tool descriptions.** Per-session tool description mutation (unread count, talk partner). Persistent connection makes this natural — daemon knows which session is asking.
- **Belt-and-suspenders simplification.** Current two-path notification design (tool-handler "belt" + background-poller "suspenders") collapses to one: poller → daemon → session connection → proxy → stdout.
- **Session cleanup.** Currently deletes PPID file on shutdown. With persistent connection, cleanup triggers on proxy disconnect (detected via keepalive timeout).
- **Session lifetime vs connection lifetime.** DES-008 requires 30-day session persistence. Session state persists in the relay (NATS KV / filesystem); the proxy connection is a transient delivery channel, not the session's lifetime.

### Vox (new daemon needed)

No `serve` command exists. Must be built. Currently uses `flock` on `~/.punt-vox/playback.lock` for cross-session audio serialization.

**Complications:**

- **Audio queue replaces flock.** Daemon manages one in-process FIFO queue — strictly simpler than file locking. But hooks (`notify.sh`, `notify-permission.sh`) currently call `vox play` directly. The CLI must forward to the daemon for playback.
- **Hook call path change.** DES-017: hooks call CLI directly (~110ms). Path becomes hook → CLI → daemon → playback instead of hook → CLI → direct playback.
- **Per-session config.** `.vox/config.md` is per-project. Proxy passes project directory at connection time.

### Lux (`lux display` + `lux serve`)

Display server already exists as a persistent Unix socket server (length-prefixed JSON, DES-002/003). MCP server (`lux serve`) is per-session, bridging MCP to the display protocol.

**Complications:**

- **Protocol mismatch.** Display server speaks its own protocol, not MCP JSON-RPC. `lux serve` becomes the shared daemon, bridging MCP to the display's native protocol.
- **Bidirectional events.** Display pushes interaction events (clicks, slider changes). With persistent connection, daemon pushes to the correct proxy immediately — eliminates `recv()` polling.
- **Scene ownership.** Multiple sessions showing content simultaneously. Session identity auto-scopes content to per-session tabs.

---

## Open Questions

1. **Daemon auto-start.** Proxy starts daemon if missing, or always user's responsibility? (Currently: fail fast, per DES-005.)
2. **Graceful degradation.** Daemon down → fall back to in-process server, or fail fast? (Currently: fail fast.)
3. **Lux daemon identity.** `lux serve` becomes the shared daemon, or MCP added to display server directly?
4. **Hook CLI forwarding.** `vox play` and `lux hook post-bash` — forward to daemon, or work independently?
5. **WebSocket over Unix socket vs TCP.** Unix socket (lowest latency, no port conflicts) or TCP localhost (simpler URLs)? (Currently: TCP localhost.)

---

## Prior Art

### SageOx (sageox/ox) — closest match

Go CLI (97.8%), **shipped per-workspace daemon** with NDJSON over Unix domain socket. The most relevant prior art.

**Architecture:** `ox daemon start` launches a background Go process. Clients connect over Unix socket, send one JSON object per line (NDJSON), get one response. 18+ message types: status, sync, heartbeat (one-way), checkout (streaming progress), telemetry, agent instance tracking.

**IPC protocol:**

```text
Client → Daemon:  {"type":"ping"}\n
Daemon → Client:  {"success":true}\n

Client → Daemon:  {"type":"checkout","payload":{...}}\n
Daemon → Client:  {"progress":{"stage":"cloning","percent":30}}\n
Daemon → Client:  {"success":true,"data":{...}}\n
```

Chose NDJSON over length-prefix for debuggability: `echo '{"type":"ping"}' | socat - UNIX:/path/sock` works. Can't do that with length-prefix or WebSocket.

**Session tracking:** Heartbeat-based. Agents send `{"type":"heartbeat","payload":{"agent_id":"Oxa7b3"}}`. Daemon tracks instances (last heartbeat, active/idle status, context tokens consumed). No process tree walking.

**Daemon lifecycle:** Per-workspace scope. Inactivity auto-exit. Restart loop detection (exponential backoff 5s→2min). Liveness via socket ping (not PID file). Socket mode 0600 (owner-only). Max 100 concurrent connections.

**What they got right:** NDJSON debuggability. Per-workspace scoping. Heartbeat session tracking. Inactivity timeout. `NeedsHelp` pattern (daemon flags issues for LLM reasoning).

**The push gap:** SageOx is request-response only — no persistent connections where the daemon pushes unsolicited messages. Works for sync status, but wouldn't work for biff's `tools/list_changed` or lux's interaction events.

**Key difference:** SageOx's daemon is a separate IPC service alongside the MCP server. Ours *is* the MCP server — the proxy bridges stdio to it.

Sources: [GitHub](https://github.com/sageox/ox), [daemon.go](https://github.com/sageox/ox/blob/8553ad83/cmd/ox/daemon.go), [ipc.go](https://github.com/sageox/ox/blob/8553ad83/internal/daemon/ipc.go), [ipc_unix.go](https://github.com/sageox/ox/blob/8553ad83/internal/daemon/ipc_unix.go)

### Beads (steveyegge/beads) — daemon removed

Go CLI (92.9%) with Python MCP server (stateless CLI wrapper). **Had a daemon, deleted it in v0.51.0** — removed ~70K lines (daemon, RPC, SQLite, JSONL sync, 3-way merge). Replaced with [Dolt](https://github.com/dolthub/dolt) (versioned MySQL-compatible database) for native multi-writer.

**Lesson:** If shared state lives in a database with native multi-writer support, you don't need a daemon. **Warning:** They found their daemon complex enough to delete 24K lines of it. Keep ours small.

**Why this doesn't apply:** Our shared state (ML models, NATS connections, audio queues, display servers) can't live in a database.

Sources: [GitHub](https://github.com/steveyegge/beads), [vscode-beads #65](https://github.com/jdillon/vscode-beads/issues/65)

### Entire.io (entireio/cli) — no daemon

Go CLI (97.8%, 3.5K stars). Session provenance capture via git hooks. Stores checkpoints on a separate branch (`entire/checkpoints/v1`). No daemon, no MCP server. Stateless CLI + filesystem.

**Why this doesn't apply:** Solves write-heavy provenance, not concurrent reads of expensive in-memory state.

Sources: [GitHub](https://github.com/entireio/cli), [Claude plugin](https://github.com/entireio/claude-plugins)

### When is a daemon necessary?

| Project | Shared State | Lives In | Daemon? |
|---------|-------------|----------|---------|
| Beads | Issue graph | Dolt (MySQL) | Removed — database handles multi-writer |
| Entire.io | Session transcripts | Git (filesystem) | Never had one |
| SageOx | Sync state, code index | In-memory + filesystem | Yes — coordinates sync, tracks agents |
| **Quarry** | LanceDB + embedding model | In-memory | **Needed** — ~200MB per session without sharing |
| **Biff** | NATS connection + session state | In-memory + NATS | **Needed** — push notifications require persistent connection |
| **Vox** | Audio queue + provider clients | In-memory | **Needed** — serialized playback requires single queue |
| **Lux** | Display server + scene state | In-memory (ImGui) | **Already exists** — Unix socket to display |

A daemon is necessary when per-invocation state loading is prohibitive, or the system requires server-initiated push. Our projects have both constraints.

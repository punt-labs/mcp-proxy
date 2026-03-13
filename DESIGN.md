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

### Package Map

| Package | What It Does |
|---------|-------------|
| `main` | Entry point: parse args, health check, reconnecting proxy, signal handling |
| `internal/bridge` | Bidirectional stdin↔WebSocket forwarding (two goroutines + WaitGroup) |
| `internal/reconnect` | Reconnecting bridge: stdin channel, per-connection goroutines, backoff |
| `internal/transport` | WebSocket dial with typed errors, session key injection, bearer token auth |
| `internal/session` | Process-tree walking to resolve Claude Code session key |
| `internal/debuglog` | Structured `slog` debug logging via `MCP_PROXY_DEBUG` env var |
| `internal/testutil` | Mock daemon (`httptest.Server` + WebSocket), stdio pipe helpers |
| `internal/e2e` | Black-box binary tests (build tag `e2e`) |
| `internal/integration` | Real daemon roundtrip tests (build tag `integration`) |

---

## DES-001: Transport — WebSocket

**Date:** 2026-03-11
**Status:** SETTLED
**Topic:** Proxy-to-daemon transport protocol

### Design

Use **WebSocket** (`github.com/coder/websocket`) for the proxy-to-daemon connection. The daemon adds a WebSocket upgrade endpoint at `/mcp` on its existing HTTP server.

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

WebSocket requires a third-party Go library (`github.com/coder/websocket` — not in stdlib). This is the only external dependency beyond testify. Acceptable given what it provides.

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

**Stdin EOF (clean):**

1. Scanner goroutine reaches EOF → cancels context
2. Reader goroutine sees context cancellation → exits, closes stdin (unblocks scanner if stuck)
3. WaitGroup completes → `Run()` returns nil

**Daemon disconnect:**

1. Reader goroutine gets WebSocket error → cancels context, closes stdin
2. Scanner goroutine sees write error or stdin close → exits
3. WaitGroup completes → `Run()` returns daemon error

**Signal (SIGINT/SIGTERM):**

1. `main.go` cancels context via `signal.NotifyContext`
2. Both goroutines see context cancellation → exit
3. Second signal force-exits via `forceExitOnSecondSignal` goroutine

### Race Safety

`-race` is mandatory for all test runs. The proxy's two goroutines share `conn` (safe — coder/websocket is concurrent-safe for one reader and one writer) and `stdout` (serialized by mutex).

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

## DES-006: Debug Logging — File-Only via MCP_PROXY_DEBUG

**Date:** 2026-03-11
**Status:** SETTLED
**Topic:** How the proxy exposes diagnostic information

### Design

Debug logging is off by default and controlled by `MCP_PROXY_DEBUG`:

| Value | Behavior |
|-------|----------|
| unset / empty | No logging (nop logger, zero-cost calls) |
| `1` or `true` | Log to `$TMPDIR/mcp-proxy-<pid>.log` |
| any other value | Treated as file path |

Uses `slog.Logger` with `slog.LevelDebug`. All log entries include structured fields (message sizes, connection events, errors).

### Why File-Only

**Stdout is the data channel.** MCP JSON-RPC messages flow through stdout — any diagnostic output would corrupt the protocol stream. Stderr is also risky: some MCP clients capture stderr, and mixed diagnostics/error output is hard to parse.

File logging provides clean separation: data on stdout, errors on stderr, diagnostics in a file.

### Why slog

`slog` is stdlib (Go 1.21+), structured, and has zero-cost disabled paths. The `Nop()` logger discards at the handler level, so disabled `logger.Debug()` calls don't allocate.

### Test Logger

`NewTestLogger(t)` writes to both `t.Log()` (visible with `-v`) and a captured buffer (for assertions). This avoids polluting test output while allowing tests to assert on log content.

---

## DES-007: Bearer Token Authentication — MCP_PROXY_TOKEN

**Date:** 2026-03-12
**Status:** SETTLED
**Topic:** How the proxy authenticates with remote or secured daemons

### Design

If `MCP_PROXY_TOKEN` is set, the proxy sends `Authorization: Bearer <token>` on the WebSocket upgrade request. No token means no header.

```bash
MCP_PROXY_TOKEN=your-api-key mcp-proxy wss://remote-host/mcp
```

### Why Environment Variable

MCP server configurations (`claude_desktop_config.json`, `plugin.json`) support `env` blocks for injecting environment variables, but have no mechanism for secrets in `args`. Environment variables are the standard channel for secrets in the MCP ecosystem.

### Why Bearer

Bearer tokens are the most common HTTP auth scheme for API keys. The header is set on the HTTP upgrade request (before WebSocket is established), which is the standard WebSocket authentication point.

### Rejected: Multiple Auth Schemes

Only Bearer is supported. Adding Basic, custom headers, or mTLS would increase surface area without evidence of demand. The proxy can add schemes later if needed — the current design doesn't preclude it.

### Rejected: Config File

A config file for a single env var would be over-engineering. If the proxy ever needs multiple config knobs, a file may make sense — but the current interface (one URL arg + two optional env vars) is sufficient.

---

## DES-008: Signal Handling — Double-Signal Pattern

**Date:** 2026-03-11
**Status:** SETTLED
**Topic:** How the proxy handles SIGINT and SIGTERM

### Design

Two-phase signal handling:

1. **First signal:** Cancels context via `signal.NotifyContext`. Bridge goroutines see cancellation and shut down gracefully (close WebSocket, drain pending writes).
2. **Second signal:** `forceExitOnSecondSignal` goroutine calls `os.Exit(1)` immediately.

### Why Two Phases

Graceful shutdown matters: an abrupt exit can leave the daemon with a half-open WebSocket connection that won't be cleaned up until the keepalive timeout. But if graceful shutdown hangs (blocked on stdin read, unresponsive daemon), the user needs an escape hatch.

This is the standard Go pattern — `kubectl`, `docker`, and most Go CLI tools use the same two-signal approach.

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
- **Session lifetime vs connection lifetime.** Biff's DES-008 requires 30-day session persistence. Session state persists in the relay (NATS KV / filesystem); the proxy connection is a transient delivery channel, not the session's lifetime.

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

## DES-009: Reconnect on Daemon Disconnect

**Date:** 2026-03-12
**Status:** SETTLED
**Topic:** Proxy behavior when the daemon restarts or disconnects

### Design

The proxy reconnects automatically with exponential backoff (250ms, 500ms, 1s, 2s, 4s, 5s cap). Messages are preserved across reconnects — no data loss. Implemented in `internal/reconnect`.

### Why bridge.Run Can't Be Looped

`bridge.Run`'s daemon reader goroutine closes stdin to unblock the scanner on disconnect. Once `os.Stdin` is closed, it can't be reopened. A simple `for { dial(); bridge.Run() }` loop breaks on the second iteration.

### Architecture

```text
os.Stdin → [stdin goroutine] → chan []byte → [writer] → conn → daemon
                                                          ↑ new conn on reconnect
daemon → conn → [reader goroutine] → stdout
          ↑ new goroutine on reconnect
```

**Stdin goroutine** (process lifetime): reads lines via `bufio.Scanner`, copies bytes, sends to buffered channel. Closes channel on EOF.

**Per-connection**: writer reads from channel, writes to conn. Reader reads from conn, writes to stdout. Either can trigger reconnect.

**Message preservation**: if `conn.Write` fails, the consumed line is returned as `pending` and retried on the next connection. When the reader detects disconnect (cancels `connCtx`), no line was consumed from the channel — nothing lost.

### Key Coordination Detail

The reader goroutine cancels `connCtx` when it detects daemon disconnect. This unblocks the writer's `select` on `<-connCtx.Done()`. The main loop then calls `conn.CloseNow()` and waits for the reader to exit before starting a new connection (no concurrent stdout writes).

On stdin EOF, the writer cancels `connCtx` to unblock the reader's `conn.Read()`, then waits for it to exit. This prevents the 5-second stall that would occur if the deferred `connCancel` ran after `<-readerDone`.

### Rejected: Looping bridge.Run

Stdin close is irreversible. Even with an `io.Pipe` wrapper, the bridge's "close stdin to unblock scanner" pattern is fundamentally at odds with process-lifetime stdin reading.

### Rejected: Separate Reconnect Binary

A wrapper script that restarts the proxy would lose in-flight messages and require re-resolving the session key. Reconnect belongs inside the proxy.

### Backpressure

The `lines` channel has capacity 64. During reconnect (daemon unreachable), stdin messages accumulate in this buffer. If Claude Code sends more than 64 messages before the proxy reconnects, the stdin goroutine blocks, which blocks Claude Code's stdin pipe. This is correct backpressure — the proxy cannot accept unbounded messages without a connection to deliver them. With a 5-second max backoff, 64 messages is unlikely to be hit in practice (MCP request rate is low), but the failure mode is a silent hang with no diagnostic output.

### Trade-off Accepted

The `internal/bridge` package remains unchanged — it's still the right primitive for unit testing the bidirectional forwarding logic in isolation. The reconnect package is a higher-level coordinator that uses the same WebSocket operations but manages connection lifecycle.

---

## DES-010: Health Check Flag

**Date:** 2026-03-12
**Status:** SETTLED
**Topic:** Liveness probe for daemon availability

### Design

`mcp-proxy --health <url>` — dial with session key 0, close immediately, exit 0/1. Prints `mcp-proxy: ok` or `mcp-proxy: health check failed: <error>` to stderr. Timeout is `DialTimeout + 1s` (safety net beyond Dial's internal timeout).

### Why

Three consumers need daemon liveness checks:

1. **`quarry doctor`** — health check in CLI diagnostics
2. **launchd `KeepAlive`** — restart daemon if not responding
3. **CI** — verify daemon is running before test suite

### Rejected: Separate Health Check Binary

A `mcp-proxy-health` binary would duplicate URL parsing, auth setup, and transport code. A single flag on the existing binary is simpler and ensures the health check uses the same transport path as the proxy.

---

## Open Questions

1. ~~**Daemon auto-start.** Proxy starts daemon if missing, or always user's responsibility?~~ Settled: no auto-start (DES-005), but reconnect with backoff (DES-009) handles daemon restarts transparently.
2. ~~**Graceful degradation.** Daemon down → fall back to in-process server, or fail fast?~~ Settled: reconnect with backoff (DES-009). No in-process fallback.
3. **Lux daemon identity.** `lux serve` becomes the shared daemon, or MCP added to display server directly?
4. **Hook CLI forwarding.** `vox play` and `lux hook post-bash` — forward to daemon, or work independently?
5. ~~**WebSocket over Unix socket vs TCP.**~~ Settled by DES-001: WebSocket over TCP localhost. URLs are simpler (`ws://localhost:8420/mcp`), and TCP allows remote daemons (enabled by DES-007 bearer auth).

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

# mcp-proxy

A lightweight Go proxy that bridges MCP stdio transport to a shared daemon process, eliminating per-session resource duplication.

## Problem

Claude Code spawns one MCP server process per session via stdio transport. When a project holds expensive shared state, every session duplicates it:

| Project | Shared Resource | Cost Per Session |
|---------|----------------|-----------------|
| Quarry  | LanceDB index + ONNX embedding model | ~200MB memory, database lock contention |
| Biff    | NATS relay connection | TCP connection per session, duplicated KV watches |
| Vox     | Audio output device | File lock contention (`flock`), redundant provider clients |
| Lux     | ImGui display server | Already centralized — but MCP layer still per-session |

## Solution

A single static Go binary reads MCP JSON-RPC from stdin and forwards it to a shared daemon. The proxy is transparent — it doesn't know what tools exist. Tool discovery, listing, and execution pass through unchanged.

```text
                    stdio                      daemon transport
Claude Code ◄──────────────► mcp-proxy ◄──────────────────────► daemon
             MCP JSON-RPC                                       (one process)
```

```json
{"mcpServers": {"quarry": {"type": "stdio", "command": "mcp-proxy", "args": ["ws://localhost:8080/mcp"]}}}
```

## Design Goals

1. **Near-zero startup cost.** <10ms spawn, <10MB memory. Go static binary. Python (~300ms, ~50MB) and Node (~100ms, ~30MB) don't meet this bar.
2. **Transparent JSON-RPC forwarding.** Forwards the entire MCP protocol unchanged. The daemon is the real MCP server.
3. **Session identity injection.** Resolves "which Claude session am I?" and passes it to the daemon at connection time. Replaces process-tree walking heuristics. See [Session Identity](#session-identity).
4. **Single transport backend.** One proxy-to-daemon protocol that supports bidirectional messaging (server push), because biff and lux require it.
5. **Single binary, no dependencies.** Static binary per platform (darwin/arm64, darwin/amd64, linux/arm64, linux/amd64).
6. **Daemon lifecycle is not the proxy's job.** Assumes the daemon is running. Exits with a clear error if it can't connect.

## Transport Options

The proxy-to-daemon transport must support **bidirectional messaging** — biff needs to push `tools/list_changed` and lux needs to push interaction events. This is the critical constraint that eliminates HTTP as a standalone option.

### Comparison

| Criterion | HTTP | Raw Unix Socket | WebSocket |
|-----------|------|----------------|-----------|
| Server push (notifications) | **No** | Yes | Yes |
| Built-in framing | Yes (HTTP) | No (DIY) | Yes (RFC 6455) |
| Built-in keepalive | No | No (DIY) | Yes (ping/pong) |
| Persistent session | No (per-request) | Yes | Yes |
| Go stdlib support | Yes | Yes | No (third-party) |
| Existing daemon compat | Direct | New listener needed | HTTP upgrade endpoint |
| Cross-machine (future) | Yes | No | Yes |
| Debuggability | curl | socat + NDJSON | wscat |

### Option A: HTTP

Request-response only. Existing daemons (`quarry serve`, `biff serve`) already speak it. Simplest to implement (Go stdlib). But **no server push** — the daemon cannot send `tools/list_changed` to a proxy unprompted. Workarounds (SSE sidecar, proxy polling, file watching) add complexity that erodes the simplicity advantage.

**Verdict:** Works for request-response-only tools. Fails for biff and lux.

### Option B: Raw Unix Domain Socket

Persistent bidirectional connection. NDJSON (newline-delimited JSON) or length-prefixed framing. Daemon can push to any connected proxy at any time. Lowest latency (kernel IPC, no TCP). SageOx ships this in production (see [Prior Art](#sageox-sageoxox)).

**Trade-off:** DIY framing and keepalive (~20 lines of Go each, but must be correct). NDJSON is debuggable with `echo '{"type":"ping"}' | socat - UNIX:/path/sock` — SageOx chose this over length-prefix specifically for debuggability. Local-only (no cross-machine).

**Verdict:** Solves push. Lower-level than necessary — reimplements what WebSocket provides.

### Option C: WebSocket

Persistent bidirectional connection with built-in framing (RFC 6455), keepalive (ping/pong), and dead connection detection. Go libraries: `nhooyr.io/websocket` or `gorilla/websocket`. **Adds as an HTTP upgrade endpoint** to existing daemons — one server serves both HTTP API clients (quarry-menubar) and WebSocket proxy connections. Works over TCP localhost or Unix socket (custom dialer).

**Verdict:** Combines Unix socket's bidirectional push with HTTP's ecosystem maturity. Solves notification routing without DIY framing or keepalive.

**Leaning toward Option C (WebSocket).** To be revisited.

## Session Identity

Biff uses process-tree walking to find the topmost `claude` ancestor PID (DES-011/011a). With a shared daemon, this breaks — the daemon has no Claude ancestor.

**Fix:** The proxy resolves the session key (it *is* a child of Claude) and passes it at connection time:

```json
{"type": "register", "session_key": "19147", "pid": 83201}
```

**Resolution algorithm:** Parse process table (`ps -eo pid=,ppid=,comm=`), walk upward from proxy PID, find topmost `claude` ancestor, fall back to PPID. Cached for process lifetime. Direct port of `find_session_key()` from `biff/src/biff/session_key.py`.

This fixes DES-011b (local vs marketplace plugin process tree divergence) — the proxy always resolves from its own tree regardless of plugin installation method.

## Target Daemons

### Quarry (`quarry serve`) — easiest migration

`quarry serve` (stdlib HTTP) already exists. Each `quarry mcp` session currently loads its own LanceDB index and ONNX embedding model (~200MB). The daemon shares one index and one model across all sessions.

**Complications:**

- **Database switching.** The `use` tool switches the active database — currently per-process state. Must become per-session (keyed by proxy session identity).
- **Fire-and-forget.** Side-effect tools (ingest, delete, sync) return optimistic responses and process in a background `ThreadPoolExecutor`. Daemon preserves this; with bidirectional transport, can optionally push completion notifications.
- **MCP `instructions` field.** Proxy must forward the daemon's `initialize` response (which includes formatting guidance) without modification.

### Biff (`biff serve`) — hardest migration

`biff serve` exists with both stdio and HTTP transports. The entire architecture revolves around per-session state: `{user}:{tty}` session key (DES-002), unread counts, talk partner, mesg mode, plan text.

**Complications:**

- **`tools/list_changed` push.** Background poller fires notifications when unread counts change. With HTTP, this is unsolvable without workarounds. **With WebSocket/socket, solved** — daemon pushes down the session's persistent connection.
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

## Install Impact

Current `install.sh` pattern: check prerequisites → install uv → install Python → `uv tool install` → register plugin.

**New step:** Download the mcp-proxy binary (~5MB static binary):

```sh
ARCH=$(uname -m)
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
curl -fsSL "https://github.com/punt-labs/mcp-proxy/releases/latest/download/mcp-proxy-${OS}-${ARCH}" \
  -o "$HOME/.local/bin/mcp-proxy"
chmod +x "$HOME/.local/bin/mcp-proxy"
```

**Plugin registration** changes from `{"command": "quarry", "args": ["mcp"]}` to `{"command": "mcp-proxy", "args": ["ws://localhost:8080/mcp"]}`. Breaking change — install script must detect and replace old registrations.

**Daemon lifecycle** options: explicit start (user runs `quarry serve`), launchd/systemd service (auto-start on login), or on-demand via proxy (proxy starts daemon if missing — violates goal 6 but improves UX).

**Shared binary.** mcp-proxy becomes a cross-org dependency like `uv`. Every project downloads the same binary. Wire format (JSON-RPC) is stable across versions.

## Build & Release

```sh
go build -o mcp-proxy .

GOOS=darwin  GOARCH=arm64 go build -o dist/mcp-proxy-darwin-arm64 .
GOOS=darwin  GOARCH=amd64 go build -o dist/mcp-proxy-darwin-amd64 .
GOOS=linux   GOARCH=arm64 go build -o dist/mcp-proxy-linux-arm64  .
GOOS=linux   GOARCH=amd64 go build -o dist/mcp-proxy-linux-amd64  .
```

## Open Questions

1. **Transport decision.** HTTP, raw Unix socket, or WebSocket? Leaning WebSocket. To be revisited.
2. **Daemon auto-start.** Proxy starts daemon if missing, or always user's responsibility?
3. **Graceful degradation.** Daemon down → fall back to Model 1 (full in-process server), or fail fast?
4. **Lux daemon identity.** `lux serve` becomes the shared daemon, or MCP added to display server directly?
5. **Hook CLI forwarding.** `vox play` and `lux hook post-bash` — forward to daemon, or work independently?
6. **WebSocket over Unix socket vs TCP.** If WebSocket: Unix socket (lowest latency, no port conflicts) or TCP localhost (simpler URLs)?

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

**The push gap:** SageOx is request-response only — no persistent connections where the daemon pushes unsolicited messages. Works for sync status, but wouldn't work for biff's `tools/list_changed` or lux's interaction events. Their streaming progress (multiple lines on one connection) is the closest, but still client-initiated.

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

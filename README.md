# mcp-proxy

A lightweight, generic proxy that bridges MCP stdio transport to a shared daemon process.

## Problem

Claude Code spawns one MCP server process per session via stdio transport. When a project holds expensive shared state — a database index, a message bus connection, an audio output device, a display server — every session duplicates that state. With many concurrent sessions on a single machine, this multiplies memory, file handles, connections, and contention.

| Project | Shared Resource | Cost Per Session |
|---------|----------------|-----------------|
| Quarry  | LanceDB index + ONNX embedding model | ~200MB memory, database lock contention |
| Biff    | NATS relay connection | TCP connection per session, duplicated KV watches |
| Vox     | Audio output device | File lock contention (`flock` on `playback.lock`), redundant provider clients |
| Lux     | ImGui display server | Already centralized via Unix socket — but MCP layer still per-session |

## Solution

A single static Go binary that reads MCP JSON-RPC from stdin and forwards it to a shared daemon. The proxy is transport-agnostic — it doesn't know what tools exist. It's a transparent JSON-RPC forwarder.

```text
                    stdio                      daemon transport
Claude Code ◄──────────────► mcp-proxy ◄──────────────────────► daemon
             MCP JSON-RPC                                       (one process)
```

Each project's `plugin.json` changes from spawning the full server to spawning the proxy:

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

## Transport Options

The proxy-to-daemon transport is the critical design decision. Three options are under consideration.

### Option A: HTTP

The proxy sends each MCP JSON-RPC message as an HTTP POST and reads the response.

```text
Session A ──stdio──▶ [mcp-proxy → POST /mcp]  ──┐
Session B ──stdio──▶ [mcp-proxy → POST /mcp]  ──┤──▶ daemon :PORT
Session C ──stdio──▶ [mcp-proxy → POST /mcp]  ──┘     (HTTP server)
```

**Pros:**

- Simplest to implement — Go's `net/http` is stdlib, no dependencies.
- Existing daemons already speak HTTP (`quarry serve`, `biff serve`). Zero daemon-side changes for basic tool calls.
- Request-response maps naturally to MCP tool calls (client sends `tools/call`, server responds).
- Stateless — any proxy can talk to any daemon instance. Load balancing and restarts are trivial.

**Cons:**

- **No server push.** HTTP is request-response. The daemon cannot send `tools/list_changed` notifications to a specific proxy unprompted. This is a critical gap for biff (unread count changes), lux (interaction events), and potentially quarry (background ingest completion). Workarounds: (a) bolt on SSE for a push sidecar, (b) proxy polls for pending notifications, (c) daemon writes to a file the proxy watches. All three add complexity that erodes the "simplest" advantage.
- Per-request overhead — HTTP headers, content-length framing, status codes. Minor for local traffic but non-zero.
- Session identity must be passed as HTTP headers (`X-Session-Key`) on every request. No persistent session context.

**Verdict:** Works for request-response-only tools (quarry search, vox synthesis). Breaks down for projects that need server-initiated push (biff, lux). The notification routing workarounds are complex enough to negate the simplicity advantage.

### Option B: Raw Unix Domain Socket

The proxy opens a persistent Unix domain socket connection to the daemon. Messages are length-prefixed JSON-RPC (4-byte big-endian length + UTF-8 payload). Bidirectional — either side can send at any time.

```text
Session A ──stdio──▶ [mcp-proxy] ◄──unix socket──▶ daemon.sock
Session B ──stdio──▶ [mcp-proxy] ◄──unix socket──▶   (one socket,
Session C ──stdio──▶ [mcp-proxy] ◄──unix socket──▶    N connections)
```

**Pros:**

- **Bidirectional.** The daemon can push `tools/list_changed` to any connected proxy at any time. Solves biff's notification routing and lux's interaction events natively.
- **Persistent connection.** Session identity is established once at connection time (registration message). No per-request headers. The daemon maintains a map of session → connection for targeted push.
- **Lowest latency.** No HTTP framing, no TCP stack (kernel IPC only). Lux already proves this: ~10ms content update, ~20ms RTT (DES-002 spike data).
- **Matches existing Lux architecture.** Lux's display server already uses 4-byte length-prefixed JSON over Unix socket (DES-002/003). Same wire format.

**Cons:**

- **DIY framing.** Must implement length-prefix encoding/decoding. Not hard (~20 lines of Go), but it's custom protocol work that must be correct.
- **DIY keepalive.** Must implement heartbeat/ping to detect dead connections. Dead proxy detection is critical for biff session cleanup.
- **Local-only.** Unix domain sockets don't work across machines. Fine for the stated use case (many sessions on one machine), but closes the door on remote daemons.
- **Socket path management.** Must agree on a socket path. Discovery logic needed (XDG_RUNTIME_DIR, fallbacks). Port numbers are simpler to configure and share.
- **No ecosystem for existing daemons.** `quarry serve` and `biff serve` speak HTTP today. Adding a Unix socket listener is new server-side code.

**Verdict:** Solves the bidirectional push problem cleanly. Lower-level than necessary — reimplements framing and keepalive that existing protocols provide.

### Option C: WebSocket

The proxy opens a persistent WebSocket connection to the daemon. Messages are MCP JSON-RPC (text frames). Bidirectional — either side can send at any time.

```text
Session A ──stdio──▶ [mcp-proxy] ◄──WebSocket──▶ daemon :PORT/mcp
Session B ──stdio──▶ [mcp-proxy] ◄──WebSocket──▶   (one server,
Session C ──stdio──▶ [mcp-proxy] ◄──WebSocket──▶    N connections)
```

**Pros:**

- **Bidirectional.** Same as Unix socket — daemon can push to any proxy at any time. `tools/list_changed`, interaction events, background completion notifications all work natively.
- **Persistent connection with session context.** Session identity sent once at connection time (as a query parameter, header, or initial message). The daemon maintains session → connection mapping.
- **Built-in framing.** RFC 6455 message framing — no DIY length-prefix encoding. Each WebSocket message is one MCP JSON-RPC message.
- **Built-in keepalive.** Ping/pong frames are part of the spec. Dead connection detection is automatic. Libraries handle this transparently.
- **Battle-tested libraries.** Go: `nhooyr.io/websocket` or `github.com/gorilla/websocket`. Python: `websockets`. Every language has production-quality implementations.
- **Additive to existing HTTP daemons.** WebSocket starts as an HTTP upgrade. Existing daemons (`quarry serve`, `biff serve`) add a `/mcp` WebSocket endpoint alongside their existing HTTP API. One server, two protocols — HTTP for REST clients (quarry-menubar, CLI), WebSocket for MCP proxy connections.
- **Works over TCP and Unix socket.** WebSocket libraries support custom dialers — can run over Unix domain socket for lowest latency, or TCP localhost for simplicity. The proxy doesn't care.
- **Network-capable (future).** Unlike raw Unix sockets, WebSocket works across machines (`wss://host/mcp`). Not needed today (all sessions are local), but doesn't close the door.

**Cons:**

- **HTTP upgrade handshake.** One extra round-trip at connection time (HTTP → 101 Switching Protocols → WebSocket). Negligible for a persistent connection, but it's there.
- **Slightly more overhead than raw socket.** WebSocket framing adds 2-10 bytes per message vs. 4 bytes for length-prefix. Irrelevant for local JSON-RPC traffic.
- **External dependency in Go.** Go's stdlib has no WebSocket support. Requires a third-party library (`nhooyr.io/websocket` is the standard choice — well-maintained, correct, minimal).
- **More complex than HTTP for simple cases.** If a project only needs request-response (no push), WebSocket is more machinery than needed. But the proxy is a shared component — it must handle the hardest case (biff), so the simpler projects come along for free.

**Verdict:** Combines the bidirectional capability of raw Unix sockets with the ecosystem maturity of HTTP. Solves the notification routing problem without DIY framing or keepalive. Adds naturally to existing HTTP daemons as a WebSocket upgrade endpoint.

### Comparison Matrix

| Criterion | HTTP | Raw Unix Socket | WebSocket |
|-----------|------|----------------|-----------|
| Server push (notifications) | No | Yes | Yes |
| Built-in framing | Yes (HTTP) | No (DIY) | Yes (RFC 6455) |
| Built-in keepalive | No | No (DIY) | Yes (ping/pong) |
| Persistent session | No (per-request) | Yes | Yes |
| Go stdlib support | Yes | Yes | No (third-party) |
| Existing daemon compat | Direct | New listener needed | HTTP upgrade endpoint |
| Cross-machine (future) | Yes | No | Yes |
| Latency | Low | Lowest | Low |
| Implementation complexity | Lowest | Medium | Low-Medium |

**Leaning toward Option C (WebSocket)** — it solves the notification routing problem (the hardest challenge identified for biff and lux) while adding naturally to existing HTTP daemons. To be revisited after further review.

## Design Goals

1. **Near-zero startup cost.** The proxy must spawn in <10ms and use <10MB memory. This is the entire point — if the proxy is expensive, it defeats the purpose. Go produces static binaries with instant startup; Python (~300ms, ~50MB) and Node (~100ms, ~30MB) do not meet this bar.

2. **Transparent JSON-RPC forwarding.** The proxy does not interpret tool names, arguments, or responses. It forwards the entire MCP protocol — `initialize`, `tools/list`, `tools/call`, `notifications/*`, everything. Tool discovery, listing, and execution pass through unchanged. The daemon is the real MCP server.

3. **Session identity injection.** The proxy resolves "which Claude Code session am I in?" and passes that identity to the daemon at connection time. This replaces process-tree walking heuristics in downstream projects. See [Session Identity](#session-identity).

4. **Single transport backend.** One proxy-to-daemon protocol. Every daemon speaks it. The transport must support bidirectional messaging (server push) because biff and lux require it.

5. **Single binary, no dependencies.** Ships as a static binary per platform (darwin/arm64, darwin/amd64, linux/arm64, linux/amd64). No runtime dependencies. Distributed alongside Python packages via `install.sh` or independently.

6. **Daemon lifecycle is not the proxy's job.** The proxy assumes the daemon is already running. If it can't connect, it exits with a clear error. Starting/stopping the daemon is the project's responsibility (e.g., `quarry serve`, `biff serve`, `lux display`).

## Session Identity

### The Problem

Biff uses the session key to determine "which Claude Code session is this?" — critical for per-session unread counts, TTY names, and `/talk` presence. The current mechanism (DES-011/011a in biff's DESIGN.md) walks the process tree upward to find the topmost `claude` ancestor PID. Both the MCP server and the status line converge on the same PID as a file key.

With a shared daemon, this breaks. The daemon has no Claude ancestor — it's a shared process serving all sessions. It cannot infer session identity from its own process tree.

### The Fix

The proxy *can* resolve the session key — it is still a child of the Claude Code process tree. It performs the ancestor walk once at startup, then passes the result to the daemon at connection time:

**HTTP (Option A):** Header on every request.

```text
mcp-proxy → POST /mcp
            X-Session-Key: 19147
```

**WebSocket / Unix Socket (Options B, C):** Registration message at connection time.

```json
{"type": "register", "session_key": "19147", "pid": 83201}
```

The daemon uses the provided key instead of inferring it. This is cleaner than the current model — no more process-tree walking heuristics inside the daemon, no more DES-011a/011b edge cases with local-vs-marketplace plugin process tree divergence.

### Resolution Algorithm

1. Parse the full process table (`ps -eo pid=,ppid=,comm=`)
2. Walk upward from the proxy's own PID
3. Find the topmost ancestor whose `comm` basename is `claude`
4. Fall back to `os.Getppid()` if no `claude` ancestor found
5. Cache the result — the ancestor PID never changes for the lifetime of the process

This is a direct port of `find_session_key()` from `biff/src/biff/session_key.py`.

## Target Daemons

### Quarry (`quarry serve`)

**Daemon**: `quarry serve` — stdlib HTTP server, already exists. Translates JSON requests to core library calls.

**Current model**: `quarry mcp` spawns a full MCP server per session. Each loads the LanceDB index and ONNX embedding model into memory. The `quarry serve` HTTP server exists independently but is not used by the MCP server.

**Proxy model**: Proxy connects to `quarry serve` (which gains a WebSocket/socket endpoint). One index, one model, many sessions.

**Complications**:

- **Fire-and-forget pattern.** Quarry's MCP server uses a `ThreadPoolExecutor(max_workers=4)` for side-effect tools (ingest, delete, sync). These return an optimistic response immediately and process in the background. The daemon must preserve this behavior — the proxy forwards the optimistic response, the daemon does the background work. With a bidirectional transport (Options B/C), the daemon could optionally push a completion notification when background work finishes — an improvement over the current fire-and-forget-with-no-feedback model.
- **Database switching.** The `use` MCP tool switches the active database. In the current model, this is per-process state. With a shared daemon, database selection must be per-session (keyed by session identity from the proxy). The daemon needs session-scoped state for the active database name.
- **Embedding model memory.** The ONNX model is ~100MB in memory. Sharing it across sessions is the primary memory win. But the model is loaded lazily on first search — the daemon must load it once and share it.
- **Sync registry.** Directory registrations (`register_directory`, `deregister_directory`) are currently global. With a shared daemon this is fine — registrations are per-database, not per-session.
- **MCP `instructions` field.** Quarry's MCP server sets an `instructions` field on the server that loads formatting guidance. The proxy must forward this from the daemon's `initialize` response without modification.

### Biff (`biff serve`)

**Daemon**: `biff serve` — exists, supports both stdio and HTTP transports via the same `_create_mcp_server()` function.

**Current model**: `biff mcp` spawns per session. Each creates its own `ServerState` with a `Relay` (NATS or local). The session key (user:tty) is generated at startup.

**Proxy model**: Proxy connects to `biff serve` (which gains a WebSocket/socket endpoint). One NATS connection, many sessions.

**Complications**:

- **Session identity is foundational.** Biff's entire architecture revolves around the `{user}:{tty}` session key (DES-002). The daemon must maintain per-session state: TTY name, unread counts, talk partner, mesg mode, plan text. The proxy's session registration becomes the demux key.
- **PPID-keyed unread files.** The status line reads `~/.biff/unread/{ppid}.json`. With a shared daemon, the daemon still needs to write these files keyed by the Claude ancestor PID — which it receives from the proxy at connection time. This actually *fixes* DES-011b (local vs marketplace process tree divergence) because the proxy always resolves from its own tree.
- **Dynamic tool descriptions.** Biff mutates tool descriptions per-session (unread count, talk partner). With a shared daemon, the `tools/list` response must be customized per session. The persistent connection (Options B/C) makes this natural — the daemon knows which session is asking because each connection is registered.
- **`tools/list_changed` notifications.** The background poller fires `tools/list_changed` when unread counts change. With HTTP (Option A), this is the hardest problem — no way to push. **With WebSocket or Unix socket (Options B/C), this is solved** — the daemon pushes the notification down the correct session's persistent connection. The proxy writes it to stdout. Claude Code receives it as if the MCP server sent it directly.
- **Belt-and-suspenders notification paths.** The current "belt" path (inside tool handler) and "suspenders" path (background poller) both assume one session per process. With a shared daemon, the suspenders path becomes: poller detects count change → daemon looks up session's WebSocket/socket connection → pushes notification. Simpler than the current two-path design because the daemon has direct access to every session's connection.
- **Lifespan cleanup.** The MCP server deletes its PPID-keyed unread file on shutdown. With a persistent connection (Options B/C), the daemon detects proxy disconnect via connection close (or keepalive timeout). Cleanup triggers on disconnect, not on daemon stop. WebSocket ping/pong (Option C) makes dead connection detection automatic.
- **NATS connection conservation.** DES-019 settled on keeping the NATS connection open even during idle (nap mode). With a shared daemon, this is simpler — one persistent connection serves all sessions. The nap-mode polling frequency reduction can still apply per-session for the poller logic.
- **Long-lived sessions with idle time.** DES-008 requires sessions to persist for 30 days. The daemon must not garbage-collect sessions just because a proxy disconnected temporarily (e.g., Claude Code restart). Session state should persist in the relay (NATS KV / local filesystem) with the proxy connection being a transient delivery channel, not the session's lifetime.

### Vox (new daemon needed)

**Daemon**: Does not exist yet. Vox currently has no `serve` command. One would need to be built.

**Current model**: `vox mcp` spawns per session. Each loads provider configuration and manages its own audio queue. Cross-session coordination uses `flock` on `~/.punt-vox/playback.lock`.

**Proxy model**: Proxy connects to a new `vox serve` daemon. One audio queue, no file locking needed.

**Complications**:

- **Audio serialization moves from flock to in-process queue.** The daemon replaces file-based locking (DES-013) with an in-process audio queue. This is strictly simpler — one queue, FIFO, no lock files, no cross-process coordination. But it's a behavioral change: currently, hooks (`notify.sh`, `notify-permission.sh`) call `vox play <path>` directly, acquiring the flock independently. With a daemon, these callers must either go through the daemon or the flock must coexist with the daemon's queue.
- **Hook call path.** DES-017 establishes that hooks call CLI directly (~110ms, no LLM round-trip). The Stop hook calls `vox play chime.mp3`. If audio moves to the daemon, the `vox play` CLI command must forward to the daemon instead of playing directly. This changes the hook → CLI → direct-playback path to hook → CLI → daemon → playback. The CLI becomes a thin client to the daemon for playback commands.
- **Per-session config.** Vox has per-project config (`.vox/config.md` with YAML frontmatter, DES-012). The daemon would need to know which session is asking for config to apply the right project settings. The proxy passes project directory at connection time alongside session identity.
- **Notification architecture.** DES-001 uses a Stop hook with `decision: "block"` to force a spoken summary. The hook reads `.vox/tts.local.md` for config state. This is all shell-side and doesn't touch the MCP server — it calls `vox` CLI. The daemon change doesn't affect this path as long as the CLI forwards to the daemon.
- **Provider API clients.** ElevenLabs, Polly, and OpenAI clients are currently instantiated per session. Sharing them via a daemon saves connection setup time but requires thread-safe access.

### Lux (`lux display` + `lux serve`)

**Daemon**: The display server (`lux display`) already exists as a persistent Unix socket server. The MCP server (`lux serve`) is separate — spawned per session, connects to the display socket.

**Current model**: Each session spawns `lux serve` (stdio MCP), which connects to the shared `lux display` Unix socket. The display server is already centralized.

**Proxy model**: Proxy connects to a Lux daemon that bridges MCP to the display protocol.

**Complications**:

- **Protocol mismatch.** The display server speaks its own length-prefixed JSON protocol (DES-003), not MCP JSON-RPC. The proxy cannot forward MCP directly to the display socket. Options: (a) add a WebSocket/socket MCP endpoint to `lux serve` so it becomes the daemon, bridging MCP JSON-RPC to the display's native protocol, (b) add MCP support directly to the display server. Option (a) is more natural — `lux serve` already does this translation, it just needs to become a persistent shared process instead of per-session.
- **Bidirectional events.** The display sends interaction events (button clicks, slider changes) back to the client. In MCP, the client (Claude) calls `recv()` to poll for events. With a persistent connection (Options B/C), the daemon can push events to the correct proxy immediately — the proxy writes them as MCP responses or notifications. This eliminates the `recv()` polling pattern entirely if events become push-based.
- **Scene ownership.** Multiple sessions may want to show content on the display simultaneously. The display uses tabs to separate content (DES-005 element vocabulary includes `tab_bar`). Session identity from the proxy could auto-scope each session's content to its own tab.
- **Socket discovery.** Lux uses `$XDG_RUNTIME_DIR/lux/display.sock` with fallbacks (DES-002). The daemon (whether `lux serve` or an extended `lux display`) must advertise its MCP endpoint. A known port or socket path in the same XDG directory works.
- **`recv()` blocking.** The `recv` MCP tool blocks waiting for user interaction events. With a persistent connection, this is a standard async pattern — the daemon holds the pending `tools/call` response until an event arrives or the timeout expires. Cleaner than the current synchronous poll.

## Install Impact

Current install scripts (`install.sh` for quarry, biff, vox) follow this pattern:

1. Check prerequisites (claude CLI, git)
2. Install uv (Python package manager)
3. Install Python 3.13+
4. `uv tool install` the Python package
5. Register the Claude Code plugin via marketplace

With mcp-proxy, the install script gains one additional step: download the mcp-proxy binary for the current platform. This is a ~5MB static binary — a single `curl` to a GitHub release asset, placed on `$PATH`.

```sh
# New step in install.sh (between step 4 and 5):
info "Installing mcp-proxy..."
ARCH=$(uname -m)
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
curl -fsSL "https://github.com/punt-labs/mcp-proxy/releases/latest/download/mcp-proxy-${OS}-${ARCH}" \
  -o "$HOME/.local/bin/mcp-proxy"
chmod +x "$HOME/.local/bin/mcp-proxy"
ok "mcp-proxy installed"
```

### Daemon Lifecycle

The install script must also ensure the daemon is running (or instruct the user to start it). Options:

- **Explicit start**: The install script prints "Run `quarry serve` to start the daemon" — simple, manual.
- **Launchd/systemd service**: The install script registers a launch agent/service file for the daemon. Auto-starts on login. More complex but zero-touch.
- **On-demand via proxy**: The proxy could start the daemon if it's not running. This couples the proxy to the daemon lifecycle (violating goal 6) but improves UX.

### Plugin Registration

The plugin.json `mcpServers` entry changes from:

```json
{"type": "stdio", "command": "quarry", "args": ["mcp"]}
```

to:

```json
{"type": "stdio", "command": "mcp-proxy", "args": ["ws://localhost:8080/mcp"]}
```

This is a breaking change for existing installs. The install script must handle the upgrade path — detect the old registration and replace it.

### Shared Binary

mcp-proxy becomes a shared dependency across the org, similar to `uv` today. Every project's `install.sh` downloads the same binary. The version can be pinned per-project (in `install.sh`) or always-latest. A version mismatch between proxy and daemon should not break the protocol — the wire format (JSON-RPC) is stable.

## Build & Release

```sh
# Build for current platform
go build -o mcp-proxy .

# Cross-compile for all targets
GOOS=darwin  GOARCH=arm64 go build -o dist/mcp-proxy-darwin-arm64 .
GOOS=darwin  GOARCH=amd64 go build -o dist/mcp-proxy-darwin-amd64 .
GOOS=linux   GOARCH=arm64 go build -o dist/mcp-proxy-linux-arm64  .
GOOS=linux   GOARCH=amd64 go build -o dist/mcp-proxy-linux-amd64  .
```

Release artifacts are GitHub release assets. Each project's `install.sh` downloads the correct binary by platform.

## Open Questions

1. **Transport decision.** HTTP, raw Unix socket, or WebSocket? See [Transport Options](#transport-options). Leaning WebSocket. To be revisited.

2. **Daemon auto-start.** Should the proxy start the daemon if it's not running, or is that always the user's/system's responsibility?

3. **Graceful degradation.** If the daemon is down, should the proxy fall back to spawning a full in-process server (i.e., degrade to Model 1), or fail fast with a clear error?

4. **Lux daemon identity.** Should `lux serve` become the shared MCP daemon (bridging to the display socket), or should MCP support be added directly to the display server?

5. **Hook CLI commands.** Vox and Lux hooks call CLI commands directly (e.g., `vox play`, `lux hook post-bash`). Should these CLI commands forward to the daemon, or continue to work independently? If they forward, the daemon must also handle non-MCP requests from CLI callers.

6. **WebSocket over Unix socket vs TCP.** If WebSocket is chosen, should daemons listen on a Unix socket (lowest latency, no port conflicts) or TCP localhost (simpler configuration, standard URLs)? Both work — the proxy library supports custom dialers.

## Prior Art to Research

- **Beads.** The beads project moved away from a daemon architecture to something else. Research what they changed to and why — the lessons learned may directly apply. Believed to be Go-based.
- **Entire.io.** Check their approach to MCP server architecture and proxy/daemon patterns. Also believed to use Go. Their "provenance gap" work (reason-trace lineage) may have solved similar multi-session coordination problems.

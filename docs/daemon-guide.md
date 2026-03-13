# Daemon Guide

How to build an MCP daemon that works with mcp-proxy.

## Requirements

Your daemon must:

1. **Accept WebSocket connections** on the path the proxy will dial (typically `/mcp`).
2. **Negotiate the `mcp` subprotocol** via `Sec-WebSocket-Protocol: mcp` on the upgrade handshake.
3. **Speak MCP JSON-RPC 2.0** — one JSON object per WebSocket text frame, newline-terminated on stdout.
4. **Respond to WebSocket pings.** The proxy sends WebSocket-level ping frames every 5 seconds (configurable). Your daemon must respond with pong frames. Most WebSocket libraries handle this automatically at the protocol layer — you typically don't need to write any code for this. If pongs stop arriving, the proxy treats the daemon as unresponsive and reconnects.

## WebSocket Ping/Pong

The proxy uses standard [RFC 6455 ping/pong frames](https://www.rfc-editor.org/rfc/rfc6455#section-5.5.2) for keepalive. These are control frames handled by the WebSocket implementation, not application-level messages.

**Default behavior by library:**

| Library | Auto-pong | Notes |
|---------|-----------|-------|
| Go `coder/websocket` | Yes | Handled internally by the read loop |
| Go `gorilla/websocket` | Yes, with `SetPingHandler` | Default handler sends pong; override to add logging |
| Python `websockets` | Yes | Handled transparently |
| Python `aiohttp` | Yes | With `autoping=True` (default) |
| Node `ws` | Yes | Responds to pings automatically |
| Rust `tokio-tungstenite` | No | Must explicitly handle `Message::Ping` and send `Message::Pong` |

**If your library doesn't auto-pong**, you need to handle ping frames in your read loop:

```python
# Python websockets — already automatic, but for illustration:
async for message in websocket:
    if isinstance(message, websockets.frames.Ping):
        await websocket.pong(message.data)
    else:
        handle_mcp_message(message)
```

```rust
// Rust tokio-tungstenite — must handle explicitly:
while let Some(msg) = ws_stream.next().await {
    match msg? {
        Message::Ping(data) => ws_stream.send(Message::Pong(data)).await?,
        Message::Text(text) => handle_mcp_message(&text),
        _ => {}
    }
}
```

## Timing

| Parameter | Default | Env var | Effect |
|-----------|---------|---------|--------|
| Ping interval | 5s | `MCP_PROXY_PING_INTERVAL` | How often the proxy pings |
| Pong timeout | 2s | `MCP_PROXY_PONG_TIMEOUT` | How long to wait for pong before reconnecting |
| Dial timeout | 5s | — | How long to wait for initial connection |

Worst case, a hung daemon is detected in **ping interval + pong timeout** (7 seconds with defaults).

Set `MCP_PROXY_PING_INTERVAL=0` to disable keepalive entirely (not recommended).

## Session Identity

The proxy appends `?session_key=<pid>` to the WebSocket upgrade URL. This is the PID of the topmost `claude` process in the proxy's process tree. Use it to maintain per-session state:

```go
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
    sessionKey := r.URL.Query().Get("session_key")
    // sessionKey identifies which Claude Code tab spawned this connection
}
```

## Authentication

If `MCP_PROXY_TOKEN` is set, the proxy sends `Authorization: Bearer <token>` on the WebSocket upgrade request. Your daemon should validate this header if it requires authentication.

For local daemons binding to `127.0.0.1`, authentication is typically unnecessary.

## Hook Endpoint

If your daemon handles [hook relay](../README.md#hook-relay) messages, expose a `/hook` WebSocket endpoint. The proxy connects to `/hook` (not `/mcp`) for hook mode.

Hook messages are JSON-RPC with method `hook/<event>` (e.g., `hook/PreToolUse`). The proxy wraps the hook payload as `params`:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "hook/PreToolUse",
  "params": { "tool": "Bash", "input": "ls" }
}
```

For sync hooks, respond with a JSON-RPC result. For async hooks (notifications without `id`), no response is needed — the proxy performs a graceful WebSocket close after sending.

## Read Limits

The proxy sets a 1MB read limit on WebSocket frames. Keep your MCP responses under this size, or the connection will be closed with a protocol error.

## Reconnect Behavior

The proxy reconnects with exponential backoff (250ms to 5s cap) on:

- TCP connection loss
- WebSocket close
- Keepalive failure (pong timeout)

Your daemon should be prepared for connections to drop and reconnect. Avoid storing critical state solely in the WebSocket connection — use the session key to look up persisted state.

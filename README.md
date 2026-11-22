# mcp-proxy

> Go binary that bridges MCP stdio to a shared daemon over WebSocket.

[![License](https://img.shields.io/github/license/punt-labs/mcp-proxy)](LICENSE)
[![CI](https://img.shields.io/github/actions/workflow/status/punt-labs/mcp-proxy/test.yml?label=CI)](https://github.com/punt-labs/mcp-proxy/actions/workflows/test.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/punt-labs/mcp-proxy.svg)](https://pkg.go.dev/github.com/punt-labs/mcp-proxy)

`mcp-proxy` lets many Claude Code sessions share one MCP server process. MCP (Model Context Protocol) is the protocol Claude Code uses to call tools in a separate server program; normally Claude Code starts one server process per session and talks to it over stdin and stdout (stdio). You configure Claude Code to start `mcp-proxy` instead, and the proxy forwards each message over a WebSocket to a single long-running server, called the daemon here. Any MCP server that accepts MCP over a WebSocket endpoint works with it unchanged.

**Platforms:** macOS, Linux

## Quick Start

```bash
curl -fsSL https://raw.githubusercontent.com/punt-labs/mcp-proxy/1817fa3/install.sh | sh
```

The installer requires the `claude` CLI, `git`, and `curl`. It downloads the release binary for your platform, checks its SHA-256 checksum, installs it to `~/.local/bin/mcp-proxy`, and registers the Punt Labs plugin marketplace (`punt-labs/claude-plugins`) in Claude Code. Without Claude Code installed, use the manual install below.

<details>
<summary>Manual install</summary>

Create the install directory:

```bash
mkdir -p ~/.local/bin
```

Download the binary. This example is for macOS on Apple silicon; replace `darwin-arm64` with `darwin-amd64`, `linux-arm64`, or `linux-amd64` to match your platform:

```bash
curl -fsSL https://github.com/punt-labs/mcp-proxy/releases/latest/download/mcp-proxy-darwin-arm64 -o ~/.local/bin/mcp-proxy
```

Make it executable:

```bash
chmod +x ~/.local/bin/mcp-proxy
```

Make sure `~/.local/bin` is on your `PATH`.

Or build from source with Go:

```bash
go install github.com/punt-labs/mcp-proxy/cmd/mcp-proxy@latest
```

</details>

<details>
<summary>Inspect before running</summary>

Download the installer:

```bash
curl -fsSL https://raw.githubusercontent.com/punt-labs/mcp-proxy/1817fa3/install.sh -o install.sh
```

Read it:

```bash
cat install.sh
```

Run it:

```bash
sh install.sh
```

</details>

<details>
<summary>Verify</summary>

Check the installed version:

```bash
mcp-proxy --version
```

Check that a running daemon accepts connections (replace the URL with your daemon's):

```bash
mcp-proxy --health ws://localhost:8420/mcp
```

</details>

Then, in your Claude Code MCP configuration, run `mcp-proxy` with the daemon's URL in place of the MCP server command:

```json
{
  "mcpServers": {
    "example": {
      "type": "stdio",
      "command": "mcp-proxy",
      "args": ["ws://localhost:8420/mcp"]
    }
  }
}
```

## Features

- **One server for many sessions** --- every Claude Code session connects to the same daemon, so state such as loaded models, connection pools, or a single device is held once instead of once per session.
- **Reconnect** --- when the daemon disconnects, the proxy keeps the Claude Code session open, retries with backoff, and resends the message that had not been delivered.
- **Handshake replay** --- after a reconnect, the proxy resends the client's cached MCP `initialize` request and `notifications/initialized` notification, so a restarted or upgraded daemon can take requests without a Claude Code restart.
- **Keepalive** --- WebSocket ping and pong frames detect a daemon that holds the connection open but has stopped responding.
- **Session key** --- the proxy tells the daemon which Claude Code session it serves, so the daemon can keep separate state per session.
- **Hook relay** --- forwards a Claude Code hook payload to the daemon and prints its reply.
- **Health check** --- one command that tells you whether the daemon is reachable.

## Commands

The daemon URL is given as a positional argument or through `--config <profile>` (see [Config File](#config-file)). A URL on the command line takes precedence over the profile's URL. In hook mode the URL and `--hook <event>` can appear in either order. `mcp-proxy --help` prints the full flag reference.

| Invocation | What it does |
|------------|--------------|
| `mcp-proxy <url>` | Proxy mode. Reads JSON-RPC messages from stdin, one per line, sends each to the daemon as a WebSocket text frame, and writes the daemon's responses and server-initiated messages to stdout. Reconnects when the daemon disconnects. |
| `mcp-proxy --health <url>` | Health check. Connects to the daemon and closes the connection. Prints `mcp-proxy: ok` to stderr and exits `0` on success; prints the error and exits `1` on failure. |
| `mcp-proxy <url> --hook <event>` | Hook relay. Reads a JSON payload from stdin, sends it to the daemon's `/hook` endpoint as a JSON-RPC request with method `hook/<event>`, waits for the reply, and writes the result to stdout. A JSON-RPC error reply goes to stderr and the exit code is `1`. |
| `mcp-proxy <url> --hook --async <event>` | Asynchronous hook relay. Sends the payload as a JSON-RPC notification (no reply expected) and closes the connection cleanly so the message is delivered before the proxy exits. |
| `mcp-proxy --config <profile>` | Reads the URL, extra headers, and CA certificate from `~/.punt-labs/mcp-proxy/<profile>.toml`. Combines with any of the modes above. |
| `mcp-proxy --version` or `mcp-proxy version` | Prints `mcp-proxy <version>` and exits `0`. |
| `mcp-proxy --help` or `mcp-proxy -h` | Prints the full flag reference and exits `0`. |

In proxy mode the proxy does not change message contents. It reads only the `method` and `id` fields of outgoing messages, to cache the handshake it replays after a reconnect.

## Setup

### Environment Variables

| Variable | Default | Effect |
|----------|---------|--------|
| `MCP_PROXY_TOKEN` | *(unset)* | Bearer token sent as `Authorization: Bearer <token>` when the WebSocket connection opens. |
| `MCP_PROXY_PING_INTERVAL` | `5s` | How often the proxy sends a WebSocket ping. Set to `0` to turn keepalive off. |
| `MCP_PROXY_PONG_TIMEOUT` | `2s` | How long the proxy waits for a pong before it treats the daemon as unresponsive and reconnects. |
| `MCP_PROXY_DEBUG` | *(unset)* | `1` or `true` enables DEBUG records in the private log. Custom log paths are no longer supported. |
| `PUNT_TELEMETRY` | *(unset)* | `off` disables all Punt Labs usage capture. |
| `MCP_PROXY_TELEMETRY` | *(unset)* | `off` disables mcp-proxy usage capture. |

### Config File

Instead of passing a URL, you can keep connection details in a named profile:

```bash
mcp-proxy --config example
```

This reads `~/.punt-labs/mcp-proxy/example.toml`:

```toml
[example]
url = "wss://daemon.example.com:8420/mcp"
ca_cert = "/path/to/ca.pem"

[example.headers]
Authorization = "Bearer <token>"
```

- `url` is the daemon URL (`ws://` or `wss://`).
- `ca_cert` is the path to a PEM CA certificate used to verify the daemon's TLS certificate. It applies only to `wss://` URLs; without it the system certificates are used.
- `[<profile>.headers]` lists extra HTTP headers sent when the connection opens. An `Authorization` header here overrides `MCP_PROXY_TOKEN`.

The file must have `0600` permissions or stricter; the proxy exits with an error if it does not. If the file or the `[<profile>]` section is missing, the proxy uses `ws://localhost:8420/mcp`. A URL on the command line takes precedence over `url`; the headers and `ca_cert` still apply.

### Diagnostics and Usage

Proxy, hook, and health diagnostics are written at INFO and above to
`~/.punt-labs/mcp-proxy/logs/mcp-proxy.log`. The log and its five
rotating backups are private (`0600`) and limited to 5 MiB each. Look
there for connection, retry, and shutdown diagnostics. Logs do not go to
MCP stdout or contain URLs, tokens, or message contents.
If the rotation lock is held for two seconds during startup, the command
warns once on stderr and continues without file diagnostics.

By default, each completed proxy, hook, or health operation records its
kind, duration, and outcome in `~/.punt-labs/mcp-proxy/telemetry.db`.
The private SQLite store retains usage for 90 days. No usage leaves the
machine. Either environment off switch above disables capture without
disabling diagnostic logs. To disable capture in configuration, put
`disabled = true` in `~/.punt-labs/mcp-proxy/telemetry.conf` (TOML). The same
file accepts `retention_days` from 1 to 3650. The settings file is separate
from daemon connection profiles.

## Exit Codes

| Code | Meaning |
|------|---------|
| `0` | Proxy stopped because stdin closed, health check succeeded, or hook relay succeeded |
| `1` | Runtime error, health check failure, daemon error reply, or shutdown by signal |
| `2` | Usage error (missing or malformed arguments) |

## Signal Handling

The first `SIGINT` or `SIGTERM` closes the daemon connection and stops the proxy with exit code `1`. A second signal forces an immediate exit.

## Daemon Requirements

A daemon that `mcp-proxy` connects to must:

1. Accept WebSocket connections with the `mcp` subprotocol (`Sec-WebSocket-Protocol: mcp`).
2. Speak MCP JSON-RPC 2.0, one JSON object per WebSocket text frame.
3. Be started separately. The proxy does not start the daemon; if the daemon is unreachable, the proxy retries with backoff until it is.

A daemon can also read the `session_key` query parameter to keep state per session, and can push server-initiated messages (for example `notifications/tools/list_changed`), which the proxy writes to stdout as they arrive.

[docs/daemon-guide.md](docs/daemon-guide.md) covers WebSocket ping and pong handling, session identity, authentication, and the hook endpoint.

## How It Works

```text
                    stdio                      WebSocket
Claude Code ◄──────────────► mcp-proxy ◄──────────────────────► daemon
             MCP JSON-RPC                                       (one process)
```

**Session identity.** The proxy finds the topmost `claude` process among its ancestors and sends that process ID as `?session_key=<pid>` when it connects. Every `mcp-proxy` started by the same Claude Code session therefore sends the same key. If no `claude` ancestor is found, the proxy sends its parent process ID instead.

**Forwarding.** Messages pass in both directions over one WebSocket connection. Messages the daemon sends on its own, such as `notifications/tools/list_changed`, appear on stdout as soon as they arrive.

**Reconnect.** When the connection is lost (network failure, WebSocket close, or pong timeout), the proxy reconnects with exponential backoff starting at 250 ms and capped at 5 seconds. Messages read from stdin during the outage are sent on the new connection. Connection status is recorded in the private log.

**Keepalive.** The proxy sends a WebSocket ping every `MCP_PROXY_PING_INTERVAL`. If no pong arrives within `MCP_PROXY_PONG_TIMEOUT`, it closes the connection and reconnects. This catches a daemon whose TCP connection stays open after it has stopped processing messages.

**Formal specification.** The bridge protocol is described in a [Z specification](docs/mcp-proxy.tex) that has been model-checked with ProB. Some Go tests are derived from the test partitions of that specification.

## Documentation

[Design decisions](DESIGN.md) |
[Daemon guide](docs/daemon-guide.md) |
[Distribution](docs/distribution.md) |
[Z specification](docs/mcp-proxy.tex) |
[Contributing](CONTRIBUTING.md) |
[Changelog](CHANGELOG.md)

## License

MIT

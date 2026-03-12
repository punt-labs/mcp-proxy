# Distribution Plan

This document defines how mcp-proxy and its primary consumer (quarry) are distributed and managed across macOS and Linux.

## mcp-proxy

Static Go binary with no runtime dependencies. Four distribution channels:

| Platform | Channel | Install Command |
|----------|---------|-----------------|
| macOS | Homebrew tap | `brew install punt-labs/tap/mcp-proxy` |
| Linux | `.deb` package | `sudo dpkg -i mcp-proxy_*.deb` |
| Both | `go install` | `go install github.com/punt-labs/mcp-proxy@latest` |
| Both | Binary download | `curl -fsSL .../mcp-proxy-{os}-{arch}` |

### Homebrew Formula

Lives in `punt-labs/homebrew-tap`. Downloads the GitHub Release binary for the user's platform. Updated automatically by the release workflow.

### `.deb` Package

Built by `nfpm` in the GitHub Actions release workflow. Installs `mcp-proxy` to `/usr/local/bin/`. No service unit — mcp-proxy is a short-lived process spawned by Claude Code, not a daemon.

### `go install`

Works out of the box — module path is `github.com/punt-labs/mcp-proxy` with `main.go` at the root. Requires Go toolchain.

### Binary Download

Four platform binaries attached to each GitHub Release with SHA256 checksums:

- `mcp-proxy-darwin-arm64`
- `mcp-proxy-darwin-amd64`
- `mcp-proxy-linux-arm64`
- `mcp-proxy-linux-amd64`

## quarry Daemon Management

quarry is the primary consumer of mcp-proxy. It runs as a persistent daemon (`quarry serve`) that multiple Claude Code sessions connect to through the proxy.

### Fixed Port

quarry serve uses a fixed well-known port (e.g., 8420) by default. This eliminates port discovery complexity — the MCP config uses a static URL.

### macOS: Homebrew + brew services

```bash
brew install punt-labs/tap/quarry
brew services start quarry
```

The Homebrew formula includes a `service` block that writes a launchd plist to `~/Library/LaunchAgents/`. The daemon starts at login, restarts on crash.

```ruby
service do
  run [opt_bin/"quarry", "serve", "--port", "8420"]
  keep_alive true
  log_path var/"log/quarry.log"
  error_log_path var/"log/quarry.log"
end
```

### Linux: `quarry install` + systemd

```bash
pip install punt-quarry      # or: uv tool install punt-quarry
quarry install               # writes + enables systemd user unit
```

`quarry install` detects the platform and writes the appropriate service file:

- **Linux**: `~/.config/systemd/user/quarry.service`, then `systemctl --user enable --now quarry`
- **macOS** (fallback for non-Homebrew installs): `~/Library/LaunchAgents/com.punt-labs.quarry.plist`, then `launchctl load`

The systemd unit:

```ini
[Unit]
Description=Quarry semantic search daemon
After=network.target

[Service]
ExecStart=%h/.local/bin/quarry serve --port 8420
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

### Uninstall

`quarry uninstall` removes the service file and stops the daemon. Clean inverse of `quarry install`.

## End-to-End Setup

### macOS

```bash
brew install punt-labs/tap/quarry punt-labs/tap/mcp-proxy
brew services start quarry
```

### Linux

```bash
pip install punt-quarry
quarry install
go install github.com/punt-labs/mcp-proxy@latest
```

### Claude Code Configuration

Once quarry and mcp-proxy are installed:

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

Claude Code spawns `mcp-proxy` per session (<10ms, <10MB). Each proxy connects to the shared quarry daemon over WebSocket. All sessions share one embedding model, one database connection.

## Analogous Software

| Software | macOS | Linux | Pattern |
|----------|-------|-------|---------|
| Ollama | `brew install` + brew services | curl install script + systemd | Local ML model server |
| PostgreSQL | `brew install` + brew services | `apt install` + systemd | Database daemon |
| Redis | `brew install` + brew services | `apt install` + systemd | In-memory store |
| Tailscale | `brew install` + launchd | `apt install` + systemd | VPN daemon |

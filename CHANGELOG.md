# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- `ca_cert` field in profile TOML (`~/.punt-labs/mcp-proxy/<profile>.toml`) —
  path to a PEM CA certificate for TLS verification. When set, WebSocket dials
  use a custom cert pool pinned to that CA (system roots excluded). Required for
  quarry remote connections over `wss://` with a self-signed CA.
- `CACertError` typed error for cert-file load failures (missing file, invalid PEM).

### Security

- TLS minimum version raised to TLS 1.3 when a CA cert path is configured.

## [0.3.0] - 2026-03-30

### Added

- `--config <profile>` flag reads `~/.punt-labs/mcp-proxy/<profile>.toml`, extracting `[<profile>].url` and `[<profile>.headers]` for use as daemon URL and WebSocket upgrade headers
- Config file permission enforcement: exits with error if file permissions are wider than 0600
- `internal/config` package for profile loading with silent fallback to `ws://localhost:8420/mcp` when file or section is absent

## [0.2.0] - 2026-03-13

### Added

- `--version` flag prints the binary version (`dev` locally, release version in published binaries)
- WebSocket keepalive with ping/pong liveness detection (5s/2s defaults, configurable via env vars)
- `install.sh` following punt-labs convention (platform detection, SHA256 verification, marketplace registration)
- `docs/daemon-guide.md` — how to build a daemon compatible with mcp-proxy

### Changed

- Makefile: `build` and `dist` targets inject version via `-ldflags` (`make build VERSION=X.Y.Z`)
- Release workflow uses Makefile targets (`make lint test`, `make dist`) instead of inline commands
- Static build enforcement: `CGO_ENABLED=0` on all build targets
- README reframed around runtime protection with simplified install section

## [0.1.0] — 2026-03-12

### Added

- Hook relay mode (`--hook`) for one-shot JSON-RPC calls from Claude Code hook scripts to the daemon
- Async hook mode (`--hook --async`) for fire-and-forget notifications with graceful close
- Dedicated `/hook` WebSocket endpoint (no MCP subprotocol, no initialize handshake)
- Automatic reconnect with exponential backoff when daemon disconnects (250ms–5s)
- Message preservation across reconnects — no messages lost during daemon restart
- `--health` flag for liveness probes (`mcp-proxy --health ws://localhost:8420/mcp`)
- Bidirectional JSON-RPC forwarding between stdio and WebSocket daemon
- Session identity resolution via process tree walking (ported from biff)
- WebSocket transport with typed connection errors
- Debug file logging via `MCP_PROXY_DEBUG` environment variable
- Double-signal handling: first SIGINT/SIGTERM graceful, second force-exits
- Z specification with ProB model checking (6 states, 43 transitions, all invariants verified)
- Mock daemon test infrastructure for integration testing
- E2E binary tests with build tag `e2e`
- TTF partition tests derived from Z specification (21 testable partitions covered)
- Hardening tests: 1MB boundary, broken stdout, partial line + EOF, exit timing
- GitHub Actions release workflow — cross-compile + SHA256 checksums on `v*` tags
- Install docs: Homebrew, `go install`, binary download
- Quarry integration tests: real MCP roundtrip through proxy to quarry daemon
- Bearer token authentication via `MCP_PROXY_TOKEN` environment variable for remote/authenticated daemons

### Fixed

- WebSocket dial now negotiates `mcp` subprotocol (required by MCP SDK WebSocket transport)
- Flaky `TestHardening_PartialLineBeforeEOF` — poll daemon received messages instead of asserting immediately after bridge shutdown

# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

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

### Fixed

- WebSocket dial now negotiates `mcp` subprotocol (required by MCP SDK WebSocket transport)

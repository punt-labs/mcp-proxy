# Distribution

How `mcp-proxy` ships today.

## Binary

Static Go binary, no runtime dependencies. Cross-compiled to four targets and attached to every GitHub Release:

- `mcp-proxy-darwin-arm64`
- `mcp-proxy-darwin-amd64`
- `mcp-proxy-linux-arm64`
- `mcp-proxy-linux-amd64`

Each release also carries a `checksums.txt` with SHA-256 sums. `make dist` cross-compiles release-style binaries with `CGO_ENABLED=0` and `-ldflags="-s -w -X main.version=$(VERSION)"` for a static build, stripped symbols, and a stamped version. Byte-for-byte reproducibility across machines depends on toolchain, dependency, and environment pinning that this Makefile does not enforce.

## Install channels

### `curl | sh` (recommended)

The canonical install command lives in the [README's Quick Start](../README.md#quick-start) and is pinned to the latest release commit. Do not copy an install command from this doc into other repositories — always refer downstream users at the README so they pick up the current pin.

`install.sh` detects the platform, downloads the matching binary from the latest GitHub Release, drops it in `~/.local/bin/mcp-proxy`, and adds a `PATH` hint if needed.

### `go install`

```bash
go install github.com/punt-labs/mcp-proxy/cmd/mcp-proxy@latest
```

Module path is `github.com/punt-labs/mcp-proxy` with the binary entry point under `cmd/mcp-proxy/`; requires a Go toolchain matching the module's `toolchain` directive. Callers on v0.4.x and earlier used `go install github.com/punt-labs/mcp-proxy@latest` (main package at the repo root); that path stopped working when v0.5.0 moved the entry point under `cmd/mcp-proxy/` (DES-017).

### Manual download

Grab the binary directly from the [latest release](https://github.com/punt-labs/mcp-proxy/releases/latest) and place it on your `PATH`. See the README's Quick Start for a `<details>` block with copy-pastable commands.

## Not yet shipped

- **Homebrew tap.** No formula exists.
- **Linux `.deb`.** Tracked as `mcp-proxy-e34`. Would be built by `nfpm` in the release workflow.

## Lifecycle

`mcp-proxy` is a short-lived process spawned by Claude Code, not a long-running daemon. There is no service unit to install, no launchd plist, no systemd file. Claude Code owns the process lifecycle; the proxy exits when stdin closes.

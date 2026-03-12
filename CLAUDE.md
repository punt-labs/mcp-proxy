# mcp-proxy

Lightweight Go proxy bridging MCP stdio transport to shared daemon processes.

## Principal Engineer Mindset

There is no such thing as a "pre-existing" issue. If you see a problem — in code you wrote, code a reviewer flagged, or code you happen to be reading — you fix it. Do not classify issues as "pre-existing" to justify ignoring them. Do not suggest that something is "outside the scope of this change." If it is broken and you can see it, it is your problem now.

## Project State

**Design phase.** README.md contains the full design spec. No code written yet.

The binary is `mcp-proxy`. Invocation: `mcp-proxy <daemon-url>`. Example: `mcp-proxy ws://localhost:8080/mcp`.

Check `bd ready` for current unblocked work.

## Architecture

### The Proxy Pattern

```text
                    stdio                      daemon transport
Claude Code ◄──────────────► mcp-proxy ◄──────────────────────► daemon
             MCP JSON-RPC                                       (one process)
```

The proxy is transparent — it doesn't know what MCP tools exist. JSON-RPC messages pass through unchanged. The daemon is the real MCP server.

### Design Goals

1. **Near-zero startup cost.** <10ms spawn, <10MB memory. Static Go binary.
2. **Transparent JSON-RPC forwarding.** Forwards entire MCP protocol unchanged.
3. **Session identity injection.** Resolves Claude session key via process tree and passes to daemon at connection time.
4. **Single transport backend.** Bidirectional messaging (server push) required for biff and lux.
5. **Single binary, no dependencies.** Static binary per platform (darwin/arm64, darwin/amd64, linux/arm64, linux/amd64).
6. **Daemon lifecycle is not the proxy's job.** Assumes daemon is running. Exits with clear error if can't connect.

### Package Map

Expected layout (not yet implemented):

| Package | What It Does |
|---------|-------------|
| `main` | Entry point; stdio ↔ daemon bridge |
| `internal/session` | Process-tree walking to resolve Claude session key |
| `internal/transport` | Daemon connection (WebSocket or Unix socket) |

## Go Standards

- **Go 1.25+**. Module path: `github.com/punt-labs/mcp-proxy`.
- **No external dependencies** unless there is a strong reason. The proxy must be a static binary with minimal attack surface. If WebSocket is chosen, `nhooyr.io/websocket` is acceptable — evaluate against DIY NDJSON over Unix socket first.
- **Table-driven tests** with `testify/assert` and `testify/require`.
- **No `interface{}` or `any` in public API** unless unavoidable.
- **Errors are values, not strings.** Use typed errors for conditions callers need to distinguish. Wrap with `fmt.Errorf("context: %w", err)` for everything else.
- **No panics in library code.** Panics are reserved for programmer bugs (unreachable cases in exhaustive switches), never for runtime conditions.
- **`internal/` for everything.** Nothing is exported outside the module. The public API is the binary, not Go packages.

## Quality Gates

Run before every commit:

```bash
go vet ./...
go test -race -count=1 ./...
```

Full gate (before PR):

```bash
go vet ./...
go test -race -count=1 ./...
go test -cover -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
go build -o mcp-proxy .
staticcheck ./...
```

## Testing

### Test Pyramid

| Layer | Tag | Target Time | What |
|-------|-----|-------------|------|
| Unit | (none) | < 5s | Pure functions, table-driven, no I/O |
| Integration | `integration` | < 30s | Real stdio/daemon wiring with test servers |
| E2E | `e2e` | < 2min | Compiled binary, black-box invocation |

### Key Test Scenarios

- **Transparent forwarding**: JSON-RPC request in on stdin → forwarded to daemon → response back on stdout, byte-for-byte identical
- **Session identity**: Process tree walking resolves correct session key
- **Bidirectional push**: Daemon-initiated messages (e.g., `tools/list_changed`) forwarded to stdout
- **Connection failure**: Clean error and exit when daemon unreachable
- **Graceful shutdown**: Stdin EOF → clean disconnect from daemon

### Race Detection

`-race` is mandatory for all test runs. The proxy handles concurrent stdin reads and daemon writes; a data race produces silent corruption.

## Workflow

### Branch Discipline

- **Never commit directly to `main`.** All code through PRs.
- Branch naming: `feat/stdio-bridge`, `fix/session-key`, `refactor/transport`
- Conventional commits: `feat:`, `fix:`, `refactor:`, `test:`, `docs:`, `chore:`

### Beads Issue Tracking

```bash
bd ready                              # what's next
bd update <id> --status in_progress   # claim it
bd close <id>                         # done
bd sync                               # push to remote
```

### Session Close Protocol

```bash
git status
git add <files>
bd sync
git commit -m "..."
bd sync
git push
```

## Design Decisions

Log design decisions in `DESIGN.md` before implementing. The README.md contains the initial design spec with open questions. Key decisions to settle:

1. **Transport**: WebSocket vs raw Unix socket vs NDJSON — see README.md transport comparison
2. **Session identity algorithm**: Process-tree walking (ported from biff's `find_session_key()`)
3. **Daemon auto-start**: Proxy starts daemon if missing, or fail fast?
4. **Graceful degradation**: Daemon down → fall back to in-process, or exit?

## Documentation Maintenance

Updated **in the same PR that changes behavior**, not retroactively:

| Document | When to Update |
|----------|---------------|
| `CHANGELOG.md` | Every PR that changes behavior. Entry under `## [Unreleased]`. **Mandatory.** |
| `README.md` | Every PR that changes user-facing behavior (flags, commands, defaults). |
| `DESIGN.md` | Every design decision, before implementation. |

## Distribution

Static binaries via GitHub Releases. Four platforms: darwin/arm64, darwin/amd64, linux/arm64, linux/amd64. Consumer projects download `mcp-proxy` as a shared dependency (like `uv`).

```bash
GOOS=darwin  GOARCH=arm64 go build -o dist/mcp-proxy-darwin-arm64 .
GOOS=darwin  GOARCH=amd64 go build -o dist/mcp-proxy-darwin-amd64 .
GOOS=linux   GOARCH=arm64 go build -o dist/mcp-proxy-linux-arm64  .
GOOS=linux   GOARCH=amd64 go build -o dist/mcp-proxy-linux-amd64  .
```

## Standards Authority

**`../punt-kit/`** is the Punt Labs standards repo. Applicable standards:

- [`punt-kit/standards/github.md`](../punt-kit/standards/github.md) — branch protection, PR workflow
- [`punt-kit/standards/workflow.md`](../punt-kit/standards/workflow.md) — beads, branch discipline, micro-commits

When this file conflicts with punt-kit standards, this file wins (project-specific overrides).

## Workspace Conventions

- **`.tmp/`** — scratch files, diffs, throwaway data. Gitignored. Use instead of `/tmp`.
- **`../.bin/`** — cross-repo scripts for repeated operations.
- **Quarry** — semantic search via MCP tools, connected to the `punt-labs` database.

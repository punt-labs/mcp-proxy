# mcp-proxy

Lightweight Go proxy bridging MCP stdio transport to shared daemon processes.

## Principal Engineer Mindset

There is no such thing as a "pre-existing" issue. If you see a problem — in code you wrote, code a reviewer flagged, or code you happen to be reading — you fix it. Do not classify issues as "pre-existing" to justify ignoring them. Do not suggest that something is "outside the scope of this change." If it is broken and you can see it, it is your problem now.

## Project State

**Core implemented.** Bidirectional stdio-to-WebSocket bridge with session identity, debug logging, signal handling, automatic reconnect with backoff, MCP handshake replay, WebSocket keepalive, health check, and hook relay mode. The proxy is transport-agnostic: it works against any daemon that speaks MCP JSON-RPC over a WebSocket endpoint.

The binary is `mcp-proxy`. Invocation: `mcp-proxy <daemon-url>`. Health check: `mcp-proxy --health <daemon-url>`. Hook relay: `mcp-proxy <daemon-url> --hook <event>`.

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
4. **Single transport backend.** Bidirectional messaging (server-initiated notifications such as `notifications/tools/list_changed`, interaction events) requires a persistent connection.
5. **Single binary, no dependencies.** Static binary per platform (darwin/arm64, darwin/amd64, linux/arm64, linux/amd64).
6. **Daemon lifecycle is not the proxy's job.** Assumes daemon is running. Exits with clear error if can't connect.

### Package Map

| Package | What It Does |
|---------|-------------|
| `main` | Entry point: parse args, health check, hook relay, reconnecting proxy, signal handling |
| `internal/bridge` | Bidirectional stdin↔WebSocket forwarding (two goroutines + WaitGroup) |
| `internal/config` | TOML profile loader for `~/.punt-labs/mcp-proxy/<profile>.toml` (URL + headers), permissions-checked |
| `internal/hook` | One-shot JSON-RPC relay for hook scripts (sync request/response, async notification) |
| `internal/reconnect` | Reconnecting bridge: stdin channel, per-connection goroutines, backoff, MCP handshake replay |
| `internal/transport` | WebSocket dial with typed errors, session key injection, bearer token auth |
| `internal/session` | Process-tree walking to resolve Claude session key |
| `internal/debuglog` | Structured `slog` debug logging via `MCP_PROXY_DEBUG` env var |
| `internal/testutil` | Mock daemon (`httptest.Server` + WebSocket), stdio pipe helpers |
| `internal/e2e` | Black-box binary tests (build tag `e2e`) |
| `internal/integration` | Real daemon roundtrip tests (build tag `integration`) |

## Go Standards

- **Go 1.25+**. Module path: `github.com/punt-labs/mcp-proxy`.
- **No external dependencies** unless there is a strong reason. The proxy must be a static binary with minimal attack surface. If WebSocket is chosen, `github.com/coder/websocket` is acceptable — evaluate against DIY NDJSON over Unix socket first.
- **Table-driven tests** with `testify/assert` and `testify/require`.
- **No `interface{}` or `any` in public API** unless unavoidable.
- **Errors are values, not strings.** Use typed errors for conditions callers need to distinguish. Wrap with `fmt.Errorf("context: %w", err)` for everything else.
- **No panics in library code.** Panics are reserved for programmer bugs (unreachable cases in exhaustive switches), never for runtime conditions.
- **`internal/` for everything.** Nothing is exported outside the module. The public API is the binary, not Go packages.

## Ethos & Delegation

Identity: `agent: claude` per `.punt-labs/ethos.yaml`. Sub-agent calls (`Agent(subagent_type=…)`) match ethos identity handles.

mcp-proxy is a Go static binary that sits on the trust boundary between Claude Code and shared daemons. Three concerns dominate: byte-for-byte transparent forwarding, session-identity injection, and zero-dependency build hygiene. Within each row, the worker and evaluator must be distinct handles. Claude is the leader, never the evaluator.

| Task type | Worker | Evaluator |
|-----------|--------|-----------|
| Bridge / transport / stdio↔WS forwarding | `bwk` (Kernighan) | `rop` (Pike) |
| Reconnect, backoff, signal handling | `bwk` | `rop` |
| Session-key resolution / process-tree walking | `bwk` | `djb` (Bernstein) |
| Auth / bearer token / WS upgrade trust path | `djb` | `bwk` |
| CLI flag surface / hook relay UX | `mdm` | `rop` |
| Cross-platform build matrix / static binary release | `adb` (Lovelace) | `bwk` |
| Race-condition / concurrency review (`-race`) | `bwk` | `djb` |
| Integration with a downstream daemon (its wire contract, session-key handling, hook endpoint) | `bwk` | the owning daemon's language specialist (`rmh` for Python daemons, `mdm` for Go, etc.) |

Note: `bwk` (Kernighan) covers Go implementation; `rop` (Pike) and `mdm` (McIlroy) cover CLI/Unix surface and pipe correctness. For pure-Go internals, prefer `bwk` worker / `rop` evaluator. Use the `quick` pipeline for surgical bridge fixes; `standard` for any change touching the wire format or session-identity contract.

### Mission Workflow

Every non-trivial code change goes through an ethos mission. The leader (Claude) writes contracts, dispatches workers, reviews, and closes; the leader **does not write code** in the write set. Direct-authored files are limited to `CLAUDE.md`, `DESIGN.md`, `README.md`, `CHANGELOG.md`, memory files, and mission-contract YAMLs.

**Pipelines** (list via `ethos mission pipeline list`):

| Pipeline | Stages | Use |
|----------|--------|-----|
| `quick`    | 2 | Single-module bugfix, well-understood change |
| `standard` | 5 | Feature or refactor: design → implement → test → review → document |
| `formal`   | 7 | Anything touching the Z spec or bridge invariants |
| `coe`      | 5 | Cause-of-error investigation for a recurring bug or incident |
| `coverage` | 3 | Targeted test-coverage improvement |
| `full`     | 9 | Complete lifecycle including product validation and retrospective |
| `product`  | 6 | New user-visible feature with Working Backwards up front |
| `docs`     | 2 | Doc-only change with review |

For mcp-proxy, `standard` is the default for anything touching CLI surface, transport, session identity, or reconnect. `quick` for surgical fixes inside a single package where the write set is obvious.

**Dispatch is two operations, never one.** `ethos mission create` writes the contract; a separate `Agent(subagent_type=<worker>, run_in_background=true)` spawns the worker. Skipping the Agent call leaves the contract orphaned — nothing happens.

```bash
# Author a contract, then dispatch
ethos mission create --file .tmp/missions/<name>.yaml     # writes contract, returns mission ID
# then, from the leader session:
Agent(subagent_type=<worker>, run_in_background=true)     # spawns worker; verify via TaskList

# Track
ethos mission show <id>
ethos mission log <id>
ethos mission results <id>

# Close
ethos mission close <id>                                  # pass
ethos mission reflect <id> --file <path>                  # needs another round
ethos mission advance <id>
ethos mission close <id> --status failed                  # fail
```

Mission contract YAMLs go in `.tmp/missions/`. Worker result artifacts go in `.tmp/missions/results/`.

**Between design and implementation, the leader MUST review the design and escalate substantive issues to the operator before dispatching implementation.** A substantive issue is anything that deviates from the operator's stated structure or goals, introduces a layering violation, breaks an element-purity or trust-model invariant, creates a naming conflict, or would cost more to fix in implementation than in the design. Review means: read the merged design end-to-end, cite `file:line` for each issue, write a concrete "recommend X" alternative, present all issues to the operator with an explicit ASK per issue, and **wait** for ratification. Do not dispatch implementation while a "should we discuss" question is outstanding.

**Every implementation mission contract MUST include commit-per-step in its success criteria.** Required criterion text:

> "Commit incrementally — one commit per logical step (file group, single concern, or single PR-equivalent slice). Each commit must pass `make check`. Do not accumulate more than 30 minutes of uncommitted changes."

**Watch for stuck workers by filesystem, not commits.** Assess progress by reading working-tree edits (`git status`, `git diff`, reading the files) — is code changing and advancing? Only a genuine filesystem stall (no edits changing over a long window) with an unresponsive worker justifies `SendMessage` for status and, as a last resort, taking over. Do not kill an agent for not committing, and do not commit its work by proxy while it is actively editing — let workers commit and push their own work on their own timeline.

**Sub-agent calls always run in the background.** Every `Agent(subagent_type=…)` invocation uses `run_in_background=true`. Zero exceptions. Set `subagent_type` to the ethos identity handle from the delegation table (`bwk`, `mdm`, `rop`, `djb`, `adb`, `rmh`) — bare `Agent` calls spawn an empty shell with no personality or expertise.

**Review-cycle fix rounds** (Copilot / Bugbot findings, mechanical lint sweeps where the write set is obvious) use bare `Agent(subagent_type=<handle>)` instead of a mission. Missions are for design work and multi-step implementations; a review round on a well-scoped diff does not need the ceremony.

## Quality Gates

Run before every commit:

```bash
make check
```

The Makefile is the source of truth for what `check` means (`make help` to see all targets). Expands to `make lint lint-strict vulncheck docs test`:

- `lint` — `go vet` + `staticcheck`
- `lint-strict` — `golangci-lint` (errcheck, gosec, revive, bodyclose, errorlint, misspell, ineffassign, staticcheck, govet, unused, plus gofumpt + goimports formatters — config in `.golangci.yml`)
- `vulncheck` — `govulncheck` against the Go vulnerability database
- `docs` — `markdownlint-cli2`
- `test` — `go test -race -count=1 ./...`

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

### Code Review

Copilot auto-reviews every push via branch ruleset (`review_on_push: true`). No manual review request needed.

1. **Create PR** via `gh pr create`. Include summary and test plan.
2. **Background watch** — immediately run `sleep 5 && gh pr checks <number> --watch --fail-fast` in background. Do not block the main shell. Copilot and Bugbot may take 1–3 minutes after CI completes.
3. **Read all feedback** when background watch completes:
   - `gh pr reviews <number>` — check review verdicts
   - `gh api repos/punt-labs/mcp-proxy/pulls/<number>/comments` — read inline comments
   - `gh pr checks <number>` — verify all checks green
4. **Take every comment seriously.** If a reviewer flags it, fix it. No "pre-existing" or "out of scope" excuses.
5. **Fix, re-push, repeat.** Each push triggers a new Copilot review cycle. Expect **2–6 review cycles** before merging. Run `make check` before each push.
6. **Merge only when the last cycle is uneventful** — zero new comments, all checks green. Use `gh pr merge <number> --squash --delete-branch`.

The entire PR cycle (create → review → fix → merge) should be autonomous. Do not require user intervention to land a clean PR.

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

Log design decisions in `DESIGN.md` as new `DES-###` entries before implementing. `DESIGN.md` is the authoritative record — every architectural choice, alternatives considered, and outcome. Every design change consults this log first and does not revisit a settled decision without new evidence.

The core decisions are settled (transport, session identity, concurrency, message format, daemon lifecycle, debug logging, auth, signal handling, reconnect, health check, hook relay, deadline-based stdin reads, keepalive, static build, URL canonicalization, handshake replay — DES-001 through DES-016). Any new work adds a new DES-* entry with rationale before touching code.

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
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o dist/mcp-proxy-darwin-arm64 .
CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -o dist/mcp-proxy-darwin-amd64 .
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -o dist/mcp-proxy-linux-arm64  .
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o dist/mcp-proxy-linux-amd64  .
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
@.punt-labs/ethos/CLAUDE.md

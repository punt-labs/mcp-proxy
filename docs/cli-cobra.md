# CLI Migration to Cobra

**Status:** proposed — awaiting operator ratification before implementation dispatch.
**Bead:** `mcp-proxy-6cx`
**Mission:** `m-2026-08-28-012`

This document specifies the migration of `mcp-proxy`'s hand-rolled `parseArgs`
(main.go:57-142) to [`spf13/cobra`](https://github.com/spf13/cobra). The
migration is a CLI-framework refactor: the wire format, transport, session
identity, reconnect, and MCP handshake replay are all unchanged.

Authority:

- `punt-kit/standards/cli.md` — Layer 1 / Layer 2 / diagnostics-tier structure
  (cli.md:22-102), the cobra mandate for Go CLIs (cli.md:130-133), root-command
  shape (cli.md:145-162), and global-flag conventions (cli.md:354-431).
- `punt-kit/standards/go.md` — module layout (`cmd/<binary>/main.go`,
  go.md:65-92), build flags (go.md:497-527), static-binary invariant
  (go.md:516).
- `DESIGN.md` DES-010, DES-011, DES-014 — existing behavior contracts that
  must survive.
- `main.go:26-41, 57-142` — current usage string and parser.

---

## 1. Command layout

### Shape

**A single naked root command that runs the MCP bridge, plus one Layer-2
subcommand (`version`) for standards alignment.** All three operational
modes (bridge, health, hook) remain accessible via flags on the root
command; no subcommands for the modes.

Rationale (with citations):

- `cli.md:36-44` distinguishes noun-first tools (multiple nouns to act on)
  from **single-verb tools** ("reserved for genuinely single-noun tools").
  mcp-proxy has one noun (a WebSocket bridge) and one verb (forward). A
  Layer-1 "run" subcommand would add ceremony without adding vocabulary.
- `cli.md:158-162` says "Running the binary without a subcommand prints
  help --- do not default to a subcommand like `serve`." This is the
  deviation the design has to name. **Justification:** mcp-proxy is not
  a user-facing tool — it is spawned by Claude Code from a fixed
  `command: mcp-proxy` config (README.md:64) and by hook shell scripts
  from a fixed argv. Requiring a subcommand at position 1 would break
  every downstream config on the disk in one release. The standard's
  rule is calibrated for tools with a Layer-1 vocabulary; mcp-proxy has
  none, so the "no default subcommand" rule and the "single-verb tool"
  allowance point in opposite directions. This design follows the
  single-verb allowance and documents the deviation here.
- `cli.md:80-88` lists `version` as a required Layer-2 subcommand for
  every CLI. Add `mcp-proxy version` (output: `mcp-proxy <semver>`,
  cli.md:441-448). Keep `--version` as an alias flag on the root so
  the current invocation shape stays green.
- Health check maps to the **diagnostics tier** (cli.md:47-52) — a
  read-only, safe-to-run singleton. `mcp-proxy --health` stays a flag,
  not a subcommand, so `--health` and `--hook` behave symmetrically
  and hook scripts do not need to learn a new argv shape. A hidden
  `mcp-proxy health` subcommand alias is deferred (see §7).
- Hook mode is the **Layer 3 hook dispatcher** (cli.md:89-100), which
  the standard describes as internal and de-emphasized in `--help`.
  `--hook <event>` is kept as a flag rather than a subcommand for the
  same reason as health: the current shell scripts calling
  `mcp-proxy <url> --hook <event>` (README.md:79) must keep working
  byte-for-byte.

### `mcp-proxy --help` output

```text
mcp-proxy: bridge MCP stdio to a shared daemon over WebSocket.

Usage:
  mcp-proxy [flags] [daemon-url]
  mcp-proxy version

The default mode is the long-running MCP bridge: JSON-RPC on stdin/stdout
is forwarded to the daemon over WebSocket. --health performs a one-shot
liveness dial. --hook forwards a single event payload from stdin to the
daemon's /hook endpoint.

Flags:
      --config profile     Load URL and headers from
                           ~/.punt-labs/mcp-proxy/<profile>.toml
      --health             Health check: dial, close, exit 0/1
      --hook event         Hook relay: send stdin as JSON-RPC hook/<event>
      --async              With --hook: send as notification, no response
      --version            Print the version and exit
  -h, --help               Show this help and exit

Subcommands:
  version                  Print the version and exit

Environment:
  MCP_PROXY_TOKEN          Bearer token added to the WebSocket upgrade
  MCP_PROXY_DEBUG          1|true to log to $TMPDIR/mcp-proxy-<pid>.log,
                           any other value is treated as a file path
  MCP_PROXY_PING_INTERVAL  WebSocket ping cadence (default 5s)
  MCP_PROXY_PONG_TIMEOUT   Pong wait after each ping (default 2s)

Examples:
  mcp-proxy ws://localhost:8420/mcp
  mcp-proxy --config quarry
  mcp-proxy --health ws://localhost:8420/mcp
  mcp-proxy --config quarry --health
  mcp-proxy ws://localhost:8420 --hook PreToolUse < payload.json
  mcp-proxy --config quarry --hook --async SessionEnd < payload.json

Exit codes:
  0  Success
  1  Runtime error (dial failure, daemon error response, unclean shutdown)
  2  Usage error (unrecognized flag, missing required argument)
```

The examples block mirrors the shape currently in main.go:32-40, so
`--help` remains directly comparable against the pre-migration usage
string. Cobra's default `--help` template is customized via
`SetHelpTemplate` to produce plain text (cli.md:171 — "Plain text
output --- disable typer's rich markup for help screens"; Go analog:
suppress cobra's ANSI/color output and format sections manually).

### Rejected shape 1: `mcp-proxy run|health|hook` subcommands

Cleanest cobra-idiomatic layout: one subcommand per mode. Rejected
because every existing consumer (README.md:64 Claude Code plugin
config, README.md:79 hook script pattern, DES-010 launchd
integration) is pinned to the current argv shape. A subcommand
migration would break each of them in the same release, requiring
lock-step updates across quarry, biff, vox, lux, and every operator's
launchd/systemd unit file. The saving — a slightly cleaner cobra
tree — is not worth the coordination cost.

### Rejected shape 2: `mcp-proxy bridge <url>` as default when no subcommand

Requires a runtime dispatcher that peeks at argv, notices the absence
of a subcommand name, and injects `bridge` before cobra parses. The
result is a hand-rolled preprocessor sitting in front of cobra —
exactly the code cobra was supposed to replace. Rejected.

---

## 2. Argument-order preservation

Today's parser accepts URL and `--hook <event>` in either order
(main.go:102-116) and treats the URL as optional when `--config`
supplies one (main.go:86-99 for health, main.go:112-115 for hook).

### How cobra handles the ordering

Cobra parses flags before positional arguments by default: **every
non-flag token is a positional; every `--flag` (or `--flag=value`) is
a flag, regardless of position.** So all four forms below map to the
same parsed state under a single root command that declares
`--config` (string), `--health` (bool), `--hook` (string), `--async`
(bool) and accepts `Args: cobra.MaximumNArgs(1)`:

| Invocation                                                | Positional  | `--config` | `--health` | `--hook`     | `--async` |
| --------------------------------------------------------- | ----------- | ---------- | ---------- | ------------ | --------- |
| `mcp-proxy ws://x --hook PreToolUse`                      | `ws://x`    | ""         | false      | `PreToolUse` | false     |
| `mcp-proxy --hook PreToolUse ws://x`                      | `ws://x`    | ""         | false      | `PreToolUse` | false     |
| `mcp-proxy --health ws://x`                               | `ws://x`    | ""         | true       | ""           | false     |
| `mcp-proxy --config quarry --health`                      | *(none)*    | `quarry`   | true       | ""           | false     |
| `mcp-proxy --config quarry --hook --async SessionEnd`     | *(none)*    | `quarry`   | false      | `SessionEnd` | true      |

The order-independence in the current parser is emulated by hand
(main.go:102-108's `hookIdx` search) precisely because the stdlib
`flag` package requires flags before positionals. Cobra is built on
pflag, which uses GNU-style interleaving — the two-branch hack goes
away.

### URL resolution after parsing

Resolution order stays exactly as main.go:172-181: positional URL,
then config profile URL, then `config.DefaultURL`. The `RunE` on the
root command runs this resolution once and dispatches to one of
three functions that map 1:1 onto today's private helpers:

```text
positional URL present  →  daemonURL = positional
else if config.URL      →  daemonURL = config.URL
else                    →  daemonURL = config.DefaultURL
```

`--health` and `--hook` without a positional URL are legal **only
when `--config` is set**, matching main.go:94-97 and main.go:112-115.
This is enforced in `RunE` (see §4), not in cobra's `Args` predicate,
because the requirement depends on flag interaction (the presence of
`--config`) rather than argv length alone.

### `--hook` `<event>` argument capture

Current: `--hook` is a bare flag; the event token follows either
positionally (main.go:116-124) or after `--async`. Cobra: `--hook`
becomes a **string-valued flag**: `--hook <event>`. The current
`mcp-proxy <url> --hook PreToolUse` re-parses as
`--hook=PreToolUse <url>` under cobra's `StringVar`, since pflag
accepts a following token as the flag value. This is a behavior
match, not a shape change — the tokens on the command line are
identical.

`--async` remains a bare bool flag paired with `--hook`. Validated in
`RunE`: if `--async` is set and `--hook` is empty, return
`errors.New("--async requires --hook")` (exit 2).

---

## 3. Flag surface

All flags are **local flags on the root command**. mcp-proxy has no
subcommand tree that would benefit from persistent flags. Cobra's
`RootCmd.Flags()` (not `PersistentFlags()`) is the right receiver.

| Flag         | Short | Type   | Default | Env fallback              | Scope | Source                     |
| ------------ | ----- | ------ | ------- | ------------------------- | ----- | -------------------------- |
| `--help`     | `-h`  | bool   | —       | —                         | root  | cobra built-in             |
| `--version`  |       | bool   | false   | —                         | root  | main.go:70-71, mission     |
| `--config`   |       | string | ""      | —                         | root  | main.go:64-69, README:81   |
| `--health`   |       | bool   | false   | —                         | root  | main.go:87-99, DES-010     |
| `--hook`     |       | string | ""      | —                         | root  | main.go:101-126, DES-011   |
| `--async`    |       | bool   | false   | —                         | root  | main.go:117-120, DES-011   |

Env-var-driven runtime knobs (`MCP_PROXY_TOKEN`, `MCP_PROXY_DEBUG`,
`MCP_PROXY_PING_INTERVAL`, `MCP_PROXY_PONG_TIMEOUT`) are **not
promoted to flags**. They are read directly by their consumers
(transport, debuglog, reconnect). Promotion would require plumbing
each one through `main` and would violate `cli.md`'s "help text IS
the manual" principle only if the env vars were undocumented — they
are documented in the `--help` output above and in README.md:94.

### Standard-required flags omitted, with reasons

`cli.md:356-364` lists four global flags every CLI supports:
`--json`, `--verbose`/`-v`, `--quiet`/`-q`, `--help`/`-h`. Three of
the four do not fit mcp-proxy:

- **`--json`.** mcp-proxy has no human-readable output that could be
  reformatted as JSON. Its stdout is either the opaque MCP JSON-RPC
  stream (DES-004, DESIGN.md:159-179) or, in `--hook` mode, the
  daemon's raw response payload (DES-011, DESIGN.md:517-526). Its
  stderr carries at most one line per invocation
  (`mcp-proxy: ok`, `mcp-proxy: <error>`). A `--json` flag would
  either corrupt the protocol stream or reformat single-line
  diagnostics into `{"error":"..."}` — the latter is meaningful but
  buys little for a binary that never composes into another CLI's
  pipeline. **Considered exception** per cli.md:56 ("Assess
  omissions, not inclusions"). Reason: no non-opaque output surface.
- **`--verbose` / `-v`.** DES-006 (DESIGN.md:205-236) already
  specifies file-based debug logging via `MCP_PROXY_DEBUG` because
  stdout is the data channel and stderr is captured by some MCP
  clients. Adding `--verbose` would either duplicate the env var
  (two ways to do the same thing) or contradict DES-006 by writing
  to stderr. **Considered exception.** Reason: DES-006 already
  settled the diagnostic-output channel.
- **`--quiet` / `-q`.** mcp-proxy's stderr output is already limited
  to error diagnostics — there is nothing to suppress. **Considered
  exception.** Reason: no non-essential stderr surface.
- **`--help` / `-h`.** Kept. Cobra provides it automatically; this
  closes the current bug where `mcp-proxy --help` treats `--help` as
  a URL and enters the reconnect loop (mission-brief bullet).

The three omissions above are the design's answer to the standard's
"list the omissions and demand justification for each" instruction
(cli.md:56). If a future consumer needs any of them, they can be
added without protocol changes.

### `version` subcommand output

```text
$ mcp-proxy version
mcp-proxy 0.7.3
```

Matches cli.md:442-448. Reads the same `main.version` variable
already set by `-ldflags -X main.version=$(VERSION)` (Makefile:4-5).
The `--version` flag prints the identical line and exits 0
(main.go:151-154 preserved verbatim).

---

## 4. Exit-code preservation

Contract (mission brief, main.go:44/148/153/244/276/296):

- `0` = clean shutdown, health OK, hook OK
- `1` = runtime error, health fail, daemon error response
- `2` = usage error (bad flags, missing required argument)

### How each code stays honest under cobra

Cobra's default is to print the error message returned from `RunE`
and exit **1** for any error. That collapses codes 1 and 2. The
design uses three mechanisms to split them back apart:

1. **Root `SilenceErrors: true` and `SilenceUsage: true` on the root
   command.** Cobra will not print anything itself; `main.go`
   inspects the returned error, prints it, and picks the exit code.
2. **A typed `usageError` sentinel.** `RunE` returns `&usageError{msg}`
   when the argument state is invalid (both `--health` and `--hook`
   set, `--async` without `--hook`, `--hook`/`--health` with no URL
   and no `--config`, `--version` combined with other args per
   main.go:79-82). Cobra's own flag-parse failures also route to
   exit 2 by wrapping the error with `errors.Is` on
   `pflag.ErrHelp`-adjacent sentinels — the shim in `main` treats
   any pflag-emitted error as a usage error.
3. **Everything else = exit 1.** Runtime errors from
   `runProxy` / `runHealthCheck` / `runHook` are returned unchanged
   and printed with the `mcp-proxy: <error>` prefix
   (main.go:164/203/259/275/292 pattern preserved).

Sketch (illustrative; the implementation mission writes the code):

```text
main:
    err := rootCmd.Execute()
    switch {
    case err == nil:                     os.Exit(0)
    case isUsageError(err):              print usage summary; os.Exit(2)
    default:                             print "mcp-proxy: err"; os.Exit(1)
    }
```

Special cases:

- `--help` / `-h`: cobra prints help and returns nil → exit 0. This
  is the bug fix: today's parser passes `--help` through as a URL
  positional and enters the reconnect loop.
- `--version` and `version` subcommand: both print
  `mcp-proxy <semver>` and exit 0. `--version` is wired via a small
  `RunE` bypass that short-circuits before the mode dispatch.
- SIGINT/SIGTERM double-signal handling (DES-008): unchanged. Signal
  handling is inside `runProxy`, not at the cobra layer. The first
  signal cancels context and produces a nil error → exit 0. The
  second signal calls `os.Exit(1)` directly (main.go:307).
- `runHook` returning `hook.ErrDaemonError` (main.go:288-294) still
  produces exit 1 with the error already printed to stderr by
  `hook.Run` — cobra sees a non-nil error, `main` sees it's not a
  usage error, exits 1 without a second `mcp-proxy: ...` line.

---

## 5. Package layout

### Move `main.go` under `cmd/mcp-proxy/`

`go.md:65-92` requires `cmd/<binary>/main.go` for every Go project;
rule 4 (go.md:91) requires cobra command trees to live in `cmd/`.
Today's repo has `main.go` at the module root — a pre-standard
layout. The migration is the right time to align.

Proposed layout:

```text
mcp-proxy/
  cmd/mcp-proxy/
    main.go        # os.Exit(rootCmd.Execute()); nothing else
    root.go        # cobra.Command definition, flags, RunE
    run.go         # runProxy, runHealthCheck, runHook (moved from main.go)
    version.go     # var version = "dev"; version subcommand
  internal/
    (unchanged)
  go.mod
  go.sum
```

Every function currently in main.go moves as follows:

| Current                           | Destination                        |
| --------------------------------- | ---------------------------------- |
| `main.go:43-45 main()`            | `cmd/mcp-proxy/main.go`            |
| `main.go:48-55 parsedArgs`        | deleted; replaced by cobra flags   |
| `main.go:57-142 parseArgs`        | deleted; replaced by cobra parser  |
| `main.go:144-191 run()`           | `cmd/mcp-proxy/root.go` (as RunE)  |
| `main.go:193-209 runHealthCheck`  | `cmd/mcp-proxy/run.go` (unchanged) |
| `main.go:211-246 runProxy`        | `cmd/mcp-proxy/run.go` (unchanged) |
| `main.go:248-297 runHook`         | `cmd/mcp-proxy/run.go` (unchanged) |
| `main.go:301-309 forceExitOn...`  | `cmd/mcp-proxy/run.go` (unchanged) |
| `main.go:315-330 envDuration`     | `cmd/mcp-proxy/run.go` (unchanged) |
| `main.go:24 version`              | `cmd/mcp-proxy/version.go`         |

### Build-target updates

Makefile:35 today builds `./` (root). It must change to
`./cmd/mcp-proxy/`. Both `build` and `dist` targets update. This is
mechanical and covered by the implementation mission.

The module path stays `github.com/punt-labs/mcp-proxy`. `go install`
consumers change from
`go install github.com/punt-labs/mcp-proxy@latest` to
`go install github.com/punt-labs/mcp-proxy/cmd/mcp-proxy@latest`.
This is a documented breaking change for `go install` users; the
`install.sh` binary path (README.md:38-39) is unaffected because it
downloads the pre-built binary.

### Rejected: keep main.go at repo root

Deviates from `go.md:65-92` and `go.md:91` for no gain other than
"less diff." Rejected. The migration is the natural point to align.

---

## 6. Dependency footprint

### The pin

Add exactly one direct dependency: **`github.com/spf13/cobra`
v1.10.1** (latest stable as of 2026-06). `go.mod` gains cobra as a
`require`, and its two direct transitive dependencies via `go.sum`:

- `github.com/spf13/pflag` — flag parsing (cobra's dependency)
- `github.com/inconshreveable/mousetrap` — Windows-only console
  detection; irrelevant to the darwin/linux build matrix
  (DES-014, DESIGN.md:714-741) but compiled into every binary

All three are single-purpose, MIT/Apache-licensed, and widely
vetted (used by kubectl, helm, docker, gh, hugo). No further
dependencies are pulled in. `spf13/viper` is **explicitly not
adopted** — the mission brief forbids it, and mcp-proxy has no
config-precedence problem for viper to solve
(config.go:44-112 is a 70-line TOML-and-permissions check that does
not benefit from viper's env/flag/file merging).

### Binary-size delta — assumption, not measurement

The mission's write set is `docs/cli-cobra.md` only, so this design
does not build binaries. The implementation mission must measure.
The **stated assumption** for cobra migrations in Go 1.25 with
`CGO_ENABLED=0 -ldflags="-s -w"`:

- cobra + pflag add roughly **1.0–1.5 MiB** to a stripped static
  Linux/amd64 binary. mcp-proxy today ships ~6 MiB (mission-brief
  proxy of the pre-cobra baseline; verify at implementation time).
- Post-migration size: **~7.0–7.5 MiB**. This stays within the
  distribution.md envelope for `~/.local/bin` tools and does not
  approach any GitHub-release attachment limit.
- Static-build invariant (DES-014): both cobra and pflag are pure
  Go with no cgo entry points. `CGO_ENABLED=0` remains enforceable
  by the same Makefile guard already in place.

The implementation mission must record the actual size delta in
CHANGELOG under `[Unreleased]` and confirm `file dist/mcp-proxy-linux-amd64`
still reports "statically linked" — same check DES-014 already
requires.

---

## 7. Migration risks

### Two flag-parse edge cases the current code handles

1. **Empty-string `--config` value.** main.go:65 explicitly rejects
   `--config ""` and `--config --something`. Cobra's `StringVar`
   accepts the empty string silently; the `RunE` guard has to
   re-add the rejection or the fallthrough runs Load("") and hits
   config.go:52's regex check instead of the argv-level "usage
   error" path. This is a two-line fix in `RunE`, but easy to miss.
2. **`--version` combined with other args.** main.go:78-83 rejects
   `--version --config x` and `--version ws://x` as usage errors.
   Cobra's built-in `Version` field on the root command does not
   enforce this — it prints and exits regardless of other flags.
   The design implements `--version` as a plain bool flag whose
   `RunE` handler enforces the exclusivity (returns `usageError` if
   `--config`, positional args, `--health`, or `--hook` are set).

Both are tractable. Neither is a reason to reject cobra.

### Test changes required

- `main_test.go`'s `parseArgs` table (every row exercising
  `parseArgs(...)`) is deleted with the function. Replacement:
  execute `rootCmd.SetArgs(argv)` + `rootCmd.Execute()` in tests
  that capture stdout/stderr via `SetOut`/`SetErr` and assert on
  the parsed flag values inside a test-only `RunE` shim, OR keep a
  thin `parseArgs`-shaped helper in `cmd/mcp-proxy/root.go` for
  direct table-driven tests.
- E2E tests (`internal/e2e`, build tag `e2e`) that invoke the
  compiled binary continue to work — the argv shapes are
  unchanged. But two **new** e2e cases are required (see §9):
  `mcp-proxy --help` exit code and `mcp-proxy --version` output.
- Any test that asserts on the exact usage string emitted to stderr
  on a parse failure will break. Cobra's failure output differs
  from main.go:147's `fmt.Fprint(os.Stderr, usage)`. Fix: pin the
  usage template with `SetUsageTemplate` so the emitted text is
  under test control (this is also what the §1 `--help` block
  demands).

### Distribution risk: `go install` path change

Documented in §5. Not a code risk; a communication risk. CHANGELOG
under `Changed` and README install-block update handle it.

### Risk not blocking: hidden subcommand aliases

The design could add hidden `mcp-proxy health` and
`mcp-proxy hook <event>` subcommands as forward-migration paths
for a future release that deprecates the flag surface. This is
**out of scope** for the current mission — implement flag-first,
add subcommand aliases later if a real need appears. Filed as a
follow-up.

---

## 8. Proposed DESIGN.md entry

To be inserted after DES-016. The next available number is
**DES-017** (verified against DESIGN.md's DES-001..DES-016
sequence).

```markdown
## DES-017: CLI Framework — Cobra

**Date:** 2026-08-28
**Status:** PROPOSED
**Topic:** Migrate hand-rolled flag parsing to spf13/cobra

### Design

Adopt `github.com/spf13/cobra` (v1.10.1) as mcp-proxy's CLI framework.
The root command runs the MCP bridge with `Args: cobra.MaximumNArgs(1)`.
`--health`, `--hook <event>`, `--async`, `--config <profile>`,
`--version`, and `--help` are local flags on the root command. One
Layer-2 subcommand (`version`) is added for standards alignment; every
existing argv shape from the pre-migration CLI keeps working.

Package layout moves from repo-root `main.go` to `cmd/mcp-proxy/`
per `punt-kit/standards/go.md` module-layout rules; Makefile build
paths update in the same PR.

### Why

Three drivers:

1. **Standards alignment.** `punt-kit/standards/cli.md` mandates
   cobra for Go CLIs. mcp-proxy is the last Punt Labs Go binary on
   hand-rolled `flag`.
2. **Bug fix.** Current parser treats `--help` as a URL and enters
   the reconnect loop (see docs/cli-cobra.md §1). Cobra fixes this
   for free.
3. **Order-independence.** Today's parser hand-emulates
   flag-anywhere positional handling (main.go:102-116). Cobra's
   pflag layer does this natively — deletes a class of edge cases.

Backward compatibility is preserved: every argv the current CLI
accepts continues to parse to the same behavior. Exit codes 0/1/2
retain their exact contract via `SilenceErrors: true`,
`SilenceUsage: true`, and a typed `usageError` sentinel in `main`.

### Rejected: keep stdlib `flag`

Falls behind the standard, misses the `--help` bug fix, and forces
mcp-proxy to keep growing custom parser code as flags are added.
The current parser is already 85 lines for six flag combinations
(main.go:57-142); each new mode adds another branch.

### Rejected: urfave/cli

Popular alternative to cobra. Rejected because `cli.md:130-133`
names cobra as the canonical Go framework, and no evidence
suggests urfave/cli's ergonomics buy anything cobra doesn't already
provide for a single-command binary.

### Rejected: alecthomas/kingpin

Effectively unmaintained (last release 2024, v2 archived).
Adopting a decaying dependency to save ~1 MiB of binary is a bad
trade against DES-014's static-build invariant.

### Rejected: alecthomas/kong

Struct-tag-driven flag parser; well-designed. Rejected on
consistency: every other Punt Labs Go CLI (ethos, biff-relay,
lux daemon shell) uses cobra. Divergence has an ongoing cost
(reviewers, contributors, cross-repo scaffolding) not paid back by
kong's smaller binary footprint.

### Rejected: no subcommands at all (root-command-only cobra)

Considered as a fifth option. Rejected because cli.md:80-88
requires `version` as a subcommand for every Layer-2-compliant
CLI; adding one subcommand costs nothing structural and lets
future admin verbs (`doctor`, `install`) land without another
framework migration.

### Trade-off accepted

- One new direct dependency (cobra); two new transitive
  dependencies (pflag, mousetrap-windows). All pure Go, all
  cgo-free. DES-014's `CGO_ENABLED=0` invariant is preserved.
- Estimated ~1.0–1.5 MiB stripped-binary size increase on
  linux/amd64. Confirmed at implementation time.
- `go install github.com/punt-labs/mcp-proxy@latest` becomes
  `go install github.com/punt-labs/mcp-proxy/cmd/mcp-proxy@latest`.
  Documented in the implementation PR's CHANGELOG entry and
  README.

### Related

- DES-010 — Health check flag (preserved verbatim)
- DES-011 — Hook relay mode (preserved verbatim)
- DES-014 — Static build enforcement (constrains dependency choice)
- Design doc: `docs/cli-cobra.md`
```

---

## 9. Test plan

### Existing tests that must change

| Test                                  | Change                                                             |
| ------------------------------------- | ------------------------------------------------------------------ |
| `parseArgs` table-driven tests        | Delete or retarget at cobra parse results (see §7)                 |
| Any test asserting the usage string   | Update golden or template-pin (`SetUsageTemplate`)                 |
| Unit tests importing from `main` pkg  | Repoint from `main` at repo root to `cmd/mcp-proxy`                |

### New tests required

Unit tests (in `cmd/mcp-proxy/root_test.go`):

- **Order independence.** Table with rows for each of the six
  argv variants in §2's table; assert on the parsed flag values.
- **`--config`/URL/default precedence.** Positional URL wins over
  config URL; config URL wins over `config.DefaultURL`.
- **Usage-error surface.** `--async` alone, `--version` with
  extra args, `--health` and `--hook` together, `--hook` with no
  URL and no `--config`, `--config` with empty string value.
  Each row asserts the returned error satisfies
  `errors.As(&usageError)`.
- **`--help` and `-h` return nil error.** No mode dispatch runs.
- **`--version` and `version` subcommand.** Both print
  `mcp-proxy <semver>\n` and return nil.

E2E tests (in `internal/e2e`, build tag `e2e`):

- **`mcp-proxy --help` exits 0.** The current bug fix. Assert
  stdout contains `Usage:` and exit code is 0.
- **`mcp-proxy --version` exits 0** and stdout matches
  `^mcp-proxy \S+\n$`.
- **`mcp-proxy version` (subcommand) exits 0** with the same
  stdout pattern.
- **`mcp-proxy --nonsense` exits 2** and stderr mentions
  `unknown flag`.
- **`mcp-proxy --config nope-does-not-exist --health`** exits 1
  (config load succeeds silently on ENOENT — config.go:66-70 —
  and dial fails against the default URL). This documents the
  current behavior; if the operator wants missing profiles to be
  fatal, that is a separate ADR.

Estimated additional test volume: ~150 lines of unit tests and
~40 lines of e2e tests. `make test` runtime impact is
negligible; cobra parsing runs in microseconds and no I/O is
added.

### `-race` mandate

Unchanged. Cobra's parser is not concurrent; the race-condition
surface is the bridge and reconnect packages, which are unchanged.

---

## 10. Rollout

### One PR, not split

The migration is a single self-contained change: move `main.go`
under `cmd/mcp-proxy/`, replace `parseArgs`, update Makefile
build paths, update tests, update README and CHANGELOG. Splitting
would leave the repo mid-refactor between merges, which is worse
than one reviewer holding the whole diff.

Estimated diff size: ~600 lines net (delete `parseArgs` and
`parsedArgs` = -85; add cobra root/commands = ~250; update tests
= ~150; move existing helpers = neutral; docs = ~50). Reviewable
in one pass.

### CHANGELOG entry (under `[Unreleased]`)

```markdown
### Changed

- CLI is now built on [cobra](https://cobra.dev/) instead of a
  hand-rolled `flag`-based parser. All existing argv shapes
  continue to work (`mcp-proxy <url>`, `mcp-proxy --health <url>`,
  `mcp-proxy <url> --hook <event>`, `mcp-proxy --config <profile>`,
  in either order). See DES-017.
- Source layout: the CLI entry point moved from repo-root
  `main.go` to `cmd/mcp-proxy/main.go`. Consumers who install
  from source must run
  `go install github.com/punt-labs/mcp-proxy/cmd/mcp-proxy@latest`.
  Binary consumers (release download, install.sh) are unaffected.

### Fixed

- `mcp-proxy --help` and `mcp-proxy -h` now print usage and exit 0.
  Previously, `--help` was treated as a URL positional and the proxy
  entered the reconnect loop.

### Added

- `mcp-proxy version` subcommand. Prints `mcp-proxy <semver>` and
  exits 0. `--version` continues to work identically.
```

### README changes required

1. Update the "Usage" table (README.md:77-81) to show the flag
   surface with cobra's `--help` output as the canonical source.
2. Update the `go install` line (if present) to point at
   `github.com/punt-labs/mcp-proxy/cmd/mcp-proxy`.
3. No changes required to the "MCP config" example (README.md:64)
   or the hook-script example (README.md:79) — argv shapes are
   preserved.

### DESIGN.md changes

Insert DES-017 (§8) with `Status: PROPOSED` on branch, flip to
`Status: SETTLED` in the implementation PR after operator
ratification.

### Downstream coordination

None required. Every daemon (quarry, biff, vox, lux) and every
hook script continues to invoke `mcp-proxy` with the same argv.
This is deliberate: the mission is a CLI-framework refactor, not
a UX change.

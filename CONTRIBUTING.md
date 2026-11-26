# Contributing

Development needs Go (the version in `go.mod`'s `toolchain` directive) and Node.js with `npx`, which `make docs` uses to run markdownlint. The Makefile fetches the pinned linters with `go run` on first use.

Commits are gated by the ethos commit-stage checks (`gofmt` and `go vet`, DES-019). Run the full quality gate before every push. It runs `lint`, `lint-strict`, `vulncheck`, `docs`, and `test`:

```bash
make check
```

The individual targets:

```bash
make lint
```

`go vet` and `staticcheck`.

```bash
make lint-strict
```

`golangci-lint`, configured in `.golangci.yml`.

```bash
make vulncheck
```

`govulncheck` against the Go vulnerability database.

```bash
make docs
```

`markdownlint-cli2` on every Markdown file.

```bash
make test
```

`go test -race -count=1 ./...`.

## Ethos handoff snapshot

The Linux amd64 snapshot runs the same full gate with private writable
caches and no network access beyond loopback. Install the runtime archive
from the [handoff-runtime release](https://github.com/punt-labs/mcp-proxy/releases/tag/handoff-runtime):

1. Find the Linux amd64 download URL and SHA-256 in
   `.punt-labs/ethos/checks/snapshots/go-default.yaml`.
2. Download the archive from that URL and check its SHA-256 against the
   checkout's pin.
3. Save the verified archive as `~/.punt-labs/ethos/runtimes/<sha256>.tar.gz`.
   Do not unpack it; Ethos verifies and prepares it.

Use an Ethos build that supports DES-086 worktree snapshots and managed data.
Run `ethos handoff refresh-data` from the repository to fetch the Go
vulnerability database outside the sandbox. Ethos mounts it read-only and
defers the sandbox check if the data is missing or more than 24 hours old.
The runtime archive does not include the database. If the snapshot is
unavailable, run `make check` live. Live and CI checks use
`https://vuln.go.dev` by default.

Once the workflow is on `main`, dispatch **Handoff runtime** from `main`
to refresh the bundle. Update the sidecar with the new release asset's name,
URL, and SHA-256, not the digest of a local rebuild. For a local build, use
upstream Linux x64 Node.js on `PATH` (as CI does), then run
`bash scripts/build-handoff-runtime.sh /path/to/runtime.tar.gz`.

## End-to-end tests

End-to-end tests build the binary and run it as a black box. They are not part of `make check`:

```bash
make test-e2e
```

Other targets:

```bash
make cover
```

Test coverage report.

```bash
make format
```

`gofmt -w .`.

```bash
make build
```

Builds `./mcp-proxy`.

```bash
make dist
```

Cross-compiles release binaries for darwin and linux on amd64 and arm64 into `dist/`.

`make help` lists every target. Design decisions are recorded in [DESIGN.md](DESIGN.md); read it before changing behavior.

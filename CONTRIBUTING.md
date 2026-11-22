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
